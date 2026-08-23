package prune

import (
	"context"

	"go.klarlabs.de/kiln/internal/application/ports"
)

// Prune satisfies ports.Pruner, translating the application's request into
// this package's own Options and Result.
func (p *Pruner) Prune(ctx context.Context, opts ports.PruneOptions) (ports.PruneResult, error) {
	res := p.prune(ctx, Options{
		Repos:            opts.Repos,
		Keep:             opts.Keep,
		BuildCacheMaxAge: opts.BuildCacheMaxAge,
		DryRun:           opts.DryRun,
	})
	// The port keeps an error in its signature because a different pruner
	// could fail; this one reports per-repository trouble through the log and
	// carries on, so there is never one to return.
	return ports.PruneResult{Removed: res.Removed, Kept: res.Kept, CacheFreed: res.CacheFreed}, nil
}
