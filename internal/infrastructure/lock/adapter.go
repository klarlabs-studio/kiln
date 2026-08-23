package lock

import (
	"errors"
	"fmt"

	"go.klarlabs.de/kiln/internal/application/ports"
)

// Locks adapts this package to ports.Locks.
//
// The package API is path-shaped — the caller works out the lock file's
// location with PathFor and passes it in. That is a detail of how the lock is
// implemented, so the port is repository-shaped instead and this type does the
// translation.
type Locks struct{}

// NewLocks returns a Locks backed by a lock file in the repository.
func NewLocks() Locks { return Locks{} }

func (Locks) TryAcquire(repoDir, holder string) (ports.RepoLock, error) {
	l, err := TryAcquire(PathFor(repoDir), holder)
	if errors.Is(err, ErrBusy) {
		// Translated so the application can recognise "busy" without importing
		// this package to name the error it is comparing against.
		return nil, fmt.Errorf("%w: %w", ports.ErrRepoBusy, err)
	}
	if err != nil {
		return nil, err
	}
	return l, nil
}

func (Locks) HolderOf(repoDir string) string {
	return ReadHolder(PathFor(repoDir)).String()
}
