// Package poll is watch with the untrusted half removed.
//
// `kiln poll` looks at one branch and nothing else: no pull request heads, no
// tags, no GitHub API call. That makes it the surface for an operator who
// wants a build box with no token at all — nothing it discovers can come from
// a stranger, so there is nothing for the isolation policy to withhold.
//
// It is a constructor rather than a second implementation. Two discovery paths
// would be two chances to get the fork rules wrong.
package poll

import (
	"context"

	"go.klarlabs.de/kiln/internal/application/watch"
)

// Poller watches a single branch.
type Poller struct{ w *watch.Watcher }

// New wraps a watcher, forcing branch-only discovery. The caller's
// BranchesOnly setting is overwritten rather than respected: a Poller that
// could be configured to read pull requests would not be a Poller.
func New(w *watch.Watcher) *Poller {
	branchOnly := *w
	branchOnly.BranchesOnly = true
	// A poller has no use for the API even if a token happens to be present:
	// it never produces a pull request job for the answer to apply to.
	branchOnly.Forge = nil
	return &Poller{w: &branchOnly}
}

// Once performs a single branch check.
func (p *Poller) Once(ctx context.Context, dryRun bool) (watch.Result, error) {
	return p.w.Once(ctx, dryRun)
}

// Watcher exposes the underlying watcher, for surfaces that need to inspect
// its configuration.
func (p *Poller) Watcher() *watch.Watcher { return p.w }
