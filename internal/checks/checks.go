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
	"strings"
	"sync"

	"go.klarlabs.de/kiln/internal/github"
	"go.klarlabs.de/kiln/internal/obs"
)

// The check-run names Kiln posts. Do not rename without a migration note.
const (
	NameProve   = "Kiln / Prove"
	NamePublish = "Kiln / Publish"
)

// Conclusion is a completed check's verdict, in GitHub's vocabulary.
type Conclusion string

const (
	Success Conclusion = "success"
	Failure Conclusion = "failure"
	Neutral Conclusion = "neutral"
	Skipped Conclusion = "skipped"
)

// Reporter posts check runs.
//
// Every method is best-effort from the engine's point of view: a run that
// built and signed a correct artifact must not be recorded as failed because
// GitHub was unreachable while Kiln tried to say so. The engine logs reporting
// errors and carries on.
type Reporter interface {
	// Start opens an in-progress check run for a phase.
	Start(ctx context.Context, name, sha string) error
	// Complete closes it with a verdict.
	Complete(ctx context.Context, name, sha string, conclusion Conclusion, title, summary string) error
}

// GitHub is the real reporter.
type GitHub struct {
	Client *github.Client
	Log    obs.Logger

	// runs maps name+sha to the check-run id opened by Start, so Complete can
	// close the right one. It is per-process: a `kiln run` that crashes leaves
	// an in-progress check behind, which is the honest state — nothing
	// concluded, because nothing finished.
	mu   sync.Mutex
	runs map[string]int64
}

// NewGitHub builds a reporter.
func NewGitHub(c *github.Client, log obs.Logger) *GitHub {
	if log == nil {
		log = obs.Discard()
	}
	return &GitHub{Client: c, Log: log, runs: map[string]int64{}}
}

func (g *GitHub) Start(ctx context.Context, name, sha string) error {
	if !g.Client.Enabled() {
		return nil
	}
	run, err := g.Client.CreateCheckRun(ctx, name, sha)
	if err != nil {
		return fmt.Errorf("checks: open %q: %w", name, err)
	}
	g.mu.Lock()
	g.runs[key(name, sha)] = run.ID
	g.mu.Unlock()
	g.Log.Debug("check opened", "check", name, "sha", sha, "id", int(run.ID))
	return nil
}

func (g *GitHub) Complete(ctx context.Context, name, sha string, c Conclusion, title, summary string) error {
	if !g.Client.Enabled() {
		return nil
	}

	g.mu.Lock()
	id, ok := g.runs[key(name, sha)]
	g.mu.Unlock()

	if !ok {
		// Start failed or was never called — a transient API error at the
		// beginning of the run. Opening a check just to conclude it is better
		// than losing the verdict entirely.
		created, err := g.Client.CreateCheckRun(ctx, name, sha)
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

// Noop reports nowhere.
//
// This is what a run without GITHUB_TOKEN gets. It is a deliberate, quiet
// degradation: `kiln run` on a laptop should gate a commit and print the
// result, not fail because it could not tell GitHub about it.
type Noop struct{}

func (Noop) Start(context.Context, string, string) error { return nil }
func (Noop) Complete(context.Context, string, string, Conclusion, string, string) error {
	return nil
}

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
	Conclusion Conclusion
	Title      string
	Summary    string
}

func (r *Recording) Start(_ context.Context, name, sha string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Events = append(r.Events, Event{Name: name, SHA: sha, Started: true})
	return nil
}

func (r *Recording) Complete(_ context.Context, name, sha string, c Conclusion, title, summary string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Events = append(r.Events, Event{
		Name: name, SHA: sha, Conclusion: c, Title: title, Summary: summary,
	})
	return nil
}

// Conclusions returns the verdict recorded for a check, and whether one was.
func (r *Recording) Conclusions(name string) (Conclusion, bool) {
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

// Failing wraps a Reporter and returns an error from every call, so a test can
// assert that reporting trouble does not change a run's verdict.
type Failing struct{ Err error }

func (f Failing) Start(context.Context, string, string) error { return f.err() }
func (f Failing) Complete(context.Context, string, string, Conclusion, string, string) error {
	return f.err()
}
func (f Failing) err() error {
	if f.Err != nil {
		return f.Err
	}
	return errors.New("checks unavailable")
}

// ProveSummary renders the body of the prove check.
func ProveSummary(skipped bool, reason string, err error) (Conclusion, string, string) {
	switch {
	case err != nil:
		return Failure, "gate failed", "```\n" + strings.TrimSpace(err.Error()) + "\n```"
	case skipped:
		// A skipped gate concludes `success`, not `skipped`: the commit *is*
		// gated — by the note Warden signed — and a branch protection rule
		// waiting on this check must be satisfied. The summary says how.
		return Success, "gate satisfied by warden provenance", reason
	default:
		return Success, "gate passed", reason
	}
}

// PublishSummary renders the body of the publish check.
func PublishSummary(reference string, tags []string, signed bool, err error) (Conclusion, string, string) {
	if err != nil {
		return Failure, "publish failed", "```\n" + strings.TrimSpace(err.Error()) + "\n```"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "digest `%s`\n\n", reference)
	for _, tag := range tags {
		fmt.Fprintf(&b, "- `%s`\n", tag)
	}
	if signed {
		b.WriteString("\nSigned with cosign. RollOps can pin this digest.\n")
		return Success, "published and signed", b.String()
	}
	// A dry run must never read as a real artifact on a pull request page.
	b.WriteString("\nDry run: nothing was pushed or signed.\n")
	return Neutral, "dry run", b.String()
}
