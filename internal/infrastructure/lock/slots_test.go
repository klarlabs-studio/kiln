package lock

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.klarlabs.de/kiln/internal/application/ports"
)

func newTestSlots(t *testing.T, n int, wait time.Duration) *Slots {
	t.Helper()
	s := NewSlots(t.TempDir(), n, wait)
	// Polling every few milliseconds keeps the waiting tests fast; the
	// production interval is a box's patience, not a test's.
	s.poll = 5 * time.Millisecond
	return s
}

func TestSlotsGrantUpToTheLimit(t *testing.T) {
	s := newTestSlots(t, 2, 0)

	a, err := s.TryAcquire("kiln watch a")
	if err != nil {
		t.Fatalf("first slot: %v", err)
	}
	b, err := s.TryAcquire("kiln watch b")
	if err != nil {
		t.Fatalf("second slot: %v", err)
	}
	if _, err := s.TryAcquire("kiln watch c"); !errors.Is(err, ports.ErrNoBuildSlot) {
		t.Fatalf("third acquire = %v, want ErrNoBuildSlot", err)
	}

	_ = a.Release()
	c, err := s.TryAcquire("kiln watch c")
	if err != nil {
		t.Fatalf("a released slot was not reusable: %v", err)
	}
	_ = b.Release()
	_ = c.Release()
}

func TestABusyMachineNamesWhoHoldsTheSlots(t *testing.T) {
	s := newTestSlots(t, 1, 0)
	held, err := s.TryAcquire("kiln watch /boxes/api")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Release() }()

	_, err = s.TryAcquire("kiln watch /boxes/web")
	if err == nil || !strings.Contains(err.Error(), "/boxes/api") {
		t.Errorf("err = %v, want it to name the holder", err)
	}
}

func TestZeroSlotsMeansUnlimited(t *testing.T) {
	s := newTestSlots(t, 0, 0)

	for i := range 20 {
		if _, err := s.TryAcquire("kiln run"); err != nil {
			t.Fatalf("acquire %d with no limit: %v", i, err)
		}
	}
}

func TestAcquireWaitsForASlotToFree(t *testing.T) {
	s := newTestSlots(t, 1, 10*time.Second)
	held, err := s.TryAcquire("kiln watch a")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = held.Release()
	}()

	got, err := s.Acquire(t.Context(), "kiln watch b")
	if err != nil {
		t.Fatalf("Acquire did not pick up the freed slot: %v", err)
	}
	_ = got.Release()
}

func TestAcquireGivesUpAfterTheWait(t *testing.T) {
	s := newTestSlots(t, 1, 30*time.Millisecond)
	held, err := s.TryAcquire("kiln watch a")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Release() }()

	start := time.Now()
	_, err = s.Acquire(t.Context(), "kiln watch b")
	if !errors.Is(err, ports.ErrNoBuildSlot) {
		t.Fatalf("err = %v, want ErrNoBuildSlot", err)
	}
	// Bounded: a tick must never queue behind the machine indefinitely.
	if time.Since(start) > 5*time.Second {
		t.Errorf("Acquire waited %s", time.Since(start))
	}
}

func TestAcquireStopsWhenCancelled(t *testing.T) {
	s := newTestSlots(t, 1, time.Hour)
	held, err := s.TryAcquire("kiln watch a")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Release() }()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.Acquire(ctx, "kiln watch b"); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// The reason for flock: a box agent that is killed mid-build must give its
// slot back without anybody deleting a file.
func TestASlotHeldByADeadProcessIsFreed(t *testing.T) {
	s := newTestSlots(t, 1, 0)

	held := make(chan struct{})
	cmd := holderSubprocess(t, filepath.Join(s.dir, "0"))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait(); close(held) }()
	waitUntilHeld(t, filepath.Join(s.dir, "0"))

	if _, err := s.TryAcquire("probe"); !errors.Is(err, ports.ErrNoBuildSlot) {
		t.Fatalf("another process's slot was granted: %v", err)
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-held

	got, err := s.TryAcquire("after the crash")
	if err != nil {
		t.Fatalf("the slot survived its holder: %v", err)
	}
	_ = got.Release()
}

func TestDefaultSlotDirIsMachineWide(t *testing.T) {
	dir := DefaultSlotDir()
	if !filepath.IsAbs(dir) {
		t.Errorf("DefaultSlotDir = %q, want an absolute path", dir)
	}
	if !strings.Contains(dir, "kiln") {
		t.Errorf("DefaultSlotDir = %q, want it under a kiln directory", dir)
	}
}
