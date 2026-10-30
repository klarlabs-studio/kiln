package cli

import (
	"os"
	"strings"
	"testing"
	"time"

	"go.klarlabs.de/kiln/internal/gittest"
	"go.klarlabs.de/kiln/internal/infrastructure/lock"
)

// TestMain points every command at a private slot directory. Without it the
// suite would share the machine's real build slots and, on a box that is
// building, queue behind it for the full wait before failing.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "kiln-cli-slots-")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("KILN_SLOT_DIR", dir)
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// fullMachine takes the machine's only build slot for the rest of the test and
// shortens the wait, so a command that respects the limit gives up quickly.
func fullMachine(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("KILN_SLOT_DIR", dir)
	t.Setenv("KILN_MAX_PARALLEL", "1")

	held, err := lock.NewSlots(dir, 1, 0).TryAcquire("kiln watch /boxes/other")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Release() })

	saved := slotWait
	slotWait = 50 * time.Millisecond
	t.Cleanup(func() { slotWait = saved })
}

func TestAFullMachineRefusesARunAfterTheWait(t *testing.T) {
	repoWith(t, "")
	fullMachine(t)

	out, errOut, code := capture(t, "run", "--sha", "HEAD", "--event", "push", "--quiet")

	// Explicit, so not a silent no-op — but busy, not failed: nothing is
	// wrong with the commit.
	if code != ExitBusy {
		t.Errorf("code = %d, want %d\n%s%s", code, ExitBusy, out, errOut)
	}
	if !strings.Contains(errOut, "build slot") || !strings.Contains(errOut, "/boxes/other") {
		t.Errorf("stderr should say the machine is full and who holds it: %q", errOut)
	}
}

func TestAFullMachineDefersAWatchTickWithoutFailingIt(t *testing.T) {
	upstream := gittest.New(t)
	upstream.Commit("first", "app.txt", "one\n")
	local := upstream.Clone(t)
	t.Chdir(local.Dir)
	for _, k := range []string{"GITHUB_TOKEN", "GH_TOKEN", "KILN_DB", "KILN_DIR"} {
		t.Setenv(k, "")
	}
	t.Setenv("KILN_LOG_LEVEL", "fatal")
	t.Setenv("KILN_WARDEN", fakeBin(t, "warden-pass", "exit 0"))
	fullMachine(t)

	out, _, code := capture(t, "watch", "--once")

	// Eight boxes on one schedule always find the machine full; paging on
	// that would page every half hour.
	if code != ExitOK {
		t.Errorf("code = %d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "defer") {
		t.Errorf("output should say the job was deferred:\n%s", out)
	}
	if strings.Contains(out, "built") {
		t.Errorf("a job built without a slot:\n%s", out)
	}
}

func TestZeroMaxParallelIgnoresAFullMachine(t *testing.T) {
	repoWith(t, "")
	fullMachine(t)
	t.Setenv("KILN_MAX_PARALLEL", "0")

	out, errOut, code := capture(t, "run", "--sha", "HEAD", "--event", "push", "--quiet")

	if code != ExitOK {
		t.Errorf("code = %d, want 0 with no limit\n%s%s", code, out, errOut)
	}
}
