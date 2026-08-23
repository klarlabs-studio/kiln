// Package github talks to the forge.
//
// GitHub stays the forge: it owns pull requests, Checks and (usually) the
// registry. Kiln takes the compute. This package is therefore small and
// one-directional — it reports what Kiln did and asks two questions it cannot
// answer locally (is this pull request from a fork, and what commit is at its
// head). It implements no runner protocol and receives no work from Actions.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"go.klarlabs.de/fortify/retry"

	"go.klarlabs.de/kiln/internal/infrastructure/execx"
	"go.klarlabs.de/kiln/internal/infrastructure/obs"
)

// DefaultBaseURL is the public API. GitHub Enterprise installations override
// it through the client's BaseURL field.
const DefaultBaseURL = "https://api.github.com"

// Repo identifies a repository.
type Repo struct {
	Owner string
	Name  string
}

// String renders owner/name.
func (r Repo) String() string { return r.Owner + "/" + r.Name }

// Valid reports whether both halves are present.
func (r Repo) Valid() bool { return r.Owner != "" && r.Name != "" }

// ParseRepo parses "owner/name", tolerating a full URL or a .git suffix so an
// operator can paste whatever they have to hand.
func ParseRepo(s string) (Repo, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, ".git")
	if i := strings.Index(s, "github.com"); i >= 0 {
		s = s[i+len("github.com"):]
		s = strings.TrimLeft(s, ":/")
	}
	owner, name, ok := strings.Cut(strings.Trim(s, "/"), "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return Repo{}, fmt.Errorf("cannot read owner/name from %q", s)
	}
	return Repo{Owner: owner, Name: name}, nil
}

// DiscoverRepo resolves the repository from the git remote, falling back to
// GITHUB_REPOSITORY. The remote is preferred because it is what the operator
// actually configured; the variable exists for checkouts with no remote and
// for containers that were handed a bare tree.
func DiscoverRepo(ctx context.Context, r execx.Runner, dir, remote, envRepo string) (Repo, error) {
	if remote == "" {
		remote = "origin"
	}
	res, err := r.Run(ctx, execx.Cmd{
		Name: "git", Args: []string{"remote", "get-url", remote}, Dir: dir,
	})
	if err == nil {
		if repo, perr := ParseRepo(res.Output()); perr == nil {
			return repo, nil
		}
	}
	if envRepo != "" {
		return ParseRepo(envRepo)
	}
	return Repo{}, fmt.Errorf("no github repository: %q has no usable url and GITHUB_REPOSITORY is unset", remote)
}

// Client is a minimal GitHub REST client.
//
// Not go-github: Kiln uses four endpoints, and a dependency that large would
// pull in more surface than the whole rest of this package.
type Client struct {
	Token   string
	Repo    Repo
	BaseURL string
	HTTP    *http.Client
	Log     obs.Logger
	// Attempts bounds the retry of transient API failures.
	Attempts int
}

// NewClient builds a client. A nil HTTP client gets a sane timeout: an
// unbounded default would let a hung API call stall a watch tick forever.
func NewClient(token string, repo Repo, log obs.Logger) *Client {
	if log == nil {
		log = obs.Discard()
	}
	return &Client{
		Token:    token,
		Repo:     repo,
		BaseURL:  DefaultBaseURL,
		HTTP:     &http.Client{Timeout: 30 * time.Second},
		Log:      log,
		Attempts: 3,
	}
}

// Enabled reports whether the client can talk to GitHub at all. Without a
// token, Kiln posts no Checks and treats every pull request as a fork.
func (c *Client) Enabled() bool { return c != nil && c.Token != "" && c.Repo.Valid() }

// APIError is a non-2xx response.
type APIError struct {
	Status int
	Method string
	Path   string
	Body   string
}

func (e *APIError) Error() string {
	body := strings.TrimSpace(e.Body)
	if len(body) > 300 {
		body = body[:300] + "…"
	}
	return fmt.Sprintf("github %s %s: %d %s", e.Method, e.Path, e.Status, body)
}

// Retryable reports whether trying again could plausibly work. A 5xx is the
// API having a bad moment; a 429 is a rate limit that clears. A 401 or 404
// will say the same thing forever, and retrying a 422 on a check-run would
// just post the same invalid payload again.
func (e *APIError) Retryable() bool {
	return e.Status >= 500 || e.Status == http.StatusTooManyRequests
}

// CheckRun is the subset of a check run Kiln reads back.
type CheckRun struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// CreateCheckRun opens an in-progress check run for a commit.
func (c *Client) CreateCheckRun(ctx context.Context, name, sha string) (CheckRun, error) {
	payload := map[string]any{
		"name":       name,
		"head_sha":   sha,
		"status":     "in_progress",
		"started_at": time.Now().UTC().Format(time.RFC3339),
	}
	var out CheckRun
	err := c.do(ctx, http.MethodPost, c.path("check-runs"), payload, &out)
	if err != nil && isAppOnly(err) {
		// Distinguished from any other 403 because the caller's response is
		// different: not "fix your permissions" but "this token can never do
		// this, use statuses instead".
		return out, fmt.Errorf("%w: %w", ErrNeedsGitHubApp, err)
	}
	return out, err
}

// isAppOnly recognises GitHub refusing a non-App token on the Checks API.
func isAppOnly(err error) bool {
	return strings.Contains(err.Error(), "must authenticate via a GitHub App")
}

// CompleteCheckRun closes a check run with a conclusion and a summary.
//
// GitHub truncates output.summary at 65535 characters and rejects a longer
// one outright, so a verbose build log would fail the *reporting* of a build
// that otherwise succeeded. Truncating here keeps the Check honest.
func (c *Client) CompleteCheckRun(ctx context.Context, id int64, conclusion, title, summary string) error {
	payload := map[string]any{
		"status":       "completed",
		"conclusion":   conclusion,
		"completed_at": time.Now().UTC().Format(time.RFC3339),
		"output": map[string]any{
			"title":   truncate(title, 255),
			"summary": truncate(summary, 65000),
		},
	}
	return c.do(ctx, http.MethodPatch, c.path(fmt.Sprintf("check-runs/%d", id)), payload, nil)
}

// Pull is what Kiln needs to know about a pull request.
type Pull struct {
	Number int
	// HeadSHA is the commit to build.
	HeadSHA string
	// HeadRef is the source branch name.
	HeadRef string
	// Fork reports that the head lives in a different repository. This is the
	// input to the isolation policy, and getting it wrong in the permissive
	// direction hands a stranger the operator's credentials — so callers that
	// cannot reach the API must assume true, not false.
	Fork bool
	// Draft pull requests are still proven; the flag is reported so a caller
	// can choose to skip them.
	Draft bool
}

type pullPayload struct {
	Number int  `json:"number"`
	Draft  bool `json:"draft"`
	Head   struct {
		SHA  string `json:"sha"`
		Ref  string `json:"ref"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
}

// LookupPull fetches one pull request.
func (c *Client) LookupPull(ctx context.Context, number int) (Pull, error) {
	var p pullPayload
	if err := c.do(ctx, http.MethodGet, c.path(fmt.Sprintf("pulls/%d", number)), nil, &p); err != nil {
		return Pull{}, err
	}
	return Pull{
		Number:  p.Number,
		HeadSHA: p.Head.SHA,
		HeadRef: p.Head.Ref,
		Fork:    isFork(p),
		Draft:   p.Draft,
	}, nil
}

// isFork compares the head and base repositories.
//
// A deleted fork leaves head.repo null. That is treated as a fork, not as
// same-repo: the safe reading of "I cannot tell where this came from" is the
// one that withholds credentials.
func isFork(p pullPayload) bool {
	if p.Head.Repo == nil {
		return true
	}
	return !strings.EqualFold(p.Head.Repo.FullName, p.Base.Repo.FullName)
}

// ListOpenPulls returns the open pull requests, for watch discovery.
func (c *Client) ListOpenPulls(ctx context.Context) ([]Pull, error) {
	var payload []pullPayload
	if err := c.do(ctx, http.MethodGet, c.path("pulls?state=open&per_page=100"), nil, &payload); err != nil {
		return nil, err
	}
	out := make([]Pull, 0, len(payload))
	for _, p := range payload {
		out = append(out, Pull{
			Number:  p.Number,
			HeadSHA: p.Head.SHA,
			HeadRef: p.Head.Ref,
			Fork:    isFork(p),
			Draft:   p.Draft,
		})
	}
	return out, nil
}

// WhoAmI validates a token and returns the login it belongs to.
//
// A cheap round trip that turns "the token is wrong" from something discovered
// on the next scheduled tick, in a log nobody is reading, into something said
// at the moment it is pasted.
func WhoAmI(ctx context.Context, token string) (string, error) {
	// NewClient, not a hand-built struct: a literal here left HTTP nil and
	// BaseURL empty, and `kiln login` panicked on a nil-pointer dereference
	// before it ever reached the API. Step two of the three-command quick
	// start, on every path including the documented --with-token one.
	//
	// The repo is a placeholder — /user is not repository-scoped — but the
	// constructor requires one, and giving it a real-looking pair is cheaper
	// than a second constructor for the one call that needs no repo.
	return whoAmIAt(ctx, token, DefaultBaseURL)
}

// whoAmIAt is WhoAmI with the API base injectable, so the constructor path —
// the thing that was broken — is what the tests exercise.
func whoAmIAt(ctx context.Context, token, baseURL string) (string, error) {
	c := NewClient(token, Repo{Owner: "x", Name: "y"}, nil)
	c.BaseURL = baseURL

	var out struct {
		Login string `json:"login"`
	}
	if err := c.do(ctx, http.MethodGet, "/user", nil, &out); err != nil {
		return "", err
	}
	if out.Login == "" {
		// A fine-grained token with no user scope still authenticates; the
		// caller only needs to know the credential works.
		return "the token", nil
	}
	return out.Login, nil
}

// ErrNeedsGitHubApp reports the Checks API refusing a personal access token.
//
// Check runs can only be created by a GitHub App. Inside Actions this is
// invisible, because the GITHUB_TOKEN there *is* an app installation token —
// which is exactly why it surfaces the first time kiln runs somewhere else.
var ErrNeedsGitHubApp = errors.New("the checks api requires a github app")

// CreateStatus posts a commit status.
//
// The older sibling of check runs, and the one a personal access token is
// allowed to write. Branch protection accepts either as a required context, so
// for kiln's purpose — a name that can gate a merge — a status does the job
// with a plainer body.
func (c *Client) CreateStatus(ctx context.Context, sha, state, context, description string) error {
	payload := map[string]any{
		"state":       state,
		"context":     context,
		"description": truncateDescription(description),
	}
	path := fmt.Sprintf("/repos/%s/%s/statuses/%s", c.Repo.Owner, c.Repo.Name, sha)
	if err := c.do(ctx, http.MethodPost, path, payload, nil); err != nil {
		return fmt.Errorf("github: post status %q: %w", context, err)
	}
	return nil
}

// truncateDescription keeps within GitHub's 140-character limit, which it
// enforces by rejecting the request rather than trimming.
func truncateDescription(s string) string {
	s = strings.TrimSpace(strings.SplitN(s, "\n", 2)[0])
	const max = 140
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

// OpenPullRequest creates a pull request, or returns the open one that already
// exists for this head branch.
//
// Idempotent by head branch, which is what makes a repeating task safe: a
// daily remediation run pushes to the same branch and updates its existing
// pull request rather than opening one per day until somebody notices thirty
// of them.
func (c *Client) OpenPullRequest(ctx context.Context, head, base, title, body string) (Pull, bool, error) {
	if existing, found, err := c.pullForHead(ctx, head); err != nil {
		return Pull{}, false, err
	} else if found {
		return existing, false, nil
	}

	payload := map[string]any{"head": head, "title": title, "body": body}
	if base != "" {
		payload["base"] = base
	}

	var p pullPayload
	if err := c.do(ctx, http.MethodPost, c.path("pulls"), payload, &p); err != nil {
		return Pull{}, false, fmt.Errorf("github: open pull request for %s: %w", head, err)
	}
	return Pull{Number: p.Number, HeadSHA: p.Head.SHA, HeadRef: p.Head.Ref, Draft: p.Draft}, true, nil
}

// pullForHead finds an open pull request whose head is this branch.
func (c *Client) pullForHead(ctx context.Context, head string) (Pull, bool, error) {
	// Qualified with the owner, because GitHub's head filter matches
	// `owner:branch` and an unqualified name silently matches nothing.
	query := fmt.Sprintf("pulls?state=open&head=%s:%s", url.QueryEscape(c.Repo.Owner), url.QueryEscape(head))

	var payload []pullPayload
	if err := c.do(ctx, http.MethodGet, c.path(query), nil, &payload); err != nil {
		return Pull{}, false, fmt.Errorf("github: look for an existing pull request on %s: %w", head, err)
	}
	if len(payload) == 0 {
		return Pull{}, false, nil
	}
	p := payload[0]
	return Pull{Number: p.Number, HeadSHA: p.Head.SHA, HeadRef: p.Head.Ref, Draft: p.Draft}, true, nil
}

// LabelPull adds labels to a pull request. Labels are an issue-level concept
// in GitHub's API, which is why this is not a pulls/ path.
func (c *Client) LabelPull(ctx context.Context, number int, labels []string) error {
	if len(labels) == 0 {
		return nil
	}
	path := c.path(fmt.Sprintf("issues/%d/labels", number))
	if err := c.do(ctx, http.MethodPost, path, map[string]any{"labels": labels}, nil); err != nil {
		return fmt.Errorf("github: label pull request #%d: %w", number, err)
	}
	return nil
}

func (c *Client) path(suffix string) string {
	return fmt.Sprintf("/repos/%s/%s/%s", c.Repo.Owner, c.Repo.Name, suffix)
}

// do performs a request with retries, decoding a JSON response into out.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	if !c.Enabled() {
		return errors.New("github: no token or repository configured")
	}

	var encoded []byte
	if body != nil {
		var err error
		if encoded, err = json.Marshal(body); err != nil {
			return fmt.Errorf("github: encode %s %s: %w", method, path, err)
		}
	}

	attempts := c.Attempts
	if attempts <= 0 {
		attempts = 3
	}
	r := retry.New[[]byte](retry.Config{
		MaxAttempts:   attempts,
		InitialDelay:  time.Second,
		MaxDelay:      15 * time.Second,
		Multiplier:    2,
		BackoffPolicy: retry.BackoffExponential,
		Jitter:        true,
		IsRetryable:   retryableAPIError,
		OnRetry: func(attempt int, err error) {
			c.Log.Warn("retrying github call", "method", method, "path", path, "attempt", attempt, "err", err)
		},
	})

	data, err := r.Execute(ctx, func(ctx context.Context) ([]byte, error) {
		return c.once(ctx, method, path, encoded)
	})
	if err != nil {
		return err
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("github: decode %s %s: %w", method, path, err)
	}
	return nil
}

func (c *Client) once(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL()+path, reader)
	if err != nil {
		return nil, fmt.Errorf("github: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "kiln")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("github: read %s %s: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &APIError{Status: resp.StatusCode, Method: method, Path: path, Body: string(data)}
	}
	return data, nil
}

func (c *Client) baseURL() string {
	if c.BaseURL == "" {
		return DefaultBaseURL
	}
	return strings.TrimSuffix(c.BaseURL, "/")
}

func retryableAPIError(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Retryable()
	}
	// A transport error — DNS, TLS, a reset — is worth another try. A context
	// cancellation is not, and retry stops on a done context anyway.
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

// truncate bounds a string to max BYTES, which is the unit GitHub's limits are
// expressed in. It backs up to a rune boundary so the result is still valid
// UTF-8 — a JSON body with a half-written rune is rejected outright, which
// would turn a cosmetic overflow into a failed report.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	const ellipsis = "…"
	cut := max - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ellipsis
}

// Release is the subset of a GitHub Release Kiln needs to attach an asset.
type Release struct {
	ID      int64  `json:"id"`
	TagName string `json:"tag_name"`
	// UploadURL is a URI template ending in "{?name,label}"; the braces are
	// stripped before use.
	UploadURL string `json:"upload_url"`
}

// ReleaseByTag finds an existing release. Kiln does not create releases —
// goreleaser already did, and two components racing to create the same one
// would be a good way to lose a changelog.
func (c *Client) ReleaseByTag(ctx context.Context, tag string) (Release, error) {
	var r Release
	err := c.do(ctx, http.MethodGet, c.path("releases/tags/"+url.PathEscape(tag)), nil, &r)
	return r, err
}

// UploadReleaseAsset attaches a file to a release.
//
// The upload host is api.github.com's sibling, uploads.github.com, and the
// endpoint takes raw bytes rather than JSON — so this cannot go through `do`
// and carries its own request. Retries are deliberately absent: a half-
// uploaded asset that succeeded on retry would be indistinguishable from one
// that uploaded twice, and GitHub rejects a duplicate name anyway.
func (c *Client) UploadReleaseAsset(ctx context.Context, rel Release, name string, body []byte) error {
	if !c.Enabled() {
		return errors.New("github: no token or repository configured")
	}

	endpoint, err := assetURL(rel.UploadURL, name)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("github: build upload request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "kiln")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = int64(len(body))

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("github: upload %s: %w", name, err)
	}
	defer func() { _ = resp.Body.Close() }()

	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &APIError{Status: resp.StatusCode, Method: http.MethodPost, Path: name, Body: string(payload)}
	}
	return nil
}

// assetURL turns GitHub's "{?name,label}" template into a concrete URL.
func assetURL(template, name string) (string, error) {
	base, _, _ := strings.Cut(template, "{")
	if base == "" {
		return "", fmt.Errorf("github: release has no upload url")
	}
	return base + "?name=" + url.QueryEscape(name), nil
}
