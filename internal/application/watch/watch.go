// Package watch is unattended discovery.
//
// Kiln is daemon-less by default. There is no webhook to receive, no runner to
// register and nothing to keep alive: a cron entry runs `kiln watch --once`,
// which fetches, works out what is new, builds it, and exits. `--every` is the
// same tick in a loop, for an operator who would rather run a process than a
// crontab.
//
// Idempotence is what makes that safe. Every tick recomputes the whole set of
// interesting refs from scratch and drops the ones a successful run already
// covers, so a missed tick self-heals and a doubled tick is a no-op.
package watch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"go.klarlabs.de/kiln/internal/application/ports"

	"go.klarlabs.de/fortify/retry"

	"go.klarlabs.de/kiln/internal/application/engine"
	"go.klarlabs.de/kiln/internal/domain/config"
	"go.klarlabs.de/kiln/internal/domain/forge"
	"go.klarlabs.de/kiln/internal/domain/isolation"
	"go.klarlabs.de/kiln/internal/domain/run"
)

// PRRefNamespace is where pull request heads are parked locally. A private
// namespace rather than a branch: these are other people's commits, and they
// must never be mistaken for something this checkout is tracking.
const PRRefNamespace = "refs/kiln/pr/"

// Job is one unit of discovered work.
type Job struct {
	SHA   string
	Ref   string
	Event isolation.Event
	Fork  bool
	// Label is a short human name for the log and `--dry-run` output.
	Label string
}

// Forge is the part of the GitHub client discovery needs: which pull requests
// are open, and which of those come from a fork.
//
// Nil, or Enabled reporting false, means the question cannot be asked. That is
// not the same as "none are open", and the difference decides whether a pull
// ref that is missing from the answer is known-closed or merely unknown — see
// pullDecision.
type Forge interface {
	Enabled() bool
	ListOpenPulls(ctx context.Context) ([]forge.Pull, error)
}

// Watcher discovers and executes work.
type Watcher struct {
	Engine *engine.Engine
	Store  ports.Ledger
	// Forge answers which pull requests are open. An interface rather than
	// *github.Client because that is the whole question this package asks of
	// GitHub, and because a concrete client cannot be substituted in a test —
	// which is why the closed-pull-request logic had to be extracted as a pure
	// function to be testable at all. ports.PullProposer is the same idea.
	Forge Forge
	Log   ports.Logger
	// Output receives the subprocess output of every run a tick starts —
	// warden's step results, the image build. Nil keeps them quiet. On an
	// unattended box it is the only record of why a gate failed, so a
	// surface that has somewhere to write must set it.
	Output io.Writer

	// Dir is the repository to watch.
	Dir string
	// Pipeline supplies the watch configuration and the event routing.
	Pipeline config.Pipeline
	// Repo is owner/name for the ledger.
	Repo string
	// BranchesOnly restricts discovery to the tracked branch. `kiln poll` sets
	// it; it is the subset of watch that needs no GitHub token at all.
	BranchesOnly bool
	// FetchAttempts bounds the per-fetch retry. Zero uses the default.
	FetchAttempts int
	// Schedule remembers when each scheduled task last fired. Nil disables
	// scheduled tasks, which is what a caller with no persistent directory —
	// a test, a dry run — should get.
	Schedule ports.Schedule
	// Now is the clock, injectable so a test can state what "tomorrow" means
	// rather than sleep through it.
	Now func() time.Time
	// Git answers the questions discovery asks of the repository. The
	// application never names a git command: which format string produces a
	// peeled annotated tag is not a decision it makes.
	Git ports.Git
	// Locks serialises this repository against another kiln — a box ticking
	// while an operator runs one by hand in the same checkout.
	Locks ports.Locks
	// Worktrees reaps checkouts a dying process left behind. Nil disables the
	// sweep, which is what a caller with no repository — a test — should get.
	Worktrees ports.Worktrees
	// Pruner reclaims docker disk each tick. Nil disables it.
	Pruner ports.Pruner
	// BuildCacheMaxAge is passed through to the pruner.
	BuildCacheMaxAge time.Duration
	// Slots bounds concurrent builds across every repository on the machine.
	// Nil means no limit, which is what a test should get.
	Slots ports.BuildSlots
	// Health answers whether the box can build at all. Nil skips the checks,
	// which is what a test that is not about them should get.
	Health ports.BoxHealth
	// MinFree is the free space below which a tick builds nothing. Zero
	// disables the disk check.
	MinFree uint64
	// PlanImages mirrors KILN_DRY: images are planned rather than built, so
	// they need no docker daemon.
	PlanImages bool
}

// Result summarises one tick.
type Result struct {
	// Discovered is every job the tick found, before filtering.
	Discovered []Job
	// Skipped are the jobs a successful run already covers.
	Skipped []Job
	// Executed are the jobs that ran, with their outcomes.
	Executed []Outcome
	// Deferred are the jobs left for the next tick because the box could not
	// run them now: every machine build slot was taken, or they need docker
	// and the daemon is not answering. They are not failures and nothing is
	// recorded against them, so a later tick builds them.
	Deferred []Job
}

// Outcome pairs a job with what happened to it.
type Outcome struct {
	Job Job
	Run *run.Run
	Err error
}

// Failures counts the jobs that failed.
func (r Result) Failures() int {
	n := 0
	for _, o := range r.Executed {
		if o.Err != nil {
			n++
		}
	}
	return n
}

// Once performs a single tick: fetch, discover, filter, execute.
//
// One job's failure does not abort the tick. A broken pull request must not
// stop main from building — that would let anybody with push access to a fork
// halt the pipeline by opening a PR that fails to check out.
func (w *Watcher) Once(ctx context.Context, dryRun bool) (Result, error) {
	if !dryRun {
		w.reap(ctx)
		w.prune(ctx)
		// After the housekeeping, which is what frees the space.
		if err := w.checkDisk(); err != nil {
			return Result{}, err
		}
	}

	// Before the fetch, because the answer decides what to fetch; once, because
	// the same answer then decides fork-ness and closed-ness in discovery.
	pulls := w.askForge(ctx)

	if err := w.fetch(ctx, pulls); err != nil {
		return Result{}, err
	}

	// After the fetch, so a scheduled task runs against the head that exists
	// now rather than whatever was last pulled.
	if !dryRun {
		w.runScheduled(ctx)
	}

	jobs, err := w.discover(ctx, pulls)
	if err != nil {
		return Result{}, err
	}

	all := jobs
	jobs, base, err := w.applyBaseline(ctx, jobs, pulls.authoritative, dryRun)
	if err != nil {
		return Result{}, err
	}
	jobs = w.dropSuperseded(jobs, all, base)

	return w.runJobs(ctx, jobs, dryRun)
}

// runJobs filters the discovered jobs against the ledger and builds the rest,
// each inside a machine build slot.
func (w *Watcher) runJobs(ctx context.Context, jobs []Job, dryRun bool) (Result, error) {
	result := Result{Discovered: jobs}
	full := false
	docker := w.dockerProbe(ctx)
	for _, job := range jobs {
		switch {
		case w.alreadyCovered(job):
			result.Skipped = append(result.Skipped, job)
			continue
		case dryRun:
			result.Executed = append(result.Executed, Outcome{Job: job})
			continue
		case full:
			// The machine was full a moment ago. Waiting the whole bound again
			// per job would hold this repository's lock for an hour behind a
			// machine already known to be busy.
			result.Deferred = append(result.Deferred, job)
			continue
		}
		if w.Pipeline.NeedsDocker(job.Event.String(), !w.PlanImages) && docker() != nil {
			result.Deferred = append(result.Deferred, job)
			continue
		}

		slot, err := w.acquireSlot(ctx, job)
		if errors.Is(err, ports.ErrNoBuildSlot) {
			full = true
			w.logger().Info("deferring to the next tick: no build slot free",
				"ref", job.Ref, "sha", run.ShortSHA(job.SHA), "busy", err)
			result.Deferred = append(result.Deferred, job)
			continue
		}
		if err != nil {
			return result, err
		}

		outcome := w.execute(ctx, job)
		_ = slot.Release()
		result.Executed = append(result.Executed, outcome)

		// A cancelled context means the operator pressed Ctrl-C or the
		// deadline passed. Continuing would run the remaining jobs against a
		// dead context and record a cascade of confusing failures.
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
	}
	return result, nil
}

// alreadyCovered reports whether the ledger says this job needs no run now.
func (w *Watcher) alreadyCovered(job Job) bool {
	log := w.logger()
	switch verdict, wait := engine.Decide(w.Store, job.SHA, job.Ref, w.now(), w.Engine.Inputs(w.Pipeline)); verdict {
	case engine.Built:
		log.Debug("already built", "ref", job.Ref, "sha", run.ShortSHA(job.SHA))
		return true
	case engine.Backoff:
		// A failing commit is not retried every tick. Without this a pull
		// request whose gate fails is re-gated forever, which on a real
		// box meant 205 failed runs in an afternoon.
		log.Debug("failed recently", "ref", job.Ref, "sha", run.ShortSHA(job.SHA),
			"wait", engine.DescribeBackoff(wait))
		return true
	case engine.Settled:
		// The commit failed on its own merits. Running it again with the
		// same kiln, tools and pipeline would only repeat the verdict.
		log.Debug("failed on this commit, waiting for a change", "ref", job.Ref,
			"sha", run.ShortSHA(job.SHA), "wait", engine.DescribeBackoff(wait))
		return true
	default:
		return false
	}
}

// acquireSlot claims one of the machine's build slots for job.
//
// Per job, and only around the build: discovery and a fetch cost the machine
// nothing worth limiting, and a tick holding a slot across several jobs would
// starve every other box for as long as it ran.
//
// No free slot is ErrNoBuildSlot, which the caller turns into a deferral — not
// a failure. Nothing is recorded, so the commit is neither blamed nor backed
// off, and the next tick finds it unbuilt. Any other error is the machine's
// fault and fails the tick loudly: building anyway would quietly remove the
// limit, and recording it would blame the commit.
func (w *Watcher) acquireSlot(ctx context.Context, job Job) (ports.BuildSlot, error) {
	if w.Slots == nil {
		return noSlot{}, nil
	}
	holder := "kiln watch " + w.Dir + " " + job.Label
	slot, err := w.Slots.TryAcquire(holder)
	if !errors.Is(err, ports.ErrNoBuildSlot) {
		return slot, err
	}
	// Said before the wait, so a tick that sits quiet for minutes explains
	// itself while it does.
	w.logger().Info("waiting for a build slot", "ref", job.Ref, "busy", err)
	return w.Slots.Acquire(ctx, holder)
}

// execute builds one job.
func (w *Watcher) execute(ctx context.Context, job Job) Outcome {
	log := w.logger()
	log.Info("building", "ref", job.Ref, "sha", run.ShortSHA(job.SHA),
		"event", job.Event.String(), "fork", job.Fork)

	r, runErr := w.Engine.Execute(ctx, engine.Request{
		SHA:      job.SHA,
		Event:    job.Event,
		Fork:     job.Fork,
		Ref:      job.Ref,
		Repo:     w.Repo,
		Dir:      w.Dir,
		Pipeline: w.Pipeline,
		Output:   w.Output,
	})
	if runErr != nil {
		log.Error("job failed", "ref", job.Ref, "sha", run.ShortSHA(job.SHA), "err", runErr)
	}
	return Outcome{Job: job, Run: r, Err: runErr}
}

// noSlot stands in when no limit is configured.
type noSlot struct{}

func (noSlot) Release() error { return nil }

// Every loops Once until the context is cancelled. The first tick runs
// immediately: an operator who starts a watcher wants to know now whether it
// works, not in a minute.
func (w *Watcher) Every(ctx context.Context, interval time.Duration, dryRun bool) error {
	if interval <= 0 {
		return errors.New("watch: interval must be positive")
	}
	log := w.logger()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		res, err := w.tickLocked(ctx, dryRun)
		switch {
		case errors.Is(err, context.Canceled):
			return nil
		case err != nil:
			// A tick that could not even discover work — no network, a broken
			// remote — is logged and retried on the next interval rather than
			// killing the loop. An unattended watcher that exits on the first
			// flaky fetch is worse than one that keeps trying.
			log.Error("tick failed", "err", err)
		default:
			log.Debug("tick complete",
				"discovered", len(res.Discovered), "skipped", len(res.Skipped),
				"executed", len(res.Executed), "failed", res.Failures(),
				"deferred", len(res.Deferred))
		}

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// reap collects worktrees abandoned by killed runs.
//
// Once per tick, under the lock, before any work: a box building all day for
// months accumulates leavings from SIGKILLs and OOM kills that no run's own
// cleanup could have handled, and nothing else ever collects them. The disk
// fills quietly and the first symptom is an unrelated build failing.
//
// Never fatal. Housekeeping that could fail the tick would be the
// housekeeping causing the outage it exists to prevent.
func (w *Watcher) reap(ctx context.Context) {
	if w.Worktrees == nil {
		return
	}
	removed, err := w.Worktrees.Reap(ctx, w.Dir, 0)
	if err != nil {
		w.logger().Warn("could not reap abandoned worktrees", "err", err)
		return
	}
	if removed > 0 {
		w.logger().Info("reaped abandoned worktrees", "removed", removed)
	}
}

// prune reclaims the disk docker holds.
//
// Beside the worktree reaper and for the same reason, but the numbers are
// lopsided: checkouts are megabytes and docker is usually gigabytes of images
// on top of several more of build cache. Collecting one without the other
// reclaims the smaller half.
//
// Never fatal, and never wider than what this pipeline publishes.
func (w *Watcher) prune(ctx context.Context) {
	if w.Pruner == nil {
		return
	}

	images := w.Pipeline.PrunableImages()
	repos := make([]string, 0, len(images))
	keep := 0
	for image, n := range images {
		repos = append(repos, image)
		// One retention across the sweep; per-image limits differing would be
		// a refinement nobody has asked for.
		if n > keep {
			keep = n
		}
	}
	sort.Strings(repos)

	res, err := w.Pruner.Prune(ctx, ports.PruneOptions{
		Repos:            repos,
		Keep:             keep,
		BuildCacheMaxAge: w.BuildCacheMaxAge,
	})
	if err != nil {
		w.logger().Warn("could not prune", "err", err)
		return
	}
	if len(res.Removed) > 0 || res.CacheFreed != "" {
		w.logger().Info("pruned",
			"images", len(res.Removed), "kept", res.Kept, "cache", res.CacheFreed)
	}
}

// checkDisk refuses a tick on a box too full to finish one.
//
// Before this, a full disk failed every job in the tick with the linker's
// "no space left on device", and recorded each failure against a commit that
// had done nothing wrong. One refused tick says what is actually wrong.
func (w *Watcher) checkDisk() error {
	if w.Health == nil || w.MinFree == 0 || w.Dir == "" {
		return nil
	}
	free, err := w.Health.FreeBytes(w.Dir)
	if err != nil {
		// A check that cannot look is not a reason to stop building.
		w.logger().Warn("could not read free disk space", "err", err)
		return nil
	}
	if free < w.MinFree {
		return fmt.Errorf("%w: %s free, below the %s minimum (KILN_MIN_FREE_GB)",
			ports.ErrBoxNotReady, humanBytes(free), humanBytes(w.MinFree))
	}
	return nil
}

// dockerProbe returns a check that asks the daemon at most once per tick, and
// only if some job needs it: a tick of prove-only jobs never pays for it.
func (w *Watcher) dockerProbe(ctx context.Context) func() error {
	var (
		asked bool
		err   error
	)
	return func() error {
		if w.Health == nil {
			return nil
		}
		if !asked {
			asked = true
			if err = w.Health.DockerReady(ctx); err != nil {
				w.logger().Warn("deferring jobs that need docker", "err", err)
			}
		}
		return err
	}
}

func humanBytes(n uint64) string {
	const gib = 1 << 30
	return fmt.Sprintf("%.1f GiB", float64(n)/gib)
}

// tickLocked runs one tick under the repository lock.
//
// Per tick rather than for the loop's lifetime: a watcher that held the lock
// while sleeping would shut out an operator's `kiln run` for as long as it
// ran, which is not what serialising builds is meant to cost.
func (w *Watcher) tickLocked(ctx context.Context, dryRun bool) (Result, error) {
	if dryRun || w.Dir == "" {
		return w.Once(ctx, dryRun)
	}

	l, err := w.Locks.TryAcquire(w.Dir, "kiln watch --every")
	if errors.Is(err, ports.ErrRepoBusy) {
		// Something else is working this repository. Skipping is the whole
		// point; the next tick will find whatever is left.
		w.logger().Info("skipping tick: another kiln holds this repository",
			"holder", w.Locks.HolderOf(w.Dir))
		return Result{}, nil
	}
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = l.Release() }()

	return w.Once(ctx, dryRun)
}

// fetch updates the local refs discovery reads.
//
// Three fetches rather than one, because they fail independently: a repository
// with no pull requests, or a non-GitHub remote that has no refs/pull/*, must
// still get its branch built. Only the branch fetch is fatal.
func (w *Watcher) fetch(ctx context.Context, pulls openPulls) error {
	remote := w.remote()
	branch := w.Pipeline.Watch.Ref
	if branch == "" {
		branch = "main"
	}
	log := w.logger()

	if err := w.fetchWithRetry(ctx, remote,
		fmt.Sprintf("+refs/heads/%s:refs/remotes/%s/%s", branch, remote, branch)); err != nil {
		return fmt.Errorf("watch: fetch %s/%s: %w", remote, branch, err)
	}

	if w.BranchesOnly {
		return nil
	}

	if w.Pipeline.WatchPullRequests() {
		w.fetchPulls(ctx, remote, pulls)
	}
	if w.Pipeline.WatchTags() {
		if err := w.fetchWithRetry(ctx, remote, "+refs/tags/*:refs/tags/*"); err != nil {
			log.Debug("could not fetch tags", "remote", remote, "err", err)
		}
	}
	return nil
}

// fetchPulls parks the pull request heads discovery will consider.
//
// GitHub keeps refs/pull/N/head for every pull request ever opened, so the
// wildcard fetches the repository's whole history of them every tick for
// discovery to throw nearly all of it away — 372 skipped against one build,
// every thirty minutes, on one box here. When the API has said which are open,
// only those are fetched, and the parked heads of the rest are dropped so the
// local namespace stops growing.
//
// Without that answer nothing is known to be closed, and the wildcard is kept
// exactly: it is how a tokenless box still sees the pull request opened since
// its baseline, and why pullDecision and the baseline exist at all.
func (w *Watcher) fetchPulls(ctx context.Context, remote string, pulls openPulls) {
	log := w.logger()
	if pulls.authoritative {
		err := w.fetchOpenPulls(ctx, remote, pulls)
		if err == nil {
			w.dropClosedPulls(ctx, pulls)
			return
		}
		// A fetch by number fails whole when any one ref is missing — a remote
		// that is not the repository the token answers for, say. That must
		// cost the narrowing, not the pull requests.
		log.Warn("could not fetch the open pull requests by number; fetching every pull ref",
			"remote", remote, "err", err)
	}
	// GitHub exposes pull request heads as refs/pull/N/head. A non-GitHub
	// remote simply has none, and the failure is not interesting.
	if err := w.fetchWithRetry(ctx, remote, "+refs/pull/*/head:"+PRRefNamespace+"*"); err != nil {
		log.Debug("no pull request refs on this remote", "remote", remote, "err", err)
	}
	if pulls.authoritative {
		w.dropClosedPulls(ctx, pulls)
	}
}

// fetchOpenPulls fetches exactly the heads of the open pull requests, in one
// fetch: a round trip per pull request would trade one waste for another.
func (w *Watcher) fetchOpenPulls(ctx context.Context, remote string, pulls openPulls) error {
	if len(pulls.fork) == 0 {
		return nil
	}
	numbers := slices.Sorted(maps.Keys(pulls.fork))
	refspecs := make([]string, 0, len(numbers))
	for _, n := range numbers {
		refspecs = append(refspecs, fmt.Sprintf("+refs/pull/%d/head:%s%d", n, PRRefNamespace, n))
	}
	return w.fetchWithRetry(ctx, remote, refspecs...)
}

// dropClosedPulls removes the parked heads of pull requests the API did not
// list as open. Only ever called with an authoritative answer: without one, a
// missing number means "unknown", and deleting an unknown pull request's head
// would stop it being gated.
//
// Never fatal. A leftover ref is filtered by pullDecision regardless; it only
// costs disk and a line in the log.
func (w *Watcher) dropClosedPulls(ctx context.Context, pulls openPulls) {
	refs, err := w.Git.PullRefs(ctx, w.Dir)
	if err != nil {
		w.logger().Warn("could not list parked pull request refs", "err", err)
		return
	}
	var closed []string
	for _, r := range refs {
		if n, err := pullNumber(r.Name); err == nil && !isOpen(pulls, n) {
			closed = append(closed, r.Name)
		}
	}
	if len(closed) == 0 {
		return
	}
	if err := w.Git.DropPullRefs(ctx, w.Dir, closed); err != nil {
		w.logger().Warn("could not drop the parked refs of closed pull requests", "err", err)
		return
	}
	w.logger().Debug("dropped the parked refs of closed pull requests", "count", len(closed))
}

func isOpen(pulls openPulls, number int) bool {
	_, ok := pulls.fork[number]
	return ok
}

// fetchWithRetry retries a fetch. Network flakiness on a box running a tick a
// minute is routine, and a single dropped connection should not skip a build.
func (w *Watcher) fetchWithRetry(ctx context.Context, remote string, refspecs ...string) error {
	attempts := w.FetchAttempts
	if attempts <= 0 {
		attempts = 3
	}
	r := retry.New[struct{}](retry.Config{
		MaxAttempts:   attempts,
		InitialDelay:  time.Second,
		MaxDelay:      10 * time.Second,
		Multiplier:    2,
		BackoffPolicy: retry.BackoffExponential,
		Jitter:        true,
	})
	_, err := r.Execute(ctx, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, w.Git.Fetch(ctx, w.Dir, remote, refspecs...)
	})
	return err
}

// applyBaseline drops the tags a box inherited rather than earned.
//
// The first tick of a new box records what is already there; every tick after
// that skips it. A tag is a publishing event, so without this a fresh box
// republishes every release the repository ever cut — 133 of them on one repo
// here, each pushing images and writing fresh provenance for a version that
// was signed long ago.
//
// Only tags are baselined. The branch tip and any open pull requests are
// current work, and building them on the first tick is the point of installing
// a box — it is also the only sign the operator gets that the pipeline runs at
// all.
//
// The baseline is returned, nil when the box has none, because it is also a
// watermark: dropSuperseded counts an inherited release as shipped.
func (w *Watcher) applyBaseline(ctx context.Context, jobs []Job, authoritative, dryRun bool) ([]Job, *Baseline, error) {
	base, err := LoadBaseline(w.Dir)
	if err != nil {
		return nil, nil, err
	}
	if base == nil {
		if !ledgerIsEmpty(w.Store) {
			// An existing box has already built its tags. Writing a baseline
			// underneath it would silence one that is mid-backoff.
			return jobs, nil, nil
		}
		pulls, err := w.baselinePulls(ctx, authoritative)
		if err != nil {
			return nil, nil, err
		}
		base = baselineFrom(jobs, pulls, w.now())
		if !dryRun && w.Dir != "" {
			if err := SaveBaseline(w.Dir, base); err != nil {
				return nil, nil, err
			}
		}
		if len(base.Tags) > 0 {
			w.logger().Info("baselined the tags this repository already had",
				"count", len(base.Tags),
				"why", "a new box publishes what happens next, not what already shipped")
		}
	}

	kept := jobs[:0:0]
	var skipped int
	for _, j := range jobs {
		if base.Covers(j, authoritative) {
			skipped++
			continue
		}
		kept = append(kept, j)
	}
	if skipped > 0 && base.Tags != nil {
		w.logger().Debug("skipped baselined tags", "count", skipped)
	}
	return kept, base, nil
}

// discover builds the job list from local refs.
func (w *Watcher) discover(ctx context.Context, pulls openPulls) ([]Job, error) {
	jobs := make([]Job, 0, 8)

	branchJob, err := w.branchJob(ctx)
	if err != nil {
		return nil, err
	}
	jobs = append(jobs, branchJob)

	if w.BranchesOnly {
		return jobs, nil
	}
	if w.Pipeline.WatchTags() {
		tagJobs, err := w.tagJobs(ctx)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, tagJobs...)
	}
	if w.Pipeline.WatchPullRequests() {
		prJobs, err := w.pullJobs(ctx, branchJob.SHA, pulls)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, prJobs...)
	}
	return jobs, nil
}

// remote is the watched remote, defaulting as config documents.
func (w *Watcher) remote() string {
	if w.Pipeline.Watch.Remote == "" {
		return "origin"
	}
	return w.Pipeline.Watch.Remote
}

// now is the clock, injectable so a test can state what "twenty minutes later"
// means rather than sleep through it.
func (w *Watcher) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

// runScheduled fires the scheduled tasks that are due.
//
// Never fatal to the tick. A remediation job that cannot run is a problem, but
// stopping the watcher from discovering and building commits because of it
// would turn a broken errand into a stopped pipeline.
func (w *Watcher) runScheduled(ctx context.Context) {
	log := w.logger()
	if w.Schedule == nil {
		return
	}
	due := w.Pipeline.TasksFor(config.ScheduleEvent)
	if len(due) == 0 {
		return
	}

	var fire []config.NamedTask
	for _, nt := range due {
		ready, err := w.Schedule.DueAt(nt.Name, nt.Task.Every.Std(), w.now())
		if err != nil {
			log.Warn("could not read schedule state", "task", nt.Name, "err", err)
			continue
		}
		if ready {
			fire = append(fire, nt)
		}
	}
	if len(fire) == 0 {
		return
	}

	head, err := w.branchJob(ctx)
	if err != nil {
		log.Warn("scheduled tasks skipped: cannot resolve the tracked branch", "err", err)
		return
	}

	// Marked as fired before running, not after. A task that takes down the
	// process would otherwise re-fire on every restart until it stopped doing
	// so — a loop with an audience, for anything that opens a pull request.
	for _, nt := range fire {
		if err := w.Schedule.Fired(nt.Name, w.now()); err != nil {
			log.Warn("could not record schedule state; skipping to avoid a repeat loop",
				"task", nt.Name, "err", err)
			return
		}
	}

	log.Info("running scheduled tasks", "count", len(fire), "sha", run.ShortSHA(head.SHA))
	if _, err := w.Engine.RunScheduled(ctx, engine.Request{
		Dir: w.Dir, SHA: head.SHA, Ref: head.Ref, Repo: w.Repo, Pipeline: w.Pipeline,
		Output: w.Output,
	}, fire); err != nil {
		log.Error("scheduled tasks failed", "err", err)
	}
}

func (w *Watcher) branchJob(ctx context.Context) (Job, error) {
	remote := w.remote()
	branch := w.Pipeline.Watch.Ref
	if branch == "" {
		branch = "main"
	}

	sha, err := w.Git.HeadSHA(ctx, w.Dir, remote, branch)
	if err != nil {
		return Job{}, fmt.Errorf("watch: resolve %s/%s: %w", remote, branch, err)
	}
	return Job{
		SHA:   sha,
		Ref:   "refs/heads/" + branch,
		Event: isolation.EventPush,
		Label: branch,
	}, nil
}

// tagJobs lists tags, peeling annotated ones to the commit they point at. An
// annotated tag's own object id is not something a worktree can check out and
// not something a warden note is bound to.
func (w *Watcher) tagJobs(ctx context.Context) ([]Job, error) {
	tags, err := w.Git.Tags(ctx, w.Dir)
	if err != nil {
		return nil, fmt.Errorf("watch: list tags: %w", err)
	}

	jobs := make([]Job, 0, len(tags))
	for _, t := range tags {
		jobs = append(jobs, Job{
			SHA:   t.SHA,
			Ref:   t.Name,
			Event: isolation.EventTag,
			Label: strings.TrimPrefix(t.Name, "refs/tags/"),
		})
	}
	return jobs, nil
}

// baselinePulls lists every pull request head the repository has, whatever its
// state. This is deliberately unfiltered: the baseline has to record what a
// repository already had, not the subset a token happened to say was open.
//
// With an authoritative answer only the open heads were fetched, so the
// parked refs no longer hold the closed ones — and the closed ones are exactly
// what the baseline exists to remember for the day the token stops answering.
// The remote still has them, and listing them costs no objects. Without one,
// the wildcard just parked everything, as it always has.
func (w *Watcher) baselinePulls(ctx context.Context, authoritative bool) (map[string]string, error) {
	var refs []ports.Ref
	var err error
	if authoritative {
		refs, err = w.Git.RemotePullRefs(ctx, w.Dir, w.remote())
	} else {
		refs, err = w.Git.PullRefs(ctx, w.Dir)
	}
	if err != nil {
		return nil, fmt.Errorf("watch: list pull request refs: %w", err)
	}
	out := make(map[string]string, len(refs))
	for _, r := range refs {
		out[r.Name] = r.SHA
	}
	return out, nil
}

// pullJobs turns the fetched pull request heads into jobs.
//
// Fork status is the security-critical field here. With a token, it comes from
// the API. Without one, every pull request is treated as a fork — the only
// safe reading of "I cannot tell", since the alternative hands an unknown
// author the operator's credentials.
func (w *Watcher) pullJobs(ctx context.Context, branchTip string, pulls openPulls) ([]Job, error) {
	refs, err := w.Git.PullRefs(ctx, w.Dir)
	if err != nil {
		return nil, fmt.Errorf("watch: list pull request refs: %w", err)
	}

	open, authoritative := pulls.fork, pulls.authoritative
	log := w.logger()

	var jobs []Job
	var closed int
	for _, r := range refs {
		number, convErr := pullNumber(r.Name)
		if convErr != nil {
			log.Debug("skipping unparsable pull ref", "ref", r.Name)
			continue
		}
		sha := r.SHA

		fork, build := pullDecision(number, open, authoritative, func() bool {
			return w.mergedIntoBranch(ctx, sha, branchTip)
		})
		if !build {
			closed++
			continue
		}
		jobs = append(jobs, Job{
			SHA:   sha,
			Ref:   fmt.Sprintf("refs/pull/%d/head", number),
			Event: isolation.EventPullRequest,
			Fork:  fork,
			Label: fmt.Sprintf("PR #%d", number),
		})
	}
	// Said out loud: a box that quietly builds nothing looks identical to a
	// box that is broken, and "why is my pull request not being gated" is the
	// question this number answers.
	if closed > 0 {
		log.Info("skipped closed pull requests",
			"count", closed, "built", len(jobs),
			"why", "refs/pull/N/head outlives the pull request it belonged to")
	}
	return jobs, nil
}

// pullDecision says whether a discovered pull ref should be built, and whether
// it must be treated as a fork.
//
// GitHub keeps refs/pull/N/head forever, for every pull request ever opened, so
// the refs on a remote are the repository's whole history of them — 390 against
// 2 still open, on one repo here. Building them all re-gates that history on a
// box's first tick and posts a status on every long-merged commit.
//
// merged is a thunk because it costs a git call: it is only consulted when
// there is no authoritative answer to be had.
func pullDecision(number int, open map[int]bool, authoritative bool, merged func() bool) (fork, build bool) {
	fork, isOpen := open[number]
	switch {
	case isOpen:
		// The API listed it: build it, with the fork-ness it reported.
		return fork, true
	case authoritative:
		// The API answered and this number is not in it — closed or merged.
		return false, false
	case merged():
		// No authoritative list, but the head is already contained in the
		// watched branch, so it has certainly been merged.
		return false, false
	default:
		// Unknown and unmerged: build it, failing closed as a fork. The
		// alternative hands an unknown author the operator's credentials.
		return true, true
	}
}

// openPulls is GitHub's answer to "which pull requests are open", asked once
// per tick and used three times: to choose what to fetch, to drop what is
// closed, and to say which of what is left comes from a fork.
type openPulls struct {
	// fork maps each OPEN pull request's number to its fork-ness.
	fork map[int]bool
	// authoritative means the API answered: a number missing from fork is
	// then known to be closed, rather than merely unknown.
	authoritative bool
}

// askForge asks the forge, or reports that it cannot be asked. The zero value
// — unknown — is what a tick that does not look at pull requests gets: `kiln
// poll` is the subset of watch that needs no token, and must not warn about
// lacking one.
func (w *Watcher) askForge(ctx context.Context) openPulls {
	if w.BranchesOnly || !w.Pipeline.WatchPullRequests() {
		return openPulls{}
	}
	if w.Forge == nil || !w.Forge.Enabled() {
		w.logger().Warn("no github token: treating every pull request as a fork",
			"effect", "no secrets, no publish, no provenance skip on any PR")
		return openPulls{}
	}
	pulls, err := w.Forge.ListOpenPulls(ctx)
	if err != nil {
		w.logger().Warn("could not list pull requests: treating every one as a fork", "err", err)
		return openPulls{}
	}
	out := make(map[int]bool, len(pulls))
	for _, p := range pulls {
		out[p.Number] = p.Fork
	}
	return openPulls{fork: out, authoritative: true}
}

// mergedIntoBranch reports that a pull request head is already contained in the
// watched branch.
//
// The fallback for a box with no token: a head that is an ancestor of the
// branch has been merged, whatever the API would have said. It costs one
// merge-base per ref and no network.
func (w *Watcher) mergedIntoBranch(ctx context.Context, sha, branchTip string) bool {
	if sha == "" || branchTip == "" {
		return false
	}
	contained, err := w.Git.Contains(ctx, w.Dir, sha, branchTip)
	return err == nil && contained
}

func (w *Watcher) logger() ports.Logger {
	if w.Log == nil {
		return ports.DiscardLogger()
	}
	return w.Log
}

// pullNumber reads N out of refs/pull/N/head.
func pullNumber(ref string) (int, error) {
	var n int
	_, err := fmt.Sscanf(ref, "refs/pull/%d/head", &n)
	return n, err
}
