package task

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// KeptFile is one retained output.
type KeptFile struct {
	// Name is the path relative to the worktree, as the operator wrote it.
	Name string
	// Bytes is the size, so `kiln status` can say what is taking up the disk.
	Bytes int64
}

// Keep copies a task's declared outputs out of the worktree before it is
// destroyed.
//
// The worktree is deleted the moment the run ends, which is exactly when
// somebody wants to read the coverage report or the scan output that explains
// why the run failed. This is the local equivalent of upload-artifact, and
// deliberately local: kiln keeps the files where the build happened, and a
// task that wants them elsewhere can copy them itself.
//
// Failures to copy are returned, not swallowed. A retention that quietly kept
// nothing looks identical to a task that produced nothing.
func Keep(worktree, dest string, globs []string) ([]KeptFile, error) {
	if len(globs) == 0 {
		return nil, nil
	}

	var kept []KeptFile
	var problems []string

	for _, pattern := range globs {
		matches, err := matchWithin(worktree, pattern)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if len(matches) == 0 {
			// Said out loud rather than passed over. A glob that matches
			// nothing is nearly always a typo or a build that did not get far
			// enough, and silence there is how somebody discovers the report
			// was never kept a week after they needed it.
			problems = append(problems, fmt.Sprintf("%s matched nothing", pattern))
			continue
		}
		for _, rel := range matches {
			size, err := copyOut(filepath.Join(worktree, rel), filepath.Join(dest, rel))
			if err != nil {
				problems = append(problems, err.Error())
				continue
			}
			kept = append(kept, KeptFile{Name: rel, Bytes: size})
		}
	}

	sort.Slice(kept, func(i, j int) bool { return kept[i].Name < kept[j].Name })
	if len(problems) > 0 {
		return kept, fmt.Errorf("keep: %s", strings.Join(problems, "; "))
	}
	return kept, nil
}

// matchWithin resolves a glob relative to the worktree and refuses anything
// that escapes it.
//
// The pattern comes from the repository, so `keep: ["../../.ssh/id_ed25519"]`
// is a thing somebody can write in a pull request. Retention copies files to a
// directory the operator later reads; it must not be a way to exfiltrate the
// build box.
func matchWithin(worktree, pattern string) ([]string, error) {
	if filepath.IsAbs(pattern) {
		return nil, fmt.Errorf("%s is absolute; keep patterns are relative to the worktree", pattern)
	}

	root, err := filepath.EvalSymlinks(worktree)
	if err != nil {
		return nil, fmt.Errorf("resolve worktree: %w", err)
	}

	matches, err := filepath.Glob(filepath.Join(root, pattern))
	if err != nil {
		return nil, fmt.Errorf("%s is not a valid pattern: %w", pattern, err)
	}

	out := make([]string, 0, len(matches))
	for _, m := range matches {
		resolved, err := filepath.EvalSymlinks(m)
		if err != nil {
			continue
		}
		// After resolving, so a symlink pointing outside is caught too.
		rel, err := filepath.Rel(root, resolved)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("%s escapes the worktree", pattern)
		}
		info, err := os.Stat(resolved)
		if err != nil || info.IsDir() {
			// Directories are skipped rather than walked: a pattern that
			// accidentally matches `.` would otherwise copy the whole
			// checkout, including .git.
			continue
		}
		out = append(out, rel)
	}
	return out, nil
}

// copyOut copies one file, creating the directories under it.
func copyOut(from, to string) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(to), 0o750); err != nil {
		return 0, fmt.Errorf("create %s: %w", filepath.Dir(to), err)
	}

	src, err := os.Open(filepath.Clean(from))
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", filepath.Base(from), err)
	}
	defer func() { _ = src.Close() }()

	dst, err := os.OpenFile(filepath.Clean(to), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, fmt.Errorf("write %s: %w", filepath.Base(to), err)
	}

	written, copyErr := io.Copy(dst, src)
	closeErr := dst.Close()
	if copyErr != nil {
		return 0, fmt.Errorf("copy %s: %w", filepath.Base(from), copyErr)
	}
	if closeErr != nil {
		return 0, fmt.Errorf("close %s: %w", filepath.Base(to), closeErr)
	}
	return written, nil
}

// KeepDir is where a run's retained files live.
func KeepDir(root, runID, taskName string) string {
	return filepath.Join(root, "runs", runID, taskName)
}

// Sweep removes retained files for runs older than the newest keep.
//
// Bounded for the same reason the ledger caps itself at 500 runs and the
// docker prune keeps ten builds: a box that retains every artifact forever
// fills its disk, and the first symptom is an unrelated build failing with no
// obvious connection to the thing that caused it.
func Sweep(root string, keep int) error {
	if keep <= 0 {
		return nil
	}
	runsDir := filepath.Join(root, "runs")

	entries, err := os.ReadDir(runsDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("keep: read %s: %w", runsDir, err)
	}

	dirs := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	if len(dirs) <= keep {
		return nil
	}

	// Run ids are timestamp-prefixed, so lexical order is chronological. That
	// is a property of run.New's format and this depends on it deliberately —
	// reading mtimes would make the sweep disagree with the ledger's own
	// ordering.
	sort.Strings(dirs)
	for _, name := range dirs[:len(dirs)-keep] {
		if err := os.RemoveAll(filepath.Join(runsDir, name)); err != nil {
			return fmt.Errorf("keep: remove %s: %w", name, err)
		}
	}
	return nil
}
