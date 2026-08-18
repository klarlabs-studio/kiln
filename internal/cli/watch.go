package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.klarlabs.de/kiln/internal/boot"
	"go.klarlabs.de/kiln/internal/lock"
	"go.klarlabs.de/kiln/internal/poll"
	"go.klarlabs.de/kiln/internal/prune"
	"go.klarlabs.de/kiln/internal/run"
	"go.klarlabs.de/kiln/internal/watch"
)

// runWatch backs both `kiln watch` and `kiln poll`.
//
// One implementation, one flag. Two commands that discover work in two
// slightly different ways would be two chances to get the fork rules wrong,
// so poll is watch with the untrusted half switched off.
func runWatch(ctx context.Context, args []string, io IO, branchesOnly bool) error {
	name := "watch"
	if branchesOnly {
		name = "poll"
	}

	fs := newFlagSet(name, io)
	repos := fs.String("repos", "", "comma-separated repositories to watch, or a glob like /srv/*")
	once := fs.Bool("once", false, "run a single tick and exit (this is the cron shape)")
	every := fs.String("every", "", "loop with this interval, e.g. 1m")
	dryRun := fs.Bool("dry-run", false, "print the jobs a tick would run, and run none of them")
	dir := fs.String("dir", "", "repository directory")
	pipelinePath := fs.String("pipeline", "", "pipeline file (default <dir>/.kiln.yaml)")
	quiet := fs.Bool("quiet", false, "do not stream subprocess output")
	if err := fs.Parse(args); err != nil {
		return wrapExit(ExitUsage, err)
	}
	if *once && *every != "" {
		return failWith(ExitUsage, "--once and --every are mutually exclusive")
	}
	if *repos != "" && *dir != "" {
		return failWith(ExitUsage, "--repos and --dir are mutually exclusive")
	}

	if *repos != "" {
		dirs, err := expandRepos(*repos)
		if err != nil {
			return wrapExit(ExitUsage, err)
		}
		return watchMany(ctx, io, dirs, watchOptions{
			name: name, branchesOnly: branchesOnly, pipelinePath: *pipelinePath,
			every: *every, dryRun: *dryRun, quiet: *quiet,
		})
	}

	deps, err := boot.Build(ctx, boot.Options{
		Dir:          *dir,
		PipelinePath: *pipelinePath,
		Output:       quietOr(*quiet, os.Stderr),
	})
	if err != nil {
		return wrapExit(ExitConfig, err)
	}

	watcher := newWatcher(deps, branchesOnly)

	if *every != "" {
		interval, err := parseInterval(*every)
		if err != nil {
			return wrapExit(ExitUsage, err)
		}
		// Every returns nil on cancellation: Ctrl-C is how an operator stops a
		// watcher, not a failure.
		//
		// The lock is taken per tick rather than for the whole loop, so a
		// long-lived `--every` watcher does not shut out a one-off `kiln run`
		// between its ticks.
		return watcher.Every(ctx, interval, *dryRun)
	}

	// Default to a single tick. `kiln watch` with no flags is the cron entry,
	// and a command that unexpectedly never returned would be a nasty surprise
	// in a crontab.
	//
	// A dry run takes no lock: it writes nothing and builds nothing, and
	// refusing to show an operator the plan because a build is in flight would
	// be obstructive at exactly the wrong moment.
	if *dryRun {
		return finishTick(ctx, watcher, io, branchesOnly, true)
	}

	return withRepoLock(deps.Dir, "kiln "+name,
		func(h lock.Holder) error {
			// Overlap is expected under cron and is not a fault. Exiting
			// non-zero here would page somebody every time a build outran the
			// schedule.
			io.printf("busy   %s is already working here (%s)\n", name, h)
			return nil
		},
		func() error { return finishTick(ctx, watcher, io, branchesOnly, false) })
}

// watchOptions carries the flags shared by every repository in a fleet tick.
type watchOptions struct {
	name         string
	branchesOnly bool
	pipelinePath string
	every        string
	dryRun       bool
	quiet        bool
}

// expandRepos turns --repos into directories.
//
// Accepts both a comma-separated list and a glob, because an operator with
// /srv/{a,b,c} should not have to name them and an operator with three
// unrelated paths should not have to invent a common parent.
func expandRepos(spec string) ([]string, error) {
	seen := map[string]bool{}
	var dirs []string

	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		matches := []string{part}
		if strings.ContainsAny(part, "*?[") {
			var err error
			if matches, err = filepath.Glob(part); err != nil {
				return nil, fmt.Errorf("--repos %q: %w", part, err)
			}
			sort.Strings(matches)
		}
		for _, m := range matches {
			abs, err := filepath.Abs(m)
			if err != nil {
				return nil, fmt.Errorf("--repos %q: %w", m, err)
			}
			if info, err := os.Stat(abs); err != nil || !info.IsDir() {
				// A glob that matched a file is a typo, not a repository.
				continue
			}
			if !seen[abs] {
				seen[abs] = true
				dirs = append(dirs, abs)
			}
		}
	}

	if len(dirs) == 0 {
		return nil, fmt.Errorf("--repos %q matched no directories", spec)
	}
	return dirs, nil
}

// watchMany ticks a fleet from one process.
//
// Sequential rather than concurrent, deliberately. The expensive parts of a
// tick — docker build, a cross-compile, the gate's test suite — already
// saturate a build box, so running four at once makes all four slower and the
// output impossible to read. The repository lock also means a parallel fleet
// would mostly be waiting on itself wherever two entries share a checkout.
//
// One repository failing never stops the others: that is the whole reason to
// run a fleet from one process rather than accept a single point of failure.
func watchMany(ctx context.Context, io IO, dirs []string, opts watchOptions) error {
	if opts.every != "" {
		interval, err := parseInterval(opts.every)
		if err != nil {
			return wrapExit(ExitUsage, err)
		}
		return fleetLoop(ctx, io, dirs, opts, interval)
	}
	return fleetTick(ctx, io, dirs, opts)
}

func fleetLoop(ctx context.Context, io IO, dirs []string, opts watchOptions, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		// A failing tick is logged by fleetTick and does not end the loop, for
		// the same reason a single watcher's does not: an unattended process
		// that exits on the first bad minute is worse than one that retries.
		_ = fleetTick(ctx, io, dirs, opts)

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func fleetTick(ctx context.Context, io IO, dirs []string, opts watchOptions) error {
	failed := 0
	for _, dir := range dirs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		io.printf("== %s\n", dir)

		if err := watchOne(ctx, io, dir, opts); err != nil {
			failed++
			io.printf("   %v\n", err)
		}
	}
	if failed > 0 {
		return failWith(ExitFailed, "%d of %d repositories failed", failed, len(dirs))
	}
	return nil
}

// watchOne runs a single repository's tick with its own pipeline, ledger and
// lock. Booting per repository rather than reusing one graph is the point:
// two repositories share nothing but the binary.
func watchOne(ctx context.Context, io IO, dir string, opts watchOptions) error {
	deps, err := boot.Build(ctx, boot.Options{
		Dir:          dir,
		PipelinePath: opts.pipelinePath,
		Output:       quietOr(opts.quiet, os.Stderr),
	})
	if err != nil {
		return err
	}

	watcher := newWatcher(deps, opts.branchesOnly)

	if opts.dryRun {
		return finishTick(ctx, watcher, io, opts.branchesOnly, true)
	}
	return withRepoLock(deps.Dir, "kiln "+opts.name,
		func(h lock.Holder) error {
			io.printf("   busy: already working here (%s)\n", h)
			return nil
		},
		func() error { return finishTick(ctx, watcher, io, opts.branchesOnly, false) })
}

// newWatcher builds a watcher from the assembled graph, so the single-repo and
// fleet paths cannot drift in what they wire.
func newWatcher(deps *boot.Deps, branchesOnly bool) *watch.Watcher {
	return &watch.Watcher{
		Engine:           deps.Engine,
		Store:            deps.Store,
		Runner:           deps.Runner,
		GitHub:           deps.GitHub,
		Log:              deps.Log,
		Dir:              deps.Dir,
		Pipeline:         deps.Pipeline,
		Repo:             repoName(deps),
		BranchesOnly:     branchesOnly,
		Pruner:           prune.New(deps.Runner, deps.Log),
		BuildCacheMaxAge: deps.Env.BuildCacheMaxAge,
	}
}

func finishTick(ctx context.Context, w *watch.Watcher, io IO, branchesOnly, dryRun bool) error {
	result, err := tick(ctx, w, branchesOnly, dryRun)
	if err != nil {
		return wrapExit(ExitError, err)
	}
	printTick(io, result, dryRun)

	if n := result.Failures(); n > 0 {
		// The tick itself worked; some jobs did not. Cron should see this as a
		// build failure, not as a broken watcher.
		return failWith(ExitFailed, "%d job(s) failed", n)
	}
	return nil
}

func tick(ctx context.Context, w *watch.Watcher, branchesOnly, dryRun bool) (watch.Result, error) {
	if branchesOnly {
		return poll.New(w).Once(ctx, dryRun)
	}
	return w.Once(ctx, dryRun)
}

func printTick(io IO, res watch.Result, dryRun bool) {
	if len(res.Discovered) == 0 {
		io.print("nothing to build\n")
		return
	}

	for _, job := range res.Skipped {
		io.printf("skip    %-24s %s (already built)\n", job.Label, run.ShortSHA(job.SHA))
	}
	for _, outcome := range res.Executed {
		job := outcome.Job
		switch {
		case dryRun:
			io.printf("plan    %-24s %s %s\n", job.Label, run.ShortSHA(job.SHA), job.Event)
		case outcome.Err != nil:
			io.printf("FAIL    %-24s %s: %v\n", job.Label, run.ShortSHA(job.SHA), outcome.Err)
		default:
			io.printf("built   %-24s %s%s\n", job.Label, run.ShortSHA(job.SHA), digestSuffix(outcome))
		}
	}
}

func digestSuffix(o watch.Outcome) string {
	if o.Run == nil || o.Run.Digest == "" {
		return ""
	}
	return " → " + o.Run.Digest
}
