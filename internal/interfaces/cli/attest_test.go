package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// captureIO gives a command somewhere to write and hands back the buffer.
func captureIO() (IO, *bytes.Buffer) {
	var buf bytes.Buffer
	return IO{Out: &buf, Err: &buf}, &buf
}

// predicateFrom runs `kiln attest` and returns the emitted predicate.
func predicateFrom(t *testing.T, args ...string) map[string]any {
	t.Helper()
	io, out := captureIO()
	if err := runAttest(t.Context(), args, io); err != nil {
		t.Fatalf("kiln attest %v: %v", args, err)
	}
	var pred map[string]any
	if err := json.Unmarshal(out.Bytes(), &pred); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out.String())
	}
	return pred
}

func sourceGate(t *testing.T, pred map[string]any) map[string]any {
	t.Helper()
	bd, _ := pred["buildDefinition"].(map[string]any)
	ip, _ := bd["internalParameters"].(map[string]any)
	sg, ok := ip["sourceGate"].(map[string]any)
	if !ok {
		t.Fatalf("no sourceGate in predicate: %+v", pred)
	}
	return sg
}

const (
	testSubject = "registry.example/app@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	testCommit  = "c3f7aca23fa4bfa8d65b3741f46c509713cd618e"
)

func baseArgs() []string {
	return []string{
		"--subject", testSubject,
		"--commit", testCommit,
		"--repo", "acme/app",
		"--builder", "https://gitlab.com/acme/app",
	}
}

// A pipeline that ran no gate must not emit a verdict saying one passed.
//
// This is the whole reason the command can be trusted by somebody verifying
// its output. The predicate used to hardcode verified: true, which was
// harmless while kiln was the only producer — kiln reaches a publish only
// after the gate is satisfied — and becomes a lie the moment anything else
// builds one.
func TestNoGateClaimsNoGate(t *testing.T) {
	gate := sourceGate(t, predicateFrom(t, baseArgs()...))

	if gate["verified"] != false {
		t.Errorf("verified = %v, want false: no gate was named, so none ran", gate["verified"])
	}
	if gate["reproved"] != false {
		t.Errorf("reproved = %v, want false", gate["reproved"])
	}
	// Naming warden here would read as "warden looked and was unhappy" rather
	// than "nothing looked".
	if tool, _ := gate["tool"].(string); tool != "" {
		t.Errorf("tool = %q, want empty: attributing an absent verdict to a gate misleads", tool)
	}
}

func TestANamedGateIsRecordedWithItsVerdict(t *testing.T) {
	gate := sourceGate(t, predicateFrom(t, append(baseArgs(), "--gate", "warden", "--gate-reproved")...))

	if gate["tool"] != "warden" || gate["verified"] != true || gate["reproved"] != true {
		t.Errorf("sourceGate = %+v, want warden verified and reproved", gate)
	}
}

// Inheriting a verdict from a signed note is legitimate, and a reader deciding
// how far to trust the artifact has to be able to tell it apart from checks
// that ran during this build.
func TestAnInheritedVerdictIsNotReproved(t *testing.T) {
	gate := sourceGate(t, predicateFrom(t, append(baseArgs(), "--gate", "warden")...))

	if gate["verified"] != true {
		t.Errorf("verified = %v, want true: an inherited verdict is still a verdict", gate["verified"])
	}
	if gate["reproved"] != false {
		t.Errorf("reproved = %v, want false: nothing re-ran here", gate["reproved"])
	}
}

// The builder id is what a verifier pins its trust on — RollOps calls it
// AllowedBuilders, kiln's own policy file calls it provenance.builders. A
// default would have every foreign pipeline quietly signing a claim to be kiln.
func TestTheBuilderMustBeNamed(t *testing.T) {
	io, _ := captureIO()
	args := []string{"--subject", testSubject, "--commit", testCommit, "--repo", "acme/app"}

	err := runAttest(t.Context(), args, io)
	if err == nil {
		t.Fatal("a predicate was emitted with no builder named")
	}
	if !strings.Contains(err.Error(), "--builder") {
		t.Errorf("error = %q, want it to name the missing flag", err)
	}
}

func TestTheBuilderIsCarriedThrough(t *testing.T) {
	pred := predicateFrom(t, baseArgs()...)
	rd, _ := pred["runDetails"].(map[string]any)
	b, _ := rd["builder"].(map[string]any)

	if got, _ := b["id"].(string); got != "https://gitlab.com/acme/app" {
		t.Errorf("builder id = %q, want the platform that actually built it", got)
	}
}

// The commit is the join: it is what lets a verifier check that the artifact
// it is about to deploy came from the commit a source gate vouched for.
func TestTheCommitIsPinned(t *testing.T) {
	pred := predicateFrom(t, baseArgs()...)
	bd, _ := pred["buildDefinition"].(map[string]any)
	deps, _ := bd["resolvedDependencies"].([]any)

	for _, d := range deps {
		dm, _ := d.(map[string]any)
		dig, _ := dm["digest"].(map[string]any)
		if dig["gitCommit"] == testCommit {
			return
		}
	}
	t.Errorf("no resolvedDependency pins %s: %+v", testCommit, deps)
}

// A tag moves. Provenance about a moving reference says nothing a week later,
// so the subject has to be content-addressed.
func TestATagIsRefusedAsASubject(t *testing.T) {
	io, _ := captureIO()
	args := []string{"--subject", "registry.example/app:v1.2.3", "--commit", testCommit,
		"--repo", "acme/app", "--builder", "https://gitlab.com/acme/app"}

	err := runAttest(t.Context(), args, io)
	if err == nil {
		t.Fatal("a tag was accepted as a subject")
	}
	if !strings.Contains(err.Error(), "sha256") {
		t.Errorf("error = %q, want it to say what the subject must look like", err)
	}
}
