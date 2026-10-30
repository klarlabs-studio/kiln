package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TokenSource supplies the bearer token for each request.
//
// A personal token is one string for the life of the process. A GitHub App
// installation token lasts an hour, so a daemon or an --every loop has to
// ask again rather than keep what it booted with.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// TokenFunc adapts a function to TokenSource.
type TokenFunc func(ctx context.Context) (string, error)

// Token implements TokenSource.
func (f TokenFunc) Token(ctx context.Context) (string, error) { return f(ctx) }

// AppTokens mints installation tokens for a GitHub App.
//
// The recommended credential for a box: an installation token is scoped to
// the repositories and permissions the operator granted the app, expires
// within the hour, and belongs to their organisation rather than to a
// person — and it is what the Checks API requires. What the box keeps is the
// app's private key, which can mint nothing but those tokens.
type AppTokens struct {
	AppID string
	// InstallationID names the installation to mint for. Empty asks GitHub
	// which installation covers Repo — an app installed on two organisations
	// has two, and one box may watch repositories in both.
	InstallationID string
	// Repo is what an empty InstallationID is resolved against.
	Repo Repo
	Key  *rsa.PrivateKey
	// BaseURL is the API root; GitHub Enterprise overrides it.
	BaseURL string
	HTTP    *http.Client

	now func() time.Time

	mu      sync.Mutex
	token   string
	expires time.Time
	// missing is the answer when the app is not installed on Repo at all.
	// Kept, because it will be the answer for the rest of the process, and
	// asking again on every request is how one box logged 900 warnings.
	missing error
}

// renewBefore is how long before expiry a token is replaced. Long enough
// that a token handed to goreleaser or a slow upload does not die mid-use.
const renewBefore = 5 * time.Minute

// Token returns a live installation token, minting one when the cached
// token is missing or near expiry.
func (a *AppTokens) Token(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.token != "" && a.clock().Add(renewBefore).Before(a.expires) {
		return a.token, nil
	}
	token, expires, err := a.mint(ctx)
	if err != nil {
		return "", err
	}
	a.token, a.expires = token, expires
	return token, nil
}

func (a *AppTokens) mint(ctx context.Context) (string, time.Time, error) {
	installation, err := a.installation(ctx)
	if err != nil {
		return "", time.Time{}, err
	}
	status, body, err := a.call(ctx, http.MethodPost, "/app/installations/"+installation+"/access_tokens")
	if err != nil {
		return "", time.Time{}, fmt.Errorf("github app: mint installation token: %w", err)
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("github app %s, installation %s: minting a token returned %d: %s",
			a.AppID, installation, status, strings.TrimSpace(string(body)))
	}

	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Token == "" {
		return "", time.Time{}, fmt.Errorf("github app: unreadable token response: %w", errors.Join(err, errors.New("no token")))
	}
	return out.Token, out.ExpiresAt, nil
}

// installation returns the configured installation, or asks GitHub which
// one covers the repository. Called under a.mu.
func (a *AppTokens) installation(ctx context.Context) (string, error) {
	if a.InstallationID != "" {
		return a.InstallationID, nil
	}
	if a.missing != nil {
		return "", a.missing
	}
	if !a.Repo.Valid() {
		return "", errors.New("github app: no installation id, and no repository to look one up for")
	}
	status, body, err := a.call(ctx, http.MethodGet, "/repos/"+a.Repo.String()+"/installation")
	if err != nil {
		return "", fmt.Errorf("github app: look up the installation for %s: %w", a.Repo, err)
	}
	if status == http.StatusNotFound {
		a.missing = fmt.Errorf("%w: github app %s is not installed on %s — install it on %s",
			ErrRepoNotVisible, a.AppID, a.Repo, a.Repo.Owner)
		return "", a.missing
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("github app %s: looking up the installation for %s returned %d: %s",
			a.AppID, a.Repo, status, strings.TrimSpace(string(body)))
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.ID == 0 {
		return "", fmt.Errorf("github app: unreadable installation for %s: %w", a.Repo, errors.Join(err, errors.New("no id")))
	}
	// Cached on the struct: the installation does not move while a process
	// runs, and the next token for this repository needs the same answer.
	a.InstallationID = strconv.FormatInt(out.ID, 10)
	return a.InstallationID, nil
}

// call makes one request authenticated as the app itself.
func (a *AppTokens) call(ctx context.Context, method, path string) (int, []byte, error) {
	jwt, err := a.jwt()
	if err != nil {
		return 0, nil, err
	}
	base := strings.TrimSuffix(a.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, http.NoBody)
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "kiln")

	client := a.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body, nil
}

// jwt signs the app's identity for the token exchange. GitHub rejects one
// that lives past ten minutes, and the issued-at is backdated a minute for
// clock skew, as GitHub's own documentation recommends.
func (a *AppTokens) jwt() (string, error) {
	if a.Key == nil || a.AppID == "" {
		return "", errors.New("github app: needs an app id and a private key")
	}
	now := a.clock()
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": a.AppID,
	})
	if err != nil {
		return "", err
	}
	signing := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.Key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("github app: sign jwt: %w", err)
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

func (a *AppTokens) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

// ParseAppKey reads a GitHub App private key. GitHub has issued both PKCS#1
// ("RSA PRIVATE KEY") and PKCS#8 ("PRIVATE KEY") PEM files.
func ParseAppKey(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("github app key: not a PEM file")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("github app key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("github app key: not an RSA key")
	}
	return key, nil
}
