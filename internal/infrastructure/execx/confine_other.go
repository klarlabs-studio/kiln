//go:build !linux

package execx

import "fmt"

// LandlockAvailable is false off Linux. A worktree is not a sandbox here
// either; there is simply no kernel restriction to apply.
func LandlockAvailable() bool { return false }

// LandlockABI is 0 off Linux.
func LandlockABI() int { return 0 }

func applyLandlock(string) error {
	return fmt.Errorf("landlock is a Linux LSM")
}
