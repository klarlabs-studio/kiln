package task_test

import (
	"errors"
	"path/filepath"
	"testing"

	"go.klarlabs.de/kiln/internal/application/ports"
	"go.klarlabs.de/kiln/internal/domain/config"
	"go.klarlabs.de/kiln/internal/gittest"
)

// oldCommit is a repository whose only commit predates the pipeline's scripts —
// the shape of an old pull request branch opened before scripts/ci existed.
func oldCommit(t *testing.T) *gittest.Repo {
	t.Helper()
	repo := gittest.New(t)
	repo.Commit("before ci scripts", "README.md", "hello\n")
	return repo
}

func wantNotApplicable(t *testing.T, res ports.TaskResult) {
	t.Helper()
	if !errors.Is(res.Err, ports.ErrNotApplicable) {
		t.Fatalf("err = %v, want ErrNotApplicable", res.Err)
	}
	if errors.Is(res.Err, ports.ErrTaskFailed) {
		t.Errorf("err = %v also claims the task failed; nothing in this commit could run", res.Err)
	}
	if !res.NotApplicable() {
		t.Error("NotApplicable() false for a commit that predates the script")
	}
}

func wantFailure(t *testing.T, res ports.TaskResult) {
	t.Helper()
	if !errors.Is(res.Err, ports.ErrTaskFailed) {
		t.Fatalf("err = %v, want ErrTaskFailed", res.Err)
	}
	if res.NotApplicable() {
		t.Errorf("err = %v was excused as not applicable", res.Err)
	}
}

// ~400 box runs failed exactly like this: an old pull request branch, a
// pipeline that now calls scripts/ci/build.sh, and `exit 127` forever.
func TestAScriptTheCommitPredatesIsNotApplicable(t *testing.T) {
	repo := oldCommit(t)

	for _, command := range []string{
		"bash scripts/ci/build.sh",
		"./scripts/ci/build.sh",
		"scripts/ci/build.sh --flag",
		`bash "scripts/ci/build.sh"`,
	} {
		t.Run(command, func(t *testing.T) {
			res, _ := run(t, repo.Dir, config.Task{Run: command}, trusted)
			wantNotApplicable(t, res)

			// The check title names the file, so the error has to carry it.
			var na *ports.NotApplicableError
			if !errors.As(res.Err, &na) || filepath.Clean(na.Path) != "scripts/ci/build.sh" {
				t.Errorf("err = %#v, want the missing path scripts/ci/build.sh", res.Err)
			}
		})
	}
}

// Not applicable is not a failure, so allow_failure has nothing to tolerate.
func TestANotApplicableTaskIsNotATolerance(t *testing.T) {
	res, _ := run(t, oldCommit(t).Dir, config.Task{Run: "bash scripts/ci/build.sh", AllowFailure: true}, trusted)

	wantNotApplicable(t, res)
	if res.Tolerated {
		t.Error("Tolerated set on a task that never ran")
	}
}

// A missing tool on PATH is the box's problem, not the commit's. Excusing it
// would hide a broken machine behind a grey check forever.
func TestAToolMissingFromPathIsStillAFailure(t *testing.T) {
	res, _ := run(t, oldCommit(t).Dir, config.Task{Run: "kiln-no-such-tool-anywhere --version"}, trusted)
	wantFailure(t, res)
}

// The bypass this must not open: a pull request that deletes the script a
// required check runs. Its history contains the file, so it did not predate it.
func TestAScriptTheCommitDeletedIsStillAFailure(t *testing.T) {
	repo := oldCommit(t)
	repo.Commit("add ci", "scripts/ci/build.sh", "#!/bin/sh\nexit 0\n")
	repo.Git("rm", "-q", "scripts/ci/build.sh")
	repo.Git("commit", "-q", "-m", "dodge the check")

	res, _ := run(t, repo.Dir, config.Task{Run: "bash scripts/ci/build.sh"}, trusted)
	wantFailure(t, res)
}

// node_modules/.bin is gitignored: it is missing because nothing installed the
// dependencies, which no commit could ever fix by itself containing it.
func TestAMissingIgnoredFileIsStillAFailure(t *testing.T) {
	repo := oldCommit(t)
	repo.Commit("ignore deps", ".gitignore", "node_modules/\n")

	res, _ := run(t, repo.Dir, config.Task{Run: "./node_modules/.bin/eslint ."}, trusted)
	wantFailure(t, res)
}

// A script that exists and exits 127 of its own accord reached a verdict.
func TestAScriptThatExits127ItselfIsStillAFailure(t *testing.T) {
	repo := oldCommit(t)
	repo.Commit("add ci", "scripts/ci/build.sh", "exit 127\n")

	res, _ := run(t, repo.Dir, config.Task{Run: "bash scripts/ci/build.sh"}, trusted)
	wantFailure(t, res)
}

// bash under -e exits 1, not 127, for a lone command it cannot find, which is
// why exit 1 is accepted at all. A redirect into a directory that does not
// exist exits 1 with the same words, and is a real failure.
func TestAMissingRedirectTargetIsStillAFailure(t *testing.T) {
	res, _ := run(t, oldCommit(t).Dir, config.Task{Run: "echo hi > out/report.log"}, trusted)
	wantFailure(t, res)
}

// Exit 1 is bash's quirk for a lone missing command and nothing more; any other
// status means the script went on to decide something.
func TestAnotherExitStatusIsStillAFailure(t *testing.T) {
	res, _ := run(t, oldCommit(t).Dir, config.Task{
		Run: "scripts/ci/build.sh 2>/dev/null || { echo 'scripts/ci/build.sh: not found' >&2; exit 3; }",
	}, trusted)
	wantFailure(t, res)
}

// A shallow clone has lost the history that would show a pull request deleted
// the script, so silence from git log there proves nothing.
func TestInAShallowCloneAMissingScriptIsStillAFailure(t *testing.T) {
	repo := oldCommit(t)
	repo.Commit("add ci", "scripts/ci/build.sh", "#!/bin/sh\nexit 0\n")
	repo.Git("rm", "-q", "scripts/ci/build.sh")
	repo.Git("commit", "-q", "-m", "dodge the check")

	shallow := t.TempDir()
	repo.Git("clone", "-q", "--depth", "1", "file://"+repo.Dir, shallow)

	res, _ := run(t, shallow, config.Task{Run: "bash scripts/ci/build.sh"}, trusted)
	wantFailure(t, res)
}

// A path outside the worktree is not something the commit could contain.
func TestAMissingPathOutsideTheWorktreeIsStillAFailure(t *testing.T) {
	res, _ := run(t, oldCommit(t).Dir, config.Task{Run: "bash ../elsewhere/build.sh"}, trusted)
	wantFailure(t, res)
}

// Without git history to consult there is no evidence the commit predates
// anything, and an unproven excuse is not one.
func TestOutsideARepositoryAMissingScriptIsStillAFailure(t *testing.T) {
	res, _ := run(t, t.TempDir(), config.Task{Run: "bash scripts/ci/build.sh"}, trusted)
	wantFailure(t, res)
}

// workdir moves where the command runs, and the path is relative to it.
func TestWorkdirIsHonouredWhenLookingForTheScript(t *testing.T) {
	repo := oldCommit(t)
	repo.Commit("site", "site/index.md", "x\n")

	res, _ := run(t, repo.Dir, config.Task{Run: "bash scripts/build.sh", Workdir: "site"}, trusted)
	wantNotApplicable(t, res)
}
