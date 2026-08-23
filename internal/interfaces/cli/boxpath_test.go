package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A schedule outlives the build that wrote it. Naming the resolved binary —
// which under Homebrew is Caskroom/kiln/<version>/kiln — means `brew upgrade`
// deletes it and the box stops running, with no log line, no status and no
// non-zero exit. That is exactly what happened to a real box today.
func TestScheduledPathPrefersTheStableNameOnPath(t *testing.T) {
	dir := t.TempDir()
	versioned := filepath.Join(dir, "kiln-1.2.3", "kiln")
	stable := filepath.Join(dir, "bin", "kiln")
	mustExe(t, versioned)
	if err := os.MkdirAll(filepath.Dir(stable), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(versioned, stable); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(stable))

	if got := scheduledPath(versioned); got != stable {
		t.Errorf("scheduledPath = %q, want the symlink %q that survives an upgrade", got, stable)
	}
}

// The safety condition: only borrow the name on PATH when it is the same
// binary. Otherwise a box installed from a local build would silently be
// scheduled against whatever kiln happens to be installed.
func TestScheduledPathKeepsTheBinaryItWasRunFrom(t *testing.T) {
	dir := t.TempDir()
	running := filepath.Join(dir, "build", "kiln")
	other := filepath.Join(dir, "bin", "kiln")
	mustExe(t, running)
	mustExe(t, other)
	t.Setenv("PATH", filepath.Dir(other))

	if got := scheduledPath(running); got != running {
		t.Errorf("scheduledPath = %q, want %q: a different kiln on PATH must not be substituted", got, running)
	}
}

// Nothing named kiln on PATH at all — a box installed from a build directory.
func TestScheduledPathFallsBackWhenNothingIsOnPath(t *testing.T) {
	dir := t.TempDir()
	running := filepath.Join(dir, "build", "kiln")
	mustExe(t, running)
	t.Setenv("PATH", t.TempDir())

	if got := scheduledPath(running); got != running {
		t.Errorf("scheduledPath = %q, want %q", got, running)
	}
}

func mustExe(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// The check that would have caught this without anyone going to look. A box
// whose binary has been deleted keeps its plist, stays listed by launchctl,
// and simply stops — so "installed" on its own is not a useful thing to say.
func TestStatusReportsAScheduleWhoseBinaryIsGone(t *testing.T) {
	dir := t.TempDir()
	unit := filepath.Join(dir, "unit.plist")
	if err := os.WriteFile(unit, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := &agent{repo: dir, unit: unit, exe: filepath.Join(dir, "upgraded-away", "kiln")}
	var out bytes.Buffer

	if err := a.status(IO{Out: &out, Err: &out}); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out.String(), "no longer exists") {
		t.Errorf("status said nothing about the missing binary:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "kiln box install") {
		t.Errorf("status did not say how to repair it:\n%s", out.String())
	}
}
