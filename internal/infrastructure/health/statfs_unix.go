//go:build unix

package health

import "golang.org/x/sys/unix"

// freeBytes is Bavail, not Bfree: the blocks reserved for root are no use to
// a build running as the operator.
func freeBytes(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil //nolint:gosec // block size is never negative
}
