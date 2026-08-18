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

	"go.klarlabs.de/kiln/internal/attest"
	"go.klarlabs.de/kiln/internal/checks"
	"go.klarlabs.de/kiln/internal/config"
	"go.klarlabs.de/kiln/internal/github"
	"go.klarlabs.de/kiln/internal/isolation"
	"go.klarlabs.de/kiln/internal/obs"
	"go.klarlabs.de/kiln/internal/prove"
	"go.klarlabs.de/kiln/internal/provenance"
	"go.klarlabs.de/kiln/internal/publish"
	"go.klarlabs.de/kiln/internal/run"
	"go.klarlabs.de/kiln/internal/service"
	"go.klarlabs.de/kiln/internal/store"
	"go.klarlabs.de/kiln/internal/task"
	"go.klarlabs.de/kiln/internal/version"
	"go.klarlabs.de/kiln/internal/worktree"
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
	// ServiceEnv is the addresses of the running service containers, exported
	// to the gate and to tasks. Set by the engine, not by callers.
	ServiceEnv []string
}

// SourceAttester produces the source gate's own attestation for a commit.
//
// Separate from Verifier because they answer different questions and a
// deployment may want one without the other: Verifier decides whether kiln can
// skip re-proving, this carries warden's verdict to whoever ends up holding
// the artifact.
type SourceAttester interface {
	SourceAttestation(ctx context.Context, repoDir, sha string) ([]byte, error)
}

// Engine runs the phase sequence.
type Engine struct {
	Prover prove.Prover
	// Publisher builds images; ReleasePublisher builds binary releases. Two
	// fields rather than a registry because there are two kinds, and a map
	// would be indirection without a second caller to justify it.
	Publisher        publish.Publisher
	ReleasePublisher publish.Publisher
	Provenance       provenance.Verifier
	// SourceAttester carries warden's verification summary onto the artifact.
	// Nil publishes build provenance alone.
	SourceAttester SourceAttester
	Checks         checks.Reporter
	// Tasks runs the pipeline's automation. Nil disables tasks entirely, which
	// is what a caller that only wants prove-and-publish gets by default.
	Tasks *task.Runner
	// GitHub opens the pull requests tasks propose. Nil pushes the branch and
	// stops there, which is the right behaviour on a box with no token: the
	// work is not thrown away, it just needs a human to notice it.
	GitHub *github.Client
	Store  store.Store
	Log    obs.Logger
	// ToolVersions pins the components whose behaviour affected the result,
	// for the provenance predicate. Empty is acceptable — an unknown version
	// is better recorded as absent than guessed.
	ToolVersions map[string]string
	// Services starts the containers a gate needs beside it. Nil disables
	// them, which is what a pipeline with no services: block gets anyway.
	Services *service.Runner
	// KeepRoot is where retained task output is written, normally the .kiln
	// directory beside the ledger. Empty disables retention.
	KeepRoot string
	// KeepRuns bounds how many runs of retained output survive. Zero uses the
	// default; a box that keeps every artifact forever fills its disk, and the
	// first symptom is an unrelated build failing.
	KeepRuns int
	// PhaseTimeout bounds each phase separately. Zero means unbounded.
	//
	// Per phase rather than per run, because the phases fail differently: a
	// gate that hangs and a registry that stops answering mid-push are
	// separate incidents, and one budget covering both would let a slow gate
	// eat the publish's headroom.
	PhaseTimeout time.Duration
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

	// Services come up before the gate and go down after the tasks, whatever
	// happened in between. The gate is the thing that usually needs them —
	// a test suite talking to postgres — so starting them after it would
	// serve nobody.
	services, err := e.startServices(ctx, req, r, log)
	if err != nil {
		r.Fail(err)
		e.persist(r, log)
		return r, err
	}
	defer services.Stop()

	req.ServiceEnv = services.Env()

	gate, err := e.doProve(ctx, req, r, policy, log)
	if err != nil {
		r.Fail(err)
		e.persist(r, log)
		return r, err
	}

	if err := e.doPublish(ctx, req, r, policy, gate, log); err != nil {
		r.Fail(err)
		e.persist(r, log)
		return r, err
	}

	// Tasks run last, after the artifact exists. Most of them are about a
	// build that happened — uploading its scan, announcing its release — and
	// the ones that are not lose nothing by waiting. Running them before
	// publish would also mean an automation failure could stop an artifact
	// that was otherwise ready to ship.
	if err := e.doTasks(ctx, req, r, policy, log); err != nil {
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

// ErrPhaseTimeout reports a phase that ran out of time.
//
// Distinct from a phase that failed, because the responses differ: a failing
// gate means fix the code, a timing-out one means look at the machine. A run
// that reported both the same way would send an operator to the wrong place.
var ErrPhaseTimeout = errors.New("phase timed out")

// withPhaseTimeout bounds one phase.
func (e *Engine) withPhaseTimeout(ctx context.Context, phase string, fn func(context.Context) error) error {
	if e.PhaseTimeout <= 0 {
		return fn(ctx)
	}

	phaseCtx, cancel := context.WithTimeout(ctx, e.PhaseTimeout)
	defer cancel()

	err := fn(phaseCtx)
	// The caller's own cancellation — Ctrl-C, a daemon shutdown — is not a
	// timeout and must not be reported as one.
	if errors.Is(phaseCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		return fmt.Errorf("%w: %s exceeded %s (raise KILN_PHASE_TIMEOUT, or find what is hanging): %w",
			ErrPhaseTimeout, phase, e.PhaseTimeout, err)
	}
	return err
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
//
// It returns the provenance verdict as well as the error, because the publish
// phase has to state in the attestation whether this build ran the checks or
// inherited them — a fact only this phase knows.
func (e *Engine) doProve(
	ctx context.Context, req Request, r *run.Run, policy isolation.Policy, log obs.Logger,
) (provenance.Result, error) {
	if !req.Pipeline.Wants(req.Event.String(), config.StepProve) {
		// A pipeline that routes an event to nothing is a pipeline that wants
		// nothing done. It is not an error, but it is worth saying out loud:
		// silence here looks identical to a broken trigger.
		log.Info("prove not routed for this event", "event", req.Event.String())
		return provenance.Result{Decision: provenance.Reprove, Reason: "prove not routed for this event"}, nil
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
		err = e.withPhaseTimeout(ctx, "prove", func(ctx context.Context) error {
			return e.Prover.Prove(ctx, prove.Request{
				RepoDir:    req.Dir,
				SHA:        req.SHA,
				Policy:     policy,
				Nox:        req.Pipeline.Prove.Nox,
				Output:     req.Output,
				ServiceEnv: req.ServiceEnv,
			})
		})
	}

	conclusion, title, summary := checks.ProveSummary(r.Skipped, verdict.Reason, err)
	e.report(ctx, func() error {
		return e.Checks.Complete(ctx, checks.NameProve, req.SHA, conclusion, title, summary)
	}, log, checks.NameProve)

	if err != nil {
		log.Error("prove failed", "err", err)
		return verdict, err
	}
	return verdict, nil
}

// doPublish produces every artifact the pipeline routes to this event — if,
// and only if, the policy also allows it.
func (e *Engine) doPublish(
	ctx context.Context, req Request, r *run.Run, policy isolation.Policy,
	gate provenance.Result, log obs.Logger,
) error {
	wanted := req.Pipeline.ArtifactsFor(req.Event.String())

	// The overrule, stated plainly. A caller asking to publish on a fork pull
	// request is not an error to reject loudly — an automated surface may
	// simply have passed the pipeline through — but it must be visible in the
	// log, or an operator will spend an afternoon wondering where their image
	// went.
	if len(wanted) > 0 && !policy.Publish {
		log.Warn("publish suppressed by isolation policy",
			"artifacts", len(wanted), "event", req.Event.String(), "fork", req.Fork,
			"why", "a pull request head is a proposal, not a release; RollOps deploys from branches and tags")
		return nil
	}
	if len(wanted) == 0 || !policy.Publish {
		return nil
	}

	r.Phase = run.PhasePublishing
	e.persist(r, log)
	e.report(ctx, func() error { return e.Checks.Start(ctx, checks.NamePublish, req.SHA) }, log, checks.NamePublish)

	produced, err := e.publishAll(ctx, req, r, wanted,
		e.provenanceInput(req, r, policy, gate), e.sourceSummary(ctx, req, log), log)

	conclusion, title, summary := checks.PublishSummary(produced, err)
	e.report(ctx, func() error {
		return e.Checks.Complete(ctx, checks.NamePublish, req.SHA, conclusion, title, summary)
	}, log, checks.NamePublish)

	if err != nil {
		log.Error("publish failed", "err", err)
		return err
	}
	return nil
}

// publishAll runs each artifact's publisher in order, stopping at the first
// failure.
//
// Stopping rather than continuing is deliberate. The artifacts of one commit
// are a set, not independent jobs: a release whose image built but whose
// binaries did not is a half-published version, and carrying on would hide
// which half is missing behind a second, unrelated error. Whatever succeeded
// before the failure is still recorded, so the operator can see how far it got.
func (e *Engine) publishAll(
	ctx context.Context, req Request, r *run.Run, artifacts []config.Artifact,
	prov attest.Input, sourceVSA []byte, log obs.Logger,
) ([]run.Artifact, error) {
	produced := make([]run.Artifact, 0, len(artifacts))

	for i, artifact := range artifacts {
		publisher := e.publisherFor(artifact)
		if publisher == nil {
			return produced, fmt.Errorf("engine: no publisher for artifact kind %q", artifact.Kind)
		}

		var res publish.Result
		// Each artifact gets its own budget. A release that cross-compiles
		// four targets should not have its clock started by the image build
		// that preceded it.
		err := e.withPhaseTimeout(ctx, "publish "+string(artifact.Kind), func(ctx context.Context) error {
			var pubErr error
			res, pubErr = publisher.Publish(ctx, publish.Request{
				RepoDir:    req.Dir,
				SHA:        req.SHA,
				Ref:        req.Ref,
				Artifact:   artifact,
				Provenance: prov,
				SourceVSA:  sourceVSA,
				Output:     req.Output,
			})
			return pubErr
		})
		if err != nil {
			return produced, fmt.Errorf("publish[%d] (%s): %w", i, artifact.Kind, err)
		}

		entry := run.Artifact{
			Kind:      string(res.Kind),
			Reference: res.Reference,
			Digest:    res.Digest,
			Names:     res.Tags,
			Signed:    res.Signed,
			Attested:  res.Attested,
		}
		r.AddArtifact(entry)
		produced = append(produced, entry)
		e.persist(r, log)

		log.Info("published",
			"kind", string(res.Kind), "reference", res.Reference,
			"digest", res.Digest, "signed", res.Signed, "attested", res.Attested)
	}
	return produced, nil
}

// sourceSummary fetches warden's verdict for the commit, once per run.
//
// Best-effort. A repository still adopting warden, or a commit whose note has
// not been written, publishes build provenance without the source half rather
// than failing — refusing would make adoption all-or-nothing. The absence is
// logged, because "no source summary attached" is something an operator
// enforcing one downstream needs to be able to find out about here rather than
// at deploy time.
func (e *Engine) sourceSummary(ctx context.Context, req Request, log obs.Logger) []byte {
	if e.SourceAttester == nil {
		return nil
	}
	vsa, err := e.SourceAttester.SourceAttestation(ctx, req.Dir, req.SHA)
	if err != nil {
		log.Warn("no source summary to attach", "err", err)
		return nil
	}
	return vsa
}

// provenanceInput assembles the run-level facts every artifact's attestation
// shares. The publisher adds the subject, which is the only part that is not
// known until the artifact exists.
func (e *Engine) provenanceInput(
	req Request, r *run.Run, policy isolation.Policy, gate provenance.Result,
) attest.Input {
	return attest.Input{
		Repo:  req.Repo,
		SHA:   req.SHA,
		Ref:   req.Ref,
		Event: req.Event.String(),
		// A build that could not see the operator's credentials is a
		// materially different build, and a reader deciding what to trust
		// should not have to infer it from the event name.
		Isolated:     !policy.Secrets,
		GateTool:     "warden",
		GateReproved: !r.Skipped,
		GateReason:   gate.Reason,
		KilnVersion:  version.Version,
		ToolVersions: e.ToolVersions,
		InvocationID: r.ID,
		StartedOn:    r.StartedAt,
	}
}

// publisherFor selects the publisher for an artifact kind. A nil result is a
// configuration kiln cannot honour, which the caller turns into a failure
// rather than a silent no-op.
func (e *Engine) publisherFor(a config.Artifact) publish.Publisher {
	switch a.Kind {
	case config.KindBinaries:
		return e.ReleasePublisher
	case config.KindImage:
		return e.Publisher
	default:
		return nil
	}
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
// RunScheduled executes the tasks a schedule is due to fire.
//
// A separate entry point from Execute rather than a fourth event, because a
// scheduled run is a different animal: there is no new commit, nothing is
// proven, and nothing is published. It runs named errands against the head of
// a branch. Squeezing it into the event model would mean answering "may a
// schedule publish?" — and the honest answer, "no, because a schedule is not
// evidence that anything changed", is better expressed by not offering it.
func (e *Engine) RunScheduled(ctx context.Context, req Request, tasks []config.NamedTask) (*run.Run, error) {
	r := run.New(req.SHA, req.Ref, config.ScheduleEvent, false, req.Repo)
	log := e.Log.With("run", r.ID, "sha", run.ShortSHA(req.SHA), "event", config.ScheduleEvent)

	if e.Tasks == nil || len(tasks) == 0 {
		r.Succeed()
		return r, nil
	}
	if strings.TrimSpace(req.SHA) == "" {
		err := errors.New("engine: no commit to run scheduled tasks against")
		r.Fail(err)
		e.persist(r, log)
		return r, err
	}

	r.Phase = run.PhaseTasks
	e.persist(r, log)
	log.Info("scheduled tasks", "count", len(tasks))

	// A schedule fires on the tracked ref of the operator's own repository —
	// never a fork head — so the tasks get the trusted policy. The publish
	// bit is off regardless: RunScheduled never publishes.
	policy := isolation.Policy{Secrets: true, Skip: true}

	err := worktree.With(ctx, e.Tasks.Exec, req.Dir, req.SHA, func(dir string) error {
		return e.runTasks(ctx, req, r, policy, tasks, dir, log)
	})
	if err != nil {
		r.Fail(err)
		e.persist(r, log)
		return r, err
	}

	r.Succeed()
	e.persist(r, log)
	return r, nil
}

// startServices brings up the pipeline's service containers.
func (e *Engine) startServices(
	ctx context.Context, req Request, r *run.Run, log obs.Logger,
) (*service.Set, error) {
	if e.Services == nil || len(req.Pipeline.Services) == 0 {
		return &service.Set{}, nil
	}
	log.Info("starting services", "count", len(req.Pipeline.Services))

	set, err := e.Services.Start(ctx, req.Pipeline.Services, r.ID)
	if err != nil {
		return &service.Set{}, fmt.Errorf("services: %w", err)
	}
	return set, nil
}

// doTasks runs every task routed to this event.
//
// Unlike publish, one failure does not stop the rest. The artifacts of a
// commit are a set and a half-published release is incoherent; tasks are
// independent errands, and refusing to upload a scan because a docs build
// broke would just hide the second problem behind the first. Every task
// reports its own check, and the run fails if any intolerable one did.
func (e *Engine) doTasks(
	ctx context.Context, req Request, r *run.Run, policy isolation.Policy, log obs.Logger,
) error {
	if e.Tasks == nil {
		return nil
	}
	wanted := req.Pipeline.TasksFor(req.Event.String())
	if len(wanted) == 0 {
		return nil
	}

	r.Phase = run.PhaseTasks
	e.persist(r, log)

	// One disposable checkout for the whole set, pinned to the commit.
	//
	// Not the operator's working copy, for the same reason prove and publish
	// are not: a task runs repository-authored commands, and an uncommitted
	// edit sitting in that checkout would silently become part of what the
	// task saw — or, for a task that writes, part of what the operator finds
	// afterwards. One tree for all tasks rather than one each: they belong to
	// the same commit, and a task that leaves a file behind for the next one
	// is a legitimate thing to want.
	// The task runner's own execer, rather than a second one on the engine:
	// there is exactly one subprocess seam in this path and duplicating it
	// would mean a test could stub one and not the other.
	return worktree.With(ctx, e.Tasks.Exec, req.Dir, req.SHA, func(dir string) error {
		return e.runTasks(ctx, req, r, policy, wanted, dir, log)
	})
}

// runTasks executes the routed tasks inside an already-prepared worktree.
func (e *Engine) runTasks(
	ctx context.Context, req Request, r *run.Run, policy isolation.Policy,
	wanted []config.NamedTask, dir string, log obs.Logger,
) error {
	var failed []string
	for _, nt := range wanted {
		e.report(ctx, func() error {
			return e.Checks.Start(ctx, checks.TaskName(nt.Name), req.SHA)
		}, log, checks.TaskName(nt.Name))

		var output strings.Builder
		result := task.Result{}
		err := e.withPhaseTimeout(ctx, "task "+nt.Name, func(ctx context.Context) error {
			result = e.Tasks.Run(ctx, task.Request{
				Name: nt.Name, Task: nt.Task,
				Dir: dir, SHA: req.SHA, Ref: req.Ref, Event: req.Event.String(),
				Policy: policy, ServiceEnv: req.ServiceEnv,
				// Tee: the operator watching a terminal sees it live, and the
				// check body gets the same text without a second run.
				Output: io.MultiWriter(&output, orDiscard(req.Output)),
			})
			return result.Err
		})
		// A timeout is the phase's error, not the command's, and it must reach
		// the check body — otherwise a task killed at the deadline reports
		// only whatever it had printed before it hung.
		if err != nil && result.Err == nil {
			result.Err = err
			result.Tolerated = nt.Task.AllowFailure
		}

		// Retention runs whether the task passed or failed — especially when
		// it failed. The log that explains a failure is the thing somebody
		// wants after the worktree is gone, and keeping it only on success
		// would withhold it in exactly the case it matters.
		if len(nt.Task.Keep) > 0 && e.KeepRoot != "" {
			kept, kerr := task.Keep(dir, task.KeepDir(e.KeepRoot, r.ID, nt.Name), nt.Task.Keep)
			for _, f := range kept {
				fmt.Fprintf(&output, "kept %s (%d bytes)\n", f.Name, f.Bytes)
			}
			if kerr != nil {
				// Not fatal to the task: the command did its job. But it is
				// reported, because retention that quietly kept nothing looks
				// identical to a task that produced nothing.
				fmt.Fprintf(&output, "retention: %v\n", kerr)
				log.Warn("could not keep task output", "task", nt.Name, "err", kerr)
			}
		}

		// Proposing runs only for a task that succeeded. Committing whatever a
		// failed remediation left behind would open a pull request full of a
		// half-applied fix, which is worse than no pull request.
		if spec := nt.Task.PullRequest; spec != nil && result.Err == nil {
			proposal, perr := e.Tasks.Propose(ctx, task.Request{
				Name: nt.Name, Task: nt.Task, Dir: dir, SHA: req.SHA,
				Ref: req.Ref, Event: req.Event.String(), Policy: policy,
			}, *spec, e.forge())
			if perr != nil {
				result.Err = perr
				result.Tolerated = nt.Task.AllowFailure
			}
			fmt.Fprintf(&output, "\n%s\n", proposal.Summary())
			log.Info("task proposal", "task", nt.Name, "outcome", proposal.Summary())
		}

		conclusion, title, summary := checks.TaskSummary(result.Err, result.Tolerated, output.String())
		e.report(ctx, func() error {
			return e.Checks.Complete(ctx, checks.TaskName(nt.Name), req.SHA, conclusion, title, summary)
		}, log, checks.TaskName(nt.Name))

		r.Tasks = append(r.Tasks, run.Task{
			Name: nt.Name, OK: result.OK(), Tolerated: result.Tolerated,
			Duration: result.Duration.String(),
		})

		switch {
		case result.Err == nil:
			log.Info("task passed", "task", nt.Name, "duration", result.Duration.String())
		case result.Tolerated:
			log.Warn("task failed, tolerated", "task", nt.Name, "err", result.Err)
		default:
			log.Error("task failed", "task", nt.Name, "err", result.Err)
			failed = append(failed, nt.Name)
		}
	}

	if e.KeepRoot != "" {
		keep := e.KeepRuns
		if keep <= 0 {
			keep = DefaultKeepRuns
		}
		if err := task.Sweep(e.KeepRoot, keep); err != nil {
			// Housekeeping never fails a run: that would be the disk-space
			// protection causing the outage it exists to prevent.
			log.Warn("could not sweep old task output", "err", err)
		}
	}

	if len(failed) > 0 {
		return fmt.Errorf("%w: %s", task.ErrTaskFailed, strings.Join(failed, ", "))
	}
	return nil
}

// DefaultKeepRuns is how many runs of retained task output are kept.
const DefaultKeepRuns = 20

// forge returns the pull-request opener, or nil when there is no usable
// token — a typed nil in an interface would satisfy `!= nil` and then panic on
// the first call, which is the classic version of this bug.
func (e *Engine) forge() task.Forge {
	if e.GitHub == nil || !e.GitHub.Enabled() {
		return nil
	}
	return forge{client: e.GitHub}
}

// forge adapts the GitHub client to what proposing needs.
//
// A narrow interface declared by the consumer, so the task package does not
// import the whole client and a test can supply four lines instead of an HTTP
// server.
type forge struct{ client *github.Client }

func (f forge) OpenPullRequest(ctx context.Context, head, base, title, body string) (int, bool, error) {
	pull, opened, err := f.client.OpenPullRequest(ctx, head, base, title, body)
	return pull.Number, opened, err
}

func (f forge) LabelPull(ctx context.Context, number int, labels []string) error {
	return f.client.LabelPull(ctx, number, labels)
}

// orDiscard keeps io.MultiWriter from panicking on a nil writer, which is what
// a caller with no terminal passes.
func orDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}

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
