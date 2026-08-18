//go:build unix

package lock

import (
	"errors"
	"os"
	"syscall"
)

// errWouldBlock signals a lock held by someone else, distinct from a real
// error taking it.
var errWouldBlock = errors.New("would block")

// tryFlock takes an exclusive advisory lock without waiting.
//
// flock rather than a PID file because the kernel releases it when the process
// dies, however it dies. A crashed build must not leave a repository locked
// until somebody notices and deletes a file.
func tryFlock(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, syscall.EWOULDBLOCK):
		return errWouldBlock
	default:
		return err
	}
}

func unflock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
