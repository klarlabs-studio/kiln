package attest_test

import (
	"encoding/json"
	"strings"
	"testing"

	"go.klarlabs.de/kiln/internal/infrastructure/attest"
)

// wardenVSA is what `warden attest --predicate vsa` actually printed for a
// gated commit in this repository. Copied verbatim rather than invented, so a
// change in warden's output shows up here as a failing parse.
const wardenVSA = `{
  "_type": "https://in-toto.io/Statement/v1",
  "subject": [
    {
      "name": "git+commit",
      "digest": {
        "gitCommit": "8115748887775797df0398ed27080998f4d0c8d7"
      }
    }
  ],
  "predicateType": "https://slsa.dev/verification_summary/v1",
  "predicate": {
    "verifier": {
      "id": "https://warden.klarlabs.de",
      "version": { "warden": "0.28.0" }
    },
    "timeVerified": "2026-08-17T21:26:51Z",
    "resourceUri": "git+ssh://git@github.com/klarlabs-studio/kiln.git@8115748887775797df0398ed27080998f4d0c8d7",
    "policy": {
      "uri": "git+ssh://git@github.com/klarlabs-studio/kiln.git@8115748887775797df0398ed27080998f4d0c8d7#.warden.yaml"
    },
    "verificationResult": "PASSED",
    "verifiedLevels": ["WARDEN_SOURCE_GATED", "WARDEN_SOURCE_SIGNED"]
  }
}`

func TestWardensOwnOutputParses(t *testing.T) {
	vsa, err := attest.ParseVSA([]byte(wardenVSA))
	if err != nil {
		t.Fatalf("ParseVSA: %v", err)
	}

	if !vsa.Passed() {
		t.Error("a PASSED verdict did not read as passed")
	}
	// Naming the verifier is the whole point: this is warden's claim, not
	// kiln's summary of it.
	if vsa.Predicate.Verifier.ID != "https://warden.klarlabs.de" {
		t.Errorf("verifier = %q", vsa.Predicate.Verifier.ID)
	}
	if !strings.Contains(vsa.Predicate.Policy.URI, ".warden.yaml") {
		t.Errorf("policy uri = %q, want the file the commit was measured against", vsa.Predicate.Policy.URI)
	}
	if len(vsa.Predicate.VerifiedLevels) != 2 {
		t.Errorf("verifiedLevels = %v", vsa.Predicate.VerifiedLevels)
	}
}

func TestTheCommitTiesTheSummaryToABuild(t *testing.T) {
	vsa, err := attest.ParseVSA([]byte(wardenVSA))
	if err != nil {
		t.Fatal(err)
	}

	const want = "8115748887775797df0398ed27080998f4d0c8d7"
	if got := vsa.SourceCommit(); got != want {
		t.Errorf("SourceCommit = %q, want %q", got, want)
	}
}

func TestTheCommitSurvivesCosignRewritingTheSubject(t *testing.T) {
	// cosign replaces the subject with the artifact when the summary is
	// attached to one, so the resourceUri has to carry the commit too — it is
	// what a consumer joins against the build provenance.
	var s attest.VSAStatement
	if err := json.Unmarshal([]byte(wardenVSA), &s); err != nil {
		t.Fatal(err)
	}
	s.Subject = []attest.Subject{{
		Name:   "ghcr.io/x/y",
		Digest: map[string]string{"sha256": "aaa"},
	}}

	if got := s.SourceCommit(); got != "8115748887775797df0398ed27080998f4d0c8d7" {
		t.Errorf("SourceCommit = %q, want it recovered from resourceUri", got)
	}
}

func TestAnSSHRemoteIsNotMistakenForACommit(t *testing.T) {
	// git+ssh://git@github.com/o/r.git has an @ in it and no commit. Reading
	// the tail regardless makes "github.com/o/r.git" the commit — which is
	// then compared against the real one and refused, with a message naming a
	// hostname where an operator expects a sha.
	var s attest.VSAStatement
	if err := json.Unmarshal([]byte(wardenVSA), &s); err != nil {
		t.Fatal(err)
	}
	s.Subject = nil
	s.Predicate.ResourceURI = "git+ssh://git@github.com/klarlabs-studio/kiln.git"

	if got := s.SourceCommit(); got != "" {
		t.Errorf("SourceCommit = %q, want nothing: that URI names no commit", got)
	}
}

func TestAnAbbreviatedCommitIsNotACommit(t *testing.T) {
	// A prefix is not an identity. Accepting one would make every join
	// between a summary and a build provenance a prefix comparison.
	var s attest.VSAStatement
	if err := json.Unmarshal([]byte(wardenVSA), &s); err != nil {
		t.Fatal(err)
	}
	s.Subject = nil
	s.Predicate.ResourceURI = "git+ssh://git@github.com/o/r.git@8115748"

	if got := s.SourceCommit(); got != "" {
		t.Errorf("SourceCommit = %q, want nothing for an abbreviated id", got)
	}
}

func TestAFailedVerdictIsNotAPass(t *testing.T) {
	failed := strings.Replace(wardenVSA, `"PASSED"`, `"FAILED"`, 1)

	vsa, err := attest.ParseVSA([]byte(failed))
	if err != nil {
		t.Fatal(err)
	}
	if vsa.Passed() {
		t.Error("a FAILED verdict read as passed")
	}
}

func TestBuildProvenanceIsNotAVerificationSummary(t *testing.T) {
	build, err := attest.Build(attest.Input{
		SubjectName: "ghcr.io/x/y", SubjectDigest: "sha256:aaa", SHA: "abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := build.JSON()

	// The two are different claims by different authorities; confusing them
	// would let one stand in for the other.
	if _, err := attest.ParseVSA(raw); err == nil {
		t.Error("ParseVSA accepted build provenance")
	}
}

func TestVSAPredicateJSONIsTheBodyCosignWants(t *testing.T) {
	vsa, err := attest.ParseVSA([]byte(wardenVSA))
	if err != nil {
		t.Fatal(err)
	}
	body, err := vsa.PredicateJSON()
	if err != nil {
		t.Fatal(err)
	}

	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	// Same lesson as build provenance: hand cosign a whole statement and it
	// nests it inside its own.
	for _, absent := range []string{"_type", "predicateType", "subject"} {
		if _, found := doc[absent]; found {
			t.Errorf("the predicate body carries %q", absent)
		}
	}
	if doc["verificationResult"] != "PASSED" {
		t.Errorf("body lost the verdict: %v", doc)
	}
}
