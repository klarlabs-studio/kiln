//go:build !unix

package health

import "errors"

// freeBytes is unimplemented off unix, where kiln does not run. An error
// rather than a large number, so a check that cannot look does not pass.
func freeBytes(string) (uint64, error) {
	return 0, errors.New("free disk space is not implemented on this platform")
}
