// Package checks reports Kiln's phases to GitHub Checks.
//
// Humans already have a UI: the pull request page. Kiln does not build a
// dashboard; it posts two check runs and lets GitHub render them.
//
// The two names below are a contract, not labels. Branch protection rules
// require checks *by name*, and RollOps' PR writeback waits on them. Renaming
// one silently unblocks every protected branch that was waiting for it, so a
// change here needs a migration note, not just a commit.
package checks

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"go.klarlabs.de/kiln/internal/application/ports"

	"go.klarlabs.de/kiln/internal/infrastructure/github"
	"go.klarlabs.de/kiln/internal/infrastructure/obs"
)

// The check-run names Kiln posts. Do not rename without a migration note.

// GitHub is the real reporter.
type GitHub struct {
	Client *github.Client
	Log    ports.Logger

	// runs maps name+sha to the check-run id opened by Start, so Complete can
	// close the right one. It is per-process: a `kiln run` that crashes leaves
	// an in-progress check behind, which is the honest state — nothing
	// concluded, because nothing finished.
	mu   sync.Mutex
	runs map[string]int64
	// statuses records that the Checks API refused this token, so the rest of
	// the process posts commit statuses without asking again.
	statuses bool
}

// statusesOnly reports whether this process has already learned that the token
// cannot create check runs.
func (g *GitHub) statusesOnly() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.statuses
}

func (g *GitHub) useStatuses() {
	g.mu.Lock()
	g.statuses = true
	g.mu.Unlock()
}

// statusState maps a check conclusion onto the four a commit status has.
//
// ports.ConclusionNeutral and skipped become success rather than failure: both mean "this did
// not go wrong", and a required context that reads failure would block a merge
// for something the pipeline deliberately did not do.
func statusState(c ports.Conclusion) string {
	switch c {
	case ports.ConclusionFailure:
		return "failure"
	default:
		return "success"
	}
}

// NewGitHub builds a reporter.
func NewGitHub(c *github.Client, log ports.Logger) *GitHub {
	if log == nil {
		log = obs.Discard()
	}
	return &GitHub{Client: c, Log: log, runs: map[string]int64{}}
}

func (g *GitHub) Start(ctx context.Context, name, sha string) error {
	if !g.Client.Enabled() {
		return nil
	}
	if g.statusesOnly() {
		return g.Client.CreateStatus(ctx, sha, "pending", name, "running")
	}

	run, err := g.Client.CreateCheckRun(ctx, name, sha)
	if errors.Is(err, github.ErrNeedsGitHubApp) {
		// The Checks API only accepts a GitHub App. A personal access token
		// gets one 403 and then this reporter switches to commit statuses for
		// the rest of the process — branch protection accepts either as a
		// required context, so the name still gates a merge.
		g.useStatuses()
		g.Log.Info("posting commit statuses instead of check runs",
			"why", "the checks api requires a github app; this token is not one")
		return g.Client.CreateStatus(ctx, sha, "pending", name, "running")
	}
	if err != nil {
		return fmt.Errorf("checks: open %q: %w", name, err)
	}
	g.mu.Lock()
	g.runs[key(name, sha)] = run.ID
	g.mu.Unlock()
	g.Log.Debug("check opened", "check", name, "sha", sha, "id", int(run.ID))
	return nil
}

func (g *GitHub) Complete(ctx context.Context, name, sha string, c ports.Conclusion, title, summary string) error {
	if !g.Client.Enabled() {
		return nil
	}

	if g.statusesOnly() {
		return g.Client.CreateStatus(ctx, sha, statusState(c), name, title)
	}

	g.mu.Lock()
	id, ok := g.runs[key(name, sha)]
	g.mu.Unlock()

	if !ok {
		// Start failed or was never called — a transient API error at the
		// beginning of the run. Opening a check just to conclude it is better
		// than losing the verdict entirely.
		created, err := g.Client.CreateCheckRun(ctx, name, sha)
		if errors.Is(err, github.ErrNeedsGitHubApp) {
			g.useStatuses()
			return g.Client.CreateStatus(ctx, sha, statusState(c), name, title)
		}
		if err != nil {
			return fmt.Errorf("checks: reopen %q to conclude it: %w", name, err)
		}
		id = created.ID
	}

	if err := g.Client.CompleteCheckRun(ctx, id, string(c), title, summary); err != nil {
		return fmt.Errorf("checks: conclude %q: %w", name, err)
	}
	g.Log.Debug("check completed", "check", name, "sha", sha, "conclusion", string(c))
	return nil
}

func key(name, sha string) string { return name + "@" + sha }

// Recording captures calls in memory, for tests and for `--dry-run`.
type Recording struct {
	mu     sync.Mutex
	Events []Event
}

// Event is one recorded call.
type Event struct {
	Name       string
	SHA        string
	Started    bool
	Conclusion ports.Conclusion
	Title      string
	Summary    string
}

func (r *Recording) Start(_ context.Context, name, sha string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Events = append(r.Events, Event{Name: name, SHA: sha, Started: true})
	return nil
}

func (r *Recording) Complete(_ context.Context, name, sha string, c ports.Conclusion, title, summary string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Events = append(r.Events, Event{
		Name: name, SHA: sha, Conclusion: c, Title: title, Summary: summary,
	})
	return nil
}

// Conclusions returns the verdict recorded for a check, and whether one was.
func (r *Recording) Conclusions(name string) (ports.Conclusion, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.Events) - 1; i >= 0; i-- {
		if r.Events[i].Name == name && !r.Events[i].Started {
			return r.Events[i].Conclusion, true
		}
	}
	return "", false
}

// Started reports whether a check was opened.
func (r *Recording) Started(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.Events {
		if e.Name == name && e.Started {
			return true
		}
	}
	return false
}

// Summary returns the summary body recorded for a check.
func (r *Recording) Summary(name string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.Events) - 1; i >= 0; i-- {
		if r.Events[i].Name == name && !r.Events[i].Started {
			return r.Events[i].Summary
		}
	}
	return ""
}

// Failing wraps a ports.Reporter and returns an error from every call, so a test can
// assert that reporting trouble does not change a run's verdict.
type Failing struct{ Err error }

func (f Failing) Start(context.Context, string, string) error { return f.err() }
func (f Failing) Complete(context.Context, string, string, ports.Conclusion, string, string) error {
	return f.err()
}
func (f Failing) err() error {
	if f.Err != nil {
		return f.Err
	}
	return errors.New("checks unavailable")
}
