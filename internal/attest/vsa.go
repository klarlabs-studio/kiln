package attest

import (
	"encoding/json"
	"fmt"
	"strings"
)

// VSAPredicateType is the SLSA Verification Summary Attestation.
const VSAPredicateType = "https://slsa.dev/verification_summary/v1"

// VSA is the subset of a verification summary Kiln reads and republishes.
//
// Kiln does not author these — Warden does. Kiln's only job is to carry one
// intact from the commit to the artifact, so a consumer holding an image can
// see the source verdict without needing a clone of the repository to check
// the note themselves.
type VSAStatement struct {
	Type          string    `json:"_type"`
	Subject       []Subject `json:"subject"`
	PredicateType string    `json:"predicateType"`
	Predicate     VSA       `json:"predicate"`
}

// VSA is the predicate body.
type VSA struct {
	Verifier struct {
		ID      string            `json:"id"`
		Version map[string]string `json:"version,omitempty"`
	} `json:"verifier"`
	TimeVerified string `json:"timeVerified"`
	// ResourceURI names what was verified. For a source gate this is the git
	// commit, and it is the field that ties the summary to a build.
	ResourceURI string `json:"resourceUri"`
	Policy      struct {
		URI string `json:"uri"`
	} `json:"policy"`
	VerificationResult string   `json:"verificationResult"`
	VerifiedLevels     []string `json:"verifiedLevels,omitempty"`
}

// ParseVSA reads a verification summary statement.
func ParseVSA(data []byte) (VSAStatement, error) {
	var s VSAStatement
	if err := json.Unmarshal(data, &s); err != nil {
		return VSAStatement{}, fmt.Errorf("attest: parse verification summary: %w", err)
	}
	if s.PredicateType != VSAPredicateType {
		return VSAStatement{}, fmt.Errorf("attest: predicate type %q, want %q", s.PredicateType, VSAPredicateType)
	}
	return s, nil
}

// PredicateJSON renders the body cosign's --predicate flag wants, for the same
// reason build provenance does: cosign builds the statement itself.
func (s VSAStatement) PredicateJSON() ([]byte, error) {
	data, err := json.MarshalIndent(s.Predicate, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("attest: encode verification summary: %w", err)
	}
	return append(data, '\n'), nil
}

// Passed reports a clean verdict.
func (s VSAStatement) Passed() bool {
	return strings.EqualFold(s.Predicate.VerificationResult, "PASSED")
}

// SourceCommit extracts the commit the summary is about.
//
// Warden writes resourceUri as a git URI ending in @<sha>, and the subject
// carries the same commit as a gitCommit digest. The subject is preferred
// because it is the structured form; the URI is the fallback, since cosign
// rewrites the subject when the summary is attached to an artifact.
func (s VSAStatement) SourceCommit() string {
	for _, sub := range s.Subject {
		if c := sub.Digest["gitCommit"]; c != "" {
			return c
		}
	}
	if i := strings.LastIndex(s.Predicate.ResourceURI, "@"); i >= 0 {
		return s.Predicate.ResourceURI[i+1:]
	}
	return ""
}
