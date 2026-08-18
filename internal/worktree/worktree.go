// Package worktree checks a commit out into a disposable directory.
//
// Both phases that touch repository content — prove and publish — run inside
// one of these, never in the operator's working copy. That is not tidiness. A
// box running `kiln watch --every 1m` has a checkout an operator is also using;
// without an isolated tree, an uncommitted edit sitting in that checkout would
// end up inside a signed image, and the digest Kiln hands RollOps would attest
// to a commit that never contained the code it shipped.
//
// A detached worktree also pins the content: `git worktree add --detach <sha>`
// cannot drift while the build runs, however many refs move underneath it.
package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.klarlabs.de/kiln/internal/execx"
)

// Tree is a checked-out commit. Close removes it; callers must defer that.
type Tree struct {
	// Path is the directory the commit was checked out into.
	Path string
	// SHA is the commit it holds.
	SHA string

	repo   string
	runner execx.Runner
	closed bool
}

// Add checks sha out of the repository at repoDir into a fresh temp directory.
//
// The commit must already be present locally; Add does not fetch. Discovery is
// watch's job, and a worktree that could reach the network would make prove
// depend on remote state at exactly the moment it is supposed to be pinned.
func Add(ctx context.Context, r execx.Runner, repoDir, sha string) (*Tree, error) {
	if strings.TrimSpace(sha) == "" {
		return nil, fmt.Errorf("worktree: no commit to check out")
	}

	dir, err := os.MkdirTemp("", TempPrefix+"*")
	if err != nil {
		return nil, fmt.Errorf("worktree: create temp dir: %w", err)
	}

	// git refuses to add a worktree onto an existing directory, so hand it a
	// path that does not exist yet inside the temp dir we just made. The outer
	// dir is what Close removes, which keeps cleanup a single RemoveAll.
	target := dir + "/tree"

	if _, err := r.Run(ctx, execx.Cmd{
		Name: "git",
		Args: []string{"worktree", "add", "--detach", "--force", target, sha},
		Dir:  repoDir,
	}); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("worktree: check out %s: %w", sha, err)
	}

	return &Tree{Path: target, SHA: sha, repo: repoDir, runner: r}, nil
}

// Close removes the worktree and its temp directory. It is idempotent, and it
// removes the directory even if git's own bookkeeping fails, because a leaked
// checkout on a long-running watch box is a disk-space incident.
func (t *Tree) Close(ctx context.Context) error {
	if t == nil || t.closed {
		return nil
	}
	t.closed = true

	_, gitErr := t.runner.Run(ctx, execx.Cmd{
		Name: "git",
		Args: []string{"worktree", "remove", "--force", t.Path},
		Dir:  t.repo,
	})
	// Always remove the directory. If git already did, this is a no-op; if git
	// failed, this is the part that actually matters.
	rmErr := os.RemoveAll(strings.TrimSuffix(t.Path, "/tree"))

	// Prune the stale administrative entry git leaves behind when a worktree
	// directory vanishes without `worktree remove` succeeding. Without this,
	// `git worktree list` accumulates ghosts on a watch box.
	if gitErr != nil {
		_, _ = t.runner.Run(ctx, execx.Cmd{Name: "git", Args: []string{"worktree", "prune"}, Dir: t.repo})
	}

	if gitErr != nil && rmErr != nil {
		return fmt.Errorf("worktree: remove %s: %w", t.Path, gitErr)
	}
	if rmErr != nil {
		return fmt.Errorf("worktree: clean %s: %w", t.Path, rmErr)
	}
	return nil
}

// With runs fn inside a worktree at sha and cleans up afterwards, even when fn
// panics. Every caller in Kiln uses this rather than Add/Close directly: a
// forgotten Close leaks a checkout, and this shape makes forgetting impossible.
func With(ctx context.Context, r execx.Runner, repoDir, sha string, fn func(dir string) error) (err error) {
	tree, addErr := Add(ctx, r, repoDir, sha)
	if addErr != nil {
		return addErr
	}
	defer func() {
		// Cleanup uses a context detached from ctx: when a run is cancelled,
		// ctx is already done, and a cleanup that inherited it would refuse to
		// run at precisely the moment it is needed.
		if cErr := tree.Close(context.WithoutCancel(ctx)); cErr != nil && err == nil {
			err = cErr
		}
	}()
	return fn(tree.Path)
}

// ResolveSHA turns a commit-ish into a full object id. `kiln run --sha HEAD`
// goes through here, so the ledger and the image tag always record the real
// commit rather than a name that will mean something else tomorrow.
func ResolveSHA(ctx context.Context, r execx.Runner, repoDir, commitish string) (string, error) {
	if strings.TrimSpace(commitish) == "" {
		return "", fmt.Errorf("no commit given")
	}
	res, err := r.Run(ctx, execx.Cmd{
		Name: "git",
		// ^{commit} peels an annotated tag to the commit it points at. Without
		// it, `kiln run --sha v1.2.0` would resolve to the tag object's own id,
		// which no worktree can check out and no note is bound to.
		Args: []string{"rev-parse", "--verify", commitish + "^{commit}"},
		Dir:  repoDir,
	})
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", commitish, err)
	}
	sha := res.Output()
	if sha == "" {
		return "", fmt.Errorf("resolve %q: git returned nothing", commitish)
	}
	return sha, nil
}

// IsRepo reports whether dir is inside a git work tree.
func IsRepo(ctx context.Context, r execx.Runner, dir string) bool {
	res, err := r.Run(ctx, execx.Cmd{
		Name: "git", Args: []string{"rev-parse", "--is-inside-work-tree"}, Dir: dir,
	})
	return err == nil && res.Output() == "true"
}

// TempPrefix is the name every Kiln worktree directory starts with. Reaping
// keys off it, so nothing outside Kiln's own leavings is ever a candidate.
const TempPrefix = "kiln-tree-"

// ReapAfter is how long an abandoned tree must have sat before it is removed.
//
// Generous on purpose. The reaper cannot ask a directory whether a build is
// still using it, so age is the only evidence available, and it has to exceed
// any build somebody might legitimately be running. Deleting a live checkout
// mid-build would be a far worse failure than leaving a stale one an extra
// day.
const ReapAfter = 24 * time.Hour

// Reap removes abandoned worktrees and prunes git's record of them.
//
// A run cleans up after itself, including on cancellation — but not through a
// SIGKILL, an OOM kill or a power cut, and a box building all day for months
// accumulates those. Nothing else ever collects them, so the disk fills
// quietly and the first symptom is an unrelated build failing.
//
// Returns how many directories it removed. Errors are reported but never
// fatal: failing a build because housekeeping did not work would be the
// housekeeping causing the outage it exists to prevent.
func Reap(ctx context.Context, r execx.Runner, repoDir string, olderThan time.Duration) (int, error) {
	if olderThan <= 0 {
		olderThan = ReapAfter
	}

	// git first: it knows which of its own worktrees are gone and will drop
	// the administrative entries, which is what stops `git worktree list`
	// filling with ghosts.
	if _, err := r.Run(ctx, execx.Cmd{
		Name: "git", Args: []string{"worktree", "prune"}, Dir: repoDir,
	}); err != nil {
		return 0, fmt.Errorf("worktree: prune: %w", err)
	}

	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		return 0, fmt.Errorf("worktree: read temp dir: %w", err)
	}

	live, err := liveTrees(ctx, r, repoDir)
	if err != nil {
		// Without the live set the age check alone is not enough to be safe,
		// because a very long build would look abandoned. Stop rather than
		// guess.
		return 0, err
	}

	cutoff := time.Now().Add(-olderThan)
	removed := 0
	var firstErr error

	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), TempPrefix) {
			continue
		}
		path := filepath.Join(os.TempDir(), e.Name())
		if live[resolve(filepath.Join(path, "tree"))] {
			// Another kiln — or this one — is building in it right now.
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("worktree: remove %s: %w", path, err)
			}
			continue
		}
		removed++
	}
	return removed, firstErr
}

// liveTrees lists the worktrees git currently has registered for this
// repository, so the reaper never removes one in use.
//
// This only covers trees belonging to *this* repository. A tree from another
// checkout on the same box is protected by the age cutoff alone, which is why
// that cutoff is a day rather than an hour.
func liveTrees(ctx context.Context, r execx.Runner, repoDir string) (map[string]bool, error) {
	res, err := r.Run(ctx, execx.Cmd{
		Name: "git", Args: []string{"worktree", "list", "--porcelain"}, Dir: repoDir,
	})
	if err != nil {
		return nil, fmt.Errorf("worktree: list: %w", err)
	}

	live := map[string]bool{}
	for line := range strings.SplitSeq(res.Stdout, "\n") {
		if path, ok := strings.CutPrefix(strings.TrimSpace(line), "worktree "); ok {
			live[resolve(path)] = true
		}
	}
	return live, nil
}

// resolve canonicalises a path so the two sides of the live-tree comparison
// can actually match.
//
// This is not defensive tidying. On macOS os.TempDir() is /var/folders/…,
// which is a symlink to /private/var/folders/…, and git reports the resolved
// form — so a naive string compare finds no live trees and the reaper deletes
// the checkout of a build that is still running. A symlinked /tmp does the
// same on Linux.
//
// An unresolvable path falls back to the input: it is about to be compared
// against a live set that will not contain it, and the age cutoff still
// applies.
func resolve(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}
