// Package engine orchestrates one run.
//
// Every surface — CLI, MCP, HTTP, webhook — reduces to a Request and calls
// Execute. There is exactly one implementation of the phase sequence, so a run
// triggered by an agent and a run triggered by cron cannot diverge.
//
// The engine's central rule: the *caller states intent, the policy decides*.
// A caller can ask for a publish on a fork pull request; it will not get one.
// That inversion is deliberate — every new surface added later inherits the
// isolation guarantees for free, because it cannot express a way around them.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"go.klarlabs.de/kiln/internal/checks"
	"go.klarlabs.de/kiln/internal/config"
	"go.klarlabs.de/kiln/internal/isolation"
	"go.klarlabs.de/kiln/internal/obs"
	"go.klarlabs.de/kiln/internal/prove"
	"go.klarlabs.de/kiln/internal/provenance"
	"go.klarlabs.de/kiln/internal/publish"
	"go.klarlabs.de/kiln/internal/run"
	"go.klarlabs.de/kiln/internal/store"
)

// Request is what a surface asks for.
type Request struct {
	// SHA is the commit to build. Already resolved: the engine does not read
	// git refs, so "HEAD" must be turned into an object id before it gets here.
	SHA string
	// Event and Fork are the isolation inputs.
	Event isolation.Event
	Fork  bool
	// Ref is the ref the commit was discovered on, e.g. refs/heads/main or
	// refs/tags/v1.2.0. It decides the semver tag and scopes the watch
	// already-built check.
	Ref string
	// Repo is owner/name, for the ledger.
	Repo string
	// Dir is the repository to work in.
	Dir string
	// Pipeline is the loaded `.kiln.yaml`.
	Pipeline config.Pipeline
	// Output, when set, streams subprocess output to a terminal.
	Output io.Writer
}

// Engine runs the phase sequence.
type Engine struct {
	Prover     prove.Prover
	Publisher  publish.Publisher
	Provenance provenance.Verifier
	Checks     checks.Reporter
	Store      store.Store
	Log        obs.Logger
}

// New builds an engine, defaulting the optional collaborators so a caller that
// only wires the essentials still gets a working, silent engine.
func New(e Engine) *Engine {
	if e.Checks == nil {
		e.Checks = checks.Noop{}
	}
	if e.Store == nil {
		e.Store = store.NewMemory()
	}
	if e.Log == nil {
		e.Log = obs.Discard()
	}
	if e.Provenance == nil {
		e.Provenance = provenance.Always{Reason: "no provenance verifier configured"}
	}
	return &e
}

// Execute runs one request to completion.
//
// It returns the persisted run and, separately, the failure. Both are always
// meaningful: a failed run is still a record worth storing and worth showing,
// so callers should read the *Run even when err is non-nil.
func (e *Engine) Execute(ctx context.Context, req Request) (*run.Run, error) {
	r := run.New(req.SHA, req.Ref, req.Event.String(), req.Fork, req.Repo)
	log := e.Log.With("run", r.ID, "sha", run.ShortSHA(req.SHA), "event", req.Event.String())

	if err := validate(req); err != nil {
		r.Fail(err)
		e.persist(r, log)
		return r, err
	}

	r.Phase = run.PhaseIsolating
	policy := isolation.For(req.Event, req.Fork)
	log.Info("run started",
		"ref", req.Ref, "fork", req.Fork,
		"secrets", policy.Secrets, "may_publish", policy.Publish, "may_skip", policy.Skip)
	e.persist(r, log)

	if err := e.doProve(ctx, req, r, policy, log); err != nil {
		r.Fail(err)
		e.persist(r, log)
		return r, err
	}

	if err := e.doPublish(ctx, req, r, policy, log); err != nil {
		r.Fail(err)
		e.persist(r, log)
		return r, err
	}

	r.Succeed()
	e.persist(r, log)
	log.Info("run succeeded",
		"skipped_prove", r.Skipped, "digest", r.Digest, "duration", r.Duration().String())
	return r, nil
}

func validate(req Request) error {
	if strings.TrimSpace(req.SHA) == "" {
		return errors.New("engine: no commit to build")
	}
	if !req.Event.Valid() {
		return fmt.Errorf("engine: unknown event %q (want pull_request, push or tag)", req.Event)
	}
	if strings.TrimSpace(req.Dir) == "" {
		return errors.New("engine: no repository directory")
	}
	return nil
}

// doProve gates the commit, or establishes that a trusted note already did.
func (e *Engine) doProve(ctx context.Context, req Request, r *run.Run, policy isolation.Policy, log obs.Logger) error {
	if !req.Pipeline.Wants(req.Event.String(), config.StepProve) {
		// A pipeline that routes an event to nothing is a pipeline that wants
		// nothing done. It is not an error, but it is worth saying out loud:
		// silence here looks identical to a broken trigger.
		log.Info("prove not routed for this event", "event", req.Event.String())
		return nil
	}

	r.Phase = run.PhaseProving
	e.persist(r, log)
	e.report(ctx, func() error { return e.Checks.Start(ctx, checks.NameProve, req.SHA) }, log, checks.NameProve)

	verdict := e.Provenance.Verify(ctx, req.Dir, req.SHA, policy)
	log.Info("provenance", "decision", string(verdict.Decision), "reason", verdict.Reason)

	var err error
	if verdict.Skip() {
		r.Skipped = true
	} else {
		err = e.Prover.Prove(ctx, prove.Request{
			RepoDir: req.Dir,
			SHA:     req.SHA,
			Policy:  policy,
			Nox:     req.Pipeline.Prove.Nox,
			Output:  req.Output,
		})
	}

	conclusion, title, summary := checks.ProveSummary(r.Skipped, verdict.Reason, err)
	e.report(ctx, func() error {
		return e.Checks.Complete(ctx, checks.NameProve, req.SHA, conclusion, title, summary)
	}, log, checks.NameProve)

	if err != nil {
		log.Error("prove failed", "err", err)
		return err
	}
	return nil
}

// doPublish builds and signs, if — and only if — both the pipeline and the
// policy allow it.
func (e *Engine) doPublish(ctx context.Context, req Request, r *run.Run, policy isolation.Policy, log obs.Logger) error {
	wanted := req.Pipeline.Wants(req.Event.String(), config.StepPublish)

	// The overrule, stated plainly. A caller asking to publish on a fork pull
	// request is not an error to reject loudly — an automated surface may
	// simply have passed the pipeline through — but it must be visible in the
	// log, or an operator will spend an afternoon wondering where their image
	// went.
	if wanted && !policy.Publish {
		log.Warn("publish suppressed by isolation policy",
			"event", req.Event.String(), "fork", req.Fork,
			"why", "a pull request head is a proposal, not a release; RollOps deploys from branches and tags")
		return nil
	}
	if !wanted || !policy.Publish {
		return nil
	}
	if req.Pipeline.Publish == nil {
		return errors.New("engine: publish is routed for this event but .kiln.yaml has no publish block")
	}

	plan, err := publish.BuildPlan(*req.Pipeline.Publish, req.SHA, req.Ref)
	if err != nil {
		e.report(ctx, func() error { return e.Checks.Start(ctx, checks.NamePublish, req.SHA) }, log, checks.NamePublish)
		conclusion, title, summary := checks.PublishSummary("", nil, false, err)
		e.report(ctx, func() error {
			return e.Checks.Complete(ctx, checks.NamePublish, req.SHA, conclusion, title, summary)
		}, log, checks.NamePublish)
		return err
	}

	r.Phase = run.PhasePublishing
	e.persist(r, log)
	e.report(ctx, func() error { return e.Checks.Start(ctx, checks.NamePublish, req.SHA) }, log, checks.NamePublish)

	res, pubErr := e.Publisher.Publish(ctx, publish.Request{
		RepoDir: req.Dir,
		SHA:     req.SHA,
		Plan:    plan,
		Output:  req.Output,
	})
	if pubErr == nil {
		r.Digest = res.Digest
		r.Tags = res.Tags
	}

	conclusion, title, summary := checks.PublishSummary(res.Reference, res.Tags, res.Signed, pubErr)
	e.report(ctx, func() error {
		return e.Checks.Complete(ctx, checks.NamePublish, req.SHA, conclusion, title, summary)
	}, log, checks.NamePublish)

	if pubErr != nil {
		log.Error("publish failed", "err", pubErr)
		return pubErr
	}
	log.Info("published", "digest", res.Digest, "tags", res.Tags, "signed", res.Signed)
	return nil
}

// report performs a best-effort Checks call.
//
// A run that built and signed a correct artifact must not be recorded as
// failed because GitHub was unreachable while Kiln tried to say so. The
// artifact is the deliverable; the Check is the announcement.
func (e *Engine) report(ctx context.Context, fn func() error, log obs.Logger, name string) {
	if err := fn(); err != nil {
		log.Warn("could not report to github", "check", name, "err", err)
	}
	_ = ctx
}

// persist saves the run, logging rather than failing on a storage error. The
// ledger is runtime bookkeeping; git is the desired state. Losing a write
// costs a duplicate build on the next tick, never correctness.
func (e *Engine) persist(r *run.Run, log obs.Logger) {
	if err := e.Store.Save(r); err != nil {
		log.Warn("could not persist run", "err", err)
	}
}

// AlreadyBuilt reports whether a successful run already covers this SHA on
// this ref. Watch uses it to stay idempotent across ticks.
//
// Ref-scoped, not SHA-scoped: a tag pointing at an already-built branch head
// still needs its own run, because the tag routes to different steps and
// produces a different moving tag.
func AlreadyBuilt(s store.Store, sha, ref string) bool {
	if s == nil {
		return false
	}
	prior, err := s.LastSuccess(sha, ref)
	return err == nil && prior != nil
}

// StaleAfter is how long an in-progress run may sit before a reader should
// treat it as abandoned. `kiln run` is a one-shot process, so a run left in a
// non-terminal phase means the process died.
const StaleAfter = 6 * time.Hour
