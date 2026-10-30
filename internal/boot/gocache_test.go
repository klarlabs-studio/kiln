package boot

import (
	"os"
	"path/filepath"
	"testing"

	"go.klarlabs.de/kiln/internal/gittest"
	"go.klarlabs.de/kiln/internal/infrastructure/gocache"
	"go.klarlabs.de/kiln/internal/infrastructure/prove"
	"go.klarlabs.de/kiln/internal/infrastructure/publish"
	"go.klarlabs.de/kiln/internal/infrastructure/task"
)

func TestBuildClaimsTheGoCacheAndHandsItToEveryBuilder(t *testing.T) {
	repo := gittest.New(t)
	repo.Commit("first", "app.txt", "one\n")
	e := env(t)
	e.GoCache = filepath.Join(t.TempDir(), "kiln", "go-build")

	deps := build(t, repo.Dir, e)

	if !gocache.Owned(e.GoCache) {
		t.Error("boot should create and mark the cache directory")
	}
	if got := deps.Engine.Prover.(*prove.Warden).GoCache; got != e.GoCache {
		t.Errorf("gate GoCache = %q", got)
	}
	if got := deps.Engine.Tasks.(*task.Runner).GoCache; got != e.GoCache {
		t.Errorf("task GoCache = %q", got)
	}
	if got := deps.Engine.ReleasePublisher.(*publish.Goreleaser).GoCache; got != e.GoCache {
		t.Errorf("release GoCache = %q", got)
	}
}

// A directory kiln cannot claim — somebody's populated cache — is not adopted.
// The box runs on, inheriting GOCACHE as it did before, and the trimmer is
// never pointed at it.
func TestBuildWithAnUnclaimableGoCacheRunsWithoutOne(t *testing.T) {
	repo := gittest.New(t)
	repo.Commit("first", "app.txt", "one\n")
	foreign := t.TempDir()
	if err := os.WriteFile(filepath.Join(foreign, "README"), []byte("not kiln's"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := env(t)
	e.GoCache = foreign

	deps := build(t, repo.Dir, e)

	if deps.Env.GoCache != "" {
		t.Errorf("Env.GoCache = %q, want it dropped", deps.Env.GoCache)
	}
	if got := deps.Engine.Prover.(*prove.Warden).GoCache; got != "" {
		t.Errorf("gate GoCache = %q, want empty", got)
	}
	if gocache.Owned(foreign) {
		t.Error("boot must not mark a directory it refused")
	}
}
