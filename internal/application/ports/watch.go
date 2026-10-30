package ports

import (
	"context"
	"errors"
	"time"
)

// Schedule remembers when each scheduled task last fired, so an interval
// survives a restart. A box that forgot would fire everything on every boot.
type Schedule interface {
	// DueAt reports whether a task whose interval is every is due at now.
	DueAt(task string, every time.Duration, now time.Time) (bool, error)
	// Fired records that it just ran.
	Fired(task string, now time.Time) error
}

// RepoLock is one held claim on a repository, released when the work is done.
type RepoLock interface {
	Release() error
}

// ErrRepoBusy reports that somebody else holds the repository. It is not a
// failure: skipping is the point, and the next tick finds whatever is left.
var ErrRepoBusy = errors.New("repository is busy")

// Locks serialises work on a repository between processes.
//
// A box ticks on a timer while an operator may run kiln by hand in the same
// checkout. Two of them building the same commit into the same worktree is not
// slow, it is wrong.
type Locks interface {
	// TryAcquire claims repoDir for holder, or returns ErrRepoBusy.
	TryAcquire(repoDir, holder string) (RepoLock, error)
	// HolderOf describes whoever currently holds it, for the log line that
	// explains why a tick did nothing.
	HolderOf(repoDir string) string
}

// PruneOptions asks for docker disk to be reclaimed.
type PruneOptions struct {
	// Repos are the image repositories to consider. Only kiln's own sha tags
	// are ever removed.
	Repos []string
	// Keep is how many sha-tagged builds of each image to leave behind.
	Keep int
	// DryRun reports what would go without removing it.
	DryRun bool
	// BuildCacheMaxAge bounds the builder cache by age. Zero leaves it alone.
	BuildCacheMaxAge time.Duration
}

// PruneResult is what a prune actually did, so a box can say so rather than
// reclaiming disk silently.
type PruneResult struct {
	Removed []string
	Kept    int
	// CacheFreed is docker's own report of the space the cache prune
	// reclaimed, verbatim.
	CacheFreed string
}

// Pruner reclaims docker disk on the box.
type Pruner interface {
	Prune(ctx context.Context, opts PruneOptions) (PruneResult, error)
}

// ErrBoxNotReady reports a box that cannot do the work a tick would start: a
// disk too full to link a binary, a docker daemon that does not answer.
//
// Like ErrRepoBusy it is not a build failure. Before it existed, a full disk
// surfaced as hundreds of failed runs — every commit's gate dying in the
// linker — and each one was recorded against a commit that had done nothing
// wrong. Nothing is recorded now; the next tick looks again.
var ErrBoxNotReady = errors.New("box is not ready")

// BoxHealth answers whether this machine can build at all right now.
type BoxHealth interface {
	// FreeBytes reports the space an unprivileged writer can still use on
	// the filesystem holding path.
	FreeBytes(path string) (uint64, error)
	// DockerReady reports whether the docker daemon answers. A missing
	// docker binary is an error too: a run that needs one cannot start.
	DockerReady(ctx context.Context) error
}
