// Package lock serialises Kiln processes over one repository.
//
// The unattended story is a cron entry, which means overlap is not an edge
// case: a five-minute build under a one-minute schedule produces five kilns
// working the same checkout. Without a lock they all fetch, all decide the
// head is unbuilt — none of them has finished writing a success yet — and all
// build it. They also race the run ledger, whose read-modify-write is atomic
// per process and not across them, so the losers' records vanish.
//
// One exclusive lock per repository fixes both. It is an advisory flock held
// on an open descriptor, which matters more than it sounds: the kernel drops
// it when the process dies, so a hard-killed build leaves nothing to clean up
// by hand. A PID file would need exactly that.
package lock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ErrBusy reports that another process holds the repository. It is not
// necessarily a failure — see the callers: a watch tick treats it as "nothing
// to do", a one-shot run treats it as a refusal.
var ErrBusy = errors.New("another kiln process holds this repository")

// Lock is a held repository lock.
type Lock struct {
	path string
	file *os.File
}

// Holder describes whoever is holding a lock, read from its contents. It is
// advisory information for a message, not something to make decisions on: the
// file is written after the lock is taken, so a reader can catch it empty.
type Holder struct {
	PID     int
	Since   time.Time
	Command string
}

// String renders the holder for an error message.
func (h Holder) String() string {
	if h.PID == 0 {
		return "another process"
	}
	s := fmt.Sprintf("pid %d", h.PID)
	if h.Command != "" {
		s += " (" + h.Command + ")"
	}
	if !h.Since.IsZero() {
		s += fmt.Sprintf(", running %s", time.Since(h.Since).Round(time.Second))
	}
	return s
}

// TryAcquire takes the lock without waiting, returning ErrBusy if it is held.
//
// Non-blocking on purpose. A cron tick that blocked would pile up processes
// behind a slow build until the box ran out of them, which is a worse failure
// than the duplicate work it was avoiding.
func TryAcquire(path, command string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("lock: create directory: %w", err)
	}

	// O_CREATE, never O_TRUNC: truncating would blank the holder record of a
	// lock somebody else is holding right now.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // operator-controlled repository path
	if err != nil {
		return nil, fmt.Errorf("lock: open %s: %w", path, err)
	}

	if err := tryFlock(f); err != nil {
		_ = f.Close()
		if errors.Is(err, errWouldBlock) {
			return nil, fmt.Errorf("%w: %s", ErrBusy, ReadHolder(path))
		}
		return nil, fmt.Errorf("lock: %s: %w", path, err)
	}

	l := &Lock{path: path, file: f}
	l.writeHolder(command)
	return l, nil
}

// writeHolder records who we are, best-effort. Failing to write it is not
// worth failing the lock over: the lock is held either way, and the contents
// only improve somebody else's error message.
func (l *Lock) writeHolder(command string) {
	body := fmt.Sprintf("pid=%d\nsince=%s\ncommand=%s\n",
		os.Getpid(), time.Now().UTC().Format(time.RFC3339), command)

	if err := l.file.Truncate(0); err != nil {
		return
	}
	if _, err := l.file.WriteAt([]byte(body), 0); err != nil {
		return
	}
	_ = l.file.Sync()
}

// Release drops the lock. Safe to call twice, so a deferred Release beside an
// explicit one is not a bug.
func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	f := l.file
	l.file = nil

	// Blank the holder record before unlocking, so a reader that arrives after
	// the lock is free does not report a stale owner. Best-effort: the
	// unlocking is what matters.
	_ = f.Truncate(0)
	unlockErr := unflock(f)
	closeErr := f.Close()

	if unlockErr != nil {
		return fmt.Errorf("lock: release %s: %w", l.path, unlockErr)
	}
	if closeErr != nil {
		return fmt.Errorf("lock: close %s: %w", l.path, closeErr)
	}
	return nil
}

// Path is where the lock lives, for diagnostics.
func (l *Lock) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// ReadHolder reads the holder record. A missing or unparsable file yields a
// zero Holder, which renders as "another process" — the lock state is what is
// authoritative, not this.
func ReadHolder(path string) Holder {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return Holder{}
	}

	var h Holder
	for line := range strings.SplitSeq(string(data), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "pid":
			h.PID, _ = strconv.Atoi(value)
		case "since":
			if t, err := time.Parse(time.RFC3339, value); err == nil {
				h.Since = t
			}
		case "command":
			h.Command = value
		}
	}
	return h
}

// PathFor returns the lock path for a repository. It sits beside the ledger,
// in the directory kiln already owns, so a repository gains no new top-level
// clutter.
func PathFor(repoDir string) string {
	return filepath.Join(repoDir, ".kiln", "lock")
}
