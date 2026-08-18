package verify

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"go.klarlabs.de/kiln/internal/attest"
	"go.klarlabs.de/kiln/internal/execx"
)

// gateKey stands in for warden's note key: the gate signs, the verifier holds
// only the public half, and nothing in between is trusted.
func gateKey(t *testing.T) (ed25519.PrivateKey, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv, base64.StdEncoding.EncodeToString(pub)
}

// signedSummary is the DSSE envelope kiln attaches, as `cosign download
// attestation` prints it: one JSON object per line.
func signedSummary(t *testing.T, priv ed25519.PrivateKey, forCommit, verdict string, levels []string) string {
	t.Helper()
	statement, err := json.Marshal(map[string]any{
		"_type":         "https://in-toto.io/Statement/v1",
		"predicateType": attest.VSAPredicateType,
		"subject": []any{map[string]any{
			"name": "git+commit", "digest": map[string]string{"gitCommit": forCommit},
		}},
		"predicate": map[string]any{
			"verifier":           map[string]any{"id": "https://warden.klarlabs.de"},
			"resourceUri":        "git+ssh://git@github.com/o/r.git@" + forCommit,
			"verificationResult": verdict,
			"verifiedLevels":     levels,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	const pt = "application/vnd.in-toto+json"
	pae := fmt.Appendf(nil, "DSSEv1 %d %s %d %s", len(pt), pt, len(statement), statement)
	env, err := json.Marshal(map[string]any{
		"payloadType": pt,
		"payload":     base64.StdEncoding.EncodeToString(statement),
		"signatures": []any{map[string]string{
			"keyid": "139e6eb9e2611c76",
			"sig":   base64.StdEncoding.EncodeToString(ed25519.Sign(priv, pae)),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(env) + "\n"
}

var wardenLevels = []string{"WARDEN_SOURCE_GATED", "WARDEN_SOURCE_SIGNED"}

// gated scripts a cosign that verifies the artifact, returns kiln provenance,
// and hands back the given source summary on `download attestation`.
func gated(t *testing.T, s attest.Statement, summary string) *execx.Fake {
	t.Helper()
	f := healthy(t, s)
	f.On("cosign download attestation", execx.Response{Stdout: summary})
	return f
}

func policyOpts(sourceKeys ...string) Options {
	o := keyed()
	o.AllowedBuilders = []string{attest.BuilderIDPrefix + "@"}
	o.SourceKeys = sourceKeys
	o.AllowedGates = []string{"https://warden.klarlabs.de"}
	o.RequiredLevels = []string{"WARDEN_SOURCE_SIGNED"}
	o.SourceRequired = true
	return o
}

func TestTheSourceVerdictIsCheckedFromTheArtifactWithNoClone(t *testing.T) {
	priv, pub := gateKey(t)
	fake := gated(t, statement(t, nil), signedSummary(t, priv, commit, "PASSED", wardenLevels))

	// No RepoDir and no warden binary: the point of carrying the gate's signed
	// summary is that a consumer needs neither.
	report, err := New(fake).Verify(t.Context(), policyOpts(pub))
	if err != nil {
		t.Fatalf("Verify: %v\n%s", err, report)
	}

	got := link(report, "source gate")
	if got.Status != Pass {
		t.Fatalf("source gate = %s (%s)", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, "139e6eb9e2611c76") {
		t.Errorf("detail = %q, want the signing key named", got.Detail)
	}
	if !report.Complete() {
		t.Errorf("chain not complete:\n%s", report)
	}
}

func TestASummaryTheGateDidNotSignIsRefused(t *testing.T) {
	imposter, _ := gateKey(t)
	_, pub := gateKey(t)
	fake := gated(t, statement(t, nil), signedSummary(t, imposter, commit, "PASSED", wardenLevels))

	// A build platform that re-signed the verdict would be attesting to
	// itself. Only the gate's own key settles it.
	report, _ := New(fake).Verify(t.Context(), policyOpts(pub))

	if got := link(report, "source gate"); got.Status != Fail {
		t.Errorf("source gate = %s (%s)", got.Status, got.Detail)
	}
	if report.OK() {
		t.Error("OK with an unauthenticated source verdict")
	}
}

func TestASummaryForADifferentCommitIsRefused(t *testing.T) {
	priv, pub := gateKey(t)
	other := "0000000000000000000000000000000000000000"
	fake := gated(t, statement(t, nil), signedSummary(t, priv, other, "PASSED", wardenLevels))

	// The attack the join exists to stop: a summary for a well-gated commit
	// riding on an artifact built from an ungated one. Both signatures verify
	// perfectly on their own.
	report, _ := New(fake).Verify(t.Context(), policyOpts(pub))

	got := link(report, "source gate")
	if got.Status != Fail {
		t.Fatalf("source gate = %s (%s)", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, "but the artifact was built from") {
		t.Errorf("detail = %q, want the mismatch spelled out", got.Detail)
	}
}

func TestAFailedVerdictIsRefused(t *testing.T) {
	priv, pub := gateKey(t)
	fake := gated(t, statement(t, nil), signedSummary(t, priv, commit, "FAILED", wardenLevels))

	report, _ := New(fake).Verify(t.Context(), policyOpts(pub))

	if got := link(report, "source gate"); got.Status != Fail {
		t.Errorf("a FAILED verdict was accepted: %s (%s)", got.Status, got.Detail)
	}
}

func TestARequiredLevelIsEnforced(t *testing.T) {
	priv, pub := gateKey(t)
	// Gated, but the note was never signed — exactly the difference
	// WARDEN_SOURCE_SIGNED exists to express.
	fake := gated(t, statement(t, nil), signedSummary(t, priv, commit, "PASSED", []string{"WARDEN_SOURCE_GATED"}))

	report, _ := New(fake).Verify(t.Context(), policyOpts(pub))

	got := link(report, "source gate")
	if got.Status != Fail {
		t.Fatalf("source gate = %s (%s)", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, "WARDEN_SOURCE_SIGNED") {
		t.Errorf("detail = %q, want the missing level named", got.Detail)
	}
}

func TestAGateThePolicyDoesNotAllowIsRefused(t *testing.T) {
	priv, pub := gateKey(t)
	fake := gated(t, statement(t, nil), signedSummary(t, priv, commit, "PASSED", wardenLevels))

	opts := policyOpts(pub)
	opts.AllowedGates = []string{"https://someone-elses-gate.example"}

	report, _ := New(fake).Verify(t.Context(), policyOpts(pub))
	if !report.OK() {
		t.Fatalf("the control case failed:\n%s", report)
	}

	report, _ = New(fake).Verify(t.Context(), opts)
	if got := link(report, "source gate"); got.Status != Fail {
		t.Errorf("a summary from an unlisted gate was accepted: %s (%s)", got.Status, got.Detail)
	}
}

func TestAnArtifactCarryingNoSummaryFailsAPolicyThatRequiresOne(t *testing.T) {
	_, pub := gateKey(t)
	// Only the build provenance on the artifact — no source verdict at all.
	fake := gated(t, statement(t, nil), envelope(t, statement(t, nil)))

	report, err := New(fake).Verify(t.Context(), policyOpts(pub))

	if err == nil {
		t.Fatal("a policy requiring a source verdict passed an artifact without one")
	}
	if got := link(report, "source gate"); got.Status != Fail {
		t.Errorf("source gate = %s (%s)", got.Status, got.Detail)
	}
}

func TestAnArtifactKilnDidNotBuildVerifiesWhenThePolicyNamesItsBuilder(t *testing.T) {
	// The adoption case: a consumer checking a GitHub Actions build with the
	// same command and the same report, having adopted nothing.
	const gha = "https://github.com/actions/runner"
	s := statement(t, nil)
	s.Predicate.RunDetails.Builder.ID = gha

	opts := keyed()
	opts.AllowedBuilders = []string{gha}

	report, err := New(healthy(t, s)).Verify(t.Context(), opts)
	if err != nil {
		t.Fatalf("Verify: %v\n%s", err, report)
	}
	if got := link(report, "builder"); got.Status != Pass {
		t.Errorf("builder = %s (%s)", got.Status, got.Detail)
	}
}

func TestABuilderOutsideThePolicyIsRefused(t *testing.T) {
	s := statement(t, nil)
	s.Predicate.RunDetails.Builder.ID = "https://build.example/anyone"

	opts := keyed()
	opts.AllowedBuilders = []string{"https://github.com/actions/runner"}

	report, _ := New(healthy(t, s)).Verify(t.Context(), opts)

	got := link(report, "builder")
	if got.Status != Fail {
		t.Fatalf("builder = %s (%s)", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, "policy does not allow") {
		t.Errorf("detail = %q", got.Detail)
	}
}

func TestTheVersionWildcardMatchesOnlyThatBuilder(t *testing.T) {
	allowed := []string{attest.BuilderIDPrefix + "@"}

	if !builderAllowed(attest.BuilderIDPrefix+"@v0.1.0", allowed) {
		t.Error("the wildcard did not match the builder it is for")
	}
	// A prefix match must not be a substring match: an attacker who registers
	// a lookalike must not inherit the trust.
	if builderAllowed("https://github.com/evil/kiln@v0.1.0", allowed) {
		t.Error("a lookalike builder matched")
	}
	if builderAllowed("https://github.com/klarlabs-studio/kilnx@v1", []string{attest.BuilderIDPrefix + "@"}) {
		t.Error("a longer name matched the wildcard")
	}
}

func TestWithoutAPolicyOnlyKilnsOwnProvenanceIsRead(t *testing.T) {
	s := statement(t, nil)
	s.Predicate.RunDetails.Builder.ID = "https://build.example/anyone"

	// No AllowedBuilders: this is plain `kiln verify`, and the kiln-specific
	// fields it goes on to read are only meaningful if kiln wrote them.
	report, _ := New(healthy(t, s)).Verify(t.Context(), keyed())

	got := link(report, "builder")
	if got.Status != Fail {
		t.Fatalf("builder = %s", got.Status)
	}
	if !strings.Contains(got.Detail, "provenance.builders") {
		t.Errorf("detail = %q, want it to point at the policy field that would accept this", got.Detail)
	}
}

func TestAnUnrequiredSourceVerdictIsACaveatNotABreak(t *testing.T) {
	priv, pub := gateKey(t)
	other := "0000000000000000000000000000000000000000"
	fake := gated(t, statement(t, nil), signedSummary(t, priv, other, "PASSED", wardenLevels))

	opts := policyOpts(pub)
	opts.SourceRequired = false

	// A broken source link still fails — Fail is Fail. What `required` changes
	// is whether an *unestablished* one counts, so use the no-keys shape for
	// that half.
	if report, err := New(fake).Verify(t.Context(), opts); err == nil {
		t.Errorf("a mismatched commit passed because the policy did not require the verdict:\n%s", report)
	}

	bare := keyed()
	report, err := New(healthy(t, statement(t, nil))).Verify(t.Context(), bare)
	if err != nil {
		t.Fatalf("Verify: %v\n%s", err, report)
	}
	if report.Complete() {
		t.Error("an unchecked source gate reported as a complete chain")
	}
}
