package daemon

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.klarlabs.de/kiln/internal/application/ports"
	"go.klarlabs.de/kiln/internal/boot"
	"go.klarlabs.de/kiln/internal/gittest"
	"go.klarlabs.de/kiln/internal/infrastructure/envconfig"
	"go.klarlabs.de/kiln/internal/infrastructure/obs"
	"go.klarlabs.de/kiln/internal/infrastructure/publish"
)

const (
	token  = "test-token"
	secret = "test-secret"
)

// newServer builds a server over a real repository whose gate always passes.
func newServer(t *testing.T) (*Server, *gittest.Repo) {
	t.Helper()
	repo := gittest.New(t)
	repo.Commit("first", "app.txt", "one\n")

	env := envconfig.Load()
	env.DB = t.TempDir() + "/state.json"
	env.Token = ""
	env.TrustedKeys = nil
	env.LogLevel = "fatal"
	// readyz reports 503 when the gate is missing, which is correct behaviour
	// and a poor thing to leave depending on the developer's PATH: with a real
	// warden installed the probe test passes locally and fails on a runner.
	env.Warden = stubGate(t)

	deps, err := boot.Build(t.Context(), boot.Options{
		Dir: repo.Dir, Env: &env, Log: obs.Discard(),
	})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	// The gate and the publisher are stubbed: this package's job is the HTTP
	// contract, not the build.
	deps.Engine.Prover = ports.ProveFunc(func(context.Context, ports.ProveRequest) error { return nil })
	deps.Engine.Publisher = publish.Func(func(_ context.Context, r publish.Request) (publish.Result, error) {
		return publish.Result{Digest: "sha256:abc", Tags: []string{"ghcr.io/x/y:latest"}, Signed: true}, nil
	})

	srv, err := New(deps, token, secret, obs.Discard())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv, repo
}

// stubGate puts an always-succeeding executable on PATH and returns its name.
// The daemon tests stub the prover anyway, so the binary is only ever
// LookPath'd — but readyz does look for it.
func stubGate(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	const name = "warden-stub"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return name
}

func do(t *testing.T, srv *Server, method, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func bearer() map[string]string { return map[string]string{"Authorization": "Bearer " + token} }

func sign(body []byte) map[string]string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return map[string]string{
		"X-Hub-Signature-256": "sha256=" + hex.EncodeToString(mac.Sum(nil)),
		"X-GitHub-Event":      "push",
	}
}

func TestServerRefusesToBootWithoutAToken(t *testing.T) {
	_, err := New(nil, "", secret, obs.Discard())

	// There must be no anonymous mode to forget to turn off.
	if !errors.Is(err, ErrNoToken) {
		t.Errorf("err = %v, want ErrNoToken", err)
	}
	if _, err := New(nil, "   ", secret, obs.Discard()); !errors.Is(err, ErrNoToken) {
		t.Error("whitespace is not a token")
	}
}

func TestProbesNeedNoAuth(t *testing.T) {
	srv, _ := newServer(t)

	for _, path := range []string{"/healthz", "/readyz"} {
		rec := do(t, srv, http.MethodGet, path, nil, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("%s = %d, want 200 (a load balancer has no token)", path, rec.Code)
		}
	}
}

func TestReadyzIsUnavailableWithoutTheGate(t *testing.T) {
	srv, _ := newServer(t)
	srv.Deps.Env.Warden = "kiln-no-such-warden"

	rec := do(t, srv, http.MethodGet, "/readyz", nil, nil)

	// A kilnd whose warden is missing is alive but useless.
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "kiln-no-such-warden") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestJSONRoutesRequireTheBearerToken(t *testing.T) {
	srv, _ := newServer(t)

	cases := []struct {
		name    string
		headers map[string]string
	}{
		{"no header", nil},
		{"wrong token", map[string]string{"Authorization": "Bearer nope"}},
		{"wrong scheme", map[string]string{"Authorization": "Basic " + token}},
		{"bare token", map[string]string{"Authorization": token}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(t, srv, http.MethodPost, "/v1/run", []byte(`{"sha":"HEAD","event":"push"}`), tt.headers)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("code = %d, want 401", rec.Code)
			}
		})
	}
}

func TestUnauthorizedResponseRevealsNothing(t *testing.T) {
	srv, _ := newServer(t)

	rec := do(t, srv, http.MethodPost, "/v1/run", []byte(`{}`), map[string]string{"Authorization": "Bearer nope"})

	// A caller without the token must not learn whether it was missing,
	// malformed or merely wrong.
	if body := rec.Body.String(); strings.Contains(body, token) || len(body) > 60 {
		t.Errorf("body leaks detail: %s", body)
	}
}

func TestRunExecutesSynchronously(t *testing.T) {
	srv, repo := newServer(t)

	rec := do(t, srv, http.MethodPost, "/v1/run",
		[]byte(`{"sha":"HEAD","event":"push","ref":"refs/heads/main"}`), bearer())

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["succeeded"] != true {
		t.Errorf("out = %v", out)
	}
	// HEAD must be resolved: the ledger records commits, not names.
	if out["sha"] != repo.Head() {
		t.Errorf("sha = %v, want %s", out["sha"], repo.Head())
	}
}

func TestRunRejectsABadEvent(t *testing.T) {
	srv, _ := newServer(t)

	rec := do(t, srv, http.MethodPost, "/v1/run", []byte(`{"sha":"HEAD","event":"release"}`), bearer())

	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "pull_request, push or tag") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestRunRejectsAnUnknownField(t *testing.T) {
	srv, _ := newServer(t)

	// A typo that silently does nothing is worse than an error — the same
	// reasoning as .kiln.yaml's KnownFields.
	rec := do(t, srv, http.MethodPost, "/v1/run",
		[]byte(`{"sha":"HEAD","event":"push","publish":true}`), bearer())

	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestRunRejectsAnUnresolvableCommit(t *testing.T) {
	srv, _ := newServer(t)

	rec := do(t, srv, http.MethodPost, "/v1/run", []byte(`{"sha":"no-such-ref","event":"push"}`), bearer())

	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestAFailedBuildIsA200WithSucceededFalse(t *testing.T) {
	srv, _ := newServer(t)
	srv.Deps.Engine.Prover = ports.ProveFunc(func(context.Context, ports.ProveRequest) error {
		return errors.New("lint failed")
	})

	rec := do(t, srv, http.MethodPost, "/v1/run", []byte(`{"sha":"HEAD","event":"push"}`), bearer())

	// A failed build is a result, not an HTTP error: the caller needs the
	// record to see which phase failed.
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["succeeded"] != false || out["error"] == nil {
		t.Errorf("out = %v", out)
	}
}

func TestGetRun(t *testing.T) {
	srv, _ := newServer(t)
	rec := do(t, srv, http.MethodPost, "/v1/run", []byte(`{"sha":"HEAD","event":"push"}`), bearer())
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	got := do(t, srv, http.MethodGet, "/v1/runs/"+created["id"].(string), nil, bearer())

	if got.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", got.Code, got.Body.String())
	}
	if !strings.Contains(got.Body.String(), created["id"].(string)) {
		t.Errorf("body = %s", got.Body.String())
	}
}

func TestGetUnknownRunIs404(t *testing.T) {
	srv, _ := newServer(t)

	rec := do(t, srv, http.MethodGet, "/v1/runs/run-nope", nil, bearer())

	if rec.Code != http.StatusNotFound {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestListRuns(t *testing.T) {
	srv, _ := newServer(t)
	_ = do(t, srv, http.MethodPost, "/v1/run", []byte(`{"sha":"HEAD","event":"push"}`), bearer())

	rec := do(t, srv, http.MethodGet, "/v1/runs", nil, bearer())

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	var out []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Errorf("got %d runs, want 1", len(out))
	}
}

func TestWebhookWithoutASignatureIs401(t *testing.T) {
	srv, _ := newServer(t)
	body := []byte(`{"ref":"refs/heads/main","after":"abc"}`)

	rec := do(t, srv, http.MethodPost, "/v1/github/webhook", body,
		map[string]string{"X-GitHub-Event": "push"})

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401", rec.Code)
	}
}

func TestWebhookWithAForgedSignatureIs401(t *testing.T) {
	srv, _ := newServer(t)
	body := []byte(`{"ref":"refs/heads/main","after":"abc"}`)

	rec := do(t, srv, http.MethodPost, "/v1/github/webhook", body, map[string]string{
		"X-Hub-Signature-256": "sha256=" + strings.Repeat("0", 64),
		"X-GitHub-Event":      "push",
	})

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401", rec.Code)
	}
}

func TestWebhookWithNoConfiguredSecretIs401(t *testing.T) {
	srv, _ := newServer(t)
	// An endpoint with no secret lets anybody on the internet make this
	// machine build and sign a commit of their choosing.
	srv.WebhookSecret = ""
	body := []byte(`{"ref":"refs/heads/main","after":"abc"}`)

	rec := do(t, srv, http.MethodPost, "/v1/github/webhook", body, sign(body))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401 for an unconfigured secret", rec.Code)
	}
}

func TestWebhookAcceptsAGenuinePush(t *testing.T) {
	srv, repo := newServer(t)
	body := []byte(`{"ref":"refs/heads/main","after":"` + repo.Head() + `","repository":{"full_name":"o/r"}}`)

	rec := do(t, srv, http.MethodPost, "/v1/github/webhook", body, sign(body))

	// 202 now, build later: GitHub's ten-second window is not the build
	// budget, and a timed-out delivery gets retried.
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}

	if err := srv.Shutdown(t.Context()); err != nil {
		t.Fatalf("waiting for the background build: %v", err)
	}
	if _, err := srv.Deps.Store.Latest(); err != nil {
		t.Errorf("the delivery did not produce a run: %v", err)
	}
}

func TestWebhookIgnoresAPing(t *testing.T) {
	srv, _ := newServer(t)
	body := []byte(`{"zen":"Keep it logically awesome."}`)
	headers := sign(body)
	headers["X-GitHub-Event"] = "ping"

	rec := do(t, srv, http.MethodPost, "/v1/github/webhook", body, headers)

	// A 4xx here would show up as a red delivery in the repository settings
	// for an entirely normal event.
	if rec.Code != http.StatusAccepted {
		t.Errorf("code = %d, want 202", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ignored") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestWebhookIgnoresABranchDeletion(t *testing.T) {
	srv, _ := newServer(t)
	body := []byte(`{"ref":"refs/heads/gone","after":"0000000000000000000000000000000000000000","deleted":true}`)

	rec := do(t, srv, http.MethodPost, "/v1/github/webhook", body, sign(body))

	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), "ignored") {
		t.Errorf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestWebhookRejectsAMalformedPayload(t *testing.T) {
	srv, _ := newServer(t)
	body := []byte(`{not json`)

	rec := do(t, srv, http.MethodPost, "/v1/github/webhook", body, sign(body))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", rec.Code)
	}
}

func TestWebhookForkPullRequestNeverPublishes(t *testing.T) {
	srv, repo := newServer(t)
	published := false
	srv.Deps.Engine.Publisher = publish.Func(func(context.Context, publish.Request) (publish.Result, error) {
		published = true
		return publish.Result{}, nil
	})

	body := []byte(`{
		"action":"opened","number":7,
		"pull_request":{"number":7,
			"head":{"sha":"` + repo.Head() + `","ref":"f","repo":{"full_name":"stranger/kiln"}},
			"base":{"repo":{"full_name":"klarlabs-studio/kiln"}}},
		"repository":{"full_name":"klarlabs-studio/kiln"}
	}`)
	headers := sign(body)
	headers["X-GitHub-Event"] = "pull_request"

	rec := do(t, srv, http.MethodPost, "/v1/github/webhook", body, headers)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if err := srv.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}

	if published {
		t.Error("a fork pull request delivered by webhook published an image")
	}
	latest, err := srv.Deps.Store.Latest()
	if err != nil {
		t.Fatal(err)
	}
	if !latest.Fork {
		t.Error("the fork status from the payload did not reach the engine")
	}
}

func TestOversizedBodyIsRejected(t *testing.T) {
	srv, _ := newServer(t)
	huge := bytes.Repeat([]byte("x"), MaxBodyBytes+1024)

	rec := do(t, srv, http.MethodPost, "/v1/run", huge, bearer())

	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", rec.Code)
	}
}

func TestShutdownWaitsForInFlightBuilds(t *testing.T) {
	srv, repo := newServer(t)
	started := make(chan struct{})
	release := make(chan struct{})
	srv.Deps.Engine.Prover = ports.ProveFunc(func(context.Context, ports.ProveRequest) error {
		close(started)
		<-release
		return nil
	})

	body := []byte(`{"ref":"refs/heads/main","after":"` + repo.Head() + `"}`)
	_ = do(t, srv, http.MethodPost, "/v1/github/webhook", body, sign(body))
	<-started

	// A build killed midway leaves a half-written registry and a check that
	// never concludes.
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if err := srv.Shutdown(ctx); err == nil {
		t.Error("Shutdown returned before the in-flight build finished")
	}

	close(release)
	if err := srv.Shutdown(t.Context()); err != nil {
		t.Errorf("Shutdown after the build finished: %v", err)
	}
}
