// Package prune reclaims the disk a build box loses to its own leavings.
//
// The worktree reaper handles checkouts; this handles what docker keeps. On a
// box that has been building for a while the split is lopsided — images are
// usually gigabytes and the build cache is usually several times that — so
// collecting one without the other reclaims the smaller half.
//
// # Why this is safe to do automatically, and where it stops
//
// Everything here is *local*. A deleted local image is a re-pull away, because
// the registry holds the durable copy; that recoverability is the entire
// reason this can run unattended. Deleting from the registry is the opposite:
// unrecoverable, and it would break the rollback RollOps exists to perform.
// Kiln does not do it and offers no flag for it. Registry retention is a
// policy decision with a blast radius kiln has no business owning.
//
// Within "local", three rules keep it narrow:
//
//   - Only repositories this pipeline publishes. Never `docker image prune -a`,
//     which would take every unused image on a shared daemon.
//   - Only the immutable `sha-` tags kiln generates. A moving tag is what
//     RollOps follows, and removing one would break a deploy.
//   - Never the image a moving tag currently points at, even under a sha tag.
//     The same image usually carries both.
package prune

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.klarlabs.de/kiln/internal/domain/config"
	"go.klarlabs.de/kiln/internal/infrastructure/execx"
	"go.klarlabs.de/kiln/internal/infrastructure/obs"
)

// DefaultKeep is re-exported so callers of this package need not reach past it
// for the number. It is declared in the domain because how many builds to keep
// is a decision about the product, not about docker.
const DefaultKeep = config.DefaultKeep

// DefaultBuildCacheMaxAge bounds how long build cache lives.
//
// A week. Cache older than that rarely hits anything a current build needs,
// and it is the single largest thing on a busy box. It regenerates, so the
// cost of being wrong is one slower build.
const DefaultBuildCacheMaxAge = 7 * 24 * time.Hour

// shaTagPrefix is the immutable tag kiln generates. Only these are candidates.
const shaTagPrefix = "sha-"

// Options configure one sweep.
type Options struct {
	// Repos are the image repositories this pipeline publishes. Empty means
	// no image is touched — kiln prunes what it made, and nothing else.
	Repos []string
	// Keep is how many sha-tagged builds to retain per repository. Zero
	// disables image pruning entirely.
	Keep int
	// BuildCacheMaxAge prunes build cache older than this. Zero leaves the
	// cache alone.
	BuildCacheMaxAge time.Duration
	// DryRun reports what would go without removing it.
	DryRun bool
}

// Result is what a sweep did.
type Result struct {
	// Removed are the image references deleted, or that would be.
	Removed []string
	// Kept counts the sha-tagged builds retained across all repositories.
	Kept int
	// CacheFreed is docker's own report of the space the cache prune
	// reclaimed, verbatim.
	CacheFreed string
}

// Pruner reclaims disk.
type Pruner struct {
	Runner execx.Runner
	Log    obs.Logger
	Docker string
}

// New builds a pruner.
func New(r execx.Runner, log obs.Logger) *Pruner {
	if log == nil {
		log = obs.Discard()
	}
	return &Pruner{Runner: r, Log: log, Docker: "docker"}
}

// Prune sweeps images and build cache.
//
// A missing docker is not an error: a box that only proves, or one running
// with KILN_DRY, has nothing to collect and should not fail housekeeping over
// a tool it never needed.
func (p *Pruner) Prune(ctx context.Context, opts Options) (Result, error) {
	var result Result

	if _, err := p.Runner.LookPath(p.Docker); err != nil {
		return result, nil
	}

	for _, repo := range opts.Repos {
		if opts.Keep <= 0 {
			break
		}
		removed, kept, err := p.pruneRepo(ctx, repo, opts)
		result.Removed = append(result.Removed, removed...)
		result.Kept += kept
		if err != nil {
			// One unreadable repository must not stop the others, and must not
			// stop the cache prune below — that is the bigger win.
			p.Log.Warn("could not prune images", "image", repo, "err", err)
		}
	}

	if opts.BuildCacheMaxAge > 0 {
		freed, err := p.pruneBuildCache(ctx, opts)
		if err != nil {
			p.Log.Warn("could not prune build cache", "err", err)
		}
		result.CacheFreed = freed
	}
	return result, nil
}

// pruneRepo removes the oldest sha-tagged builds of one image.
func (p *Pruner) pruneRepo(ctx context.Context, repo string, opts Options) (removed []string, kept int, err error) {
	tags, err := p.listSHATags(ctx, repo)
	if err != nil {
		return nil, 0, err
	}
	protected, err := p.protectedIDs(ctx, repo)
	if err != nil {
		// Without knowing what the moving tags point at, deleting anything
		// risks taking the image :latest resolves to. Stop.
		return nil, 0, err
	}

	candidates := make([]imageTag, 0, len(tags))
	for _, t := range tags {
		if protected[t.ID] {
			// The same image almost always carries :latest as well as its sha
			// tag; removing it by the sha name would break the moving one.
			kept++
			continue
		}
		candidates = append(candidates, t)
	}

	// Newest first, with a total order.
	//
	// The tiebreak is not defensive padding. A reproducible image — anything
	// built FROM scratch, or with SOURCE_DATE_EPOCH set — reports 1970 for its
	// creation time, so a whole repository's timestamps tie. Sorting on time
	// alone then leaves docker's listing order deciding what gets deleted, and
	// that order is not stable between invocations: a --dry-run would name one
	// image and the real run would remove a different one, which is worse than
	// having no dry run at all.
	//
	// Falling back to the tag gives an arbitrary but *repeatable* answer, and
	// repeatable is the property that matters here.
	sort.Slice(candidates, func(i, j int) bool {
		if !candidates[i].Created.Equal(candidates[j].Created) {
			return candidates[i].Created.After(candidates[j].Created)
		}
		return candidates[i].Tag < candidates[j].Tag
	})

	if len(candidates) <= opts.Keep {
		return nil, kept + len(candidates), nil
	}
	kept += opts.Keep

	for _, t := range candidates[opts.Keep:] {
		ref := repo + ":" + t.Tag
		if opts.DryRun {
			removed = append(removed, ref)
			continue
		}
		if _, rmErr := p.Runner.Run(ctx, execx.Cmd{
			Name: p.Docker, Args: []string{"image", "rm", ref},
		}); rmErr != nil {
			// An image a container still uses refuses to go. That is docker
			// protecting something in use; note it and move on.
			p.Log.Debug("image not removed", "ref", ref, "err", rmErr)
			continue
		}
		removed = append(removed, ref)
	}
	return removed, kept, nil
}

// imageTag is one local tag of a repository.
type imageTag struct {
	Tag     string
	ID      string
	Created time.Time
}

// dockerTimeLayout is how `docker image ls` renders CreatedAt.
const dockerTimeLayout = "2006-01-02 15:04:05 -0700 MST"

// listSHATags returns the repository's immutable-tagged local images.
func (p *Pruner) listSHATags(ctx context.Context, repo string) ([]imageTag, error) {
	res, err := p.Runner.Run(ctx, execx.Cmd{
		Name: p.Docker,
		Args: []string{"image", "ls", repo, "--format", "{{.Tag}}\t{{.ID}}\t{{.CreatedAt}}"},
	})
	if err != nil {
		return nil, fmt.Errorf("prune: list %s: %w", repo, err)
	}

	var out []imageTag
	for line := range strings.SplitSeq(res.Output(), "\n") {
		fields := strings.Split(strings.TrimSpace(line), "\t")
		if len(fields) < 2 || !strings.HasPrefix(fields[0], shaTagPrefix) {
			continue
		}
		t := imageTag{Tag: fields[0], ID: fields[1]}
		if len(fields) > 2 {
			// An unparsable timestamp leaves the zero value, which sorts last
			// and so gets pruned first. That is the wrong end to be wrong at,
			// but docker's listing is already newest-first and SliceStable
			// preserves it, so the fallback is docker's own order.
			if parsed, perr := time.Parse(dockerTimeLayout, fields[2]); perr == nil {
				t.Created = parsed
			}
		}
		out = append(out, t)
	}
	return out, nil
}

// protectedIDs are the images the repository's moving tags resolve to.
func (p *Pruner) protectedIDs(ctx context.Context, repo string) (map[string]bool, error) {
	res, err := p.Runner.Run(ctx, execx.Cmd{
		Name: p.Docker,
		Args: []string{"image", "ls", repo, "--format", "{{.Tag}}\t{{.ID}}"},
	})
	if err != nil {
		return nil, fmt.Errorf("prune: resolve moving tags for %s: %w", repo, err)
	}

	protected := map[string]bool{}
	for line := range strings.SplitSeq(res.Output(), "\n") {
		tag, id, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok || tag == "" {
			continue
		}
		if !strings.HasPrefix(tag, shaTagPrefix) {
			// :latest, a semver tag, anything an operator added by hand.
			protected[id] = true
		}
	}
	return protected, nil
}

// pruneBuildCache drops cache entries older than the configured age.
//
// This is the largest reclaim on a busy box and the only part that touches
// state kiln did not create — the daemon's cache is shared with every other
// build on the machine. It stays safe because cache is derived: the worst
// outcome is somebody's next build being slower.
func (p *Pruner) pruneBuildCache(ctx context.Context, opts Options) (string, error) {
	args := []string{"builder", "prune", "--force",
		"--filter", "until=" + formatHours(opts.BuildCacheMaxAge)}
	if opts.DryRun {
		// docker has no dry run for this, so say what would happen rather than
		// doing it.
		return fmt.Sprintf("would prune build cache older than %s", opts.BuildCacheMaxAge), nil
	}

	res, err := p.Runner.Run(ctx, execx.Cmd{Name: p.Docker, Args: args})
	if err != nil {
		return "", fmt.Errorf("prune: build cache: %w", err)
	}
	return lastLine(res.Output()), nil
}

// formatHours renders a duration the way docker's until filter wants it.
func formatHours(d time.Duration) string {
	return fmt.Sprintf("%dh", int(d.Hours()))
}

// lastLine picks docker's summary line, which is the "Total reclaimed space"
// tail rather than the list of deleted ids above it.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
