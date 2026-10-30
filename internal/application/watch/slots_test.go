package watch

import (
	"context"
	"errors"
	"testing"

	"go.klarlabs.de/kiln/internal/application/ports"
)

// fakeSlots is a machine with a fixed number of free slots. It counts what was
// asked of it so a test can say how the watcher behaved, not just what came
// out.
type fakeSlots struct {
	free     int
	held     int
	maxHeld  int
	tries    int
	waits    int
	failWith error
}

type fakeSlot struct{ s *fakeSlots }

func (f fakeSlot) Release() error {
	f.s.held--
	f.s.free++
	return nil
}

func (s *fakeSlots) TryAcquire(string) (ports.BuildSlot, error) {
	s.tries++
	return s.take()
}

func (s *fakeSlots) Acquire(ctx context.Context, _ string) (ports.BuildSlot, error) {
	s.waits++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.take()
}

func (s *fakeSlots) take() (ports.BuildSlot, error) {
	if s.failWith != nil {
		return nil, s.failWith
	}
	if s.free == 0 {
		return nil, ports.ErrNoBuildSlot
	}
	s.free--
	s.held++
	s.maxHeld = max(s.maxHeld, s.held)
	return fakeSlot{s}, nil
}

func TestEachBuildHoldsAMachineSlotWhileItRuns(t *testing.T) {
	f := newFixture(t)
	f.alreadyWatching(t)
	f.upstream.Tag("v1.0.0")
	slots := &fakeSlots{free: 1}
	f.watcher.Slots = slots

	heldDuringBuild := []int{}
	eng := *f.watcher.Engine
	eng.Prover = ports.ProveFunc(func(context.Context, ports.ProveRequest) error {
		heldDuringBuild = append(heldDuringBuild, slots.held)
		return nil
	})
	f.watcher.Engine = &eng

	res, err := f.watcher.Once(t.Context(), false)
	if err != nil {
		t.Fatalf("Once: %v", err)
	}

	// One slot, two jobs: both build, one after the other, because each gives
	// its slot back before the next asks.
	if len(res.Executed) != 2 {
		t.Fatalf("executed %d jobs, want 2", len(res.Executed))
	}
	for i, n := range heldDuringBuild {
		if n != 1 {
			t.Errorf("build %d ran holding %d slots, want 1", i, n)
		}
	}
	if slots.held != 0 {
		t.Errorf("%d slot(s) still held after the tick", slots.held)
	}
}

func TestAJobWithNoFreeSlotIsDeferredAndNotRecorded(t *testing.T) {
	f := newFixture(t)
	f.watcher.Slots = &fakeSlots{free: 0}

	res, err := f.watcher.Once(t.Context(), false)
	if err != nil {
		t.Fatalf("a full machine must not fail the tick: %v", err)
	}
	if len(res.Executed) != 0 || len(res.Deferred) != 1 {
		t.Fatalf("executed %d, deferred %d; want 0 and 1", len(res.Executed), len(res.Deferred))
	}
	if res.Failures() != 0 {
		t.Errorf("a deferral counted as %d failure(s)", res.Failures())
	}

	// Nothing in the ledger: a full machine says nothing about the commit, so
	// it must neither be blamed nor backed off.
	runs, err := f.store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Errorf("ledger has %d run(s) for a deferred job", len(runs))
	}

	// The next tick on a free machine builds it.
	f.watcher.Slots = &fakeSlots{free: 1}
	res, err = f.watcher.Once(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Executed) != 1 {
		t.Errorf("deferred job was not built on the next tick: %+v", res)
	}
}

func TestAfterOneDeferralTheRestOfTheTickDoesNotWaitAgain(t *testing.T) {
	f := newFixture(t)
	f.alreadyWatching(t)
	// Separate major lines, so neither tag supersedes the other.
	f.upstream.Tag("v1.0.0")
	f.upstream.Tag("v2.0.0")
	slots := &fakeSlots{free: 0}
	f.watcher.Slots = slots

	res, err := f.watcher.Once(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Deferred) != 3 {
		t.Errorf("deferred %d jobs, want all 3", len(res.Deferred))
	}
	// Waiting the full bound once per job would hold the repository lock for
	// an hour behind a machine that was already known to be full.
	if slots.waits != 1 {
		t.Errorf("waited %d times, want once", slots.waits)
	}
}

func TestASlotThatCannotBeTakenFailsTheTick(t *testing.T) {
	f := newFixture(t)
	f.watcher.Slots = &fakeSlots{failWith: errors.New("permission denied")}

	if _, err := f.watcher.Once(t.Context(), false); err == nil {
		t.Error("a broken slot directory must fail loudly, not build unbounded")
	}
	runs, _ := f.store.List()
	if len(runs) != 0 {
		t.Errorf("ledger has %d run(s); a machine problem was blamed on a commit", len(runs))
	}
}

func TestCancellationWhileWaitingForASlotStopsTheTick(t *testing.T) {
	f := newFixture(t)

	// Cancelled from inside the wait rather than up front: a context dead
	// before the tick would fail the fetch and never reach the slot.
	ctx, cancel := context.WithCancel(t.Context())
	f.watcher.Slots = &cancellingSlots{cancel: cancel}

	_, err := f.watcher.Once(ctx, false)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// cancellingSlots is a full machine on which the operator presses Ctrl-C
// while the tick waits.
type cancellingSlots struct{ cancel context.CancelFunc }

func (*cancellingSlots) TryAcquire(string) (ports.BuildSlot, error) {
	return nil, ports.ErrNoBuildSlot
}

func (c *cancellingSlots) Acquire(ctx context.Context, _ string) (ports.BuildSlot, error) {
	c.cancel()
	return nil, ctx.Err()
}

func TestNoSlotsMeansNoLimit(t *testing.T) {
	f := newFixture(t)
	f.watcher.Slots = nil

	res, err := f.watcher.Once(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Executed) != 1 {
		t.Errorf("executed %d jobs, want 1", len(res.Executed))
	}
}
