package attest

import (
	"encoding/base64"
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

// Envelope is a signed DSSE envelope, as warden emits it.
//
// Kiln never opens this to re-sign it. It reads the payload only to sanity-
// check what it is carrying — that the verdict passed and the commit matches —
// and then attaches the envelope byte for byte, so the signature a consumer
// checks is warden's rather than kiln's.
type Envelope struct {
	PayloadType string `json:"payloadType"`
	Payload     string `json:"payload"`
	Signatures  []struct {
		KeyID string `json:"keyid"`
		Sig   string `json:"sig"`
	} `json:"signatures"`
}

// Signed reports whether the envelope actually carries a signature. An
// envelope with none is a statement wearing a costume.
func (e Envelope) Signed() bool {
	return len(e.Signatures) > 0 && e.Signatures[0].Sig != ""
}

// KeyID names the key that signed it.
func (e Envelope) KeyID() string {
	if len(e.Signatures) == 0 {
		return ""
	}
	return e.Signatures[0].KeyID
}

// ParseEnvelope reads a signed DSSE envelope and the statement inside it.
//
// Kiln refuses an unsigned one rather than signing it itself. Signing somebody
// else's claim is exactly the substitution this whole arrangement exists to
// avoid: the resulting attestation would be kiln's, and a reader could only
// take kiln's word for warden's verdict.
func ParseEnvelope(data []byte) (Envelope, VSAStatement, error) {
	var e Envelope
	if err := json.Unmarshal(data, &e); err != nil {
		return Envelope{}, VSAStatement{}, fmt.Errorf("attest: parse envelope: %w", err)
	}
	if !e.Signed() {
		return Envelope{}, VSAStatement{}, fmt.Errorf(
			"attest: the source attestation is unsigned; kiln will not sign it on warden's behalf " +
				"(warden attest needs --sign)")
	}

	payload, err := base64.StdEncoding.DecodeString(e.Payload)
	if err != nil {
		return Envelope{}, VSAStatement{}, fmt.Errorf("attest: envelope payload is not base64: %w", err)
	}
	stmt, err := ParseVSA(payload)
	if err != nil {
		return Envelope{}, VSAStatement{}, err
	}
	return e, stmt, nil
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
