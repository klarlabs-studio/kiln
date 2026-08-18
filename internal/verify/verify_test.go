package verify

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"go.klarlabs.de/kiln/internal/attest"
	"go.klarlabs.de/kiln/internal/execx"
)

const (
	ref    = "ghcr.io/felixgeelhaar/glossa-api@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	commit = "c3f7aca23fa4bfa8d65b3741f46c509713cd618e"
)

func statement(t *testing.T, mutate func(*attest.Input)) attest.Statement {
	t.Helper()
	in := attest.Input{
		SubjectName:   "ghcr.io/felixgeelhaar/glossa-api",
		SubjectDigest: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		Repo:          "felixgeelhaar/glossa", SHA: commit, Ref: "refs/heads/main",
		Event: "push", ArtifactKind: "image", GateReproved: true,
		KilnVersion: "v0.1.0", InvocationID: "run-1", StartedOn: time.Unix(0, 0).UTC(),
	}
	if mutate != nil {
		mutate(&in)
	}
	s, err := attest.Build(in)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// envelope renders a statement the way cosign prints it: one DSSE object per
// line with a base64 payload.
func envelope(t *testing.T, s attest.Statement) string {
	t.Helper()
	payload, err := s.JSON()
	if err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(map[string]string{"payload": base64.StdEncoding.EncodeToString(payload)})
	if err != nil {
		t.Fatal(err)
	}
	return string(line) + "\n"
}

// healthy scripts a cosign that verifies and returns the given statement.
func healthy(t *testing.T, s attest.Statement) *execx.Fake {
	t.Helper()
	f := execx.NewFake()
	f.On("cosign verify-attestation", execx.Response{Stdout: envelope(t, s)})
	return f
}

func keyed() Options {
	return Options{Reference: ref, CosignKey: "cosign.pub"}
}

func link(r Report, name string) Link {
	for _, l := range r.Links {
		if l.Name == name {
			return l
		}
	}
	return Link{Name: name, Status: "missing"}
}

func TestAWholeChainVerifies(t *testing.T) {
	fake := healthy(t, statement(t, nil))

	report, err := New(fake).Verify(t.Context(), keyed())
	if err != nil {
		t.Fatalf("Verify: %v\n%s", err, report)
	}

	for _, name := range []string{"signature", "provenance", "builder"} {
		if got := link(report, name); got.Status != Pass {
			t.Errorf("%s = %s (%s)", name, got.Status, got.Detail)
		}
	}
	if !report.OK() {
		t.Errorf("OK = false:\n%s", report)
	}
}

func TestABrokenSignatureDoesNotHideTheRest(t *testing.T) {
	fake := healthy(t, statement(t, nil))
	fake.On("cosign verify ", execx.Response{ExitCode: 1, Stderr: "no matching signatures"})

	report, err := New(fake).Verify(t.Context(), keyed())

	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("err = %v, want ErrIncomplete", err)
	}
	if got := link(report, "signature"); got.Status != Fail {
		t.Errorf("signature = %s", got.Status)
	}
	// "The signature is bad but the provenance reads fine" is a more useful
	// answer than a bare "invalid".
	if got := link(report, "provenance"); got.Status != Pass {
		t.Errorf("a broken signature hid the provenance link: %s", got.Status)
	}
}

func TestAMissingAttestationFails(t *testing.T) {
	fake := execx.NewFake()
	fake.On("cosign verify-attestation", execx.Response{ExitCode: 1, Stderr: "no matching attestations"})

	report, err := New(fake).Verify(t.Context(), keyed())

	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("err = %v", err)
	}
	if got := link(report, "provenance"); got.Status != Fail {
		t.Errorf("provenance = %s", got.Status)
	}
	// Nothing downstream can be judged without a predicate to read.
	if got := link(report, "source gate"); got.Status != "missing" {
		t.Errorf("source gate was judged with no provenance to judge from: %+v", got)
	}
}

func TestAForeignBuilderIsRejected(t *testing.T) {
	s := statement(t, nil)
	s.Predicate.RunDetails.Builder.ID = "https://github.com/someone/else@v1"
	fake := healthy(t, s)

	report, err := New(fake).Verify(t.Context(), keyed())

	// cosign proved a trusted key signed *an* attestation. It did not check
	// what the attestation says, and anyone with that key can claim anything.
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("err = %v, want the impostor rejected", err)
	}
	got := link(report, "builder")
	if got.Status != Fail || !strings.Contains(got.Detail, "sourceGate") {
		t.Errorf("builder = %+v, want an explanation of why it matters", got)
	}
}

func TestSourceGateIsUnknownWithoutAClone(t *testing.T) {
	fake := healthy(t, statement(t, nil))

	report, _ := New(fake).Verify(t.Context(), keyed())

	got := link(report, "source gate")
	// The predicate's own claim is kiln vouching for itself; only the note
	// settles it. Reporting "ok" here would be the verifier taking the
	// artifact's word for the artifact.
	if got.Status != Unknown {
		t.Errorf("source gate = %s, want unknown with no clone to read", got.Status)
	}
	if report.Complete() {
		t.Error("Complete = true with an unchecked link")
	}
	if !report.OK() {
		t.Error("OK = false: an uncheckable link is not a broken one")
	}
}

func TestSourceGateChecksTheNote(t *testing.T) {
	fake := healthy(t, statement(t, nil))
	opts := keyed()
	opts.RepoDir = t.TempDir()
	opts.TrustedKeys = []string{"139e6eb9e2611c76"}

	report, err := New(fake).Verify(t.Context(), opts)
	if err != nil {
		t.Fatalf("Verify: %v\n%s", err, report)
	}

	cmd := fake.Find("warden verify")
	if cmd == nil {
		t.Fatalf("the note was never read: %s", fake.Transcript())
	}
	for _, want := range []string{"--commit " + commit, "--require-signed", "--key 139e6eb9e2611c76"} {
		if !strings.Contains(cmd.String(), want) {
			t.Errorf("warden call missing %q: %s", want, cmd.String())
		}
	}
	if !report.Complete() {
		t.Errorf("Complete = false with every link checked:\n%s", report)
	}
}

func TestAnUngatedCommitFails(t *testing.T) {
	fake := healthy(t, statement(t, nil))
	fake.On("warden verify", execx.Response{ExitCode: 1})
	opts := keyed()
	opts.RepoDir = t.TempDir()

	report, err := New(fake).Verify(t.Context(), opts)

	// This is the link that makes the chain a chain: everything else proves
	// the artifact came from a commit, only this says the commit was gated.
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("err = %v", err)
	}
	if got := link(report, "source gate"); got.Status != Fail {
		t.Errorf("source gate = %s", got.Status)
	}
}

func TestUnpinnedNoteVerificationSaysSo(t *testing.T) {
	fake := healthy(t, statement(t, nil))
	opts := keyed()
	opts.RepoDir = t.TempDir()

	report, _ := New(fake).Verify(t.Context(), opts)

	got := link(report, "source gate")
	if got.Status != Pass {
		t.Fatalf("source gate = %s", got.Status)
	}
	// Without pinned keys the note was checked for existence, not authorship.
	// Reporting a bare "ok" would overstate what was established.
	if !strings.Contains(got.Detail, "signer not checked") {
		t.Errorf("detail = %q, want the caveat", got.Detail)
	}
}

func TestAnInheritedGateIsReported(t *testing.T) {
	s := statement(t, func(in *attest.Input) {
		in.GateReproved = false
		in.GateReason = "warden note is signed by a trusted key"
	})
	fake := healthy(t, s)
	opts := keyed()
	opts.RepoDir = t.TempDir()
	opts.TrustedKeys = []string{"KEY"}

	report, err := New(fake).Verify(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}

	// Legitimate, and the reader still needs to know the checks did not run
	// for this artifact.
	if got := link(report, "source gate"); !strings.Contains(got.Detail, "inherited") {
		t.Errorf("detail = %q, want the inheritance surfaced", got.Detail)
	}
}

func TestKeylessNeedsAnIdentity(t *testing.T) {
	fake := healthy(t, statement(t, nil))

	report, err := New(fake).Verify(t.Context(), Options{Reference: ref})

	if err == nil {
		t.Fatal("want a refusal")
	}
	got := link(report, "signature")
	if got.Status != Unknown || !strings.Contains(got.Detail, "proves nothing") {
		t.Errorf("signature = %+v, want the reason spelled out", got)
	}
	if fake.Ran("cosign verify ") {
		t.Error("ran an unpinned keyless verification")
	}
}

func TestKeylessWithAnIdentityIsPinned(t *testing.T) {
	fake := healthy(t, statement(t, nil))
	opts := Options{
		Reference: ref,
		Identity:  "https://github.com/klarlabs-studio/kiln/.github/workflows/ci.yml@refs/heads/main",
		Issuer:    "https://token.actions.githubusercontent.com",
	}

	if _, err := New(fake).Verify(t.Context(), opts); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	cmd := fake.Find("cosign verify ")
	if cmd == nil || !strings.Contains(cmd.String(), "--certificate-identity") {
		t.Errorf("identity not pinned: %s", fake.Transcript())
	}
}

func TestMissingCosignIsUnknownNotPass(t *testing.T) {
	fake := execx.NewFake().Absent("cosign")

	report, err := New(fake).Verify(t.Context(), keyed())

	if err == nil {
		t.Fatal("want a failure when nothing could be checked")
	}
	// A verifier that reports "fine" after looking at nothing is worse than
	// useless.
	if got := link(report, "signature"); got.Status != Unknown {
		t.Errorf("signature = %s", got.Status)
	}
	if report.Complete() {
		t.Error("Complete = true having checked nothing")
	}
}

func TestSeveralAttestationsPickTheProvenance(t *testing.T) {
	sbom, _ := json.Marshal(map[string]string{
		"payload": base64.StdEncoding.EncodeToString(
			[]byte(`{"_type":"https://in-toto.io/Statement/v1","predicateType":"https://spdx.dev/Document"}`)),
	})
	fake := execx.NewFake()
	fake.On("cosign verify-attestation", execx.Response{
		Stdout: string(sbom) + "\n" + envelope(t, statement(t, nil)),
	})

	report, err := New(fake).Verify(t.Context(), keyed())
	if err != nil {
		t.Fatalf("Verify: %v\n%s", err, report)
	}

	// An image commonly carries an SBOM attestation too; the walk must find
	// the provenance among them rather than choke on the first line.
	if report.Statement == nil || report.Statement.SourceCommit() != commit {
		t.Errorf("did not select the provenance: %+v", report.Statement)
	}
}

func TestURLSafePayloadIsAccepted(t *testing.T) {
	payload, err := statement(t, nil).JSON()
	if err != nil {
		t.Fatal(err)
	}
	line, _ := json.Marshal(map[string]string{
		"payload": base64.RawURLEncoding.EncodeToString(payload),
	})
	fake := execx.NewFake()
	fake.On("cosign verify-attestation", execx.Response{Stdout: string(line)})

	// Refusing a valid attestation over an encoding detail is how a verifier
	// gets routed around.
	if _, err := New(fake).Verify(t.Context(), keyed()); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

func TestGarbageAttestationFails(t *testing.T) {
	fake := execx.NewFake()
	fake.On("cosign verify-attestation", execx.Response{Stdout: "not json at all\n"})

	report, err := New(fake).Verify(t.Context(), keyed())

	if err == nil {
		t.Fatal("want a failure")
	}
	if got := link(report, "provenance"); got.Status != Fail {
		t.Errorf("provenance = %+v", got)
	}
}

func TestReportRendersEveryLink(t *testing.T) {
	fake := healthy(t, statement(t, nil))
	report, _ := New(fake).Verify(t.Context(), keyed())

	out := report.String()
	for _, want := range []string{ref, "signature", "provenance", "builder", "source gate"} {
		if !strings.Contains(out, want) {
			t.Errorf("report omits %q:\n%s", want, out)
		}
	}
}

func TestAReleaseTagGetsTheBlobRecipe(t *testing.T) {
	fake := healthy(t, statement(t, nil))

	report, err := New(fake).Verify(t.Context(), Options{Reference: "v1.4.0", CosignKey: "cosign.pub"})

	if err == nil {
		t.Fatal("want a refusal")
	}
	got := link(report, "reference")
	// Pointing this at a release tag would otherwise produce a cosign error
	// about a missing image: true, and useless.
	if got.Status != Fail || !strings.Contains(got.Detail, "verify-blob-attestation") {
		t.Errorf("reference = %+v, want the blob recipe", got)
	}
	if fake.Ran("cosign") {
		t.Error("asked a registry about something that is not in one")
	}
}

func TestAnImageReferenceIsAccepted(t *testing.T) {
	for _, r := range []string{
		"ghcr.io/x/y@sha256:aaa",
		"ghcr.io/x/y:latest",
		"ghcr.io/x/y",
	} {
		if err := checkReferenceShape(r); err != nil {
			t.Errorf("checkReferenceShape(%q) = %v", r, err)
		}
	}
	if err := checkReferenceShape("  "); err == nil {
		t.Error("an empty reference must be refused")
	}
}
