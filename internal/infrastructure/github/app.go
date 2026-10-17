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
	AppID          string
	InstallationID string
	Key            *rsa.PrivateKey
	// BaseURL is the API root; GitHub Enterprise overrides it.
	BaseURL string
	HTTP    *http.Client

	now func() time.Time

	mu      sync.Mutex
	token   string
	expires time.Time
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
	jwt, err := a.jwt()
	if err != nil {
		return "", time.Time{}, err
	}
	base := strings.TrimSuffix(a.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	url := base + "/app/installations/" + a.InstallationID + "/access_tokens"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, http.NoBody)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("github app: build token request: %w", err)
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
		return "", time.Time{}, fmt.Errorf("github app: mint installation token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("github app %s, installation %s: minting a token returned %d: %s",
			a.AppID, a.InstallationID, resp.StatusCode, strings.TrimSpace(string(body)))
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

// jwt signs the app's identity for the token exchange. GitHub rejects one
// that lives past ten minutes, and the issued-at is backdated a minute for
// clock skew, as GitHub's own documentation recommends.
func (a *AppTokens) jwt() (string, error) {
	if a.Key == nil || a.AppID == "" || a.InstallationID == "" {
		return "", errors.New("github app: needs an app id, an installation id and a private key")
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
