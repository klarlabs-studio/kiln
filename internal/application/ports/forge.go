package ports

import (
	"context"

	"go.klarlabs.de/kiln/internal/domain/forge"
)

// Host is the authority-critical surface of a code host.
//
// Surfaces ask which pull requests are open and whether a given one is a
// fork. They do not pick an isolation policy. A missing or disabled host
// is "I cannot tell", which authority treats as a fork.
//
// GitHub is one implementation. Gitea and Forgejo are another. Adding a
// host must not require a new check language or a runner protocol.
type Host interface {
	Enabled() bool
	LookupPull(ctx context.Context, number int) (forge.Pull, error)
	ListOpenPulls(ctx context.Context) ([]forge.Pull, error)
}
