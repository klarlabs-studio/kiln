package poll

import (
	"context"
	"testing"

	"go.klarlabs.de/kiln/internal/config"
	"go.klarlabs.de/kiln/internal/engine"
	"go.klarlabs.de/kiln/internal/execx"
	"go.klarlabs.de/kiln/internal/github"
	"go.klarlabs.de/kiln/internal/gittest"
	"go.klarlabs.de/kiln/internal/obs"
	"go.klarlabs.de/kiln/internal/prove"
	"go.klarlabs.de/kiln/internal/publish"
	"go.klarlabs.de/kiln/internal/store"
	"go.klarlabs.de/kiln/internal/watch"
)

func newWatcher(t *testing.T) (*watch.Watcher, *gittest.Repo) {
	t.Helper()
	upstream := gittest.New(t)
	upstream.Commit("first", "app.txt", "one\n")
	local := upstream.Clone(t)

	s := store.NewMemory()
	return &watch.Watcher{
		Engine: engine.New(engine.Engine{
			Prover:    prove.Func(func(context.Context, prove.Request) error { return nil }),
			Publisher: publish.Func(func(context.Context, publish.Request) (publish.Result, error) { return publish.Result{}, nil }),
			Store:     s,
			Log:       obs.Discard(),
		}),
		Store:         s,
		Runner:        execx.NewSystem(),
		Log:           obs.Discard(),
		Dir:           local.Dir,
		Pipeline:      config.Default(),
		FetchAttempts: 1,
	}, upstream
}

func TestPollSeesOnlyTheTrackedBranch(t *testing.T) {
	w, upstream := newWatcher(t)
	upstream.Tag("v1.0.0")
	prSHA := upstream.Commit("pr", "f.txt", "x\n")
	upstream.Git("update-ref", "refs/pull/7/head", prSHA)
	upstream.Git("reset", "--hard", "HEAD~1")

	res, err := New(w).Once(t.Context(), true)
	if err != nil {
		t.Fatalf("Once: %v", err)
	}

	if len(res.Discovered) != 1 {
		t.Fatalf("discovered %+v, want only the branch", res.Discovered)
	}
	if res.Discovered[0].Ref != "refs/heads/main" {
		t.Errorf("Ref = %q", res.Discovered[0].Ref)
	}
}

func TestPollForcesBranchOnlyAndDropsTheAPIClient(t *testing.T) {
	w, _ := newWatcher(t)
	w.BranchesOnly = false
	w.GitHub = github.NewClient("tok", github.Repo{Owner: "o", Name: "r"}, obs.Discard())

	got := New(w).Watcher()

	if !got.BranchesOnly {
		t.Error("a poller that can be configured to read pull requests is not a poller")
	}
	if got.GitHub != nil {
		t.Error("poll has no pull request jobs for an API answer to apply to")
	}
}

func TestNewDoesNotMutateTheOriginalWatcher(t *testing.T) {
	w, _ := newWatcher(t)
	client := github.NewClient("tok", github.Repo{Owner: "o", Name: "r"}, obs.Discard())
	w.GitHub = client

	_ = New(w)

	if w.BranchesOnly {
		t.Error("New mutated the caller's watcher")
	}
	if w.GitHub != client {
		t.Error("New stripped the caller's API client")
	}
}
