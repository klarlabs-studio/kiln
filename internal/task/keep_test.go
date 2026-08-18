package task_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.klarlabs.de/kiln/internal/task"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDeclaredFilesSurviveTheWorktree(t *testing.T) {
	tree, dest := t.TempDir(), t.TempDir()
	write(t, filepath.Join(tree, "coverage.out"), "mode: set\n")
	write(t, filepath.Join(tree, "reports", "nox.sarif"), "{}\n")

	kept, err := task.Keep(tree, dest, []string{"coverage.out", "reports/*.sarif"})
	if err != nil {
		t.Fatalf("Keep: %v", err)
	}
	if len(kept) != 2 {
		t.Fatalf("kept %d files: %+v", len(kept), kept)
	}

	// The point of the feature: readable after the tree is gone.
	if err := os.RemoveAll(tree); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dest, "reports", "nox.sarif"))
	if err != nil {
		t.Fatalf("the retained file did not survive: %v", err)
	}
	if string(body) != "{}\n" {
		t.Errorf("content = %q", body)
	}
}

func TestAGlobThatMatchesNothingIsReported(t *testing.T) {
	tree, dest := t.TempDir(), t.TempDir()

	// Nearly always a typo, or a build that did not get far enough. Silence
	// here is how somebody finds out a week later that the report was never
	// kept.
	_, err := task.Keep(tree, dest, []string{"coverage.out"})
	if err == nil || !strings.Contains(err.Error(), "matched nothing") {
		t.Errorf("err = %v, want it to say the pattern matched nothing", err)
	}
}

func TestOneBadPatternDoesNotLoseTheGoodOnes(t *testing.T) {
	tree, dest := t.TempDir(), t.TempDir()
	write(t, filepath.Join(tree, "coverage.out"), "mode: set\n")

	kept, err := task.Keep(tree, dest, []string{"missing.txt", "coverage.out"})
	if err == nil {
		t.Error("the missing pattern was not reported")
	}
	if len(kept) != 1 || kept[0].Name != "coverage.out" {
		t.Errorf("kept = %+v, want the file that did exist", kept)
	}
}

func TestAPatternCannotEscapeTheWorktree(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "tree")
	dest := filepath.Join(root, "dest")
	write(t, filepath.Join(tree, "keep.txt"), "fine\n")
	write(t, filepath.Join(root, "secret.txt"), "ssh key\n")

	// The pattern comes from the repository, so this is reachable from a pull
	// request: retention copies to a directory the operator later reads, and
	// must not be a way to lift files off the build box.
	for _, pattern := range []string{"../secret.txt", "/etc/hosts"} {
		kept, err := task.Keep(tree, dest, []string{pattern})
		if err == nil {
			t.Errorf("%s was allowed", pattern)
		}
		if len(kept) != 0 {
			t.Errorf("%s copied %+v", pattern, kept)
		}
	}
}

func TestASymlinkOutOfTheWorktreeIsRefused(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "tree")
	dest := filepath.Join(root, "dest")
	write(t, filepath.Join(tree, "placeholder"), "x")
	write(t, filepath.Join(root, "secret.txt"), "ssh key\n")

	// The second half of the same attack: the pattern looks innocent and the
	// file it matches is a link to somewhere else.
	if err := os.Symlink(filepath.Join(root, "secret.txt"), filepath.Join(tree, "report.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	kept, err := task.Keep(tree, dest, []string{"report.txt"})
	if err == nil {
		t.Error("a symlink pointing outside the worktree was followed")
	}
	if len(kept) != 0 {
		t.Errorf("copied %+v", kept)
	}
}

func TestADirectoryMatchIsSkippedRatherThanWalked(t *testing.T) {
	tree, dest := t.TempDir(), t.TempDir()
	write(t, filepath.Join(tree, "reports", "a.txt"), "a\n")

	// A pattern that accidentally matches a directory — `*`, or `.` — would
	// otherwise copy the whole checkout, .git included.
	kept, err := task.Keep(tree, dest, []string{"reports"})
	if err == nil {
		t.Error("a directory-only match reported success")
	}
	if len(kept) != 0 {
		t.Errorf("kept %+v", kept)
	}
}

func TestSweepKeepsTheNewestRuns(t *testing.T) {
	root := t.TempDir()
	// Run ids are timestamp-prefixed, so lexical order is chronological.
	for _, id := range []string{
		"run-20260818T100000Z-a", "run-20260818T110000Z-b",
		"run-20260818T120000Z-c", "run-20260818T130000Z-d",
	} {
		write(t, filepath.Join(task.KeepDir(root, id, "scan"), "out.txt"), id)
	}

	if err := task.Sweep(root, 2); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(filepath.Join(root, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("kept %d runs, want 2", len(entries))
	}
	if entries[0].Name() != "run-20260818T120000Z-c" || entries[1].Name() != "run-20260818T130000Z-d" {
		t.Errorf("kept %s and %s, want the two newest", entries[0].Name(), entries[1].Name())
	}
}

func TestSweepOnAnEmptyRootIsNotAnError(t *testing.T) {
	// The first run on a fresh box sweeps before anything has been kept.
	if err := task.Sweep(t.TempDir(), 5); err != nil {
		t.Errorf("Sweep: %v", err)
	}
}
