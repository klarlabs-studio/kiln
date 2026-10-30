package ports

import (
	"context"
	"errors"
)

// BuildSlot is one held claim on the machine's build capacity, released when
// the job is done.
type BuildSlot interface {
	Release() error
}

// ErrNoBuildSlot reports that every build slot on the machine is taken. Like
// ErrRepoBusy it is not a failure of the work that asked: nothing is wrong
// with the commit, the machine is simply full.
var ErrNoBuildSlot = errors.New("no build slot free on this machine")

// BuildSlots bounds how many kiln builds run on one machine at once, across
// every repository and every process.
//
// The repository lock serialises work on one checkout and says nothing about
// the box as a whole. A Mac running eight box agents on the same half-hour
// schedule started eight `go test -race` runs and docker builds in the same
// minute; the machine went 11.8 GB into swap and builds failed with "no space
// left on device" — failures that were then blamed on the commits.
type BuildSlots interface {
	// TryAcquire takes a free slot without waiting, or returns ErrNoBuildSlot.
	TryAcquire(holder string) (BuildSlot, error)
	// Acquire waits a bounded time for a slot to free, returning
	// ErrNoBuildSlot when the wait runs out and ctx's error when it is
	// cancelled.
	Acquire(ctx context.Context, holder string) (BuildSlot, error)
}
