package task

import "go.klarlabs.de/kiln/internal/application/ports"

// Keep, KeepDir and Sweep are package functions because they are pure file
// handling with no runner behind them. The port is a single interface, so the
// Runner carries them across rather than the application importing this
// package for three helpers.

func (t *Runner) Keep(worktreeDir, dest string, globs []string) ([]ports.KeptFile, error) {
	return Keep(worktreeDir, dest, globs)
}

func (t *Runner) KeepDir(root, runID, taskName string) string {
	return KeepDir(root, runID, taskName)
}

func (t *Runner) Sweep(root string, keep int) error {
	return Sweep(root, keep)
}
