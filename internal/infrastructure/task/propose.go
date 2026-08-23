package task

import (
	"context"
	"fmt"
	"strings"

	"go.klarlabs.de/kiln/internal/config"
	"go.klarlabs.de/kiln/internal/infrastructure/execx"
)

// Proposal is what a task left behind for review.
type Proposal struct {
	// Changed reports whether the task modified anything at all. False is the
	// common and healthy case for a remediation task: nothing to fix today.
	Changed bool
	// Branch is the head branch the changes were pushed to.
	Branch string
	// Number is the pull request, when one was opened or found.
	Number int
	// Opened distinguishes a new pull request from an update to the one that
	// was already there.
	Opened bool
}

// Summary renders the proposal for a check body.
func (p Proposal) Summary() string {
	switch {
	case !p.Changed:
		return "no changes to propose"
	case p.Number == 0:
		return "pushed " + p.Branch
	case p.Opened:
		return fmt.Sprintf("opened #%d from %s", p.Number, p.Branch)
	default:
		return fmt.Sprintf("updated #%d from %s", p.Number, p.Branch)
	}
}

// Forge is the part of the GitHub client proposing needs.
type Forge interface {
	OpenPullRequest(ctx context.Context, head, base, title, body string) (number int, opened bool, err error)
	LabelPull(ctx context.Context, number int, labels []string) error
}

// Propose commits whatever the task changed, pushes it and opens or updates a
// pull request.
//
// Nothing here runs unless the worktree is actually dirty. A remediation task
// that found nothing to fix must not push an empty commit or open a pull
// request saying so — that is how a useful automation becomes noise people
// filter out, and then miss the day it matters.
func (t *Runner) Propose(
	ctx context.Context, req Request, spec config.PullRequest, forge Forge,
) (Proposal, error) {
	if !req.Policy.Secrets {
		// Structural, not a configuration mistake: an untrusted head must
		// never hold a credential that can write to the base repository.
		// Config validation already refuses pull_request on a pull_request
		// task; this is the second lock, for any caller that assembles a
		// request by hand.
		return Proposal{}, fmt.Errorf("task %s: refusing to open a pull request from an untrusted head", req.Name)
	}

	dirty, err := t.dirty(ctx, req.Dir)
	if err != nil {
		return Proposal{}, err
	}
	if !dirty {
		return Proposal{Changed: false}, nil
	}

	if err := t.commit(ctx, req, spec); err != nil {
		return Proposal{}, err
	}
	if err := t.push(ctx, req.Dir, spec.Branch); err != nil {
		return Proposal{}, err
	}

	proposal := Proposal{Changed: true, Branch: spec.Branch}
	if forge == nil {
		// No token, no forge. The branch is pushed and says what happened;
		// failing here would throw away work that succeeded.
		return proposal, nil
	}

	number, opened, err := forge.OpenPullRequest(ctx, spec.Branch, spec.Base, spec.Title, spec.Body)
	if err != nil {
		return proposal, err
	}
	proposal.Number, proposal.Opened = number, opened

	if opened {
		// Only on creation. Re-applying labels on every run would fight an
		// operator who deliberately removed one.
		if err := forge.LabelPull(ctx, number, spec.Labels); err != nil {
			return proposal, err
		}
	}
	return proposal, nil
}

// dirty reports whether the worktree has changes, tracked or not.
func (t *Runner) dirty(ctx context.Context, dir string) (bool, error) {
	res, err := t.Exec.Run(ctx, execx.Cmd{
		Name: "git", Args: []string{"status", "--porcelain"}, Dir: dir,
	})
	if err != nil {
		return false, fmt.Errorf("task: read worktree status: %w", err)
	}
	return strings.TrimSpace(res.Stdout) != "", nil
}

// commit puts the task's changes on the head branch.
//
// The worktree is a detached checkout of the commit under test, so this
// creates the branch there rather than switching an existing one: the base is
// deliberately the commit the task ran against, not whatever the branch
// pointed at last time. A remediation is a statement about *this* code.
func (t *Runner) commit(ctx context.Context, req Request, spec config.PullRequest) error {
	steps := [][]string{
		{"switch", "--force-create", spec.Branch},
		{"add", "--all"},
		{"-c", "user.name=kiln", "-c", "user.email=kiln@klarlabs.de",
			"commit", "--message", commitMessage(req.Name, spec.Title)},
	}
	for _, args := range steps {
		if _, err := t.Exec.Run(ctx, execx.Cmd{Name: "git", Args: args, Dir: req.Dir}); err != nil {
			return fmt.Errorf("task %s: git %s: %w", req.Name, args[0], err)
		}
	}
	return nil
}

// push force-updates the branch.
//
// Forced because the branch is kiln's own and is rebuilt from the commit under
// test each time; a fast-forward would require carrying yesterday's
// remediation into today's, which is how a stale fix outlives the code it was
// fixing. --force-with-lease is not the safer choice here, since there is no
// expected old value to lease against on a branch that may not exist yet.
func (t *Runner) push(ctx context.Context, dir, branch string) error {
	_, err := t.Exec.Run(ctx, execx.Cmd{
		Name: "git",
		Args: []string{"push", "--force", "origin", "HEAD:refs/heads/" + branch},
		Dir:  dir,
	})
	if err != nil {
		return fmt.Errorf("task: push %s: %w", branch, err)
	}
	return nil
}

func commitMessage(task, title string) string {
	body := strings.TrimSpace(title)
	if body == "" {
		body = "changes from task " + task
	}
	return body + "\n\nProduced by kiln task " + task + "."
}
