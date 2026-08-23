//go:build !unix

package lock

import (
	"errors"
	"os"
)

var errWouldBlock = errors.New("would block")

// tryFlock is unimplemented off unix.
//
// Kiln targets linux and darwin — it shells out to git, docker and cosign on a
// build box. Rather than pretend to lock, this refuses, so nobody runs
// overlapping builds believing they are serialised.
func tryFlock(*os.File) error {
	return errors.New("repository locking is not implemented on this platform")
}

func unflock(*os.File) error { return nil }
