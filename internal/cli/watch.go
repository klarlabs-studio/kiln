package cli

import (
	"context"
	"os"

	"go.klarlabs.de/kiln/internal/boot"
	"go.klarlabs.de/kiln/internal/poll"
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

	deps, err := boot.Build(ctx, boot.Options{
		Dir:          *dir,
		PipelinePath: *pipelinePath,
		Output:       quietOr(*quiet, os.Stderr),
	})
	if err != nil {
		return wrapExit(ExitConfig, err)
	}

	watcher := &watch.Watcher{
		Engine:       deps.Engine,
		Store:        deps.Store,
		Runner:       deps.Runner,
		GitHub:       deps.GitHub,
		Log:          deps.Log,
		Dir:          deps.Dir,
		Pipeline:     deps.Pipeline,
		Repo:         repoName(deps),
		BranchesOnly: branchesOnly,
	}

	if *every != "" {
		interval, err := parseInterval(*every)
		if err != nil {
			return wrapExit(ExitUsage, err)
		}
		// Every returns nil on cancellation: Ctrl-C is how an operator stops a
		// watcher, not a failure.
		return watcher.Every(ctx, interval, *dryRun)
	}

	// Default to a single tick. `kiln watch` with no flags is the cron entry,
	// and a command that unexpectedly never returned would be a nasty surprise
	// in a crontab.
	result, err := tick(ctx, watcher, branchesOnly, *dryRun)
	if err != nil {
		return wrapExit(ExitError, err)
	}
	printTick(io, result, *dryRun)

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
