// Package trust is the established context a run is allowed to proceed under.
//
// A caller may request work. It does not define reality. Event, fork status
// and publishability are derived from evidence — a signed webhook, a fetched
// ref, a forge lookup — and only then handed to the engine. The zero Context
// is untrusted: no secrets, no publish, no provenance skip.
package trust

import (
	"strings"

	"go.klarlabs.de/kiln/internal/domain/isolation"
)

// EvidenceMode is how complete the source half of the chain must be
// before a publish is allowed to succeed.
type EvidenceMode string

const (
	// EvidenceRequired fails a publish that cannot attach a source verdict.
	EvidenceRequired EvidenceMode = "required"
	// EvidenceBestEffort publishes build provenance alone and records that
	// the source half is incomplete. For adoption, not production.
	EvidenceBestEffort EvidenceMode = "best-effort"
)

// ParseEvidenceMode accepts the two documented spellings. Anything else is
// refused: a typo that quietly selected best-effort would be a policy that
// does nothing.
func ParseEvidenceMode(s string) (EvidenceMode, bool) {
	switch EvidenceMode(strings.TrimSpace(s)) {
	case EvidenceRequired, EvidenceBestEffort:
		return EvidenceMode(s), true
	default:
		return "", false
	}
}

// ResolveEvidence picks the effective mode.
//
// An explicit pipeline value wins. Otherwise a box that has pinned trusted
// keys is a production box and runs strict; a box that has not is still
// adopting and stays best-effort.
func ResolveEvidence(explicit string, hasTrustedKeys bool) EvidenceMode {
	if mode, ok := ParseEvidenceMode(explicit); ok {
		return mode
	}
	if hasTrustedKeys {
		return EvidenceRequired
	}
	return EvidenceBestEffort
}

// PolicyIdentity names the build policy that governed a run.
//
// The source being built and the policy controlling the build are different
// objects. Today the policy is the operator checkout's .kiln.yaml; that is
// a security decision, not an accident, and it is recorded so a verifier
// can say "commit X produced artifact Y under policy Z".
type PolicyIdentity struct {
	// Source is "operator" when the box checkout supplied the file, or
	// "default" when no .kiln.yaml existed.
	Source string `json:"source"`
	// Path is the file, relative to the checkout when it came from one.
	Path string `json:"path,omitempty"`
	// Digest is sha256:<hex> of the file bytes. Empty when there was no file.
	Digest string `json:"digest,omitempty"`
}

const (
	PolicyOperator = "operator"
	PolicyDefault  = "default"
)

// Context is the established trust classification for one run.
//
// Constructed only after Resolve (or an evidence-bearing surface such as
// the webhook parser) has done its work. The engine consumes this rather
// than reconstructing trust from caller-supplied strings.
type Context struct {
	Event isolation.Event
	Fork  bool
	Ref   string
	SHA   string
	// Established records that event and fork came from evidence, not from
	// a caller assertion. Surfaces that cannot establish that fact leave
	// this false and must not be granted publish authority without a
	// membership check.
	Established bool
}

// Policy is the isolation decision this context implies.
func (c Context) Policy() isolation.Policy {
	return isolation.For(c.Event, c.Fork)
}

// Claim is what a surface asked for. It is an identifier plus a hint, not
// a grant of authority.
type Claim struct {
	SHA   string
	Event isolation.Event
	Ref   string
	PR    int
	// Fork is a floor: true forces untrusted handling. False means
	// "please resolve", not "this is same-repo".
	Fork bool
	// Established is set by surfaces that already derived event and fork
	// from evidence (watch discovery, a verified webhook).
	Established bool
}
