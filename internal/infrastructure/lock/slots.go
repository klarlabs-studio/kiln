package lock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.klarlabs.de/kiln/internal/application/ports"
)

const (
	// DefaultSlotWait bounds how long a build waits for a machine slot before
	// it is deferred to the next tick. Half the half-hour box interval: long
	// enough that a typical build ahead of it finishes and the job runs this
	// tick, short enough that a box never stacks a second tick behind it.
	DefaultSlotWait = 15 * time.Minute

	// slotPoll is how often a waiting build looks for a free slot. flock has
	// no portable "wait for any of these N", so it is a poll — cheap, since a
	// try is one open and one syscall per slot.
	slotPoll = 2 * time.Second
)

// Slots is a machine-wide counting semaphore made of N lock files.
//
// The same flock as the repository lock, for the same reason: the kernel
// drops a slot when its process dies, so a box agent killed mid-build hands
// its capacity back without anyone deleting a file. A counter in a file would
// leak a slot on every crash until the machine could build nothing at all.
type Slots struct {
	dir  string
	n    int
	wait time.Duration
	poll time.Duration
}

// NewSlots returns n slots under dir that wait at most wait for one to free.
// n <= 0 means no limit. Every process that should share the limit must use
// the same dir.
func NewSlots(dir string, n int, wait time.Duration) *Slots {
	return &Slots{dir: dir, n: n, wait: wait, poll: slotPoll}
}

// DefaultSlotDir is where the machine's slots live: the user's cache
// directory, which every launchd agent and cron job of one user shares.
// Per-user rather than system-wide because kiln never runs as root and a
// world-writable lock directory would let any account starve the builds.
func DefaultSlotDir() string {
	base, err := os.UserCacheDir()
	if err != nil || !filepath.IsAbs(base) {
		base = os.TempDir()
	}
	return filepath.Join(base, "kiln", "slots")
}

// TryAcquire takes the first free slot without waiting.
func (s *Slots) TryAcquire(holder string) (ports.BuildSlot, error) {
	if s.n <= 0 {
		return unlimited{}, nil
	}
	for i := range s.n {
		l, err := TryAcquire(s.path(i), holder)
		if err == nil {
			return l, nil
		}
		if !errors.Is(err, ErrBusy) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("%w: %d of %d in use by %s",
		ports.ErrNoBuildSlot, s.n, s.n, s.holders())
}

// Acquire polls for a slot until one frees, the wait runs out, or ctx ends.
func (s *Slots) Acquire(ctx context.Context, holder string) (ports.BuildSlot, error) {
	deadline := time.Now().Add(s.wait)
	ticker := time.NewTicker(s.poll)
	defer ticker.Stop()

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		slot, err := s.TryAcquire(holder)
		if !errors.Is(err, ports.ErrNoBuildSlot) || !time.Now().Before(deadline) {
			return slot, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *Slots) path(i int) string {
	return filepath.Join(s.dir, strconv.Itoa(i))
}

// holders describes who occupies the slots, for the log line that explains
// why a build is waiting.
func (s *Slots) holders() string {
	names := make([]string, 0, s.n)
	for i := range s.n {
		names = append(names, ReadHolder(s.path(i)).String())
	}
	return strings.Join(names, "; ")
}

// unlimited is the slot handed out when there is no limit.
type unlimited struct{}

func (unlimited) Release() error { return nil }
