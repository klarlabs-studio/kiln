package authority

import (
	"context"
	"errors"
	"testing"

	"go.klarlabs.de/kiln/internal/domain/isolation"
	"go.klarlabs.de/kiln/internal/domain/trust"
	"go.klarlabs.de/kiln/internal/gittest"
	"go.klarlabs.de/kiln/internal/infrastructure/execx"
	"go.klarlabs.de/kiln/internal/infrastructure/gitcli"
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
