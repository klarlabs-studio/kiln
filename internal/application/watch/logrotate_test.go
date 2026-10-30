package watch

import (
	"context"
	"errors"
	"testing"

	"go.klarlabs.de/kiln/internal/infrastructure/execx"
	"go.klarlabs.de/kiln/internal/infrastructure/gitcli"
)

// fakeLogs records rotations and fails on request.
type fakeLogs struct {
	calls int
	moved int64
	err   error
}

func (f *fakeLogs) Rotate(context.Context) (int64, error) {
	f.calls++
	return f.moved, f.err
}

func TestATickRotatesTheBoxLog(t *testing.T) {
	f := newFixture(t)
	logs := &fakeLogs{moved: 6 << 20}
	f.watcher.Logs = logs

	if _, err := f.watcher.Once(t.Context(), false); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if logs.calls != 1 {
		t.Fatalf("rotated %d times, want once per tick", logs.calls)
	}
}

// First, before the fetch: a box whose remote is down for a week writes a
// failed tick every half hour, and that is exactly the log that grows.
func TestTheBoxLogIsRotatedEvenWhenTheFetchFails(t *testing.T) {
	f := newFixture(t)
	logs := &fakeLogs{}
	f.watcher.Logs = logs
	f.watcher.Git = gitcli.New(execx.NewFake().On("git fetch", execx.Response{ExitCode: 128, Stderr: "no such remote"}))

	_, _ = f.watcher.Once(t.Context(), false)
	if logs.calls != 1 {
		t.Fatalf("rotated %d times, want the rotation to run before the fetch", logs.calls)
	}
}

// Housekeeping that could fail the tick would be the housekeeping causing the
// outage — a full disk or a read-only log must not stop main from building.
func TestAFailedRotationDoesNotFailTheTick(t *testing.T) {
	f := newFixture(t)
	f.watcher.Logs = &fakeLogs{err: errors.New("read-only file system")}

	res, err := f.watcher.Once(t.Context(), false)
	if err != nil {
		t.Fatalf("Once: %v", err)
	}
	if len(res.Executed) != 1 || res.Executed[0].Err != nil {
		t.Fatalf("executed = %+v, want the branch built regardless", res.Executed)
	}
}

func TestADryRunLeavesTheBoxLogAlone(t *testing.T) {
	f := newFixture(t)
	logs := &fakeLogs{}
	f.watcher.Logs = logs

	if _, err := f.watcher.Once(t.Context(), true); err != nil {
		t.Fatalf("Once: %v", err)
	}
	if logs.calls != 0 {
		t.Fatalf("a dry run rotated the log %d times", logs.calls)
	}
}
