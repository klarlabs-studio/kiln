package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
