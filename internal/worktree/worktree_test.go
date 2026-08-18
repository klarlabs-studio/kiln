package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.klarlabs.de/kiln/internal/execx"
	"go.klarlabs.de/kiln/internal/gittest"
)

func TestAddChecksOutTheCommitNotTheWorkingCopy(t *testing.T) {
	repo := gittest.New(t)
	sha := repo.Commit("first", "app.txt", "committed\n")

	// The operator's checkout is dirty and has an untracked file. Neither may
	// reach the tree — this is the whole reason the package exists.
	repo.Write("app.txt", "uncommitted edit\n")
	repo.Write("scratch.txt", "untracked\n")

	tree, err := Add(t.Context(), execx.NewSystem(), repo.Dir, sha)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	defer func() { _ = tree.Close(context.Background()) }()

	got, err := os.ReadFile(filepath.Join(tree.Path, "app.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "committed\n" {
		t.Errorf("tree holds %q, want the committed content", got)
	}
	if _, err := os.Stat(filepath.Join(tree.Path, "scratch.txt")); !os.IsNotExist(err) {
		t.Error("an untracked file from the operator's checkout leaked into the tree")
	}
}

func TestAddPinsAnEarlierCommit(t *testing.T) {
	repo := gittest.New(t)
	first := repo.Commit("first", "v.txt", "one\n")
	repo.Commit("second", "v.txt", "two\n")

	tree, err := Add(t.Context(), execx.NewSystem(), repo.Dir, first)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	defer func() { _ = tree.Close(context.Background()) }()

	got, _ := os.ReadFile(filepath.Join(tree.Path, "v.txt"))
	if string(got) != "one\n" {
		t.Errorf("tree holds %q, want the pinned commit's content", got)
	}
}

func TestCloseRemovesTheTree(t *testing.T) {
	repo := gittest.New(t)
	sha := repo.Commit("first", "a.txt", "x\n")

	tree, err := Add(t.Context(), execx.NewSystem(), repo.Dir, sha)
	if err != nil {
		t.Fatal(err)
	}
	path := tree.Path

	if err := tree.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("Close left the tree on disk")
	}
	// A leaked administrative entry would show up here as a ghost worktree.
	if out := repo.Git("worktree", "list"); strings.Contains(out, path) {
		t.Errorf("git still lists the removed worktree:\n%s", out)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	repo := gittest.New(t)
	sha := repo.Commit("first", "a.txt", "x\n")

	tree, err := Add(t.Context(), execx.NewSystem(), repo.Dir, sha)
	if err != nil {
		t.Fatal(err)
	}
	if err := tree.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	// With's deferred Close plus an explicit one is a realistic shape; the
	// second must not fail the run.
	if err := tree.Close(t.Context()); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestCloseCleansUpWhenGitFails(t *testing.T) {
	repo := gittest.New(t)
	sha := repo.Commit("first", "a.txt", "x\n")

	tree, err := Add(t.Context(), execx.NewSystem(), repo.Dir, sha)
	if err != nil {
		t.Fatal(err)
	}
	path := tree.Path
	// Swap in a runner whose `git worktree remove` fails. The directory must
	// still go: a leaked checkout on a watch box is a disk-space incident.
	tree.runner = execx.NewFake().On("git worktree remove", execx.Response{ExitCode: 1, Stderr: "locked"})

	if err := tree.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("Close left the tree on disk after git failed")
	}
}

func TestWithCleansUpAfterAFailure(t *testing.T) {
	repo := gittest.New(t)
	sha := repo.Commit("first", "a.txt", "x\n")

	var seen string
	wantErr := errors.New("prove failed")
	err := With(t.Context(), execx.NewSystem(), repo.Dir, sha, func(dir string) error {
		seen = dir
		return wantErr
	})

	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want the callback's error", err)
	}
	if _, statErr := os.Stat(seen); !os.IsNotExist(statErr) {
		t.Error("With left the tree behind after the callback failed")
	}
}

func TestWithCleansUpAfterCancellation(t *testing.T) {
	repo := gittest.New(t)
	sha := repo.Commit("first", "a.txt", "x\n")

	ctx, cancel := context.WithCancel(t.Context())
	var seen string
	err := With(ctx, execx.NewSystem(), repo.Dir, sha, func(dir string) error {
		seen = dir
		// A run cancelled mid-prove is exactly when cleanup matters most, and
		// exactly when a cleanup inheriting ctx would refuse to run.
		cancel()
		return ctx.Err()
	})
	if err == nil {
		t.Fatal("want the cancellation error")
	}
	if _, statErr := os.Stat(seen); !os.IsNotExist(statErr) {
		t.Error("cancellation leaked a worktree")
	}
}

func TestAddRejectsAnEmptySHA(t *testing.T) {
	if _, err := Add(t.Context(), execx.NewFake(), t.TempDir(), "  "); err == nil {
		t.Error("Add accepted an empty commit")
	}
}

func TestAddOnAnUnknownCommitFails(t *testing.T) {
	repo := gittest.New(t)
	repo.Commit("first", "a.txt", "x\n")

	_, err := Add(t.Context(), execx.NewSystem(), repo.Dir, "0000000000000000000000000000000000000000")
	if err == nil {
		t.Fatal("want an error for an unknown commit")
	}
	// The temp directory must not survive a failed add.
	entries, _ := filepath.Glob(filepath.Join(os.TempDir(), "kiln-tree-*"))
	for _, e := range entries {
		if strings.Contains(err.Error(), e) {
			t.Errorf("failed Add leaked %s", e)
		}
	}
}

func TestResolveSHAPeelsAnAnnotatedTag(t *testing.T) {
	repo := gittest.New(t)
	sha := repo.Commit("first", "a.txt", "x\n")
	repo.Tag("v1.2.0")

	got, err := ResolveSHA(t.Context(), execx.NewSystem(), repo.Dir, "v1.2.0")
	if err != nil {
		t.Fatalf("ResolveSHA: %v", err)
	}
	// Without ^{commit} this would be the tag object's own id, which no
	// worktree can check out.
	if got != sha {
		t.Errorf("ResolveSHA = %s, want the commit %s", got, sha)
	}
}

func TestResolveSHAOnHEAD(t *testing.T) {
	repo := gittest.New(t)
	sha := repo.Commit("first", "a.txt", "x\n")

	got, err := ResolveSHA(t.Context(), execx.NewSystem(), repo.Dir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if got != sha {
		t.Errorf("ResolveSHA(HEAD) = %s, want %s", got, sha)
	}
}

func TestResolveSHARejectsEmptyAndUnknown(t *testing.T) {
	repo := gittest.New(t)
	repo.Commit("first", "a.txt", "x\n")

	if _, err := ResolveSHA(t.Context(), execx.NewSystem(), repo.Dir, ""); err == nil {
		t.Error("ResolveSHA accepted an empty commitish")
	}
	if _, err := ResolveSHA(t.Context(), execx.NewSystem(), repo.Dir, "no-such-ref"); err == nil {
		t.Error("ResolveSHA accepted an unknown ref")
	}
}

func TestIsRepo(t *testing.T) {
	repo := gittest.New(t)

	if !IsRepo(t.Context(), execx.NewSystem(), repo.Dir) {
		t.Error("IsRepo = false inside a repository")
	}
	if IsRepo(t.Context(), execx.NewSystem(), os.TempDir()) {
		t.Error("IsRepo = true outside a repository")
	}
}

// ownTemp gives the reaper tests a temp directory of their own.
//
// Reap sweeps os.TempDir(), and without this the suite would be rummaging
// through the real one: on a developer's box that means a `go test ./...`
// could delete the checkout of a kiln build running in another terminal, and
// in CI it means these tests and the watch package's reaper are working the
// same directory at the same time.
func ownTemp(t *testing.T) {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
}

// abandoned fabricates the leavings of a killed run: a kiln temp directory
// with a checkout inside it that git no longer knows about.
func abandoned(t *testing.T, age time.Duration) string {
	t.Helper()
	dir, err := os.MkdirTemp("", TempPrefix+"*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	if err := os.MkdirAll(filepath.Join(dir, "tree"), 0o750); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(dir, when, when); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestReapRemovesAnAbandonedTree(t *testing.T) {
	ownTemp(t)
	repo := gittest.New(t)
	repo.Commit("first", "a.txt", "x\n")
	stale := abandoned(t, 48*time.Hour)

	removed, err := Reap(t.Context(), execx.NewSystem(), repo.Dir, ReapAfter)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}

	if removed < 1 {
		t.Errorf("removed %d, want the stale tree collected", removed)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("the abandoned tree survived")
	}
}

func TestReapSparesARecentTree(t *testing.T) {
	ownTemp(t)
	repo := gittest.New(t)
	repo.Commit("first", "a.txt", "x\n")
	fresh := abandoned(t, time.Minute)

	if _, err := Reap(t.Context(), execx.NewSystem(), repo.Dir, ReapAfter); err != nil {
		t.Fatal(err)
	}

	// The reaper cannot ask a directory whether a build is using it, so age is
	// the only evidence. A minute-old tree is very likely someone's build.
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("reaped a recent tree: %v", err)
	}
}

func TestReapNeverTouchesALiveTree(t *testing.T) {
	ownTemp(t)
	repo := gittest.New(t)
	sha := repo.Commit("first", "a.txt", "x\n")

	tree, err := Add(t.Context(), execx.NewSystem(), repo.Dir, sha)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tree.Close(context.Background()) }()

	// Backdate it well past the cutoff: only the live-tree check can save it,
	// which is the point. A long build must not have its checkout deleted
	// underneath it.
	outer := strings.TrimSuffix(tree.Path, "/tree")
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(outer, old, old); err != nil {
		t.Fatal(err)
	}

	if _, err := Reap(t.Context(), execx.NewSystem(), repo.Dir, ReapAfter); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(tree.Path); err != nil {
		t.Errorf("reaped a worktree that is in use: %v", err)
	}
}

func TestReapCollectsWhatAKilledRunLeftRegistered(t *testing.T) {
	ownTemp(t)
	repo := gittest.New(t)
	sha := repo.Commit("first", "a.txt", "x\n")

	tree, err := Add(t.Context(), execx.NewSystem(), repo.Dir, sha)
	if err != nil {
		t.Fatal(err)
	}

	// A SIGKILL takes the process without running any cleanup, so the checkout
	// and git's administrative entry both survive it — and the kernel drops
	// the flock. Reproduce exactly that: no Close, no `worktree remove`.
	if err := tree.owner.Release(); err != nil {
		t.Fatal(err)
	}
	outer := strings.TrimSuffix(tree.Path, "/tree")
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(outer, old, old); err != nil {
		t.Fatal(err)
	}

	if _, err := Reap(t.Context(), execx.NewSystem(), repo.Dir, ReapAfter); err != nil {
		t.Fatal(err)
	}

	// git still lists it — `worktree prune` only drops entries whose directory
	// has gone. Trusting that listing alone would make the leavings of every
	// killed run permanent, which is the one case the reaper exists for.
	if _, err := os.Stat(outer); !os.IsNotExist(err) {
		t.Error("a killed run's checkout survived because git still had it registered")
	}
}

func TestReapSparesAnotherRepositorysLiveTree(t *testing.T) {
	ownTemp(t)
	mine := gittest.New(t)
	mine.Commit("first", "a.txt", "x\n")

	theirs := gittest.New(t)
	sha := theirs.Commit("first", "b.txt", "y\n")
	tree, err := Add(t.Context(), execx.NewSystem(), theirs.Dir, sha)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tree.Close(context.Background()) }()

	// git can only tell us about our own repository's worktrees, so a tree
	// belonging to another checkout on the same box looks exactly like
	// abandoned leavings. Backdate it past the cutoff: only the ownership lock
	// can save it now.
	outer := strings.TrimSuffix(tree.Path, "/tree")
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(outer, old, old); err != nil {
		t.Fatal(err)
	}

	if _, err := Reap(t.Context(), execx.NewSystem(), mine.Dir, ReapAfter); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(tree.Path); err != nil {
		t.Errorf("reaped another repository's live checkout: %v", err)
	}
}

func TestReapIgnoresForeignDirectories(t *testing.T) {
	ownTemp(t)
	repo := gittest.New(t)
	repo.Commit("first", "a.txt", "x\n")

	other, err := os.MkdirTemp("", "someone-elses-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(other) }()
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(other, old, old); err != nil {
		t.Fatal(err)
	}

	if _, err := Reap(t.Context(), execx.NewSystem(), repo.Dir, ReapAfter); err != nil {
		t.Fatal(err)
	}

	// Keying off kiln's own prefix is what keeps a shared /tmp safe.
	if _, err := os.Stat(other); err != nil {
		t.Errorf("reaped a directory kiln did not create: %v", err)
	}
}

func TestReapPrunesGitsRecord(t *testing.T) {
	ownTemp(t)
	repo := gittest.New(t)
	sha := repo.Commit("first", "a.txt", "x\n")

	tree, err := Add(t.Context(), execx.NewSystem(), repo.Dir, sha)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a SIGKILL: the directory goes, git's bookkeeping stays.
	if err := os.RemoveAll(strings.TrimSuffix(tree.Path, "/tree")); err != nil {
		t.Fatal(err)
	}

	if _, err := Reap(t.Context(), execx.NewSystem(), repo.Dir, ReapAfter); err != nil {
		t.Fatal(err)
	}

	if out := repo.Git("worktree", "list"); strings.Contains(out, tree.Path) {
		t.Errorf("git still lists a worktree that is gone:\n%s", out)
	}
}

func TestReapStopsRatherThanGuessWhenGitIsUnreadable(t *testing.T) {
	fake := execx.NewFake().On("git worktree list", execx.Response{ExitCode: 128, Stderr: "not a repository"})

	// Without the live set, age alone is not safe: a very long build looks
	// abandoned. Refusing beats deleting somebody's checkout.
	if _, err := Reap(t.Context(), fake, t.TempDir(), ReapAfter); err == nil {
		t.Error("Reap proceeded without knowing which trees are live")
	}
}
