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
