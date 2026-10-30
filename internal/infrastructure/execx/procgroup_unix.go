//go:build unix

package execx

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// ownGroup puts the command in a process group of its own and makes
// cancellation ask that whole group to stop, rather than killing the one
// process exec started.
//
// The default — SIGKILL to the direct child — is what leaked whole docker
// compose stacks on a real box: a task script that brings a stack up and
// tears it down in an EXIT trap never ran the trap, and the compose it had
// started was not its child's child anymore but an orphan. SIGTERM to the
// group reaches every process the task started, and a shell runs its EXIT
// trap on the way out.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return signalGroup(cmd, syscall.SIGTERM) }
	cmd.WaitDelay = StopGrace
}

// reapGroup kills whatever the command left running in its group: a cleanup
// that outlived StopGrace, or a background process a task forgot. A task
// leaves nothing behind, whether it finished or was stopped.
func reapGroup(cmd *exec.Cmd) {
	_ = signalGroup(cmd, syscall.SIGKILL)
}

func signalGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-cmd.Process.Pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}
