package task

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"go.klarlabs.de/kiln/internal/infrastructure/execx"
)

// exitCommandNotFound is what a POSIX shell exits with when the command it was
// told to run does not exist (and what bash exits with for `bash missing.sh`).
const exitCommandNotFound = 127

// notFoundMessages are how shells say a named file is missing, following the
// name: bash and macOS sh say the first, dash and busybox the second.
var notFoundMessages = []string{": No such file or directory", ": not found"}

// predates reports whether a failed task failed only because the commit does
// not contain a script its command names, and returns that path.
//
// Exit 127 alone cannot say this: it equally means a tool missing from PATH,
// which is the box's problem and must stay a red failure an operator sees. So
// every one of these has to hold before the commit is excused:
//
//   - the shell exited 127 and said a path named in the command was missing;
//   - the path is relative and stays inside the worktree;
//   - nothing exists at that path in the checkout;
//   - git does not ignore it — a gitignored node_modules/.bin/eslint is missing
//     because nothing installed dependencies, which no commit could fix;
//   - no commit in this commit's history ever had it. That is what separates
//     "branched before scripts/ci existed" from a pull request that deletes the
//     script a required check runs, which must not turn the check grey.
//
// Any doubt — a shallow clone hiding history, git failing, a nested script
// rather than the command itself — leaves the ordinary failure in place.
// Wrongly excusing a commit is worse than one more red check.
func (t *Runner) predates(ctx context.Context, root, dir, command string, err error) (string, bool) {
	var ee *execx.ExitError
	if !errors.As(err, &ee) {
		return "", false
	}
	for _, path := range repoPaths(command) {
		if couldNotExec(ee.Code, command, path) && reportedMissing(ee.Stderr, path) &&
			t.neverInHistory(ctx, root, dir, path) {
			return path, true
		}
	}
	return "", false
}

// couldNotExec reports whether an exit code is a shell failing to start path.
//
// 127 is the rule. The exception is bash — macOS's sh — under -e: when the
// whole script is the one command it cannot find, it exits 1, so `run:
// ./scripts/ci/build.sh` would otherwise be classified on Linux (dash, 127) and
// not on a Mac. Exit 1 is accepted only for that shape, the path as the
// script's first word; anywhere else it is as likely to be `> out/x.log`
// failing on a missing directory, which is a real failure.
func couldNotExec(code int, command, path string) bool {
	if code == exitCommandNotFound {
		return true
	}
	words := strings.Fields(command)
	return code == 1 && len(words) > 0 && strings.Trim(words[0], `"'`) == path
}

// repoPaths picks out the words of a command that look like files inside the
// repository: relative, containing a slash, and free of shell expansion.
//
// Only words in the command itself. A path a script computes, or one a nested
// script calls, is not visible here, and guessing at it is how a box would end
// up excusing something it does not understand.
func repoPaths(command string) []string {
	var paths []string
	for _, word := range strings.Fields(command) {
		word = strings.Trim(word, `"'`)
		word = strings.TrimRight(word, ";&|")
		if !strings.Contains(word, "/") || strings.ContainsAny(word, "$`*?=~") ||
			strings.HasPrefix(word, "-") || !filepath.IsLocal(word) {
			continue
		}
		paths = append(paths, word)
	}
	return paths
}

func reportedMissing(stderr, path string) bool {
	for _, msg := range notFoundMessages {
		if strings.Contains(stderr, path+msg) {
			return true
		}
	}
	return false
}

// neverInHistory reports whether path, relative to dir inside the worktree
// rooted at root, is absent from the checkout and from every commit before it.
func (t *Runner) neverInHistory(ctx context.Context, root, dir, path string) bool {
	full := filepath.Join(dir, path)
	if rel, err := filepath.Rel(root, full); err != nil || !filepath.IsLocal(rel) {
		return false
	}
	if _, err := os.Lstat(full); !errors.Is(err, os.ErrNotExist) {
		return false
	}

	// A shallow clone has cut off the history that would show the file was
	// once there, so its silence proves nothing.
	shallow, err := t.git(ctx, dir, "rev-parse", "--is-shallow-repository")
	if err != nil || shallow != "false" {
		return false
	}

	// check-ignore exits 1 for "not ignored"; 0 is ignored and 128 is git
	// failing, and neither is evidence about the commit.
	_, err = t.git(ctx, dir, "check-ignore", "-q", "--", path)
	if code, isExit := execx.ExitCode(err); !isExit || code != 1 {
		return false
	}

	touched, err := t.git(ctx, dir, "log", "-1", "--format=%H", "HEAD", "--", path)
	return err == nil && touched == ""
}

func (t *Runner) git(ctx context.Context, dir string, args ...string) (string, error) {
	res, err := t.Exec.Run(ctx, execx.Cmd{Name: "git", Args: args, Dir: dir})
	return res.Output(), err
}
