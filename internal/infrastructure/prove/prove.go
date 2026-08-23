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
	"os"
	"strings"

	"go.klarlabs.de/kiln/internal/application/ports"
	"go.klarlabs.de/kiln/internal/infrastructure/execx"
	"go.klarlabs.de/kiln/internal/infrastructure/worktree"
)

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

// Prove gates the commit. It returns nil only if the gate actually ran and
// actually passed.
func (w *Warden) Prove(ctx context.Context, req ports.ProveRequest) error {
	if strings.TrimSpace(req.SHA) == "" {
		return errors.New("prove: no commit")
	}

	// Check the toolchain before checking anything out. A missing binary is
	// certain to fail, and failing before the clone saves the operator a
	// worktree's worth of wait to learn it.
	if _, err := w.Runner.LookPath(w.WardenBin); err != nil {
		return fmt.Errorf("%w: %s is the source gate and kiln cannot pass a commit without it "+
			"(install warden or set KILN_WARDEN): %w", ports.ErrToolMissing, w.WardenBin, err)
	}
	if req.Nox {
		if _, err := w.Runner.LookPath(w.NoxBin); err != nil {
			return fmt.Errorf("%w: prove.nox is enabled but %s is not installed "+
				"(install nox, set KILN_NOX, or set prove.nox: false): %w", ports.ErrToolMissing, w.NoxBin, err)
		}
	}

	return worktree.With(ctx, w.Runner, req.RepoDir, req.SHA, func(dir string) error {
		return w.runGate(ctx, req, dir)
	})
}

func (w *Warden) runGate(ctx context.Context, req ports.ProveRequest, dir string) error {
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
		if code, isExit := execx.ExitCode(err); isExit {
			return fmt.Errorf("%w: %w", wardenVerdict(code), err)
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
			return fmt.Errorf("%w: nox scan reported findings: %w", ports.ErrGateFailed, err)
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
func (w *Warden) env(req ports.ProveRequest) []string {
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

// Warden's exit codes follow sysexits(3), and the distinction is the whole
// point of them: 75 and 78 both mean the gate never ran, which is not the
// claim that a gate rejection makes. Collapsing all three into one failure
// told an author their commit was bad when nothing had looked at it — a real
// box posted exactly that on a healthy main.
const (
	wardenContention  = 75 // EX_TEMPFAIL: a machine-global lock was held
	wardenEnvironment = 78 // EX_CONFIG: a step's toolchain is not installed
)

// wardenVerdict says what a warden exit code actually claims.
//
// Anything unrecognised is a gate failure, which is the safe direction: an
// exit code kiln has not been taught about must not excuse a commit.
func wardenVerdict(code int) error {
	switch code {
	case wardenContention:
		return ports.ErrGateUnavailable
	case wardenEnvironment:
		return ports.ErrToolMissing
	default:
		return ports.ErrGateFailed
	}
}
