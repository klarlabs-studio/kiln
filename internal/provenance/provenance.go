// Package provenance decides whether Kiln may trust an existing Warden note
// instead of re-running the gate.
//
// The chain Kiln maintains has two links. Warden signs a note on the commit
// saying the configured checks ran and passed; Kiln records a digest saying
// this image was built from that commit. This package is the hinge between
// them: it is the only place that can shorten a run by trusting the first link.
//
// It fails closed in every direction. No pinned keys means no skip. An
// unsigned note means no skip. A policy that forbids skipping means no skip,
// whatever the note says. Missing `warden` means no skip — and then prove
// fails too, because a gate that cannot run has not passed.
package provenance

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.klarlabs.de/kiln/internal/execx"
	"go.klarlabs.de/kiln/internal/isolation"
)

// Decision is the verdict for one commit.
type Decision string

const (
	// Skipped means a trusted, signed note covers this commit; the prove phase
	// can be satisfied without re-running the checks.
	Skipped Decision = "skipped"
	// Reprove means the note is absent, unsigned, untrusted or unreadable.
	// The gate runs again. This is the safe default and the common case.
	Reprove Decision = "reprove"
	// Forbidden means the isolation policy does not permit skipping at all —
	// a fork pull request. The note is not even consulted: it was authored on
	// the same untrusted head, so reading it proves nothing.
	Forbidden Decision = "forbidden"
)

// Result carries the verdict and a human-readable reason. The reason is
// surfaced in the Check summary and the log, because "this run skipped the
// gate" is a claim an auditor is entitled to see justified.
type Result struct {
	Decision Decision
	Reason   string
}

// Skip reports whether the prove phase may be satisfied by the note.
func (r Result) Skip() bool { return r.Decision == Skipped }

// Verifier answers whether a commit carries trustworthy provenance.
type Verifier interface {
	Verify(ctx context.Context, repoDir, sha string, policy isolation.Policy) Result
}

// Warden verifies through the `warden verify` CLI.
//
// Shelling out rather than importing Warden is a deliberate boundary: Warden
// owns the note format, the hash chain and the signature scheme, and Kiln
// deliberately knows none of it. If Warden's verification rules change, Kiln
// inherits the change by running the newer binary.
type Warden struct {
	// Runner executes the verify command.
	Runner execx.Runner
	// Binary is the warden executable name (KILN_WARDEN).
	Binary string
	// TrustedKeys are operator-pinned signer keys. Empty disables skipping
	// entirely — see Verify.
	TrustedKeys []string
}

// NewWarden builds a verifier.
func NewWarden(r execx.Runner, binary string, trustedKeys []string) *Warden {
	if binary == "" {
		binary = "warden"
	}
	return &Warden{Runner: r, Binary: binary, TrustedKeys: trustedKeys}
}

// Verify resolves the skip decision.
//
// The ordering of the guards matters. Policy is consulted first, so a fork
// pull request never causes a subprocess to run at all — there is no verdict
// a note could produce that would change the answer, and not running is one
// less thing for a hostile head to influence.
//
// The trusted-key guard comes second. `warden verify` without --key will
// happily validate a note signed by any key, including one the pull request
// author generated five minutes ago. Requiring an operator-pinned key is what
// makes the skip meaningful, so an operator who has pinned nothing gets no
// skip rather than a weak one.
func (w *Warden) Verify(ctx context.Context, repoDir, sha string, policy isolation.Policy) Result {
	if !policy.Skip {
		return Result{Forbidden, "isolation policy forbids a provenance skip for this event"}
	}
	if len(w.TrustedKeys) == 0 {
		return Result{Reprove, "no KILN_TRUSTED_KEYS pinned: kiln only skips for a note signed by a key the operator trusts"}
	}
	if strings.TrimSpace(sha) == "" {
		return Result{Reprove, "no commit to verify"}
	}

	res, err := w.Runner.Run(ctx, execx.Cmd{
		Name: w.Binary,
		Args: []string{
			"verify",
			"--commit", sha,
			// --require-signed is implied by --key, but stating it makes the
			// intent legible in a process listing and survives a future
			// warden that decouples them.
			"--require-signed",
			"--key", strings.Join(w.TrustedKeys, ","),
			"--quiet",
		},
		Dir: repoDir,
		// The verifier reads notes; it never runs repo-authored commands, so
		// it does not need a scrubbed environment. It does need the operator's,
		// to find the key material warden was configured with.
	})
	if err == nil {
		return Result{Skipped, fmt.Sprintf("warden note on %s is signed by a trusted key", shortSHA(sha))}
	}

	var notFound *execx.NotFoundError
	if errors.As(err, &notFound) {
		// Not a skip and not, by itself, a failure: prove is about to run and
		// will fail on the same missing binary with a better message.
		return Result{Reprove, fmt.Sprintf("%s not on PATH: cannot check provenance", w.Binary)}
	}
	if code, ok := execx.ExitCode(err); ok {
		return Result{Reprove, fmt.Sprintf("warden verify exited %d: %s", code, verdictHint(res, code))}
	}
	return Result{Reprove, fmt.Sprintf("warden verify could not run: %v", err)}
}

// verdictHint turns warden's --quiet exit code into something an operator can
// act on. --quiet prints nothing by design, so without this the Check summary
// would read "exited 1" and leave the reader to guess.
func verdictHint(res execx.Result, code int) string {
	if detail := strings.TrimSpace(res.Stderr); detail != "" {
		return detail
	}
	switch code {
	case 1:
		return "no trusted, signed note covers this commit — re-proving"
	default:
		return "provenance not established — re-proving"
	}
}

// Always is a verifier that never skips. The engine uses it when the operator
// has opted out, and tests use it to pin behaviour.
type Always struct{ Reason string }

func (a Always) Verify(context.Context, string, string, isolation.Policy) Result {
	reason := a.Reason
	if reason == "" {
		reason = "provenance skip disabled"
	}
	return Result{Reprove, reason}
}

func shortSHA(sha string) string {
	if len(sha) <= 7 {
		return sha
	}
	return sha[:7]
}
