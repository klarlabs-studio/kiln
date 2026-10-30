package watch

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"go.klarlabs.de/kiln/internal/application/ports"
	"go.klarlabs.de/kiln/internal/infrastructure/obs"
)

type fakeTrimmer struct {
	calls int
	res   ports.CacheTrimResult
	err   error
}

func (f *fakeTrimmer) Trim(context.Context) (ports.CacheTrimResult, error) {
	f.calls++
	return f.res, f.err
}

// Box builds grew the operator's Go cache by ~35 GB in twelve hours until an
// external script trimmed it. The tick now trims kiln's own cache beside the
// worktree reaper and the docker prune, and says what it freed.
func TestOnceTrimsTheGoBuildCacheAndReportsWhatWasFreed(t *testing.T) {
	f := newFixture(t)
	trim := &fakeTrimmer{res: ports.CacheTrimResult{Files: 3, Freed: 3 << 30}}
	var logs bytes.Buffer
	f.watcher.GoCache = trim
	f.watcher.Log = obs.NewTo(&logs, "info")

	if _, err := f.watcher.Once(t.Context(), false); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if trim.calls != 1 {
		t.Errorf("Trim called %d times, want 1", trim.calls)
	}
	if out := logs.String(); !strings.Contains(out, "trimmed go build cache") || !strings.Contains(out, "3.0 GiB") {
		t.Errorf("tick log should report the trim, got:\n%s", out)
	}
}

func TestAFailedGoCacheTrimDoesNotFailTheTick(t *testing.T) {
	f := newFixture(t)
	f.watcher.GoCache = &fakeTrimmer{err: errors.New("permission denied")}

	res, err := f.watcher.Once(t.Context(), false)
	if err != nil {
		t.Fatalf("Once: %v — housekeeping must never be fatal", err)
	}
	if len(res.Executed) != 1 {
		t.Errorf("executed %d jobs, want 1", len(res.Executed))
	}
}

func TestADryRunDoesNotTrimTheGoBuildCache(t *testing.T) {
	f := newFixture(t)
	trim := &fakeTrimmer{}
	f.watcher.GoCache = trim

	if _, err := f.watcher.Once(t.Context(), true); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if trim.calls != 0 {
		t.Error("a dry run must not delete anything")
	}
}

func TestHumanBytesReadsLikeDu(t *testing.T) {
	for in, want := range map[int64]string{
		0: "0 B", 512: "512 B", 1536: "1.5 KiB", 5 << 20: "5.0 MiB", 3 << 30: "3.0 GiB",
	} {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}
