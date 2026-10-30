// Package logrotate keeps a box's log from growing without bound.
//
// A box is a launchd agent or a systemd timer whose stdout and stderr are
// appended to <repo>/.kiln/box.log. Neither init system rotates that file and
// nothing else did either: a box ticking every half hour for months reached
// 14 MB of log that nobody would ever read past the last screenful. kiln is
// the only process that ever writes it, so kiln trims it at the start of each
// tick, the same way it already reaps worktrees and prunes docker.
package logrotate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
)

const (
	// DefaultMaxBytes is the size a live log may reach before it is rotated.
	// Several weeks of ordinary ticks, and small enough to open in an editor.
	DefaultMaxBytes int64 = 5 << 20
	// DefaultKeep is how many rotated generations survive: box.log.1 to
	// box.log.3, so at most about 20 MiB per box in total.
	DefaultKeep = 3
)

// Rotator rotates one log file by size.
type Rotator struct {
	// Path is the live log.
	Path string
	// MaxBytes is the size at which the live log is rotated.
	MaxBytes int64
	// Keep is how many rotated generations to retain. Zero only truncates.
	Keep int
}

// New is a rotator for path with the box defaults.
func New(path string) *Rotator {
	return &Rotator{Path: path, MaxBytes: DefaultMaxBytes, Keep: DefaultKeep}
}

// Rotate moves an oversized log aside and reports how many bytes it moved, or
// zero when the log is missing or under the limit.
//
// Copy-then-truncate, never rename. The process doing the rotating is the one
// writing the log: launchd (and systemd's StandardOutput=append:) opens the
// file once per launch with O_APPEND and hands that descriptor to kiln as
// stdout and stderr — `lsof +fg` on a live tick shows the AP flag. Renaming
// box.log to box.log.1 would leave the descriptor on the renamed inode, so
// the whole of the current tick, and every tick of a long-lived
// `kiln watch --every`, would land in box.log.1 while box.log stayed absent.
// Truncating in place keeps the descriptor on the live file, and because it
// is in append mode the next write goes to the new end, offset zero, rather
// than at the old offset behind a hole of zeros.
//
// The cost is a window between the copy and the truncate in which a write
// would be lost. Rotation runs first in the tick, before this process has
// anything to say, and a box never runs two ticks at once, so the window has
// nobody in it.
func (r *Rotator) Rotate(_ context.Context) (int64, error) {
	info, err := os.Stat(r.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if info.Size() < r.MaxBytes {
		return 0, nil
	}

	if r.Keep > 0 {
		if err := r.shift(); err != nil {
			return 0, err
		}
		if err := copyFile(r.Path, r.generation(1)); err != nil {
			return 0, err
		}
	}
	if err := os.Truncate(r.Path, 0); err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// shift renames box.log.N-1 to box.log.N down to .1, overwriting the oldest.
func (r *Rotator) shift() error {
	for i := r.Keep - 1; i >= 1; i-- {
		err := os.Rename(r.generation(i), r.generation(i+1))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (r *Rotator) generation(n int) string { return fmt.Sprintf("%s.%d", r.Path, n) }

func copyFile(from, to string) (err error) {
	src, err := os.Open(from) // #nosec G304 -- the box log kiln itself names.
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()

	dst, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) // #nosec G304 -- as above.
	if err != nil {
		return err
	}
	defer func() {
		if cerr := dst.Close(); err == nil {
			err = cerr
		}
	}()
	_, err = io.Copy(dst, src)
	return err
}
