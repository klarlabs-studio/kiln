// Package daemon is Kiln's optional HTTP surface.
//
// Cron plus `kiln watch --once` remains the default, and nothing in Kiln needs
// this to work. kilnd exists for operators who already run a process and would
// rather receive GitHub deliveries than poll for them.
//
// Two things are non-negotiable here, because an HTTP endpoint that builds and
// signs artifacts is a very attractive target:
//
//   - Every non-probe route requires the bearer token, and the server refuses
//     to boot without one. There is no anonymous mode to forget to turn off.
//   - Every webhook delivery must carry a valid HMAC. A server with no secret
//     configured rejects deliveries exactly as loudly as one given a forged
//     signature.
package daemon

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.klarlabs.de/kiln/internal/boot"
	"go.klarlabs.de/kiln/internal/domain/isolation"
	"go.klarlabs.de/kiln/internal/engine"
	"go.klarlabs.de/kiln/internal/infrastructure/github"
	"go.klarlabs.de/kiln/internal/infrastructure/lock"
	"go.klarlabs.de/kiln/internal/infrastructure/obs"
	"go.klarlabs.de/kiln/internal/infrastructure/store"
	"go.klarlabs.de/kiln/internal/infrastructure/worktree"
	"go.klarlabs.de/kiln/internal/interfaces/mcpsrv"
)

// MaxBodyBytes bounds a request body. A webhook payload is a few hundred
// kilobytes at the very worst; anything larger is either a mistake or an
// attempt to exhaust memory.
const MaxBodyBytes = 4 << 20

// BackgroundTimeout bounds a webhook-triggered build.
//
// GitHub's ten-second delivery window is not the build budget: the handler
// answers 202 immediately and the build continues on its own context, which
// therefore needs a deadline of its own or a hung docker pull would pin a
// goroutine forever.
const BackgroundTimeout = 60 * time.Minute

// Server is the HTTP surface.
type Server struct {
	Deps *boot.Deps
	Log  obs.Logger

	// Token authorizes the JSON API. Required.
	Token string
	// WebhookSecret authenticates GitHub deliveries. Empty disables the
	// webhook route by rejecting everything it receives.
	WebhookSecret string

	// background tracks in-flight webhook builds so Shutdown can wait for them.
	background sync.WaitGroup
}

// ErrNoToken reports a server that would have booted without authentication.
var ErrNoToken = errors.New("KILN_TOKEN is required: kilnd builds and signs artifacts, so it never serves anonymously")

// New builds a server, refusing to construct one that cannot authenticate.
func New(deps *boot.Deps, token, webhookSecret string, log obs.Logger) (*Server, error) {
	if strings.TrimSpace(token) == "" {
		return nil, ErrNoToken
	}
	if log == nil {
		log = obs.Discard()
	}
	return &Server{Deps: deps, Log: log, Token: token, WebhookSecret: webhookSecret}, nil
}

// Handler builds the route table.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Probes are unauthenticated by necessity: a load balancer or a container
	// runtime has no token, and they reveal nothing but liveness.
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)

	mux.Handle("POST /v1/run", s.authed(s.handleRun))
	mux.Handle("GET /v1/runs/{id}", s.authed(s.handleGetRun))
	mux.Handle("GET /v1/runs", s.authed(s.handleListRuns))

	// The webhook authenticates with an HMAC instead of the bearer token:
	// GitHub cannot send one, and the signature is the stronger check anyway
	// because it covers the body.
	mux.HandleFunc("POST /v1/github/webhook", s.handleWebhook)

	return mux
}

// Shutdown waits for in-flight webhook builds, bounded by ctx.
//
// Without this, SIGTERM would kill a build midway through a push and leave a
// half-written registry and an in-progress check that never concludes.
func (s *Server) Shutdown(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.background.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("daemon: gave up waiting for in-flight builds: %w", ctx.Err())
	}
}

// authed wraps a handler with bearer authentication.
func (s *Server) authed(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			// No detail: a caller who does not have the token learns nothing
			// from the difference between "missing" and "wrong".
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		h(w, r)
	})
}

func (s *Server) authorized(r *http.Request) bool {
	header := r.Header.Get("Authorization")
	scheme, provided, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	// Constant time: a byte-by-byte comparison leaks the token one character
	// at a time to anyone willing to measure.
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(provided)), []byte(s.Token)) == 1
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReady reports whether this process could actually build something.
//
// Liveness and readiness differ here in a way that matters: a kilnd whose
// warden is missing is alive but useless, and a load balancer should know that
// before it sends work.
func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	body := map[string]any{
		"status":    "ready",
		"directory": s.Deps.Dir,
		"pipeline":  s.Deps.PipelineFound,
	}
	if _, err := s.Deps.Runner.LookPath(s.Deps.Env.Warden); err != nil {
		body["status"] = "degraded"
		body["reason"] = s.Deps.Env.Warden + " is not installed: no commit can be gated"
		writeJSON(w, http.StatusServiceUnavailable, body)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// runRequest is the JSON body of POST /v1/run.
type runRequest struct {
	SHA   string `json:"sha"`
	Event string `json:"event"`
	Ref   string `json:"ref,omitempty"`
	Fork  bool   `json:"fork,omitempty"`
	PR    int    `json:"pr,omitempty"`
}

// handleRun executes synchronously.
//
// Synchronous because the caller is an operator or a script that asked for
// this specific build and wants its verdict. The asynchronous path exists for
// webhooks, where the caller is GitHub and has a ten-second patience.
func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	var req runRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	event, ok := isolation.ParseEvent(req.Event)
	if !ok {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("event must be pull_request, push or tag, got %q", req.Event))
		return
	}
	if strings.TrimSpace(req.SHA) == "" {
		writeError(w, http.StatusBadRequest, "sha is required")
		return
	}

	run, err := s.execute(r.Context(), github.Job{
		SHA: req.SHA, Ref: req.Ref, Event: event, Fork: req.Fork,
	}, req.PR)
	if err != nil && run.ID == "" {
		// Nothing ran: a bad commit, an unreadable repository.
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// A failed build is a result, not an HTTP error: the caller needs the
	// record to see which phase failed. Read `succeeded`.
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec, err := s.Deps.Store.Get(id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no run %q", id))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, mcpsrv.FromRun(rec))
}

func (s *Server) handleListRuns(w http.ResponseWriter, _ *http.Request) {
	runs, err := s.Deps.Store.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]mcpsrv.RunOutput, 0, len(runs))
	for _, r := range runs {
		out = append(out, mcpsrv.FromRun(r))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleWebhook accepts a GitHub delivery.
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read body")
		return
	}

	if err := github.VerifySignature(s.WebhookSecret, body, r.Header.Get(github.SignatureHeader)); err != nil {
		// 401 for both a forged signature and an unconfigured secret. An
		// endpoint with no secret lets anybody on the internet make this
		// machine build and sign a commit of their choosing.
		s.Log.Warn("rejected webhook delivery", "err", err, "remote", r.RemoteAddr)
		writeError(w, http.StatusUnauthorized, "invalid signature")
		return
	}

	eventType := r.Header.Get(github.EventHeader)
	job, err := github.ParseDelivery(eventType, body)
	if errors.Is(err, github.ErrIgnored) {
		// Accepted and deliberately not acted on. A 4xx here would show up as
		// a red delivery in the repository settings for an entirely normal
		// event, and an operator would go looking for a bug.
		s.Log.Debug("ignored delivery", "event", eventType, "reason", err)
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "ignored", "reason": err.Error()})
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// 202 now, build later. GitHub's ten-second delivery window is not the
	// build budget, and a delivery that times out gets retried — which would
	// mean building the same commit twice.
	s.startBackground(job)
	writeJSON(w, http.StatusAccepted, map[string]string{
		"status": "accepted",
		"sha":    job.SHA,
		"ref":    job.Ref,
		"event":  job.Event.String(),
	})
}

// startBackground runs a job on a context detached from the request.
func (s *Server) startBackground(job github.Job) {
	s.background.Add(1)
	go func() {
		defer s.background.Done()

		ctx, cancel := context.WithTimeout(context.Background(), BackgroundTimeout)
		defer cancel()

		out, err := s.execute(ctx, job, 0)
		if err != nil {
			s.Log.Error("webhook build failed",
				"sha", job.SHA, "ref", job.Ref, "event", job.Event.String(), "err", err)
			return
		}
		s.Log.Info("webhook build complete",
			"run", out.ID, "sha", job.SHA, "ref", job.Ref, "digest", out.Digest)
	}()
}

// execute resolves the commit and runs the engine under the repository lock.
//
// kilnd is the surface most likely to be handed concurrent work: GitHub
// delivers a push and a pull_request within the same second all the time, and
// each starts its own background build. Without the lock they would race each
// other's worktrees and ledger writes on one checkout.
func (s *Server) execute(ctx context.Context, job github.Job, pr int) (mcpsrv.RunOutput, error) {
	d := s.Deps

	sha, err := worktree.ResolveSHA(ctx, d.Runner, d.Dir, job.SHA)
	if err != nil {
		return mcpsrv.RunOutput{}, fmt.Errorf("resolve %s: %w", job.SHA, err)
	}

	l, err := s.repoLock(ctx)
	if err != nil {
		return mcpsrv.RunOutput{}, err
	}
	defer func() { _ = l.Release() }()

	fork := job.Fork
	if !fork && job.Event == isolation.EventPullRequest && pr > 0 {
		fork = d.ResolvePullFork(ctx, pr)
	}

	rec, execErr := d.Engine.Execute(ctx, engine.Request{
		SHA:      sha,
		Event:    job.Event,
		Fork:     fork,
		Ref:      job.Ref,
		Repo:     repoName(d),
		Dir:      d.Dir,
		Pipeline: d.Pipeline,
	})
	return mcpsrv.FromRun(rec), execErr
}

// repoLock waits for the repository, rather than refusing like the CLI does.
//
// A webhook delivery is work that arrived on its own schedule and cannot be
// retried by a human, so dropping it because another build was in flight would
// lose it. Waiting is bounded by the caller's context — the background build
// timeout — so a wedged holder cannot pile deliveries up forever.
func (s *Server) repoLock(ctx context.Context) (*lock.Lock, error) {
	path := lock.PathFor(s.Deps.Dir)
	const poll = 2 * time.Second

	for {
		l, err := lock.TryAcquire(path, "kilnd")
		if err == nil {
			return l, nil
		}
		if !errors.Is(err, lock.ErrBusy) {
			return nil, err
		}

		s.Log.Debug("waiting for the repository lock", "holder", lock.ReadHolder(path).String())
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("gave up waiting for the repository lock: %w", ctx.Err())
		case <-time.After(poll):
		}
	}
}

func repoName(d *boot.Deps) string {
	if d.RepoErr != nil {
		return ""
	}
	return d.Repo.String()
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	// Unknown fields are rejected for the same reason `.kiln.yaml` rejects
	// them: a typo that silently does nothing is worse than an error.
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
