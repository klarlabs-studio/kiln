package cli

import (
	"context"
	"sort"

	"go.klarlabs.de/kiln/internal/boot"
	"go.klarlabs.de/kiln/internal/infrastructure/prune"
)

// runPrune reclaims the disk docker holds for this pipeline.
//
// Watch ticks do this automatically; the command exists for the box that only
// runs `kiln run`, and for an operator who wants to see the plan before
// anything goes. `--dry-run` is the first thing to reach for.
func runPrune(ctx context.Context, args []string, io IO) error {
	fs := newFlagSet("prune", io)
	dir := fs.String("dir", "", "repository directory")
	pipelinePath := fs.String("pipeline", "", "pipeline file (default <dir>/.kiln.yaml)")
	keep := fs.Int("keep", -1, "sha-tagged builds to retain per image (overrides the pipeline)")
	cache := fs.String("cache-older-than", "", "prune build cache older than this, e.g. 168h")
	dryRun := fs.Bool("dry-run", false, "report what would go, and remove nothing")
	if err := fs.Parse(args); err != nil {
		return wrapExit(ExitUsage, err)
	}

	deps, err := boot.Build(ctx, boot.Options{Dir: *dir, PipelinePath: *pipelinePath})
	if err != nil {
		return wrapExit(ExitConfig, err)
	}

	images := deps.Pipeline.PrunableImages()
	repos := make([]string, 0, len(images))
	retain := 0
	for image, n := range images {
		repos = append(repos, image)
		if n > retain {
			retain = n
		}
	}
	sort.Strings(repos)
	if *keep >= 0 {
		retain = *keep
	}

	maxAge := deps.Env.BuildCacheMaxAge
	if *cache != "" {
		parsed, err := parseInterval(*cache)
		if err != nil {
			return wrapExit(ExitUsage, err)
		}
		maxAge = parsed
	}

	if len(repos) == 0 && maxAge == 0 {
		io.print("nothing to prune: this pipeline publishes no images, and no cache age is set\n")
		return nil
	}

	// A one-shot prune takes the repository lock: it removes images a
	// concurrent build may be about to tag, and the lock is what stops that.
	return withRepoLock(deps.Dir, "kiln prune", busyRefusal, func() error {
		res, err := prune.New(deps.Runner, deps.Log).Prune(ctx, prune.Options{
			Repos: repos, Keep: retain, BuildCacheMaxAge: maxAge, DryRun: *dryRun,
		})
		if err != nil {
			return wrapExit(ExitError, err)
		}

		verb := "removed"
		if *dryRun {
			verb = "would remove"
		}
		for _, ref := range res.Removed {
			io.printf("%-13s %s\n", verb, ref)
		}
		io.printf("%-13s %d build(s) across %d image(s)\n", "kept", res.Kept, len(repos))
		if res.CacheFreed != "" {
			io.printf("%-13s %s\n", "build cache", res.CacheFreed)
		}
		return nil
	})
}
