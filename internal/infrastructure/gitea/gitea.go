// Package gitea talks to Gitea and Forgejo.
//
// Those forges share a GitHub-shaped /api/v1 for pull requests and commit
// statuses. They have no Checks API. Kiln posts statuses and asks the same
// two questions it asks GitHub: is this pull request from a fork, and what
// commit is at its head. It implements no runner protocol.
package gitea

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.klarlabs.de/fortify/retry"
	"go.klarlabs.de/kiln/internal/application/ports"
	"go.klarlabs.de/kiln/internal/domain/forge"
	"go.klarlabs.de/kiln/internal/infrastructure/obs"
)

// APIPrefix is appended to an instance origin when the operator did not
// include it. KILN_FORGE_URL=https://gitea.example.com is the usual form.
const APIPrefix = "/api/v1"

var _ ports.Host = (*Client)(nil)

// Repo identifies a repository.
type Repo struct {
	Owner string
	Name  string
}

// String renders owner/name.
func (r Repo) String() string { return r.Owner + "/" + r.Name }

// Valid reports whether both halves are present.
func (r Repo) Valid() bool { return r.Owner != "" && r.Name != "" }

// Client is a minimal Gitea/Forgejo REST client.
type Client struct {
	Token   string
	Repo    Repo
	BaseURL string
	HTTP    *http.Client
	Log     ports.Logger
	// Attempts bounds the retry of transient API failures.
	Attempts int
}

// NewClient builds a client. baseURL is the instance origin or the /api/v1
// root; both are accepted. A nil HTTP client gets a sane timeout.
func NewClient(token string, repo Repo, baseURL string, log ports.Logger) *Client {
	if log == nil {
		log = obs.Discard()
	}
	return &Client{
		Token:    token,
		Repo:     repo,
		BaseURL:  NormalizeBaseURL(baseURL),
		HTTP:     &http.Client{Timeout: 30 * time.Second},
		Log:      log,
		Attempts: 3,
	}
}

// NormalizeBaseURL turns an instance origin into the API root.
func NormalizeBaseURL(raw string) string {
	raw = strings.TrimSuffix(strings.TrimSpace(raw), "/")
	if raw == "" {
		return ""
	}
	if strings.HasSuffix(raw, APIPrefix) {
		return raw
	}
	return raw + APIPrefix
}

// Enabled reports whether the client can talk to the forge. Without a token
// or an instance URL, Kiln posts no statuses and treats every pull request
// as a fork.
func (c *Client) Enabled() bool {
	return c != nil && c.Token != "" && c.Repo.Valid() && c.BaseURL != ""
}

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
	return fmt.Sprintf("gitea %s %s: %d %s", e.Method, e.Path, e.Status, body)
}

// Retryable reports whether trying again could plausibly work.
func (e *APIError) Retryable() bool {
	return e.Status >= 500 || e.Status == http.StatusTooManyRequests
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

func asPull(p pullPayload) forge.Pull {
	return forge.Pull{
		Number:  p.Number,
		HeadSHA: p.Head.SHA,
		HeadRef: p.Head.Ref,
		Fork:    isFork(p),
		Draft:   p.Draft,
	}
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

// LookupPull fetches one pull request.
func (c *Client) LookupPull(ctx context.Context, number int) (forge.Pull, error) {
	var p pullPayload
	if err := c.do(ctx, http.MethodGet, c.path(fmt.Sprintf("pulls/%d", number)), nil, &p); err != nil {
		return forge.Pull{}, err
	}
	return asPull(p), nil
}

// ListOpenPulls returns the open pull requests, for watch discovery.
func (c *Client) ListOpenPulls(ctx context.Context) ([]forge.Pull, error) {
	var payload []pullPayload
	if err := c.do(ctx, http.MethodGet, c.path("pulls?state=open&limit=50"), nil, &payload); err != nil {
		return nil, err
	}
	out := make([]forge.Pull, 0, len(payload))
	for _, p := range payload {
		out = append(out, asPull(p))
	}
	return out, nil
}

// WhoAmI validates a token against the instance and returns the login it
// belongs to.
func WhoAmI(ctx context.Context, token, baseURL string) (string, error) {
	c := NewClient(token, Repo{Owner: "x", Name: "y"}, baseURL, nil)
	var out struct {
		Login string `json:"login"`
	}
	if err := c.do(ctx, http.MethodGet, "/user", nil, &out); err != nil {
		return "", err
	}
	if out.Login == "" {
		return "the token", nil
	}
	return out.Login, nil
}

// CreateStatus posts a commit status. Gitea and Forgejo have no Checks API,
// so this is the only way Kiln reports a verdict that branch protection can
// require.
func (c *Client) CreateStatus(ctx context.Context, sha, state, context, description string) error {
	payload := map[string]any{
		"state":       state,
		"context":     context,
		"description": truncateDescription(description),
	}
	path := fmt.Sprintf("/repos/%s/%s/statuses/%s", c.Repo.Owner, c.Repo.Name, sha)
	if err := c.do(ctx, http.MethodPost, path, payload, nil); err != nil {
		return fmt.Errorf("gitea: post status %q: %w", context, err)
	}
	return nil
}

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
func (c *Client) OpenPullRequest(ctx context.Context, head, base, title, body string) (forge.Pull, bool, error) {
	if existing, found, err := c.pullForHead(ctx, head); err != nil {
		return forge.Pull{}, false, err
	} else if found {
		return existing, false, nil
	}

	payload := map[string]any{"head": head, "title": title, "body": body}
	if base != "" {
		payload["base"] = base
	}

	var p pullPayload
	if err := c.do(ctx, http.MethodPost, c.path("pulls"), payload, &p); err != nil {
		return forge.Pull{}, false, fmt.Errorf("gitea: open pull request for %s: %w", head, err)
	}
	return asPull(p), true, nil
}

// pullForHead finds an open pull request whose head is this branch.
//
// Listed and filtered locally: Gitea/Forgejo's head filter is not the
// GitHub `owner:branch` form on every version, and an empty match would
// open a duplicate.
func (c *Client) pullForHead(ctx context.Context, head string) (forge.Pull, bool, error) {
	pulls, err := c.ListOpenPulls(ctx)
	if err != nil {
		return forge.Pull{}, false, fmt.Errorf("gitea: look for an existing pull request on %s: %w", head, err)
	}
	for _, p := range pulls {
		if p.HeadRef == head {
			return p, true, nil
		}
	}
	return forge.Pull{}, false, nil
}

// LabelPull adds labels to a pull request. Labels are an issue-level concept
// in the Gitea API, matching GitHub.
func (c *Client) LabelPull(ctx context.Context, number int, labels []string) error {
	if len(labels) == 0 {
		return nil
	}
	path := c.path(fmt.Sprintf("issues/%d/labels", number))
	if err := c.do(ctx, http.MethodPost, path, map[string]any{"labels": labels}, nil); err != nil {
		return fmt.Errorf("gitea: label pull request #%d: %w", number, err)
	}
	return nil
}

func (c *Client) path(suffix string) string {
	return fmt.Sprintf("/repos/%s/%s/%s", c.Repo.Owner, c.Repo.Name, suffix)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	if !c.Enabled() {
		return errors.New("gitea: no token, repository or instance URL configured")
	}

	var encoded []byte
	if body != nil {
		var err error
		if encoded, err = json.Marshal(body); err != nil {
			return fmt.Errorf("gitea: encode %s %s: %w", method, path, err)
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
			c.Log.Warn("retrying gitea call", "method", method, "path", path, "attempt", attempt, "err", err)
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
		return fmt.Errorf("gitea: decode %s %s: %w", method, path, err)
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
		return nil, fmt.Errorf("gitea: build request: %w", err)
	}
	// token, not Bearer: that is the header Gitea and Forgejo document.
	req.Header.Set("Authorization", "token "+c.Token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "kiln")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gitea: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("gitea: read %s %s: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &APIError{Status: resp.StatusCode, Method: method, Path: path, Body: string(data)}
	}
	return data, nil
}

func (c *Client) baseURL() string {
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
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}
