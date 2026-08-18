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
	"strconv"
	"strings"
	"time"

	"go.klarlabs.de/fortify/retry"

	"go.klarlabs.de/kiln/internal/config"
	"go.klarlabs.de/kiln/internal/engine"
	"go.klarlabs.de/kiln/internal/execx"
	"go.klarlabs.de/kiln/internal/github"
	"go.klarlabs.de/kiln/internal/isolation"
	"go.klarlabs.de/kiln/internal/lock"
	"go.klarlabs.de/kiln/internal/obs"
	"go.klarlabs.de/kiln/internal/run"
	"go.klarlabs.de/kiln/internal/store"
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

// Watcher discovers and executes work.
type Watcher struct {
	Engine *engine.Engine
	Store  store.Store
	Runner execx.Runner
	GitHub *github.Client
	Log    obs.Logger

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
}

// Result summarises one tick.
type Result struct {
	// Discovered is every job the tick found, before filtering.
	Discovered []Job
	// Skipped are the jobs a successful run already covers.
	Skipped []Job
	// Executed are the jobs that ran, with their outcomes.
	Executed []Outcome
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
	log := w.logger()

	if err := w.fetch(ctx); err != nil {
		return Result{}, err
	}

	jobs, err := w.discover(ctx)
	if err != nil {
		return Result{}, err
	}

	result := Result{Discovered: jobs}
	for _, job := range jobs {
		if engine.AlreadyBuilt(w.Store, job.SHA, job.Ref) {
			result.Skipped = append(result.Skipped, job)
			log.Debug("already built", "ref", job.Ref, "sha", run.ShortSHA(job.SHA))
			continue
		}
		if dryRun {
			result.Executed = append(result.Executed, Outcome{Job: job})
			continue
		}

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
		})
		if runErr != nil {
			log.Error("job failed", "ref", job.Ref, "sha", run.ShortSHA(job.SHA), "err", runErr)
		}
		result.Executed = append(result.Executed, Outcome{Job: job, Run: r, Err: runErr})

		// A cancelled context means the operator pressed Ctrl-C or the
		// deadline passed. Continuing would run the remaining jobs against a
		// dead context and record a cascade of confusing failures.
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
	}
	return result, nil
}

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
				"executed", len(res.Executed), "failed", res.Failures())
		}

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
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

	l, err := lock.TryAcquire(lock.PathFor(w.Dir), "kiln watch --every")
	if errors.Is(err, lock.ErrBusy) {
		// Something else is working this repository. Skipping is the whole
		// point; the next tick will find whatever is left.
		w.logger().Info("skipping tick: another kiln holds this repository",
			"holder", lock.ReadHolder(lock.PathFor(w.Dir)).String())
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
func (w *Watcher) fetch(ctx context.Context) error {
	remote := w.Pipeline.Watch.Remote
	if remote == "" {
		remote = "origin"
	}
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
		// GitHub exposes pull request heads as refs/pull/N/head. A non-GitHub
		// remote simply has none, and the failure is not interesting.
		if err := w.fetchWithRetry(ctx, remote, "+refs/pull/*/head:"+PRRefNamespace+"*"); err != nil {
			log.Debug("no pull request refs on this remote", "remote", remote, "err", err)
		}
	}
	if w.Pipeline.WatchTags() {
		if err := w.fetchWithRetry(ctx, remote, "+refs/tags/*:refs/tags/*"); err != nil {
			log.Debug("could not fetch tags", "remote", remote, "err", err)
		}
	}
	return nil
}

// fetchWithRetry retries a fetch. Network flakiness on a box running a tick a
// minute is routine, and a single dropped connection should not skip a build.
func (w *Watcher) fetchWithRetry(ctx context.Context, remote, refspec string) error {
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
		_, e := w.Runner.Run(ctx, execx.Cmd{
			Name: "git",
			Args: []string{"fetch", "--prune", "--quiet", remote, refspec},
			Dir:  w.Dir,
		})
		return struct{}{}, e
	})
	return err
}

// discover builds the job list from local refs.
func (w *Watcher) discover(ctx context.Context) ([]Job, error) {
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
		prJobs, err := w.pullJobs(ctx)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, prJobs...)
	}
	return jobs, nil
}

func (w *Watcher) branchJob(ctx context.Context) (Job, error) {
	remote := w.Pipeline.Watch.Remote
	if remote == "" {
		remote = "origin"
	}
	branch := w.Pipeline.Watch.Ref
	if branch == "" {
		branch = "main"
	}

	res, err := w.Runner.Run(ctx, execx.Cmd{
		Name: "git",
		Args: []string{"rev-parse", "--verify", fmt.Sprintf("refs/remotes/%s/%s", remote, branch)},
		Dir:  w.Dir,
	})
	if err != nil {
		return Job{}, fmt.Errorf("watch: resolve %s/%s: %w", remote, branch, err)
	}
	return Job{
		SHA:   res.Output(),
		Ref:   "refs/heads/" + branch,
		Event: isolation.EventPush,
		Label: branch,
	}, nil
}

// tagJobs lists tags, peeling annotated ones to the commit they point at. An
// annotated tag's own object id is not something a worktree can check out and
// not something a warden note is bound to.
func (w *Watcher) tagJobs(ctx context.Context) ([]Job, error) {
	res, err := w.Runner.Run(ctx, execx.Cmd{
		Name: "git",
		Args: []string{"for-each-ref", "--format=%(refname)\t%(objecttype)\t%(objectname)\t%(*objectname)", "refs/tags/"},
		Dir:  w.Dir,
	})
	if err != nil {
		return nil, fmt.Errorf("watch: list tags: %w", err)
	}

	var jobs []Job
	for line := range strings.SplitSeq(res.Output(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 3 {
			continue
		}
		ref, objType, objName := fields[0], fields[1], fields[2]

		sha := objName
		if objType == "tag" && len(fields) > 3 && fields[3] != "" {
			// %(*objectname) is the peeled commit.
			sha = fields[3]
		}
		jobs = append(jobs, Job{
			SHA:   sha,
			Ref:   ref,
			Event: isolation.EventTag,
			Label: strings.TrimPrefix(ref, "refs/tags/"),
		})
	}
	return jobs, nil
}

// pullJobs turns the fetched pull request heads into jobs.
//
// Fork status is the security-critical field here. With a token, it comes from
// the API. Without one, every pull request is treated as a fork — the only
// safe reading of "I cannot tell", since the alternative hands an unknown
// author the operator's credentials.
func (w *Watcher) pullJobs(ctx context.Context) ([]Job, error) {
	res, err := w.Runner.Run(ctx, execx.Cmd{
		Name: "git",
		Args: []string{"for-each-ref", "--format=%(refname)\t%(objectname)", PRRefNamespace},
		Dir:  w.Dir,
	})
	if err != nil {
		return nil, fmt.Errorf("watch: list pull request refs: %w", err)
	}

	forks := w.forkStatus(ctx)
	log := w.logger()

	var jobs []Job
	for line := range strings.SplitSeq(res.Output(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		ref, sha, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		number, convErr := strconv.Atoi(strings.TrimPrefix(ref, PRRefNamespace))
		if convErr != nil {
			log.Debug("skipping unparsable pull ref", "ref", ref)
			continue
		}

		fork, known := forks[number]
		if !known {
			// Either there is no token, or the pull request has since closed.
			// Fail closed.
			fork = true
		}
		jobs = append(jobs, Job{
			SHA:   sha,
			Ref:   fmt.Sprintf("refs/pull/%d/head", number),
			Event: isolation.EventPullRequest,
			Fork:  fork,
			Label: fmt.Sprintf("PR #%d", number),
		})
	}
	return jobs, nil
}

// forkStatus asks GitHub which open pull requests are same-repo. An empty map
// means "unknown", which pullJobs reads as "assume fork".
func (w *Watcher) forkStatus(ctx context.Context) map[int]bool {
	if w.GitHub == nil || !w.GitHub.Enabled() {
		w.logger().Warn("no github token: treating every pull request as a fork",
			"effect", "no secrets, no publish, no provenance skip on any PR")
		return nil
	}
	pulls, err := w.GitHub.ListOpenPulls(ctx)
	if err != nil {
		w.logger().Warn("could not list pull requests: treating every one as a fork", "err", err)
		return nil
	}
	out := make(map[int]bool, len(pulls))
	for _, p := range pulls {
		out[p.Number] = p.Fork
	}
	return out
}

func (w *Watcher) logger() obs.Logger {
	if w.Log == nil {
		return obs.Discard()
	}
	return w.Log
}
