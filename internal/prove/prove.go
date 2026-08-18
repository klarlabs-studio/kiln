// Package prove runs the source gate on a commit.
//
// Kiln does not decide what "passing" means — `.warden.yaml` does. This
// package's whole job is to put Warden in front of the right tree with the
// right environment and report what it said. Everything interesting about
// which checks run, in what order, and with what verdict lives in Warden.
//
// Two properties are enforced here and nowhere else:
//
//   - The gate runs against a disposable checkout of the exact commit, never
//     the operator's working copy.
//   - On a fork pull request the gate runs with a scrubbed environment,
//     because proving means executing attacker-authored code.
package prove

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"go.klarlabs.de/kiln/internal/execx"
	"go.klarlabs.de/kiln/internal/isolation"
	"go.klarlabs.de/kiln/internal/worktree"
)

// Request is one prove invocation.
type Request struct {
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
	Prove(ctx context.Context, req Request) error
}

// Warden is the real prover: a disposable worktree plus
// `warden run pre-push --attest-only` plus an optional `nox scan`.
type Warden struct {
	Runner execx.Runner
	// WardenBin and NoxBin are the executables (KILN_WARDEN, KILN_NOX).
	WardenBin string
	NoxBin    string
}

// NewWarden builds a prover.
func NewWarden(r execx.Runner, wardenBin, noxBin string) *Warden {
	if wardenBin == "" {
		wardenBin = "warden"
	}
	if noxBin == "" {
		noxBin = "nox"
	}
	return &Warden{Runner: r, WardenBin: wardenBin, NoxBin: noxBin}
}

// ErrGateFailed reports that the gate ran and rejected the commit. It is
// distinct from an infrastructure failure so the Check summary can say
// "your change did not pass" rather than "kiln broke".
var ErrGateFailed = errors.New("warden gate failed")

// ErrToolMissing reports that a required binary is not installed.
//
// This is a prove *failure*, never a skip. A missing `warden` means the checks
// did not run, and a commit whose checks did not run has not passed them —
// treating the absence as a pass would turn an operator's broken install into
// a silently ungated pipeline, which is the one outcome Kiln exists to prevent.
var ErrToolMissing = errors.New("required tool missing")

// Prove gates the commit. It returns nil only if the gate actually ran and
// actually passed.
func (w *Warden) Prove(ctx context.Context, req Request) error {
	if strings.TrimSpace(req.SHA) == "" {
		return errors.New("prove: no commit")
	}

	// Check the toolchain before checking anything out. A missing binary is
	// certain to fail, and failing before the clone saves the operator a
	// worktree's worth of wait to learn it.
	if _, err := w.Runner.LookPath(w.WardenBin); err != nil {
		return fmt.Errorf("%w: %s is the source gate and kiln cannot pass a commit without it "+
			"(install warden or set KILN_WARDEN): %w", ErrToolMissing, w.WardenBin, err)
	}
	if req.Nox {
		if _, err := w.Runner.LookPath(w.NoxBin); err != nil {
			return fmt.Errorf("%w: prove.nox is enabled but %s is not installed "+
				"(install nox, set KILN_NOX, or set prove.nox: false): %w", ErrToolMissing, w.NoxBin, err)
		}
	}

	return worktree.With(ctx, w.Runner, req.RepoDir, req.SHA, func(dir string) error {
		return w.runGate(ctx, req, dir)
	})
}

func (w *Warden) runGate(ctx context.Context, req Request, dir string) error {
	env := w.env(req)

	if _, err := w.Runner.Run(ctx, execx.Cmd{
		Name: w.WardenBin,
		// --attest-only is not optional, and it is not a tuning knob.
		//
		// `warden run pre-push` is a git hook implementation: it gates AND then
		// performs the push, fast-forwarding the branch to the checked tree.
		// Invoked bare from here it does two unacceptable things — it aborts
		// with "branch changed mid-run" because a detached worktree has no
		// branch to advance, and, worse, on a checkout that did have one it
		// would push from a build box.
		//
		// --attest-only is warden's CI mode, and its own documentation
		// describes this exact situation: run the full gate, write the
		// provenance note, move nothing. "A gate that pushed from CI would race
		// the next human push and fail on a stale ref."
		//
		// Kiln proves commits. It does not move refs.
		Args:   []string{"run", "pre-push", "--attest-only"},
		Dir:    dir,
		Env:    env,
		Stdout: req.Output,
		Stderr: req.Output,
	}); err != nil {
		if _, isExit := execx.ExitCode(err); isExit {
			return fmt.Errorf("%w: %w", ErrGateFailed, err)
		}
		return fmt.Errorf("prove: %w", err)
	}

	if !req.Nox {
		return nil
	}

	// The scanner runs after the gate, not instead of it. Nox is a scanner
	// Kiln may invoke, not a second CI system: if `.warden.yaml` already runs
	// it as a step, this is redundant and `prove.nox: false` is the answer.
	if _, err := w.Runner.Run(ctx, execx.Cmd{
		Name:   w.NoxBin,
		Args:   []string{"scan", "."},
		Dir:    dir,
		Env:    env,
		Stdout: req.Output,
		Stderr: req.Output,
	}); err != nil {
		if _, isExit := execx.ExitCode(err); isExit {
			return fmt.Errorf("%w: nox scan reported findings: %w", ErrGateFailed, err)
		}
		return fmt.Errorf("prove: nox scan: %w", err)
	}
	return nil
}

// env builds the child environment.
//
// A run permitted to hold secrets inherits the operator's environment
// unchanged (nil means inherit) — Warden needs its signing key, and the build
// steps a repository configures may legitimately need credentials.
//
// A run *not* permitted to hold secrets gets a scrubbed copy. This is the
// concrete meaning of the fork row in the isolation matrix: the code about to
// execute was authored by whoever opened the pull request, so the environment
// it executes in must not contain a registry password to steal.
func (w *Warden) env(req Request) []string {
	if req.Policy.Secrets {
		if len(req.ServiceEnv) == 0 {
			return nil
		}
		// Non-nil replaces the whole environment, so an inherited one has to
		// be rebuilt explicitly once there is anything to add to it.
		return append(os.Environ(), req.ServiceEnv...)
	}
	scrubbed := append(execx.Scrub(os.Environ()),
		// Mark the isolation so a repo's own checks can behave accordingly —
		// skipping an integration test that needs a credential, say — instead
		// of failing confusingly on a variable that is not there.
		"KILN_ISOLATED=1",
	)
	// Service addresses are not secrets — they are ephemeral loopback ports
	// on this box — and a fork's tests need the database as much as anyone's.
	return append(scrubbed, req.ServiceEnv...)
}

// Func adapts a function to the Prover interface, for tests and for the dry
// surfaces that report a plan without gating anything.
type Func func(ctx context.Context, req Request) error

func (f Func) Prove(ctx context.Context, req Request) error { return f(ctx, req) }
