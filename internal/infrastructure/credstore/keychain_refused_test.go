package credstore

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.klarlabs.de/kiln/internal/infrastructure/execx"
)

// exitRunner stands in for `security` exiting with a status.
type exitRunner struct{ code int }

func (r exitRunner) Run(_ context.Context, c execx.Cmd) (execx.Result, error) {
	return execx.Result{ExitCode: r.code}, &execx.ExitError{Cmd: c.Name, Code: r.code}
}

func (exitRunner) LookPath(string) (string, error) { return "/usr/bin/security", nil }

// macOS ties a keychain item's access list to the reading binary's code
// signature, and an ad-hoc signed kiln is a new identity on every upgrade.
// After `brew upgrade` the item is still there but refused, and the box
// reported "no github token" — the remedy for a token that does not exist,
// not for one standing behind a lock.
func TestGet_ReportsAKeychainThatRefusesThisBinary(t *testing.T) {
	s := &Store{Runner: exitRunner{code: 36}, GOOS: "darwin", Dir: t.TempDir()}

	_, _, err := s.Get(t.Context())
	if !errors.Is(err, ErrKeychainRefused) {
		t.Fatalf("err = %v, want ErrKeychainRefused", err)
	}
	for _, want := range []string{"kiln login", "GITHUB_TOKEN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message does not mention %q: %v", want, err)
		}
	}
}

// Exit 44 is errSecItemNotFound: nothing stored, which is not a fault.
func TestGet_AMissingKeychainItemIsNotFound(t *testing.T) {
	s := &Store{Runner: exitRunner{code: 44}, GOOS: "darwin", Dir: t.TempDir()}

	if _, _, err := s.Get(t.Context()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
