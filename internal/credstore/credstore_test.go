package credstore_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.klarlabs.de/kiln/internal/credstore"
	"go.klarlabs.de/kiln/internal/execx"
)

// fileStore is a store on a machine with no keychain, which is the fallback
// path and the one that has to be safe.
func fileStore(t *testing.T) *credstore.Store {
	t.Helper()
	return &credstore.Store{Runner: execx.NewFake(), Dir: t.TempDir(), GOOS: "plan9"}
}

func TestATokenRoundTrips(t *testing.T) {
	s := fileStore(t)

	if _, err := s.Set(t.Context(), "ghp_example"); err != nil {
		t.Fatal(err)
	}
	token, kind, err := s.Get(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if token != "ghp_example" {
		t.Errorf("token = %q", token)
	}
	if kind != credstore.KindFile {
		t.Errorf("kind = %q, want the file fallback on a platform with no keychain", kind)
	}
}

func TestTheFallbackFileIsNotReadableByOthers(t *testing.T) {
	dir := t.TempDir()
	s := &credstore.Store{Runner: execx.NewFake(), Dir: dir, GOOS: "plan9"}
	if _, err := s.Set(t.Context(), "ghp_example"); err != nil {
		t.Fatal(err)
	}

	// The whole reason the keychain is preferred. When kiln does fall back, a
	// world-readable token would be worse than the cron line it replaced.
	info, err := os.Stat(filepath.Join(dir, "token"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
}

func TestNoStoredTokenIsDistinguishable(t *testing.T) {
	// "Nothing stored" and "something went wrong" lead to different advice,
	// and the CLI prints "run kiln login" for exactly one of them.
	_, _, err := fileStore(t).Get(t.Context())
	if !errors.Is(err, credstore.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestAnEmptyTokenIsRefused(t *testing.T) {
	// Storing an empty string would make Get succeed and every API call fail
	// with a 401 nobody can trace back to here.
	if _, err := fileStore(t).Set(t.Context(), "   "); err == nil {
		t.Error("an empty token was stored")
	}
}

func TestLoggingInTwiceReplaces(t *testing.T) {
	s := fileStore(t)
	if _, err := s.Set(t.Context(), "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Set(t.Context(), "second"); err != nil {
		t.Fatal(err)
	}

	token, _, err := s.Get(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if token != "second" {
		t.Errorf("token = %q, want the newer one — a stale token that still reads is worse than none", token)
	}
}

func TestLogoutRemovesIt(t *testing.T) {
	s := fileStore(t)
	if _, err := s.Set(t.Context(), "ghp_example"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Get(t.Context()); !errors.Is(err, credstore.ErrNotFound) {
		t.Errorf("err = %v, want the token gone", err)
	}
}

func TestTheKeychainIsUsedOnMacOS(t *testing.T) {
	f := execx.NewFake()
	f.On("security add-generic-password", execx.Response{})
	f.On("security find-generic-password", execx.Response{Stdout: "ghp_from_keychain\n"})

	s := &credstore.Store{Runner: f, Dir: t.TempDir(), GOOS: "darwin"}
	kind, err := s.Set(t.Context(), "ghp_example")
	if err != nil {
		t.Fatal(err)
	}
	if kind != credstore.KindKeychain {
		t.Fatalf("kind = %q, want the keychain", kind)
	}

	// -U so a second login replaces rather than erroring, and -T so a
	// background schedule can read the item without popping a dialog on a
	// machine nobody is watching.
	line := findCall(t, f, "security add-generic-password")
	for _, want := range []string{"-U", "-T"} {
		if !strings.Contains(line, " "+want+" ") {
			t.Errorf("keychain write = %q, want %s", line, want)
		}
	}

	token, kind, err := s.Get(t.Context())
	if err != nil || token != "ghp_from_keychain" {
		t.Fatalf("Get = (%q, %v)", token, err)
	}
	if kind != credstore.KindKeychain {
		t.Errorf("kind = %q", kind)
	}
}

func TestAFailedKeychainFallsBackRatherThanLosingTheToken(t *testing.T) {
	f := execx.NewFake()
	f.On("security add-generic-password", execx.Response{ExitCode: 1, Stderr: "SecKeychainItemCreateFromContent"})

	dir := t.TempDir()
	s := &credstore.Store{Runner: f, Dir: dir, GOOS: "darwin"}

	// A locked or missing keychain must not mean "you cannot use kiln here".
	kind, err := s.Set(t.Context(), "ghp_example")
	if err != nil {
		t.Fatal(err)
	}
	if kind != credstore.KindFile {
		t.Errorf("kind = %q, want the file fallback", kind)
	}
	if _, err := os.Stat(filepath.Join(dir, "token")); err != nil {
		t.Errorf("the token was not written anywhere: %v", err)
	}
}

func findCall(t *testing.T, f *execx.Fake, prefix string) string {
	t.Helper()
	for _, line := range f.Lines() {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	t.Fatalf("no call matching %q in %v", prefix, f.Lines())
	return ""
}
