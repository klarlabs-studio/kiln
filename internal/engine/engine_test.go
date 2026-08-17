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
	engine      *Engine
	checks      *checks.Recording
	store       store.Store
	proved      int
	published   int
	released    int
	proveErr    error
	pubErr      error
	releaseErr  error
	lastProve   prove.Request
	lastPub     publish.Request
	lastRelease publish.Request
	verdict     provenance.Result
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
			plan, err := publish.BuildPlan(req.Artifact, req.SHA, req.Ref)
			if err != nil {
				return publish.Result{}, err
			}
			return publish.Result{
				Kind: config.KindImage, Digest: digest, Reference: image + "@" + digest,
				Tags: plan.Refs(), Signed: true,
			}, nil
		}),
		ReleasePublisher: publish.Func(func(_ context.Context, req publish.Request) (publish.Result, error) {
			h.released++
			h.lastRelease = req
			if h.releaseErr != nil {
				return publish.Result{}, h.releaseErr
			}
			return publish.Result{
				Kind: config.KindBinaries, Digest: "sha256:manifest",
				Reference: strings.TrimPrefix(req.Ref, "refs/tags/"),
				Tags:      []string{"checksums.txt", "checksums.txt.sig"}, Signed: true,
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
  - kind: image
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
	r.Pipeline.Publish[0].Tags = []config.Tag{config.TagSHA, config.TagSemver, config.TagLatest}

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

func TestPublishRoutedWithAnEmptyListDoesNothing(t *testing.T) {
	h := newHarness(t)
	r := req(t, isolation.EventPush, false, "refs/heads/main")
	r.Pipeline.Publish = nil

	// The loader rejects this shape, so reaching the engine with it means a
	// caller built the pipeline by hand. Producing nothing is the honest
	// outcome; there is no artifact to fail about.
	got, err := h.engine.Execute(t.Context(), r)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if h.published != 0 || got.Phase != run.PhaseSucceeded {
		t.Errorf("published=%d phase=%s", h.published, got.Phase)
	}
}

func TestSemverOnlyOnABranchFailsWithAReportedCheck(t *testing.T) {
	h := newHarness(t)
	r := req(t, isolation.EventPush, false, "refs/heads/main")
	r.Pipeline.Publish[0].Tags = []config.Tag{config.TagSHA, config.TagSemver}

	got, err := h.engine.Execute(t.Context(), r)

	if err == nil {
		t.Fatal("want a plan failure")
	}
	// The operator must see this on the commit, not only in a log they are not
	// watching.
	if c, _ := h.checks.Conclusions(checks.NamePublish); c != checks.Failure {
		t.Errorf("publish check = %s, want the plan failure reported", c)
	}
	// Planning now happens inside the publisher, so what matters is that
	// nothing was recorded as shipped — not whether the publisher was called.
	if len(got.Artifacts) != 0 {
		t.Errorf("recorded %+v from an unbuildable plan", got.Artifacts)
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

// releasePipeline yields two artifacts from one commit: the image on every
// push, the binaries on tags only.
func releasePipeline(t *testing.T) config.Pipeline {
	t.Helper()
	p, err := config.Parse(strings.NewReader(`
apiVersion: kiln.klarlabs.de/v1
kind: Pipeline
on:
  push: [prove, publish]
  tag: [prove, publish]
publish:
  - kind: image
    image: ` + image + `
    tags: [sha, latest, semver]
  - kind: binaries
`))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOneTagPublishesBothArtifacts(t *testing.T) {
	h := newHarness(t)
	r := req(t, isolation.EventTag, false, "refs/tags/v1.4.0")
	r.Pipeline = releasePipeline(t)

	got, err := h.engine.Execute(t.Context(), r)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if h.published != 1 || h.released != 1 {
		t.Errorf("published=%d released=%d, want one of each", h.published, h.released)
	}
	if len(got.Artifacts) != 2 {
		t.Fatalf("ledger recorded %d artifacts, want 2: %+v", len(got.Artifacts), got.Artifacts)
	}
	// The legacy fields still point at the image, so an older reader and
	// `kiln status --json` both keep working.
	if got.Digest != digest {
		t.Errorf("Digest = %q, want the image digest", got.Digest)
	}
}

func TestAPushPublishesTheImageAlone(t *testing.T) {
	h := newHarness(t)
	r := req(t, isolation.EventPush, false, "refs/heads/main")
	r.Pipeline = releasePipeline(t)

	if _, err := h.engine.Execute(t.Context(), r); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// goreleaser derives the version from a tag; a branch push has none.
	if h.published != 1 || h.released != 0 {
		t.Errorf("published=%d released=%d, want the image alone", h.published, h.released)
	}
}

func TestTheReleaseSeesTheTagRef(t *testing.T) {
	h := newHarness(t)
	r := req(t, isolation.EventTag, false, "refs/tags/v1.4.0")
	r.Pipeline = releasePipeline(t)

	if _, err := h.engine.Execute(t.Context(), r); err != nil {
		t.Fatal(err)
	}

	if h.lastRelease.Ref != "refs/tags/v1.4.0" {
		t.Errorf("release ref = %q", h.lastRelease.Ref)
	}
	if h.lastRelease.Artifact.Kind != config.KindBinaries {
		t.Errorf("release got artifact kind %q", h.lastRelease.Artifact.Kind)
	}
}

func TestAFailedReleaseStopsTheRunAndKeepsWhatShipped(t *testing.T) {
	h := newHarness(t)
	h.releaseErr = errors.New("goreleaser: archive failed")
	r := req(t, isolation.EventTag, false, "refs/tags/v1.4.0")
	r.Pipeline = releasePipeline(t)

	got, err := h.engine.Execute(t.Context(), r)

	if err == nil {
		t.Fatal("want a failure")
	}
	if got.Phase != run.PhaseFailed {
		t.Errorf("Phase = %s", got.Phase)
	}
	// The image did publish. Losing that from the record would leave an
	// operator hunting for an artifact the ledger says was never made.
	if len(got.Artifacts) != 1 || got.Artifacts[0].Kind != "image" {
		t.Errorf("artifacts = %+v, want the image that did ship", got.Artifacts)
	}
	if !strings.Contains(err.Error(), "publish[1] (binaries)") {
		t.Errorf("error should name which artifact failed, got %v", err)
	}
}

func TestAFailedImageSkipsTheRelease(t *testing.T) {
	h := newHarness(t)
	h.pubErr = errors.New("registry refused")
	r := req(t, isolation.EventTag, false, "refs/tags/v1.4.0")
	r.Pipeline = releasePipeline(t)

	if _, err := h.engine.Execute(t.Context(), r); err == nil {
		t.Fatal("want a failure")
	}

	// A half-published version is worse than a clearly failed one.
	if h.released != 0 {
		t.Error("released binaries for a version whose image never shipped")
	}
}

func TestForkPullRequestPublishesNeitherKind(t *testing.T) {
	h := newHarness(t)
	r := req(t, isolation.EventPullRequest, true, "refs/pull/7/head")
	r.Pipeline = releasePipeline(t)
	r.Pipeline.On.PullRequest = []config.Step{config.StepProve, config.StepPublish}

	if _, err := h.engine.Execute(t.Context(), r); err != nil {
		t.Fatal(err)
	}

	if h.published != 0 || h.released != 0 {
		t.Errorf("published=%d released=%d, want nothing from a fork", h.published, h.released)
	}
}

func TestMissingReleasePublisherIsAFailure(t *testing.T) {
	h := newHarness(t)
	h.engine.ReleasePublisher = nil
	r := req(t, isolation.EventTag, false, "refs/tags/v1.4.0")
	r.Pipeline = releasePipeline(t)

	_, err := h.engine.Execute(t.Context(), r)

	// A kind kiln cannot honour must fail loudly rather than silently produce
	// one artifact where two were configured.
	if err == nil || !strings.Contains(err.Error(), "no publisher") {
		t.Errorf("err = %v, want a missing-publisher failure", err)
	}
}
