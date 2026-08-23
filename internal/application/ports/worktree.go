package ports

import (
	"context"
	"time"
)

// Worktrees hands out a disposable checkout of a commit.
//
// Everything kiln runs against a commit — the gate, a build, a task — runs in
// one of these rather than in the operator's live checkout, so a job cannot
// leave the repository on a different commit than it found it. The port exists
// so the application can say "give me this commit, somewhere safe" without
// naming git.
type Worktrees interface {
	// With creates a worktree for sha, calls fn with its path, and removes it
	// afterwards whether or not fn succeeded.
	With(ctx context.Context, repoDir, sha string, fn func(dir string) error) error
	// Reap removes worktrees left behind by a process that died before it
	// could clean up, and reports how many it took. A box runs for months; a
	// worktree leaked on every crash is a disk that fills up quietly.
	Reap(ctx context.Context, repoDir string, olderThan time.Duration) (int, error)
}
