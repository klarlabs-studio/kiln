package worktree

import (
	"context"
	"time"

	"go.klarlabs.de/kiln/internal/infrastructure/execx"
)

// Trees adapts this package's functions to ports.Worktrees.
//
// The functions take an execx.Runner as their first argument, which suited a
// package-level API and does not suit a port: the application would have to
// hold a runner it has no other use for, purely to hand it back. Trees closes
// over it once instead.
type Trees struct {
	Runner execx.Runner
}

// NewTrees returns a Worktrees backed by git.
func NewTrees(r execx.Runner) *Trees { return &Trees{Runner: r} }

func (t *Trees) With(ctx context.Context, repoDir, sha string, fn func(dir string) error) error {
	return With(ctx, t.Runner, repoDir, sha, fn)
}

func (t *Trees) Reap(ctx context.Context, repoDir string, olderThan time.Duration) (int, error) {
	return Reap(ctx, t.Runner, repoDir, olderThan)
}
