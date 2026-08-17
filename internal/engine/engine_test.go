package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.klarlabs.de/kiln/internal/checks"
	"go.klarlabs.de/kiln/internal/config"
	"go.klarlabs.de/kiln/internal/isolation"
	"go.klarlabs.de/kiln/internal/obs"
	"go.klarlabs.de/kiln/internal/prove"
	"go.klarlabs.de/kiln/internal/provenance"
	"go.klarlabs.de/kiln/internal/publish"
	"go.klarlabs.de/kiln/internal/run"
	"go.klarlabs.de/kiln/internal/store"
)

const (
	sha    = "abc1234def567890"
	digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	image  = "ghcr.io/felixgeelhaar/glossa-api"
)

// harness wires an engine whose collaborators are all recording fakes.
type harness struct {
	engine    *Engine
	checks    *checks.Recording
	store     store.Store
	proved    int
	published int
	proveErr  error
	pubErr    error
	lastProve prove.Request
	lastPub   publish.Request
	verdict   provenance.Result
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		checks:  &checks.Recording{},
		store:   store.NewMemory(),
		verdict: provenance.Result{Decision: provenance.Reprove, Reason: "no keys pinned"},
	}
	h.engine = New(Engine{
		Prover: prove.Func(func(_ context.Context, req prove.Request) error {
			h.proved++
			h.lastProve = req
			return h.proveErr
		}),
		Publisher: publish.Func(func(_ context.Context, req publish.Request) (publish.Result, error) {
			h.published++
			h.lastPub = req
			if h.pubErr != nil {
				return publish.Result{}, h.pubErr
			}
			return publish.Result{
				Digest: digest, Reference: image + "@" + digest,
				Tags: req.Plan.Refs(), Signed: true,
			}, nil
		}),
		Provenance: verifierFunc(func() provenance.Result { return h.verdict }),
		Checks:     h.checks,
		Store:      h.store,
		Log:        obs.Discard(),
	})
	return h
}

type verifierFunc func() provenance.Result

func (f verifierFunc) Verify(context.Context, string, string, isolation.Policy) provenance.Result {
	return f()
}

func pipeline(t *testing.T) config.Pipeline {
	t.Helper()
	p, err := config.Parse(strings.NewReader(`
apiVersion: kiln.klarlabs.de/v1
kind: Pipeline
on:
  pull_request: [prove]
  push: [prove, publish]
prove: {from: warden}
publish:
  image: ` + image + `
  tags: [sha, latest]
`))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func req(t *testing.T, event isolation.Event, fork bool, ref string) Request {
	t.Helper()
	return Request{
		SHA: sha, Event: event, Fork: fork, Ref: ref,
		Repo: "klarlabs-studio/kiln", Dir: t.TempDir(), Pipeline: pipeline(t),
	}
}

func TestPushProvesAndPublishes(t *testing.T) {
	h := newHarness(t)

	r, err := h.engine.Execute(t.Context(), req(t, isolation.EventPush, false, "refs/heads/main"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if h.proved != 1 || h.published != 1 {
		t.Errorf("proved=%d published=%d, want 1 each", h.proved, h.published)
	}
	if r.Phase != run.PhaseSucceeded {
		t.Errorf("Phase = %s", r.Phase)
	}
	if r.Digest != digest {
		t.Errorf("Digest = %q", r.Digest)
	}
	if c, _ := h.checks.Conclusions(checks.NameProve); c != checks.Success {
		t.Errorf("prove check = %s", c)
	}
	if c, _ := h.checks.Conclusions(checks.NamePublish); c != checks.Success {
		t.Errorf("publish check = %s", c)
	}
}

func TestForkPullRequestProvesButNeverPublishes(t *testing.T) {
	h := newHarness(t)
	// The pipeline is edited on the fork head to demand a publish. The policy
	// must win: this is the attack the isolation matrix exists to stop.
	r := req(t, isolation.EventPullRequest, true, "refs/pull/7/head")
	r.Pipeline.On.PullRequest = []config.Step{config.StepProve, config.StepPublish}

	if _, err := h.engine.Execute(t.Context(), r); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if h.proved != 1 {
		t.Errorf("proved = %d, want 1", h.proved)
	}
	if h.published != 0 {
		t.Error("a fork pull request published an image")
	}
	if h.checks.Started(checks.NamePublish) {
		t.Error("a suppressed publish must not open a check")
	}
}

func TestForkPullRequestProvesWithoutSecrets(t *testing.T) {
	h := newHarness(t)

	if _, err := h.engine.Execute(t.Context(), req(t, isolation.EventPullRequest, true, "refs/pull/7/head")); err != nil {
		t.Fatal(err)
	}

	if h.lastProve.Policy.Secrets {
		t.Error("the gate on a fork head was handed the operator's secrets")
	}
}

func TestSameRepoPullRequestDoesNotPublishEither(t *testing.T) {
	h := newHarness(t)
	r := req(t, isolation.EventPullRequest, false, "refs/pull/8/head")
	r.Pipeline.On.PullRequest = []config.Step{config.StepProve, config.StepPublish}

	if _, err := h.engine.Execute(t.Context(), r); err != nil {
		t.Fatal(err)
	}

	// An image built from an unmerged head is one nobody should be able to
	// ship, however trusted the branch.
	if h.published != 0 {
		t.Error("a pull request published an image")
	}
}

func TestTrustedNoteSkipsTheGate(t *testing.T) {
	h := newHarness(t)
	h.verdict = provenance.Result{Decision: provenance.Skipped, Reason: "signed by a trusted key"}

	r, err := h.engine.Execute(t.Context(), req(t, isolation.EventPush, false, "refs/heads/main"))
	if err != nil {
		t.Fatal(err)
	}

	if h.proved != 0 {
		t.Error("re-proved a commit covered by a trusted note")
	}
	if !r.Skipped {
		t.Error("the ledger must record that the gate was skipped")
	}
	// The commit is still gated — by the note — so a protected branch waiting
	// on this check must be satisfied.
	if c, _ := h.checks.Conclusions(checks.NameProve); c != checks.Success {
		t.Errorf("prove check = %s, want success", c)
	}
	if !strings.Contains(h.checks.Summary(checks.NameProve), "trusted key") {
		t.Errorf("the skip must be justified in the check: %q", h.checks.Summary(checks.NameProve))
	}
	// A skipped prove does not skip the publish.
	if h.published != 1 {
		t.Errorf("published = %d, want 1", h.published)
	}
}

func TestProveFailureStopsBeforePublish(t *testing.T) {
	h := newHarness(t)
	h.proveErr = errors.New("lint failed")

	r, err := h.engine.Execute(t.Context(), req(t, isolation.EventPush, false, "refs/heads/main"))

	if err == nil {
		t.Fatal("want a failure")
	}
	if h.published != 0 {
		t.Error("published an artifact from a commit that did not pass the gate")
	}
	if r.Phase != run.PhaseFailed || r.Error == "" {
		t.Errorf("run = %+v, want a recorded failure", r)
	}
	if c, _ := h.checks.Conclusions(checks.NameProve); c != checks.Failure {
		t.Errorf("prove check = %s", c)
	}
}

func TestPublishFailureFailsTheRun(t *testing.T) {
	h := newHarness(t)
	h.pubErr = errors.New("registry refused")

	r, err := h.engine.Execute(t.Context(), req(t, isolation.EventPush, false, "refs/heads/main"))

	if err == nil {
		t.Fatal("want a failure")
	}
	if r.Phase != run.PhaseFailed {
		t.Errorf("Phase = %s", r.Phase)
	}
	if c, _ := h.checks.Conclusions(checks.NamePublish); c != checks.Failure {
		t.Errorf("publish check = %s", c)
	}
}

func TestTagEventPublishesASemverTag(t *testing.T) {
	h := newHarness(t)
	r := req(t, isolation.EventTag, false, "refs/tags/v0.2.0")
	r.Pipeline.Publish.Tags = []config.Tag{config.TagSHA, config.TagSemver, config.TagLatest}

	got, err := h.engine.Execute(t.Context(), r)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var found bool
	for _, tag := range got.Tags {
		if tag == image+":v0.2.0" {
			found = true
		}
	}
	if !found {
		t.Errorf("tags = %v, want the version tag", got.Tags)
	}
}

func TestPipelineWithoutPublishJustProves(t *testing.T) {
	h := newHarness(t)
	r := req(t, isolation.EventPush, false, "refs/heads/main")
	// A library repository: no .kiln.yaml, so the default pipeline.
	r.Pipeline = config.Default()

	got, err := h.engine.Execute(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}

	if h.proved != 1 || h.published != 0 {
		t.Errorf("proved=%d published=%d", h.proved, h.published)
	}
	if got.Phase != run.PhaseSucceeded {
		t.Errorf("a prove-only repository should still succeed, got %s", got.Phase)
	}
}

func TestPublishRoutedWithoutAPublishBlockFails(t *testing.T) {
	h := newHarness(t)
	r := req(t, isolation.EventPush, false, "refs/heads/main")
	r.Pipeline.Publish = nil

	_, err := h.engine.Execute(t.Context(), r)
	if err == nil || !strings.Contains(err.Error(), "publish block") {
		t.Errorf("err = %v, want a clear configuration failure", err)
	}
}

func TestSemverOnlyOnABranchFailsWithAReportedCheck(t *testing.T) {
	h := newHarness(t)
	r := req(t, isolation.EventPush, false, "refs/heads/main")
	r.Pipeline.Publish.Tags = []config.Tag{config.TagSHA, config.TagSemver}

	_, err := h.engine.Execute(t.Context(), r)

	if err == nil {
		t.Fatal("want a plan failure")
	}
	// The operator must see this on the commit, not only in a log they are not
	// watching.
	if c, _ := h.checks.Conclusions(checks.NamePublish); c != checks.Failure {
		t.Errorf("publish check = %s, want the plan failure reported", c)
	}
	if h.published != 0 {
		t.Error("published despite an unbuildable plan")
	}
}

func TestGitHubBeingDownDoesNotFailAGoodRun(t *testing.T) {
	h := newHarness(t)
	h.engine.Checks = checks.Failing{Err: errors.New("api unreachable")}

	r, err := h.engine.Execute(t.Context(), req(t, isolation.EventPush, false, "refs/heads/main"))

	// The artifact is the deliverable; the Check is the announcement.
	if err != nil {
		t.Fatalf("a reporting failure must not fail the run: %v", err)
	}
	if r.Phase != run.PhaseSucceeded || r.Digest != digest {
		t.Errorf("run = %+v", r)
	}
}

func TestRunIsPersistedThroughEveryPhase(t *testing.T) {
	h := newHarness(t)

	got, err := h.engine.Execute(t.Context(), req(t, isolation.EventPush, false, "refs/heads/main"))
	if err != nil {
		t.Fatal(err)
	}

	stored, err := h.store.Get(got.ID)
	if err != nil {
		t.Fatalf("run not in the ledger: %v", err)
	}
	if stored.Phase != run.PhaseSucceeded || stored.Digest != digest {
		t.Errorf("stored = %+v", stored)
	}
	if stored.Ref != "refs/heads/main" || stored.Event != "push" {
		t.Errorf("stored run lost its provenance: %+v", stored)
	}
}

func TestFailedRunIsStillPersisted(t *testing.T) {
	h := newHarness(t)
	h.proveErr = errors.New("gate failed")

	got, _ := h.engine.Execute(t.Context(), req(t, isolation.EventPush, false, "refs/heads/main"))

	stored, err := h.store.Get(got.ID)
	if err != nil {
		t.Fatalf("failed run not in the ledger: %v", err)
	}
	if stored.Phase != run.PhaseFailed || stored.Error == "" {
		t.Errorf("stored = %+v", stored)
	}
}

func TestValidationRejectsBadRequests(t *testing.T) {
	h := newHarness(t)

	bad := []struct {
		name string
		fn   func(r *Request)
	}{
		{"no commit", func(r *Request) { r.SHA = "" }},
		{"unknown event", func(r *Request) { r.Event = isolation.Event("release") }},
		{"no directory", func(r *Request) { r.Dir = "" }},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			r := req(t, isolation.EventPush, false, "refs/heads/main")
			tt.fn(&r)

			got, err := h.engine.Execute(t.Context(), r)
			if err == nil {
				t.Fatal("want a validation failure")
			}
			if got.Phase != run.PhaseFailed {
				t.Errorf("Phase = %s", got.Phase)
			}
		})
	}
}

func TestUnroutedEventDoesNothing(t *testing.T) {
	h := newHarness(t)
	r := req(t, isolation.EventPush, false, "refs/heads/main")
	r.Pipeline.On.Push = nil

	got, err := h.engine.Execute(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}

	if h.proved != 0 || h.published != 0 {
		t.Errorf("proved=%d published=%d, want nothing", h.proved, h.published)
	}
	if got.Phase != run.PhaseSucceeded {
		t.Errorf("Phase = %s, want succeeded (nothing was asked for)", got.Phase)
	}
	if h.checks.Started(checks.NameProve) {
		t.Error("opened a check for a phase that was not routed")
	}
}

func TestNoxFlagReachesTheProver(t *testing.T) {
	h := newHarness(t)
	r := req(t, isolation.EventPush, false, "refs/heads/main")
	r.Pipeline.Prove.Nox = true

	if _, err := h.engine.Execute(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if !h.lastProve.Nox {
		t.Error("prove.nox did not reach the prover")
	}
}

func TestAlreadyBuilt(t *testing.T) {
	s := store.NewMemory()

	if AlreadyBuilt(s, sha, "refs/heads/main") {
		t.Error("an empty ledger cannot have built anything")
	}
	if AlreadyBuilt(nil, sha, "refs/heads/main") {
		t.Error("a nil store must answer false, not panic")
	}

	r := run.New(sha, "refs/heads/main", "push", false, "o/r")
	r.Succeed()
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}

	if !AlreadyBuilt(s, sha, "refs/heads/main") {
		t.Error("a succeeded run should suppress a rebuild")
	}
	// A tag on the same commit is a different job.
	if AlreadyBuilt(s, sha, "refs/tags/v1.0.0") {
		t.Error("already-built must be scoped to the ref")
	}
}

func TestNewDefaultsOptionalCollaborators(t *testing.T) {
	e := New(Engine{
		Prover:    prove.Func(func(context.Context, prove.Request) error { return nil }),
		Publisher: publish.Func(func(context.Context, publish.Request) (publish.Result, error) { return publish.Result{}, nil }),
	})

	if e.Checks == nil || e.Store == nil || e.Log == nil || e.Provenance == nil {
		t.Fatalf("New left a collaborator nil: %+v", e)
	}
	// The defaulted verifier must never skip: an engine wired without a
	// provenance verifier has no basis to trust anything.
	if e.Provenance.Verify(t.Context(), "/repo", sha, isolation.For(isolation.EventPush, false)).Skip() {
		t.Error("the default verifier skipped the gate")
	}
}
