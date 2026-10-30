package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.klarlabs.de/kiln/internal/application/ports"
	"go.klarlabs.de/kiln/internal/domain/config"
	"go.klarlabs.de/kiln/internal/domain/isolation"
	"go.klarlabs.de/kiln/internal/domain/run"
	"go.klarlabs.de/kiln/internal/infrastructure/checks"
)

// scriptedTasks answers each task by name with a canned error.
type scriptedTasks struct {
	errs map[string]error
}

func (s scriptedTasks) Run(_ context.Context, req ports.TaskRequest) ports.TaskResult {
	return ports.TaskResult{Name: req.Name, Err: s.errs[req.Name]}
}

func (scriptedTasks) Propose(context.Context, ports.TaskRequest, config.PullRequest, ports.PullProposer) (ports.Proposal, error) {
	return ports.Proposal{}, nil
}

func (scriptedTasks) Keep(string, string, []string) ([]ports.KeptFile, error) { return nil, nil }
func (scriptedTasks) KeepDir(root, runID, task string) string                 { return root }
func (scriptedTasks) Sweep(string, int) error                                 { return nil }

type inPlace struct{}

func (inPlace) With(_ context.Context, repoDir, _ string, fn func(string) error) error {
	return fn(repoDir)
}

func (inPlace) Reap(context.Context, string, time.Duration) (int, error) { return 0, nil }

func withTasks(t *testing.T, h *harness, errs map[string]error) Request {
	t.Helper()
	h.engine.Tasks = scriptedTasks{errs: errs}
	h.engine.Worktrees = inPlace{}

	p, err := config.Parse(strings.NewReader(`
apiVersion: kiln.klarlabs.de/v1
kind: Pipeline
on:
  pull_request: [prove]
prove: {from: warden}
tasks:
  build: {on: [pull_request], run: bash scripts/ci/build.sh}
  lint:  {on: [pull_request], run: make lint}
`))
	if err != nil {
		t.Fatal(err)
	}
	r := req(t, isolation.EventPullRequest, false, "refs/pull/7/head")
	r.Pipeline = p
	return r
}

var errPredates = &ports.NotApplicableError{Task: "build", Path: "scripts/ci/build.sh"}

// The box failure this exists for: an old pull request branch predating the
// script a task runs. Counted as a failure and retried every hour — for a
// commit that can never grow the file. It is recorded as not applicable, the
// run does not fail, and the ledger settles it. The check stays red: skipped
// would satisfy a required check, and nothing ran.
func TestATaskTheCommitPredatesDoesNotFailTheRun(t *testing.T) {
	h := newHarness(t)
	r := withTasks(t, h, map[string]error{"build": errPredates})

	got, err := h.engine.Execute(t.Context(), r)
	if err != nil {
		t.Fatalf("Execute: %v — a commit that predates a script is not a failure", err)
	}
	if got.Phase != run.PhaseSucceeded {
		t.Errorf("phase = %s, want succeeded", got.Phase)
	}

	var check checks.Event
	for _, ev := range h.checks.Events {
		if ev.Name == ports.TaskName("build") && !ev.Started {
			check = ev
		}
	}
	if check.Conclusion != ports.ConclusionFailure {
		t.Errorf("build check = %v, want failure: skipped would satisfy a required check", check.Conclusion)
	}
	if !strings.HasPrefix(check.Title, "not applicable") {
		t.Errorf("build check title = %q, want it to say not applicable", check.Title)
	}

	var build run.Task
	for _, task := range got.Tasks {
		if task.Name == "build" {
			build = task
		}
	}
	if !build.NotApplicable || build.OK {
		t.Errorf("ledger task = %+v, want not applicable and not ok — it did not run", build)
	}

	// Not retried: the same commit will predate the same script next tick.
	if verdict, _ := Decide(h.store, r.SHA, r.Ref, time.Now(), h.engine.Inputs(r.Pipeline)); verdict != Built {
		t.Errorf("Decide = %v, want Built", verdict)
	}
}

// Not applicable must not launder a real failure in the same run.
func TestARealTaskFailureBesideANotApplicableOneStillFails(t *testing.T) {
	h := newHarness(t)
	r := withTasks(t, h, map[string]error{
		"build": errPredates,
		"lint":  fmt.Errorf("%w: lint: exit 2", ports.ErrTaskFailed),
	})

	got, err := h.engine.Execute(t.Context(), r)
	if !errors.Is(err, ports.ErrTaskFailed) {
		t.Fatalf("err = %v, want ErrTaskFailed", err)
	}
	if strings.Contains(err.Error(), "build") {
		t.Errorf("err = %v names the not-applicable task as failed", err)
	}
	if got.Phase != run.PhaseFailed {
		t.Errorf("phase = %s, want failed", got.Phase)
	}
}
