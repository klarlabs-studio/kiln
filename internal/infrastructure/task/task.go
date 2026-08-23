// Package task runs the automation that is neither a check nor an artifact.
//
// A pipeline does three kinds of thing. It decides whether a commit is good —
// that is warden's, and `.warden.yaml` is the only language kiln speaks for
// it. It produces signed artifacts — that is `publish:`, and everything about
// it is shaped by the need to keep the provenance claim honest. And then there
// is the rest: uploading a scan result, opening a remediation pull request,
// refreshing a docs site. Work that has to happen on a commit and produces
// nothing anyone will verify later.
//
// That third category is what this package is for, and it is deliberately the
// weakest of the three. A task cannot mint provenance, cannot add to what a
// run publishes, and cannot make an unsigned thing look signed. Its blast
// radius is a check that goes red and whatever the command itself did.
package task

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.klarlabs.de/kiln/internal/config"
	"go.klarlabs.de/kiln/internal/domain/isolation"
	"go.klarlabs.de/kiln/internal/infrastructure/execx"
)

// Request is one task to run.
type Request struct {
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

// Result is what a task did.
type Result struct {
	Name     string
	Duration time.Duration
	// Err is the command's failure, if any. A task with allow_failure set
	// still records it here — the run does not fail, but the record must not
	// pretend nothing happened.
	Err error
	// Tolerated reports that Err was suppressed by allow_failure.
	Tolerated bool
}

// OK reports whether the task passed, or failed in a way the pipeline accepts.
func (r Result) OK() bool { return r.Err == nil || r.Tolerated }

// Runner executes tasks.
type Runner struct {
	Exec execx.Runner
	// Shell is the interpreter. `sh` rather than `bash`, because a task that
	// runs on the build box should not depend on which box.
	Shell string
}

// New builds a runner.
func New(r execx.Runner) *Runner { return &Runner{Exec: r, Shell: "sh"} }

// ErrTaskFailed reports a task whose command exited non-zero.
var ErrTaskFailed = errors.New("task failed")

// Run executes one task and returns what happened.
//
// The command runs under `sh -euc`: -e so a script that fails halfway fails,
// rather than reporting success because its last line happened to be an echo,
// and -u so a typo'd variable is an error instead of an empty string quietly
// deleting the wrong directory.
func (t *Runner) Run(ctx context.Context, req Request) Result {
	started := time.Now()

	dir := req.Dir
	if req.Task.Workdir != "" {
		dir = filepath.Join(req.Dir, req.Task.Workdir)
	}

	shell := t.Shell
	if shell == "" {
		shell = "sh"
	}

	// The command is repository-authored, so a fork pull request that could
	// read the operator's environment would make every secret on the box a
	// matter of opening a pull request. The gate already runs this way; a task
	// is no more trustworthy than the gate.
	environ := os.Environ()
	if !req.Policy.Secrets {
		environ = execx.ScrubbedEnviron()
	}

	cmd := execx.Cmd{
		Name:   shell,
		Args:   []string{"-euc", req.Task.Run},
		Dir:    dir,
		Env:    append(append(environ, req.ServiceEnv...), t.env(req)...),
		Stdout: req.Output,
		Stderr: req.Output,
	}

	_, err := t.Exec.Run(ctx, cmd)

	result := Result{Name: req.Name, Duration: time.Since(started)}
	if err != nil {
		result.Err = fmt.Errorf("%w: %s: %w", ErrTaskFailed, req.Name, err)
		result.Tolerated = req.Task.AllowFailure
	}
	return result
}

// env exports what a task needs to know about the run it belongs to.
//
// Named KILN_ rather than GITHUB_ on purpose. A task that reads GITHUB_SHA
// would keep working when somebody moved it back into Actions and silently
// mean something subtly different — the merge commit rather than the head.
func (t *Runner) env(req Request) []string {
	return []string{
		"KILN_SHA=" + req.SHA,
		"KILN_REF=" + req.Ref,
		"KILN_EVENT=" + req.Event,
		"KILN_TASK=" + req.Name,
	}
}

// humanInterval prints a duration the way an operator wrote it. Go renders 24h
// as "24h0m0s", which is correct and reads like a bug in the config file.
func humanInterval(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return d.String()
	}
}

// Describe renders a task's routing for `kiln doctor`.
func Describe(name string, t config.Task) string {
	events := strings.Join(t.On, ", ")
	if t.Every.Std() > 0 {
		events += " every " + humanInterval(t.Every.Std())
	}
	line := fmt.Sprintf("%s on %s", name, events)
	if pr := t.PullRequest; pr != nil {
		line += fmt.Sprintf(" → pull request on %s", pr.Branch)
	}
	if t.AllowFailure {
		line += " (failure tolerated)"
	}
	return line
}
