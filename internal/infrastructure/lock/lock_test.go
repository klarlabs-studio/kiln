package lock

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func lockPath(t *testing.T) string {
	t.Helper()
	return PathFor(t.TempDir())
}

func TestAcquireAndRelease(t *testing.T) {
	path := lockPath(t)

	l, err := TryAcquire(path, "kiln run")
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	if l.Path() != path {
		t.Errorf("Path = %q", l.Path())
	}
	if err := l.Release(); err != nil {
		t.Errorf("Release: %v", err)
	}

	// The next process must be able to take it.
	again, err := TryAcquire(path, "kiln watch")
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	_ = again.Release()
}

func TestReleaseIsIdempotent(t *testing.T) {
	l, err := TryAcquire(lockPath(t), "kiln run")
	if err != nil {
		t.Fatal(err)
	}

	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	// A deferred Release beside an explicit one is a realistic shape.
	if err := l.Release(); err != nil {
		t.Errorf("second Release: %v", err)
	}
	if err := (*Lock)(nil).Release(); err != nil {
		t.Errorf("nil Release: %v", err)
	}
}

func TestTheLockDirectoryIsCreated(t *testing.T) {
	dir := t.TempDir()
	path := PathFor(dir)

	l, err := TryAcquire(path, "kiln run")
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	defer func() { _ = l.Release() }()

	if _, err := os.Stat(filepath.Join(dir, ".kiln")); err != nil {
		t.Errorf("lock directory not created: %v", err)
	}
}

func TestHolderIsRecorded(t *testing.T) {
	path := lockPath(t)
	l, err := TryAcquire(path, "kiln watch --once")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()

	h := ReadHolder(path)
	if h.PID != os.Getpid() {
		t.Errorf("PID = %d, want %d", h.PID, os.Getpid())
	}
	if h.Command != "kiln watch --once" {
		t.Errorf("Command = %q", h.Command)
	}
	if time.Since(h.Since) > time.Minute {
		t.Errorf("Since = %v, want roughly now", h.Since)
	}
}

func TestHolderRendersForAMessage(t *testing.T) {
	h := Holder{PID: 4242, Command: "kiln run", Since: time.Now().Add(-90 * time.Second)}

	got := h.String()
	for _, want := range []string{"4242", "kiln run", "running"} {
		if !strings.Contains(got, want) {
			t.Errorf("Holder.String() = %q, missing %q", got, want)
		}
	}
	// An unreadable record must still produce a sentence.
	var unknown Holder
	if unknown.String() == "" {
		t.Error("the zero Holder must still render")
	}
}

func TestReadHolderOfAMissingFile(t *testing.T) {
	if got := ReadHolder(filepath.Join(t.TempDir(), "nope")); got.PID != 0 {
		t.Errorf("ReadHolder = %+v, want zero", got)
	}
}

func TestReleaseClearsTheHolder(t *testing.T) {
	path := lockPath(t)
	l, err := TryAcquire(path, "kiln run")
	if err != nil {
		t.Fatal(err)
	}
	_ = l.Release()

	// A reader arriving after the lock is free must not be told about a stale
	// owner.
	if got := ReadHolder(path); got.PID != 0 {
		t.Errorf("stale holder after release: %+v", got)
	}
}

// A second *process* is the case that matters: flock is per open file
// description, so two Acquires in one process would not necessarily conflict,
// and testing that would prove nothing about the cron overlap this exists for.
func TestASecondProcessIsRefused(t *testing.T) {
	path := lockPath(t)

	l, err := TryAcquire(path, "kiln watch")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()

	if out, err := lockFromSubprocess(t, path); err == nil {
		t.Errorf("a second process took a held lock: %s", out)
	} else if !strings.Contains(out, "busy") {
		t.Errorf("subprocess said %q, want the busy signal", out)
	}
}

func TestTheLockIsFreedWhenAProcessDies(t *testing.T) {
	path := lockPath(t)

	// Hold it in a subprocess and kill that process without letting it clean
	// up. The kernel must drop the lock; this is the whole reason for flock
	// over a PID file.
	held := make(chan struct{})
	cmd := holderSubprocess(t, path)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait(); close(held) }()

	waitUntilHeld(t, path)
	if _, err := TryAcquire(path, "probe"); !errors.Is(err, ErrBusy) {
		t.Fatalf("expected the subprocess to hold it, got %v", err)
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-held

	l, err := TryAcquire(path, "after the crash")
	if err != nil {
		t.Fatalf("the lock survived the holder: %v", err)
	}
	_ = l.Release()
}

// lockFromSubprocess runs this test binary in a mode that tries the lock once
// and reports the outcome.
func lockFromSubprocess(t *testing.T, path string) (string, error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestSubprocessLockHelper")
	cmd.Env = append(os.Environ(), "KILN_LOCK_HELPER=try", "KILN_LOCK_PATH="+path)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// holderSubprocess builds a process that takes the lock and then blocks.
func holderSubprocess(t *testing.T, path string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestSubprocessLockHelper")
	cmd.Env = append(os.Environ(), "KILN_LOCK_HELPER=hold", "KILN_LOCK_PATH="+path)
	return cmd
}

func waitUntilHeld(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ReadHolder(path).PID != 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the subprocess never took the lock")
}

// TestSubprocessLockHelper is not a test; it is the body of the helper
// processes above. It returns immediately under a normal run.
func TestSubprocessLockHelper(t *testing.T) {
	mode := os.Getenv("KILN_LOCK_HELPER")
	if mode == "" {
		t.Skip("helper process entry point")
	}
	path := os.Getenv("KILN_LOCK_PATH")

	l, err := TryAcquire(path, "helper")
	if errors.Is(err, ErrBusy) {
		t.Fatal("busy")
	}
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if mode == "try" {
		_ = l.Release()
		return
	}
	// Sleep rather than select{}: an empty select parks every goroutine, and
	// Go's deadlock detector kills the process — which releases the lock and
	// makes the test measure nothing. A timer keeps the runtime happy.
	time.Sleep(10 * time.Minute)
}
