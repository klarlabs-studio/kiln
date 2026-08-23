package credstore

import (
	"os"
	"path/filepath"
	"testing"

	"go.klarlabs.de/kiln/internal/infrastructure/binpath"
)

// The keychain item's trusted-application list was written with the resolved
// binary — Caskroom/kiln/<version>/kiln under Homebrew. `brew upgrade` deletes
// that, so the next build is not on the list, and a background schedule cannot
// read the token: it needs an approval dialog it has no way to answer.
//
// Same mistake as the launchd plist, and the same fix: name the path the
// package manager repoints.
func TestTrustedAppsNamesThePathThatSurvivesAnUpgrade(t *testing.T) {
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

	args := trustedFor(versioned)

	if !contains(args, stable) {
		t.Errorf("trusted list %v does not name %q, so the next upgrade locks the box out", args, stable)
	}
}

// Both are listed. The stable name is what survives; the resolved one is what
// the running process is, and a keychain that matches on the concrete binary
// should still recognise this build.
func TestTrustedAppsAlsoNamesTheRunningBinary(t *testing.T) {
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

	args := trustedFor(versioned)

	if !contains(args, versioned) {
		t.Errorf("trusted list %v dropped the running binary %q", args, versioned)
	}
}

// Nothing on PATH resolving to this binary: the running one is still trusted,
// and no path is listed twice.
func TestTrustedAppsListsEachPathOnce(t *testing.T) {
	dir := t.TempDir()
	only := filepath.Join(dir, "build", "kiln")
	mustExe(t, only)
	t.Setenv("PATH", t.TempDir())

	args := trustedFor(only)

	if !contains(args, only) {
		t.Errorf("trusted list %v does not name the running binary %q", args, only)
	}
	seen := map[string]bool{}
	for _, a := range args {
		if a == "-T" {
			continue
		}
		if seen[a] {
			t.Errorf("path listed twice: %q in %v", a, args)
		}
		seen[a] = true
	}
}

// contains compares by file identity, not text: on macOS /var is a symlink to
// /private/var, so the same binary has two spellings and asserting on one of
// them tests the spelling rather than the behaviour.
func contains(args []string, want string) bool {
	for _, a := range args {
		if a == want || binpath.SameFile(a, want) {
			return true
		}
	}
	return false
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
