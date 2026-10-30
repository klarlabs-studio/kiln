//go:build unix

package execx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The shape of the task that leaked compose stacks: something long-running
// started in the background, torn down by an EXIT trap.
func TestCancellingACommandRunsItsCleanupAndLeavesNothingRunning(t *testing.T) {
	dir := t.TempDir()
	script := `
sleep 300 &
echo $! > child.pid
trap 'echo cleaned > cleanup' EXIT
echo started > started
wait
`
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := NewSystem().Run(ctx, Cmd{Name: "bash", Args: []string{"-c", script}, Dir: dir})
		done <- err
	}()

	waitForFile(t, filepath.Join(dir, "started"))
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
		if _, ok := ExitCode(err); ok {
			t.Errorf("cancellation reported as an exit code: %v", err)
		}
	case <-time.After(StopGrace / 2):
		t.Fatal("cancelled command did not stop")
	}

	if _, err := os.Stat(filepath.Join(dir, "cleanup")); err != nil {
		t.Error("the EXIT trap did not run: a SIGKILL skips the task's own cleanup")
	}
	if alive(t, filepath.Join(dir, "child.pid")) {
		t.Error("the background child outlived the cancelled command")
	}
}

func TestAFinishedCommandLeavesNoBackgroundProcess(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewSystem().Run(t.Context(), Cmd{
		Name: "sh", Args: []string{"-c", "sleep 300 >/dev/null 2>&1 & echo $! > child.pid"}, Dir: dir,
	}); err != nil {
		t.Fatal(err)
	}
	if alive(t, filepath.Join(dir, "child.pid")) {
		t.Error("a background process outlived the command that started it")
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
}

func alive(t *testing.T, pidFile string) bool {
	t.Helper()
	raw, err := os.ReadFile(pidFile) //nolint:gosec // a path inside the test's own temp dir
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	// The kill is asynchronous; give the signal a moment to land.
	for range 50 {
		if syscall.Kill(pid, 0) != nil {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
	return true
}
