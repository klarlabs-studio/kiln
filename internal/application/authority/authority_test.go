package authority

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.klarlabs.de/kiln/internal/application/engine"
	"go.klarlabs.de/kiln/internal/application/ports"
	"go.klarlabs.de/kiln/internal/domain/config"
	"go.klarlabs.de/kiln/internal/domain/isolation"
	"go.klarlabs.de/kiln/internal/domain/trust"
	"go.klarlabs.de/kiln/internal/gittest"
	"go.klarlabs.de/kiln/internal/infrastructure/execx"
	"go.klarlabs.de/kiln/internal/infrastructure/gitcli"
	"go.klarlabs.de/kiln/internal/infrastructure/lock"
	"go.klarlabs.de/kiln/internal/infrastructure/store"
)

func resolver(t *testing.T, dir string) *Resolver {
	t.Helper()
	return &Resolver{
		Git:     gitcli.New(execx.NewSystem()),
		Dir:     dir,
		Remote:  "origin",
		Watched: "main",
	}
}

func TestPushOnMainIsEstablished(t *testing.T) {
	repo := gittest.New(t)
	sha := repo.Commit("first", "app.txt", "one\n")

	got, err := resolver(t, repo.Dir).Resolve(t.Context(), trust.Claim{
		SHA: sha, Event: isolation.EventPush,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !got.Established || got.Fork || !got.Policy().Publish {
		t.Errorf("context = %+v policy = %+v", got, got.Policy())
	}
}

func TestPushOfAnUnknownSHAIsRefused(t *testing.T) {
	repo := gittest.New(t)
	repo.Commit("first", "app.txt", "one\n")

	_, err := resolver(t, repo.Dir).Resolve(t.Context(), trust.Claim{
		SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Event: isolation.EventPush,
	})
	if !errors.Is(err, ErrUnestablished) {
		t.Fatalf("err = %v, want ErrUnestablished", err)
	}
}

func TestEstablishedWebhookIsTrusted(t *testing.T) {
	// A verified delivery already classified the event. Membership is not
	// re-asked — the HMAC is the evidence.
	got, err := (&Resolver{}).Resolve(t.Context(), trust.Claim{
		SHA: "abc", Event: isolation.EventPush, Ref: "refs/heads/main",
		Established: true,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !got.Policy().Publish {
		t.Error("an established push lost publish authority")
	}
}

func TestPullRequestWithoutNumberIsAFork(t *testing.T) {
	got, err := (&Resolver{}).Resolve(t.Context(), trust.Claim{
		SHA: "abc", Event: isolation.EventPullRequest,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !got.Fork || got.Policy().Publish || got.Policy().Skip {
		t.Errorf("unknown PR was not untrusted: %+v %+v", got, got.Policy())
	}
}

func TestForkFloorCannotBeLifted(t *testing.T) {
	r := &Resolver{
		ForkOf: func(context.Context, int) (bool, bool) { return false, true },
	}
	got, err := r.Resolve(t.Context(), trust.Claim{
		SHA: "abc", Event: isolation.EventPullRequest, PR: 7, Fork: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Fork {
		t.Error("an explicit fork floor was lifted by the API")
	}
}

func TestFailedForkLookupIsAFork(t *testing.T) {
	r := &Resolver{
		ForkOf: func(context.Context, int) (bool, bool) { return false, false },
	}
	got, err := r.Resolve(t.Context(), trust.Claim{
		SHA: "abc", Event: isolation.EventPullRequest, PR: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Fork {
		t.Error("an unanswerable lookup must fail closed")
	}
}

func TestTagOfAnUnknownSHAIsRefused(t *testing.T) {
	repo := gittest.New(t)
	repo.Commit("first", "app.txt", "one\n")

	_, err := resolver(t, repo.Dir).Resolve(t.Context(), trust.Claim{
		SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Event: isolation.EventTag,
	})
	if !errors.Is(err, ErrUnestablished) {
		t.Fatalf("err = %v, want ErrUnestablished", err)
	}
}

func TestSameRepoPRIsResolved(t *testing.T) {
	r := &Resolver{
		ForkOf: func(context.Context, int) (bool, bool) { return false, true },
	}
	got, err := r.Resolve(t.Context(), trust.Claim{
		SHA: "abc", Event: isolation.EventPullRequest, PR: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Fork || !got.Policy().Skip {
		t.Errorf("same-repo PR = %+v policy = %+v", got, got.Policy())
	}
	if got.Ref != "refs/pull/7/head" {
		t.Errorf("ref = %q", got.Ref)
	}
}

func TestEmptySHAIsRefused(t *testing.T) {
	_, err := (&Resolver{}).Resolve(t.Context(), trust.Claim{
		Event: isolation.EventPush,
	})
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestUnknownEventIsRefused(t *testing.T) {
	_, err := (&Resolver{}).Resolve(t.Context(), trust.Claim{
		SHA: "abc", Event: isolation.Event("release"),
	})
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestNilGitCannotEstablishPush(t *testing.T) {
	_, err := (&Resolver{}).Resolve(t.Context(), trust.Claim{
		SHA: "abc", Event: isolation.EventPush,
	})
	if !errors.Is(err, ErrUnestablished) {
		t.Fatalf("err = %v, want ErrUnestablished", err)
	}
}

func TestPushOfASideBranchIsRefused(t *testing.T) {
	repo := gittest.New(t)
	repo.Commit("first", "app.txt", "one\n")
	repo.Git("checkout", "-q", "-b", "feature")
	sha := repo.Commit("second", "app.txt", "two\n")

	_, err := resolver(t, repo.Dir).Resolve(t.Context(), trust.Claim{
		SHA: sha, Event: isolation.EventPush,
	})
	if !errors.Is(err, ErrUnestablished) {
		t.Fatalf("err = %v, want ErrUnestablished", err)
	}
}

func TestTagOnAKnownSHAIsEstablished(t *testing.T) {
	repo := gittest.New(t)
	sha := repo.Commit("first", "app.txt", "one\n")
	repo.Tag("v1.0.0")

	got, err := resolver(t, repo.Dir).Resolve(t.Context(), trust.Claim{
		SHA: sha, Event: isolation.EventTag,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !got.Established || !got.Policy().Publish {
		t.Errorf("context = %+v policy = %+v", got, got.Policy())
	}
}

func TestTagOnANamedRefIsEstablished(t *testing.T) {
	repo := gittest.New(t)
	sha := repo.Commit("first", "app.txt", "one\n")
	repo.Tag("v1.0.0")

	got, err := resolver(t, repo.Dir).Resolve(t.Context(), trust.Claim{
		SHA: sha, Event: isolation.EventTag, Ref: "refs/tags/v1.0.0",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !got.Established || got.Ref != "refs/tags/v1.0.0" {
		t.Errorf("context = %+v", got)
	}
}

func testRunner(t *testing.T, repo *gittest.Repo) *Runner {
	t.Helper()
	return &Runner{
		Engine: engine.New(engine.Engine{
			Store: store.NewMemory(),
			Prover: ports.ProveFunc(func(context.Context, ports.ProveRequest) error {
				return nil
			}),
		}),
		Resolver: resolver(t, repo.Dir),
		Locks:    lock.NewLocks(),
		Dir:      repo.Dir,
	}
}

func TestExecuteTakesTheRepositoryLock(t *testing.T) {
	repo := gittest.New(t)
	sha := repo.Commit("first", "app.txt", "one\n")
	held, err := lock.NewLocks().TryAcquire(repo.Dir, "other")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Release() }()

	_, err = testRunner(t, repo).Execute(t.Context(), Request{
		Claim: trust.Claim{SHA: sha, Event: isolation.EventPush},
	}, "test")
	if !errors.Is(err, ports.ErrRepoBusy) {
		t.Fatalf("err = %v, want ErrRepoBusy", err)
	}
}

func TestExecuteLockedDoesNotAcquire(t *testing.T) {
	repo := gittest.New(t)
	sha := repo.Commit("first", "app.txt", "one\n")
	held, err := lock.NewLocks().TryAcquire(repo.Dir, "watch")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Release() }()

	// Watch already holds the lock. Re-acquiring would deadlock the tick.
	if _, err := testRunner(t, repo).ExecuteLocked(t.Context(), Request{
		Claim: trust.Claim{SHA: sha, Event: isolation.EventPush},
	}); err != nil {
		t.Fatalf("ExecuteLocked: %v", err)
	}
}

const (
	operatorImage = "ghcr.io/operator/box"
	commitImage   = "ghcr.io/commit/app"
	pinnedDB      = "postgres@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func kilnDoc(image, extra string) string {
	return `apiVersion: kiln.klarlabs.de/v1
kind: Pipeline
on:
  pull_request: [prove]
  push: [prove, publish]
prove:
  from: warden
publish:
  - kind: image
    image: ` + image + `
    tags: [sha, latest]
` + extra
}

func parsePipe(t *testing.T, doc string) config.Pipeline {
	t.Helper()
	p, err := config.Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return p
}

func TestOperatorPolicyIsNotTheCommitUnlessOptedIn(t *testing.T) {
	repo := gittest.New(t)
	sha := repo.Commit("first", ".kiln.yaml", kilnDoc(commitImage, ""))
	r := testRunner(t, repo)
	r.Pipeline = parsePipe(t, kilnDoc(operatorImage, ""))

	pipe, restore, err := r.bindPolicy(t.Context(), trust.Context{SHA: sha})
	if err != nil {
		t.Fatal(err)
	}
	defer restore()
	if pipe.Publish[0].Image != operatorImage {
		t.Errorf("image = %q: today's default must stay the operator file", pipe.Publish[0].Image)
	}
}

func TestCommitPolicyIsLoadedOnlyWhenOptedIn(t *testing.T) {
	repo := gittest.New(t)
	sha := repo.Commit("first", ".kiln.yaml", kilnDoc(commitImage, `
watch:
  ref: from-the-commit
`))
	r := testRunner(t, repo)
	r.Pipeline = parsePipe(t, kilnDoc(operatorImage, `
policy:
  from: commit
watch:
  ref: operator-main
`))
	r.Engine.Policy = trust.PolicyIdentity{Source: trust.PolicyOperator, Digest: "sha256:operator"}

	pipe, restore, err := r.bindPolicy(t.Context(), trust.Context{SHA: sha})
	if err != nil {
		t.Fatal(err)
	}
	if pipe.Publish[0].Image != commitImage {
		t.Errorf("image = %q, want the commit's publish", pipe.Publish[0].Image)
	}
	if pipe.Watch.Ref != "operator-main" {
		t.Errorf("watch.ref = %q: discovery stays operator-owned", pipe.Watch.Ref)
	}
	if r.Engine.Policy.Source != trust.PolicyCommit || r.Engine.Policy.Commit != sha {
		t.Errorf("recorded identity = %+v, want source=commit", r.Engine.Policy)
	}
	restore()
	if r.Engine.Policy.Source != trust.PolicyOperator {
		t.Errorf("restore left %+v", r.Engine.Policy)
	}
}

func TestCommitPolicyStripsForkServices(t *testing.T) {
	repo := gittest.New(t)
	sha := repo.Commit("first", ".kiln.yaml", kilnDoc(commitImage, `
services:
  db:
    image: `+pinnedDB+`
    port: 5432
`))
	r := testRunner(t, repo)
	r.Pipeline = parsePipe(t, kilnDoc(operatorImage, "policy:\n  from: commit\n"))

	pipe, restore, err := r.bindPolicy(t.Context(), trust.Context{SHA: sha, Fork: true})
	if err != nil {
		t.Fatal(err)
	}
	defer restore()
	if len(pipe.Services) != 0 {
		t.Errorf("a fork kept services: %v", pipe.Services)
	}

	kept, restore, err := r.bindPolicy(t.Context(), trust.Context{SHA: sha, Fork: false})
	if err != nil {
		t.Fatal(err)
	}
	defer restore()
	if kept.Services["db"].Image != pinnedDB {
		t.Errorf("same-repo must keep the commit's services: %v", kept.Services)
	}
}

func TestCommitPolicyMissingFileFailsTheRun(t *testing.T) {
	repo := gittest.New(t)
	sha := repo.Commit("first", "app.txt", "one\n")
	r := testRunner(t, repo)
	r.Pipeline = parsePipe(t, kilnDoc(operatorImage, "policy:\n  from: commit\n"))

	_, _, err := r.bindPolicy(t.Context(), trust.Context{SHA: sha})
	if err == nil || !strings.Contains(err.Error(), ".kiln.yaml") {
		t.Fatalf("err = %v, want a missing-file refusal", err)
	}
}
