package ports

import (
	"context"

	"go.klarlabs.de/kiln/internal/domain/isolation"
)

// Decision is the verdict for one commit.
type Decision string

// ProvenanceResult carries the verdict and a human-readable reason. The reason is
// surfaced in the Check summary and the log, because "this run skipped the
// gate" is a claim an auditor is entitled to see justified.
const (
	// DecisionSkipped means a trusted, signed note covers this commit; the prove phase
	// can be satisfied without re-running the checks.
	DecisionSkipped Decision = "skipped"
	// DecisionReprove means the note is absent, unsigned, untrusted or unreadable.
	// The gate runs again. This is the safe default and the common case.
	DecisionReprove Decision = "reprove"
	// DecisionForbidden means the isolation policy does not permit skipping at all —
	// a fork pull request. The note is not even consulted: it was authored on
	// the same untrusted head, so reading it proves nothing.
	DecisionForbidden Decision = "forbidden"
)

type ProvenanceResult struct {
	Decision Decision
	Reason   string
}

// ProvenanceVerifier answers whether a commit carries trustworthy provenance.
type ProvenanceVerifier interface {
	Verify(ctx context.Context, repoDir, sha string, policy isolation.Policy) ProvenanceResult
}

// AlwaysProvenance is a verifier that never skips. The engine uses it when the operator
// has opted out, and tests use it to pin behaviour.
type AlwaysProvenance struct{ Reason string }

func (r ProvenanceResult) Skip() bool { return r.Decision == DecisionSkipped }

func (a AlwaysProvenance) Verify(context.Context, string, string, isolation.Policy) ProvenanceResult {
	reason := a.Reason
	if reason == "" {
		reason = "provenance skip disabled"
	}
	return ProvenanceResult{DecisionReprove, reason}
}
