package boot

import (
	"context"
	"fmt"
	"testing"

	"go.klarlabs.de/kiln/internal/application/ports"
	"go.klarlabs.de/kiln/internal/infrastructure/credstore"
	"go.klarlabs.de/kiln/internal/infrastructure/obs"
)

type fakeTokens struct {
	token string
	err   error
}

func (f fakeTokens) Get(context.Context) (string, credstore.Kind, error) {
	return f.token, credstore.KindKeychain, f.err
}

type warnLog struct {
	ports.Logger
	warns []string
}

func (l *warnLog) Warn(msg string, kv ...any) { l.warns = append(l.warns, fmt.Sprint(msg, kv)) }

// boot dropped every error from the credential store, so a keychain that
// refused an upgraded kiln reached the operator as "no github token":
// the remedy for a token that was never stored. The store's own message
// says what to do; it has to reach the log.
func TestStoredToken_ReportsWhyAStoredTokenIsUnavailable(t *testing.T) {
	refused := fmt.Errorf("%w: run `kiln login` again", credstore.ErrKeychainRefused)
	log := &warnLog{Logger: obs.Discard()}

	if got := storedToken(t.Context(), fakeTokens{err: refused}, log); got != "" {
		t.Errorf("token = %q, want none", got)
	}
	if len(log.warns) != 1 {
		t.Fatalf("warnings = %v, want one naming the refusal", log.warns)
	}
}

func TestStoredToken_NothingStoredIsQuiet(t *testing.T) {
	log := &warnLog{Logger: obs.Discard()}
	storedToken(t.Context(), fakeTokens{err: credstore.ErrNotFound}, log)
	if len(log.warns) != 0 {
		t.Errorf("warned about an empty store: %v", log.warns)
	}
	if got := storedToken(t.Context(), fakeTokens{token: "t"}, log); got != "t" {
		t.Errorf("token = %q, want t", got)
	}
}
