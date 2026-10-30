package github

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.klarlabs.de/kiln/internal/infrastructure/obs"
)

// A box whose credential could not see its repository logged a 404 for every
// pull request listing and every check run — 913 warnings in a week, none of
// which said what was wrong. One look at the repository tells "this pull
// request is gone" from "this repository is invisible", and the second one
// switches the client off with a sentence an operator can act on.
func TestAnInvisibleRepositoryDisablesTheClientOnce(t *testing.T) {
	var calls atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	var logged bytes.Buffer
	c.Log = obs.NewTo(&logged, "debug")

	_, err := c.ListOpenPulls(t.Context())
	if !errors.Is(err, ErrRepoNotVisible) {
		t.Fatalf("err = %v, want ErrRepoNotVisible", err)
	}
	if !strings.Contains(err.Error(), "the token cannot see klarlabs-studio/kiln") {
		t.Errorf("the error does not say what to fix: %v", err)
	}
	if c.Enabled() {
		t.Error("a client that cannot see its repository still reports itself enabled")
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("%d requests, want the call and one look at the repository", n)
	}

	if _, err := c.CreateCheckRun(t.Context(), "Kiln / Prove", "abc"); !errors.Is(err, ErrRepoNotVisible) {
		t.Errorf("a later call: err = %v", err)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("a disabled client kept calling GitHub: %d requests", n)
	}
	if n := strings.Count(logged.String(), "github disabled"); n != 1 {
		t.Errorf("said it %d times, want once:\n%s", n, logged.String())
	}
}

// A 404 for one resource on a repository the credential can see is just a
// missing resource. Switching GitHub off for it would silence a healthy box.
func TestAMissingResourceOnAVisibleRepositoryIsJustMissing(t *testing.T) {
	var probes atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/klarlabs-studio/kiln" {
			probes.Add(1)
			_, _ = w.Write([]byte(`{"full_name":"klarlabs-studio/kiln"}`))
			return
		}
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})

	for range 3 {
		if _, err := c.LookupPull(t.Context(), 7); err == nil || errors.Is(err, ErrRepoNotVisible) {
			t.Fatalf("err = %v, want a plain 404", err)
		}
	}
	if !c.Enabled() {
		t.Error("one missing pull request disabled the client")
	}
	if n := probes.Load(); n != 1 {
		t.Errorf("looked at the repository %d times, want once per process", n)
	}
}

// The App installation is per organisation. A box with one installation id
// for repositories in two organisations gets 404s for every repository in the
// other; without an id, kiln asks GitHub which installation covers this one.
func TestAppTokens_DiscoversTheInstallationForTheRepository(t *testing.T) {
	key := testKey(t)
	var lookups atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := verifyAppJWT(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), &key.PublicKey, "1234"); err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/other-org/service/installation":
			lookups.Add(1)
			_, _ = w.Write([]byte(`{"id":77}`))
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/77/access_tokens":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token": "ghs_other", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			})
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	now := time.Now()
	src := &AppTokens{AppID: "1234", Repo: Repo{Owner: "other-org", Name: "service"}, Key: key,
		BaseURL: srv.URL, now: func() time.Time { return now }}

	tok, err := src.Token(t.Context())
	if err != nil || tok != "ghs_other" {
		t.Fatalf("token %q, err %v", tok, err)
	}
	now = now.Add(time.Hour) // force a renewal
	if _, err := src.Token(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := lookups.Load(); n != 1 {
		t.Errorf("looked the installation up %d times, want once", n)
	}
}

func TestAppTokens_NotInstalledSaysWhereToInstallIt(t *testing.T) {
	key := testKey(t)
	var lookups atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lookups.Add(1)
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	src := &AppTokens{AppID: "1234", Repo: Repo{Owner: "other-org", Name: "service"}, Key: key, BaseURL: srv.URL}
	c := NewClient("", src.Repo, obs.Discard())
	c.BaseURL, c.Tokens = srv.URL, src

	_, err := c.ListOpenPulls(t.Context())
	if !errors.Is(err, ErrRepoNotVisible) || !strings.Contains(err.Error(), "install it on other-org") {
		t.Fatalf("err = %v", err)
	}
	if c.Enabled() {
		t.Error("a client whose app is not installed still reports itself enabled")
	}
	_, _ = c.ListOpenPulls(t.Context())
	// Once, not once per retry: an answer that holds until somebody installs
	// the app is not a transient failure.
	if n := lookups.Load(); n != 1 {
		t.Errorf("asked %d times, want once", n)
	}
}
