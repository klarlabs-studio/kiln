package gocache

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func userCache(dir string) func() (string, error) {
	return func() (string, error) { return dir, nil }
}

func TestResolveDefaultsToAKilnDirectoryUnderTheUserCache(t *testing.T) {
	got := Resolve("", "", false, userCache("/Users/op/Library/Caches"))
	want := filepath.Join("/Users/op/Library/Caches", "kiln", "go-build")
	if got != want {
		t.Errorf("Resolve = %q, want %q", got, want)
	}
}

func TestResolveRespectsAnOperatorGOCACHE(t *testing.T) {
	if got := Resolve("", "/srv/go-cache", true, userCache("/c")); got != "" {
		t.Errorf("Resolve = %q, want empty: an explicit GOCACHE is the operator's decision", got)
	}
	// Even a KILN_GOCACHE path does not override it: the operator said where
	// Go's cache lives, and kiln does not get to second-guess that per child.
	if got := Resolve("/kiln/cache", "/srv/go-cache", true, userCache("/c")); got != "" {
		t.Errorf("Resolve = %q, want empty", got)
	}
}

func TestResolveTreatsAnEmptyGOCACHEAsUnset(t *testing.T) {
	// `GOCACHE=` is how a shell unsets it in a plist; Go reads it as default.
	if got := Resolve("", "", true, userCache("/c")); got == "" {
		t.Error("an empty GOCACHE should not disable kiln's cache")
	}
}

func TestResolveOffDisablesTheKilnCache(t *testing.T) {
	for _, v := range []string{"off", "OFF", " off ", "0", "false", "none"} {
		if got := Resolve(v, "", false, userCache("/c")); got != "" {
			t.Errorf("Resolve(%q) = %q, want empty", v, got)
		}
	}
}

func TestResolveHonoursAnExplicitPath(t *testing.T) {
	if got := Resolve("/var/kiln/go-build", "", false, userCache("/c")); got != "/var/kiln/go-build" {
		t.Errorf("Resolve = %q", got)
	}
}

func TestResolveWithoutAUserCacheDirIsDisabled(t *testing.T) {
	none := func() (string, error) { return "", errors.New("no HOME") }
	if got := Resolve("", "", false, none); got != "" {
		t.Errorf("Resolve = %q, want empty", got)
	}
}

func TestValidateRefusesARelativePath(t *testing.T) {
	// Relative to what? Every child runs in a different worktree, so each
	// would get a cache of its own that nothing ever trims.
	if err := Validate("cache/go-build", userCache("/c")); err == nil {
		t.Error("want a relative KILN_GOCACHE refused")
	}
}

func TestValidateRefusesTheOperatorsOwnGoCache(t *testing.T) {
	if err := Validate("/c/go-build", userCache("/c")); err == nil {
		t.Error("want Go's default cache refused: kiln would trim the operator's cache")
	}
	if err := Validate("/c/go-build/", userCache("/c")); err == nil {
		t.Error("want a trailing slash not to slip past the guard")
	}
}

func TestValidateAcceptsDisabledAndAbsolute(t *testing.T) {
	for _, dir := range []string{"", "/c/kiln/go-build"} {
		if err := Validate(dir, userCache("/c")); err != nil {
			t.Errorf("Validate(%q) = %v", dir, err)
		}
	}
}

func TestClaimCreatesAndMarksANewDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "kiln", "go-build")
	if err := Claim(dir); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !Owned(dir) {
		t.Error("a claimed directory should be owned")
	}
	// Idempotent: every boot claims.
	if err := Claim(dir); err != nil {
		t.Errorf("second Claim: %v", err)
	}
}

func TestClaimRefusesAPopulatedDirectoryItDidNotCreate(t *testing.T) {
	dir := t.TempDir()
	writeEntry(t, dir, "ab", "cafe-d", 10, time.Now())

	if err := Claim(dir); !errors.Is(err, ErrNotOwned) {
		t.Fatalf("Claim = %v, want ErrNotOwned", err)
	}
	if Owned(dir) {
		t.Error("a refused claim must not leave a marker behind")
	}
}

func TestClaimOfNothingIsANoop(t *testing.T) {
	if err := Claim(""); err != nil {
		t.Errorf("Claim(\"\") = %v", err)
	}
}

func TestEnvAddsGOCACHEOnlyWhenKilnOwnsOne(t *testing.T) {
	base := []string{"PATH=/bin"}
	if got := Env(base, ""); !slices.Equal(got, base) {
		t.Errorf("Env with no dir = %v, want unchanged", got)
	}
	got := Env(base, "/c/kiln/go-build")
	if !slices.Contains(got, "GOCACHE=/c/kiln/go-build") || !slices.Contains(got, "PATH=/bin") {
		t.Errorf("Env = %v", got)
	}
	if len(base) != 1 {
		t.Error("Env must not write through to the caller's slice")
	}
}

// writeEntry plants a Go cache file in sub/name with the given size and mtime.
func writeEntry(t *testing.T, root, sub, name string, size int, mtime time.Time) string {
	t.Helper()
	dir := filepath.Join(root, sub)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return path
}
