package ports

import "context"

// Ref is a ref and the commit it resolves to.
//
// Resolves, not points at: an annotated tag has an object id of its own, and
// that id is not something a worktree can check out or a warden note is bound
// to. Whoever implements this port peels it; the application only ever sees
// the commit.
type Ref struct {
	// Name is the full ref, e.g. refs/tags/v1.2.0.
	Name string
	// SHA is the commit it resolves to.
	SHA string
}

// Git is what the application needs to know from the repository.
//
// It is deliberately a set of questions rather than a way to run git. The
// application asks "which pull request heads are parked here", not "run
// for-each-ref with this format string and split the answer on tabs" — the
// format string, the tab, and the peeling of annotated tags are all the
// adapter's problem, and none of them are decisions the application makes.
type Git interface {
	// Fetch updates refs from a remote. A wildcard refspec that matches
	// nothing is not an error: a repository with no pull requests, or a remote
	// that is not a forge at all, simply has none. An exact refspec naming a
	// ref the remote lacks is.
	//
	// Several refspecs are one fetch, not one each: a repository with a few
	// hundred open pull requests would otherwise pay a few hundred round trips
	// a tick. How many fit on one command line is the adapter's problem.
	Fetch(ctx context.Context, dir, remote string, refspecs ...string) error
	// HeadSHA resolves a remote-tracking branch to its commit.
	HeadSHA(ctx context.Context, dir, remote, branch string) (string, error)
	// Tags lists every tag, resolved to the commit it covers.
	Tags(ctx context.Context, dir string) ([]Ref, error)
	// PullRefs lists the parked pull request heads, keyed by the ref a job
	// carries rather than by wherever they happen to be stored locally.
	PullRefs(ctx context.Context, dir string) ([]Ref, error)
	// RemotePullRefs lists the pull request heads the remote has, without
	// fetching any of them. A box that fetches only the open pull requests
	// still has to know what the closed ones were when it records its
	// baseline, and the remote is then the only place that knows.
	RemotePullRefs(ctx context.Context, dir, remote string) ([]Ref, error)
	// DropPullRefs removes parked pull request heads, named as PullRefs names
	// them. Nothing else ever removes one: a fetch by number does not prune
	// what it was not asked about.
	DropPullRefs(ctx context.Context, dir string, refs []string) error
	// Contains reports that a commit is already an ancestor of tip — how a box
	// with no forge token can still tell a merged pull request from an open
	// one, for the merges that leave a merge commit behind.
	Contains(ctx context.Context, dir, sha, tip string) (bool, error)
}
