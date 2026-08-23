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

// The check that would have caught this without anyone going to look.
//
// It has to read the path out of the *installed* unit, not recompute it from
// the running binary. The first version of this check did the latter and
// reported a healthy box while the schedule pointed at a deleted 0.3.4 — it
// passed its unit test only because the test set the field by hand, which is
// not something that happens.
func TestStatusReadsTheBinaryFromTheInstalledUnit(t *testing.T) {
	dir := t.TempDir()
	gone := filepath.Join(dir, "upgraded-away", "kiln")
	unit := filepath.Join(dir, "unit.plist")
	if err := os.WriteFile(unit, []byte(unitNaming(gone)), 0o644); err != nil {
		t.Fatal(err)
	}

	// exe is whatever kiln is running *now*, and it exists — that is exactly
	// the state after an upgrade, and the reason recomputing it is useless.
	running := filepath.Join(dir, "bin", "kiln")
	mustExe(t, running)

	a := &agent{repo: dir, unit: unit, exe: running, goos: "darwin"}
	var out bytes.Buffer

	if err := a.status(IO{Out: &out, Err: &out}); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out.String(), "no longer exists") {
		t.Errorf("status reported a healthy box while the schedule names a deleted binary:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "kiln box install") {
		t.Errorf("status did not say how to repair it:\n%s", out.String())
	}
}

// A schedule whose binary is still there must not be reported as broken.
func TestStatusIsQuietWhenTheScheduledBinaryExists(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "bin", "kiln")
	mustExe(t, live)
	unit := filepath.Join(dir, "unit.plist")
	if err := os.WriteFile(unit, []byte(unitNaming(live)), 0o644); err != nil {
		t.Fatal(err)
	}

	a := &agent{repo: dir, unit: unit, exe: live, goos: "darwin"}
	var out bytes.Buffer

	if err := a.status(IO{Out: &out, Err: &out}); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(out.String(), "no longer exists") {
		t.Errorf("status cried wolf about a working box:\n%s", out.String())
	}
}

// unitNaming is a launchd plist naming one program, which is all the check reads.
func unitNaming(exe string) string {
	// Label first, as a real plist has it — the version of this that scanned
	// from the top of the file returned the label instead of the binary.
	return `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
  <key>Label</key>
  <string>de.klarlabs.kiln.example</string>
  <key>ProgramArguments</key>
  <array><string>` + exe + `</string><string>watch</string></array>
</dict></plist>
`
}
