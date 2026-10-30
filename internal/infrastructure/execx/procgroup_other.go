//go:build !unix

package execx

import "os/exec"

// ownGroup keeps exec's default on platforms without process groups: the
// direct child is killed on cancellation.
func ownGroup(cmd *exec.Cmd) {
	cmd.WaitDelay = StopGrace
}

func reapGroup(*exec.Cmd) {}
