package ports

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"go.klarlabs.de/kiln/internal/domain/config"
	"go.klarlabs.de/kiln/internal/domain/isolation"
)

// TaskRequest is one task to run.
type TaskRequest struct {
	// Name is the task's key in `.kiln.yaml`.
	Name string
	// Task is the definition.
	Task config.Task
	// Dir is the checked-out worktree. Tasks never run in the operator's
	// working copy, for the same reason builds do not.
	Dir string
	// SHA is the commit being worked on, exported to the command.
	SHA string
	// Ref and Event describe why this run happened.
	Ref   string
	Event string
	// Policy decides whether the command may see the operator's environment.
	Policy isolation.Policy
	// Output receives the command's stdout and stderr as it runs.
	Output io.Writer
	// ServiceEnv carries the addresses of the run's service containers.
	ServiceEnv []string
}

// TaskResult is what a task did.
type TaskResult struct {
	Name     string
	Duration time.Duration
	// Err is the command's failure, if any. A task with allow_failure set
	// still records it here — the run does not fail, but the record must not
	// pretend nothing happened.
	Err error
	// Tolerated reports that Err was suppressed by allow_failure.
	Tolerated bool
}

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

// PullProposer is the part of the GitHub client proposing needs.
type PullProposer interface {
	OpenPullRequest(ctx context.Context, head, base, title, body string) (number int, opened bool, err error)
	LabelPull(ctx context.Context, number int, labels []string) error
}

// ErrTaskFailed reports a task whose command exited non-zero.
var ErrTaskFailed = errors.New("task failed")

// KeptFile is one retained output.
type KeptFile struct {
	// Name is the path relative to the worktree, as the operator wrote it.
	Name string
	// Bytes is the size, so `kiln status` can say what is taking up the disk.
	Bytes int64
}

func (r TaskResult) OK() bool { return r.Err == nil || r.Tolerated }

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

// Tasks runs the pipeline's automation and looks after what it leaves behind.
//
// Run and Propose are the two things a task can do: change nothing, or change
// something worth opening a pull request about. Keep, KeepDir and Sweep are
// the retained output — a task that produces a report is only useful if the
// report outlives the worktree it was written in, and only affordable if old
// ones are swept.
type Tasks interface {
	Run(ctx context.Context, req TaskRequest) TaskResult
	Propose(ctx context.Context, req TaskRequest, spec config.PullRequest, forge PullProposer) (Proposal, error)
	// Keep copies files matching globs out of a finished worktree.
	Keep(worktreeDir, dest string, globs []string) ([]KeptFile, error)
	// KeepDir is where one task's output from one run belongs.
	KeepDir(root, runID, taskName string) string
	// Sweep drops all but the most recent keep runs.
	Sweep(root string, keep int) error
}

// ServiceSet is the containers started beside one run, and the way to stop
// them again. Stop is always safe to call, including on a nil set, so a caller
// can defer it before knowing whether anything started.
type ServiceSet interface {
	// Env is the addresses docker actually allocated, for the gate and the
	// tasks that talk to them.
	Env() []string
	Stop()
}

// Services starts the containers a gate needs beside it — the database a test
// suite talks to, and nothing kiln itself understands.
type Services interface {
	Start(ctx context.Context, services map[string]config.Service, runID string) (ServiceSet, error)
}

// NoServices is a ServiceSet with nothing in it.
//
// A caller defers Stop before it knows whether anything started, so "no
// services" has to be an object rather than a nil interface.
func NoServices() ServiceSet { return emptyServices{} }

type emptyServices struct{}

func (emptyServices) Env() []string { return nil }
func (emptyServices) Stop()         {}
