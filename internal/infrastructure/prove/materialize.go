package prove

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.klarlabs.de/kiln/internal/domain/isolation"
)

// materialize carries gitignored dependency directories from the clone into
// the worktree the gate will run in.
//
// It exists because of an interaction nobody designed: kiln proves inside a
// disposable worktree — deliberately, so a run cannot leave the operator's
// checkout on another commit — and a dependency directory is gitignored, so it
// is in the clone and never in the checkout. Go repositories never noticed,
// because their dependencies come from the module cache, which is global. The
// first Node repository anyone pointed a box at failed every gate with
// "vitest: command not found", while passing by hand in the clone. That reads
// as flakiness, which is the expensive kind of wrong.
//
// Nothing is copied for an untrusted run, and that is the important line here.
// The pipeline is read from the commit being gated, so a fork's author writes
// the list; without this check, `materialize: [".env"]` would hand them
// whatever the operator keeps beside their clone, inside code kiln is about to
// execute. A fork's gate failing for a missing toolchain is the safe outcome
// and an honest one.
func materialize(cloneDir, worktreeDir string, paths []string, policy isolation.Policy) error {
	if len(paths) == 0 {
		return nil
	}
	if !policy.Secrets {
		// Same bit that decides whether the run sees registry and signing
		// credentials: what is in the operator's clone but not in the commit
		// is theirs, not the proposer's.
		return nil
	}

	for _, p := range paths {
		src, err := contained(cloneDir, p)
		if err != nil {
			return err
		}
		info, err := os.Stat(src)
		if os.IsNotExist(err) {
			// A Go repository with the key set, or a clone where nobody has
			// installed anything yet. The gate will say so on its own terms.
			continue
		}
		if err != nil {
			return fmt.Errorf("materialize %s: %w", p, err)
		}

		dst := filepath.Join(worktreeDir, filepath.Clean(p))
		if _, err := os.Stat(dst); err == nil {
			// The commit provides it. What is tracked wins over what happens
			// to be lying in the clone.
			continue
		}
		if err := copyTree(src, dst, info); err != nil {
			return fmt.Errorf("materialize %s: %w", p, err)
		}
	}
	return nil
}

// contained resolves p inside root and refuses anything that leaves it.
//
// Checked here and not only at config load because the load-time check is a
// courtesy that explains itself, and this one is the boundary.
func contained(root, p string) (string, error) {
	if filepath.IsAbs(p) || strings.Contains(p, "..") {
		return "", fmt.Errorf("materialize: %q must stay inside the repository", p)
	}
	full := filepath.Join(root, filepath.Clean(p))
	rel, err := filepath.Rel(root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("materialize: %q must stay inside the repository", p)
	}
	return full, nil
}

// copyTree hardlinks where it can and copies where it cannot.
//
// Hardlinks because a node_modules is tens of thousands of files and a box
// does this every tick; a real copy per run is minutes of disk nobody asked
// for. They are safe here because the worktree is thrown away after the gate,
// and a build that rewrites a file in place gets a copy-on-write from the
// tooling rather than corrupting the clone — the same trade warden already
// makes for its own validation worktree.
func copyTree(src, dst string, info os.FileInfo) error {
	if !info.IsDir() {
		return linkOrCopy(src, dst, info)
	}
	return filepath.Walk(src, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case fi.IsDir():
			return os.MkdirAll(target, 0o755)
		case fi.Mode()&os.ModeSymlink != 0:
			// Kept as a symlink rather than followed: a dependency tree is
			// full of them, and following one could copy the world.
			link, rerr := os.Readlink(path)
			if rerr != nil {
				return rerr
			}
			return os.Symlink(link, target)
		default:
			return linkOrCopy(path, target, fi)
		}
	})
}

func linkOrCopy(src, dst string, fi os.FileInfo) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	// A different filesystem, or a link count already at its limit.
	in, err := os.Open(src) //nolint:gosec // path is contained above
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fi.Mode().Perm()) //nolint:gosec // ditto
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
