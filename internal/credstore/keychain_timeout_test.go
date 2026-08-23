package credstore

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.klarlabs.de/kiln/internal/execx"
)

// hangingRunner stands in for `security` waiting on a permission dialog: it
// returns only when the context is done.
type hangingRunner struct{}

func (hangingRunner) Run(ctx context.Context, _ execx.Cmd) (execx.Result, error) {
	<-ctx.Done()
	return execx.Result{}, ctx.Err()
}

func (hangingRunner) LookPath(string) (string, error) { return "/usr/bin/security", nil }

// A box tick blocked forever on `security find-generic-password`, having
// produced no output, because a keychain prompt never returns. An unattended
// process must not be able to block without limit on a credential lookup —
// whatever the reason: a locked keychain, an ACL invalidated by an OS update,
// a binary replaced by an upgrade.
func TestKeychainGet_DoesNotHangForever(t *testing.T) {
	s := &Store{Runner: hangingRunner{}}

	start := time.Now()
	_, err := s.keychainGet(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a hanging keychain reported success")
	}
	if !errors.Is(err, ErrKeychainBlocked) {
		t.Errorf("err = %v, want ErrKeychainBlocked", err)
	}
	if elapsed > keychainReadTimeout+2*time.Second {
		t.Errorf("took %s, want to give up near %s", elapsed, keychainReadTimeout)
	}
}

// The message has to point at the actual remedy. "no stored token" would send
// someone to `kiln login`, which is not the problem when a token is there and
// something is standing in front of it.
func TestKeychainGet_SaysWhatToDo(t *testing.T) {
	s := &Store{Runner: hangingRunner{}}

	_, err := s.keychainGet(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"permission dialog", "GITHUB_TOKEN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message does not mention %q: %v", want, err)
		}
	}
}
