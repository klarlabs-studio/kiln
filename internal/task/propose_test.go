package task_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.klarlabs.de/kiln/internal/config"
	"go.klarlabs.de/kiln/internal/execx"
	"go.klarlabs.de/kiln/internal/isolation"
	"go.klarlabs.de/kiln/internal/task"
)

// forge records what a proposal asked the forge to do.
type forge struct {
	head, base, title, body string
	labels                  []string
	number                  int
	opened                  bool
	err                     error
	calls                   int
}

func (f *forge) OpenPullRequest(_ context.Context, head, base, title, body string) (int, bool, error) {
	f.calls++
	f.head, f.base, f.title, f.body = head, base, title, body
	return f.number, f.opened, f.err
}

func (f *forge) LabelPull(_ context.Context, _ int, labels []string) error {
	f.labels = labels
	return nil
}

// repoWithRemote builds a git repository with a real bare remote to push to,
// checked out the way kiln checks one out: detached at a commit.
func repoWithRemote(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	work := filepath.Join(root, "work")

	run := func(dir string, args ...string) {
		t.Helper()
		if _, err := execx.NewSystem().Run(t.Context(), execx.Cmd{Name: "git", Args: args, Dir: dir}); err != nil {
			t.Fatalf("git %s: %v", strings.Join(args, " "), err)
		}
	}

	if err := os.MkdirAll(remote, 0o750); err != nil {
		t.Fatal(err)
	}
	run(remote, "init", "-q", "--bare", ".")
	run(root, "clone", "-q", remote, "work")
	run(work, "config", "user.email", "t@example.com")
	run(work, "config", "user.name", "t")

	if err := os.WriteFile(filepath.Join(work, "app.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(work, "add", "--all")
	run(work, "commit", "-qm", "first")
	run(work, "branch", "-M", "main")
	run(work, "push", "-q", "origin", "main")
	// Detached, as a kiln worktree is.
	run(work, "checkout", "-q", "--detach", "HEAD")
	return work
}

func propose(t *testing.T, dir string, policy isolation.Policy, f task.Forge) (task.Proposal, error) {
	t.Helper()
	return task.New(execx.NewSystem()).Propose(t.Context(), task.Request{
		Name: "remediate", Dir: dir, SHA: "deadbeef", Ref: "refs/heads/main",
		Event: "schedule", Policy: policy,
	}, config.PullRequest{
		Branch: "kiln/remediate", Title: "chore: apply remediations",
		Body: "opened by a task", Labels: []string{"security"},
	}, f)
}

func TestNothingChangedMeansNoPullRequest(t *testing.T) {
	dir := repoWithRemote(t)
	f := &forge{number: 7, opened: true}

	// The common, healthy case for a remediation task: nothing to fix today.
	// An empty commit and a pull request saying so is how a useful automation
	// becomes noise people filter out, and then miss the day it matters.
	p, err := propose(t, dir, trusted, f)
	if err != nil {
		t.Fatal(err)
	}
	if p.Changed {
		t.Error("reported a change in a clean worktree")
	}
	if f.calls != 0 {
		t.Errorf("opened a pull request for nothing (%d calls)", f.calls)
	}
	if p.Summary() != "no changes to propose" {
		t.Errorf("summary = %q", p.Summary())
	}
}

func TestChangesArePushedAndProposed(t *testing.T) {
	dir := repoWithRemote(t)
	if err := os.WriteFile(filepath.Join(dir, "app.txt"), []byte("fixed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &forge{number: 12, opened: true}

	p, err := propose(t, dir, trusted, f)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if !p.Changed || p.Number != 12 || !p.Opened {
		t.Fatalf("proposal = %+v", p)
	}
	if f.head != "kiln/remediate" || f.title != "chore: apply remediations" {
		t.Errorf("forge got head=%q title=%q", f.head, f.title)
	}
	if len(f.labels) != 1 || f.labels[0] != "security" {
		t.Errorf("labels = %v", f.labels)
	}

	// The branch really exists on the remote, with the change in it.
	out, err := execx.NewSystem().Run(t.Context(), execx.Cmd{
		Name: "git", Args: []string{"show", "kiln/remediate:app.txt"}, Dir: dir,
	})
	if err != nil {
		t.Fatalf("the branch was not created: %v", err)
	}
	if strings.TrimSpace(out.Stdout) != "fixed" {
		t.Errorf("branch content = %q, want the task's change", out.Stdout)
	}
}

func TestAnUntrackedFileCountsAsAChange(t *testing.T) {
	dir := repoWithRemote(t)
	// A remediation that adds a file — a missing licence header, a new
	// config — must not read as "nothing happened".
	if err := os.WriteFile(filepath.Join(dir, "NEW.md"), []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &forge{number: 3, opened: true}

	p, err := propose(t, dir, trusted, f)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Changed {
		t.Error("an untracked file was not treated as a change")
	}
}

func TestAnExistingPullRequestIsUpdatedNotDuplicated(t *testing.T) {
	dir := repoWithRemote(t)
	if err := os.WriteFile(filepath.Join(dir, "app.txt"), []byte("fixed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The forge reports the pull request already existed.
	f := &forge{number: 12, opened: false}

	p, err := propose(t, dir, trusted, f)
	if err != nil {
		t.Fatal(err)
	}
	if p.Opened {
		t.Error("an existing pull request was reported as newly opened")
	}
	if !strings.Contains(p.Summary(), "updated #12") {
		t.Errorf("summary = %q", p.Summary())
	}
	// Labels are applied on creation only: re-applying every day would fight
	// an operator who deliberately removed one.
	if f.labels != nil {
		t.Errorf("labels re-applied to an existing pull request: %v", f.labels)
	}
}

func TestAnUntrustedHeadCannotOpenAPullRequest(t *testing.T) {
	dir := repoWithRemote(t)
	if err := os.WriteFile(filepath.Join(dir, "app.txt"), []byte("evil\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &forge{number: 1, opened: true}

	// Config validation already refuses pull_request on a pull_request task.
	// This is the second lock: a caller assembling a request by hand must not
	// be able to hand a fork head a credential that writes to the base repo.
	fork := isolation.For(isolation.EventPullRequest, true)
	_, err := propose(t, dir, fork, f)

	if err == nil {
		t.Fatal("a fork pull request opened a pull request")
	}
	if !strings.Contains(err.Error(), "untrusted head") {
		t.Errorf("err = %v", err)
	}
	if f.calls != 0 {
		t.Error("the forge was called for an untrusted head")
	}
}

func TestNoForgeStillPushesTheBranch(t *testing.T) {
	dir := repoWithRemote(t)
	if err := os.WriteFile(filepath.Join(dir, "app.txt"), []byte("fixed\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A box with no token. Failing here would throw away work that succeeded;
	// the branch is pushed and a human can open the pull request.
	p, err := propose(t, dir, trusted, nil)
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if !p.Changed || p.Number != 0 {
		t.Fatalf("proposal = %+v", p)
	}
	if !strings.Contains(p.Summary(), "pushed kiln/remediate") {
		t.Errorf("summary = %q", p.Summary())
	}
}

func TestASecondRunReplacesTheBranchRatherThanStacking(t *testing.T) {
	dir := repoWithRemote(t)
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "app.txt"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write("first fix\n")
	if _, err := propose(t, dir, trusted, &forge{number: 1, opened: true}); err != nil {
		t.Fatal(err)
	}

	// Back to the base commit, as the next run's fresh worktree would be.
	sys := execx.NewSystem()
	if _, err := sys.Run(t.Context(), execx.Cmd{
		Name: "git", Args: []string{"checkout", "-q", "--detach", "main"}, Dir: dir,
	}); err != nil {
		t.Fatal(err)
	}
	write("second fix\n")
	if _, err := propose(t, dir, trusted, &forge{number: 1, opened: false}); err != nil {
		t.Fatal(err)
	}

	// One commit on top of the base, not two: the branch is rebuilt from the
	// commit under test, so yesterday's fix does not outlive the code it was
	// fixing.
	out, err := sys.Run(t.Context(), execx.Cmd{
		Name: "git", Args: []string{"rev-list", "--count", "main..kiln/remediate"}, Dir: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.Stdout) != "1" {
		t.Errorf("branch is %s commits ahead of main, want 1", strings.TrimSpace(out.Stdout))
	}
}
