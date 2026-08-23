package ports

import (
	"context"
	"errors"
	"io"

	"go.klarlabs.de/kiln/internal/domain/isolation"
)

// ProveRequest is one prove invocation.
type ProveRequest struct {
	// RepoDir is the repository to check the commit out of.
	RepoDir string
	// SHA is the commit to gate.
	SHA string
	// Policy decides whether the gate sees the operator's secrets.
	Policy isolation.Policy
	// Nox runs the optional scanner after the gate passes.
	Nox bool
	// Materialize names gitignored directories the gate needs, carried from
	// the clone into the worktree. Ignored unless Policy grants secrets.
	Materialize []string
	// Output, when set, receives the gate's live stdout/stderr so an operator
	// watching `kiln run` sees progress rather than a long silence.
	Output io.Writer
	// ServiceEnv carries the addresses of the run's service containers. The
	// gate is usually what needs them — a test suite talking to the database
	// beside it — so they have to survive the environment scrubbing below.
	ServiceEnv []string
}

// Prover gates a commit.
type Prover interface {
	Prove(ctx context.Context, req ProveRequest) error
}

// ProveFunc adapts a function to the Prover interface, for tests and for the dry
// surfaces that report a plan without gating anything.
type ProveFunc func(ctx context.Context, req ProveRequest) error

func (f ProveFunc) Prove(ctx context.Context, req ProveRequest) error { return f(ctx, req) }

// ErrGateFailed reports that the gate ran and rejected the commit. It is
// distinct from an infrastructure failure so the Check summary can say
// "your change did not pass" rather than "kiln broke".
var ErrGateFailed = errors.New("warden gate failed")

// ErrToolMissing reports that a step's toolchain or dependencies are not
// installed, so the gate never ran. Warden exits 78 (EX_CONFIG) for this.
//
// It is not a gate failure and must never be reported as one: saying "your
// change did not pass" when nothing looked at the change is the one direction
// this error must not go. A real box posted exactly that on a healthy main.
var ErrToolMissing = errors.New("gate could not run: toolchain or dependencies missing")

// ErrGateUnavailable reports that another process held a machine-global lock
// until the wait budget ran out. Warden exits 75 (EX_TEMPFAIL) for this.
//
// Also not a gate failure: nothing is wrong with the change and nothing is
// missing from the box, so the honest answer is "not yet".
var ErrGateUnavailable = errors.New("gate could not run: another process holds the lock")
