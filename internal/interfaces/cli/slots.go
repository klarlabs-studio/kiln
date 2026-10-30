package cli

import (
	"context"
	"errors"

	"go.klarlabs.de/kiln/internal/application/ports"
	"go.klarlabs.de/kiln/internal/boot"
	"go.klarlabs.de/kiln/internal/infrastructure/lock"
)

// slotWait bounds how long a build waits for a machine slot. A variable so a
// test can make a full machine give up in milliseconds rather than minutes.
var slotWait = lock.DefaultSlotWait

// buildSlots is the machine's build capacity as the environment configures it.
// One place, so `kiln run` and every watch path count against the same slots.
func buildSlots(deps *boot.Deps) *lock.Slots {
	dir := deps.Env.SlotDir
	if dir == "" {
		dir = lock.DefaultSlotDir()
	}
	return lock.NewSlots(dir, deps.Env.MaxParallel, slotWait)
}

// withBuildSlot runs fn holding one of the machine's build slots.
//
// For `kiln run`, which an operator asked for by name: it waits its bounded
// turn rather than skipping, and a machine still full at the end is ExitBusy —
// the same "not now" as a held repository, and deliberately not a failure,
// because nothing about the commit has been judged.
func withBuildSlot(ctx context.Context, deps *boot.Deps, holder string, fn func() error) error {
	slots := buildSlots(deps)
	slot, err := slots.TryAcquire(holder)
	if errors.Is(err, ports.ErrNoBuildSlot) {
		deps.Log.Info("waiting for a build slot", "busy", err, "wait", slotWait)
		slot, err = slots.Acquire(ctx, holder)
	}
	switch {
	case errors.Is(err, ports.ErrNoBuildSlot):
		return wrapExit(ExitBusy, err)
	case err != nil:
		return wrapExit(ExitConfig, err)
	}
	defer func() { _ = slot.Release() }()

	return fn()
}
