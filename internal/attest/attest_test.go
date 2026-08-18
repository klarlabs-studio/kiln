package attest

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func input() Input {
	return Input{
		SubjectName:   "ghcr.io/felixgeelhaar/glossa-api",
		SubjectDigest: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		Repo:          "felixgeelhaar/glossa",
		SHA:           "c3f7aca23fa4bfa8d65b3741f46c509713cd618e",
		Ref:           "refs/tags/v0.2.0",
		Event:         "tag",
		ArtifactKind:  "image",
		Config:        "Dockerfile",
		GateTool:      "warden",
		GateReproved:  true,
		GateReason:    "checks ran",
		KilnVersion:   "v0.1.0",
		ToolVersions:  map[string]string{"warden": "0.28.0"},
		InvocationID:  "run-20260817T220632Z-29ebd9f6",
		StartedOn:     time.Date(2026, 8, 17, 22, 6, 32, 0, time.UTC),
		FinishedOn:    time.Date(2026, 8, 17, 22, 7, 0, 0, time.UTC),
	}
}

func build(t *testing.T, in Input) Statement {
	t.Helper()
	s, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return s
}

func TestStatementShapeIsTheProtocol(t *testing.T) {
	s := build(t, input())

	// These are wire constants a verifier matches exactly, not settings.
	if s.Type != "https://in-toto.io/Statement/v1" {
		t.Errorf("_type = %q", s.Type)
	}
	if s.PredicateType != "https://slsa.dev/provenance/v1" {
		t.Errorf("predicateType = %q", s.PredicateType)
	}
}

func TestSubjectCarriesTheDigest(t *testing.T) {
	s := build(t, input())

	if len(s.Subject) != 1 {
		t.Fatalf("subjects = %d, want 1", len(s.Subject))
	}
	// in-toto splits the algorithm from the hex; a "sha256:..." string in the
	// digest map would not match anything.
	if got := s.Subject[0].Digest["sha256"]; got != strings.TrimPrefix(input().SubjectDigest, "sha256:") {
		t.Errorf("digest = %q", got)
	}
	if s.Subject[0].Name != "ghcr.io/felixgeelhaar/glossa-api" {
		t.Errorf("name = %q", s.Subject[0].Name)
	}
}

func TestSourceCommitIsPinned(t *testing.T) {
	s := build(t, input())

	// This is the hinge: warden's note is bound to this same commit, so it is
	// what lets a verifier walk from artifact back to source gate.
	if got := s.SourceCommit(); got != input().SHA {
		t.Errorf("SourceCommit = %q", got)
	}
	dep := s.Predicate.BuildDefinition.ResolvedDependencies[0]
	if dep.URI != "git+https://github.com/felixgeelhaar/glossa@refs/tags/v0.2.0" {
		t.Errorf("uri = %q", dep.URI)
	}
}

func TestSourceGateRecordsWhetherChecksActuallyRan(t *testing.T) {
	reproved := build(t, input())
	if !reproved.Predicate.BuildDefinition.InternalParameters.SourceGate.Reproved {
		t.Error("a build that ran the checks must say so")
	}

	in := input()
	in.GateReproved = false
	in.GateReason = "warden note on c3f7aca is signed by a trusted key"
	inherited := build(t, in)

	gate := inherited.Predicate.BuildDefinition.InternalParameters.SourceGate
	// Both are legitimate; the difference is what a reader needs to judge how
	// much the artifact is worth trusting, and a signature alone cannot say it.
	if gate.Reproved {
		t.Error("an inherited verdict must not claim the checks ran here")
	}
	if !gate.Verified {
		t.Error("an inherited verdict is still a verdict")
	}
	if !strings.Contains(gate.Reason, "trusted key") {
		t.Errorf("reason = %q, want the justification carried through", gate.Reason)
	}
}

func TestIsolationIsRecorded(t *testing.T) {
	in := input()
	in.Isolated = true

	if !build(t, in).Predicate.BuildDefinition.InternalParameters.Isolated {
		t.Error("a credential-free build should be visible in the predicate")
	}
}

func TestBuilderIsPinned(t *testing.T) {
	s := build(t, input())

	b := s.Predicate.RunDetails.Builder
	if b.ID != "https://github.com/klarlabs-studio/kiln@v0.1.0" {
		t.Errorf("builder id = %q", b.ID)
	}
	// The gate's version changes what "passed" means, so it belongs here too.
	if b.Version["warden"] != "0.28.0" {
		t.Errorf("versions = %v", b.Version)
	}
}

func TestUnknownVersionIsNamedNotOmitted(t *testing.T) {
	in := input()
	in.KilnVersion = ""

	// A builder id with a blank version would read as a valid pin.
	if got := build(t, in).Predicate.RunDetails.Builder.ID; !strings.HasSuffix(got, "@unknown") {
		t.Errorf("builder id = %q", got)
	}
}

func TestMissingRepositoryStillProducesAStatement(t *testing.T) {
	in := input()
	in.Repo = ""

	s := build(t, in)

	// A predicate that refuses to exist teaches a verifier nothing; the commit
	// is still pinned even when the remote is unknown.
	if s.SourceCommit() != in.SHA {
		t.Error("the commit must survive an unidentifiable repository")
	}
	if s.Predicate.BuildDefinition.ExternalParameters.Repository != "" {
		t.Error("a fabricated repository URL is worse than none")
	}
}

func TestBuildRejectsAnUnusableSubject(t *testing.T) {
	in := input()
	in.SubjectDigest = "not-a-digest"
	if _, err := Build(in); err == nil {
		t.Error("Build accepted a malformed digest")
	}

	in = input()
	in.SubjectName = "  "
	if _, err := Build(in); err == nil {
		t.Error("Build accepted a statement about nothing")
	}
}

func TestRoundTrip(t *testing.T) {
	original := build(t, input())

	raw, err := original.JSON()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if got.SourceCommit() != original.SourceCommit() || !got.BuiltByKiln() {
		t.Errorf("round trip lost information: %+v", got)
	}
}

func TestParseRejectsAForeignPredicate(t *testing.T) {
	raw := []byte(`{"_type":"https://in-toto.io/Statement/v1",
	  "predicateType":"https://spdx.dev/Document","predicate":{}}`)

	if _, err := Parse(raw); err == nil {
		t.Error("Parse accepted an SBOM as build provenance")
	}
}

func TestBuiltByKilnRejectsAnImpostor(t *testing.T) {
	s := build(t, input())
	s.Predicate.RunDetails.Builder.ID = "https://github.com/someone-else/builder@v1"

	// A verifier must check the builder before trusting kiln-specific fields;
	// anyone can write a predicate claiming anything.
	if s.BuiltByKiln() {
		t.Error("a foreign builder id must not pass as kiln")
	}
}

func TestJSONIsValidAndStable(t *testing.T) {
	raw, err := build(t, input()).JSON()
	if err != nil {
		t.Fatal(err)
	}

	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("emitted invalid JSON: %v", err)
	}
	for _, key := range []string{"_type", "subject", "predicateType", "predicate"} {
		if _, ok := probe[key]; !ok {
			t.Errorf("statement missing %q", key)
		}
	}
}

func TestTimesAreRFC3339UTC(t *testing.T) {
	s := build(t, input())

	m := s.Predicate.RunDetails.Metadata
	if m.StartedOn != "2026-08-17T22:06:32Z" {
		t.Errorf("startedOn = %q", m.StartedOn)
	}
	if m.FinishedOn != "2026-08-17T22:07:00Z" {
		t.Errorf("finishedOn = %q", m.FinishedOn)
	}

	in := input()
	in.FinishedOn = time.Time{}
	// An unfinished build must omit the field rather than claim the year 1.
	if got := build(t, in).Predicate.RunDetails.Metadata.FinishedOn; got != "" {
		t.Errorf("finishedOn = %q, want empty", got)
	}
}
