package provenance

import (
	"strings"
	"testing"

	"go.klarlabs.de/kiln/internal/application/ports"

	"go.klarlabs.de/kiln/internal/domain/isolation"
	"go.klarlabs.de/kiln/internal/infrastructure/execx"
)

const sha = "abc1234def5678"

var (
	trusting = isolation.For(isolation.EventPush, false)
	forkPR   = isolation.For(isolation.EventPullRequest, true)
)

func TestSkipsOnATrustedSignedNote(t *testing.T) {
	fake := execx.NewFake()
	v := NewWarden(fake, "warden", []string{"KEY1"})

	got := v.Verify(t.Context(), "/repo", sha, trusting)

	if !got.Skip() {
		t.Fatalf("ports.ProvenanceDecision = %s (%s), want skipped", got.Decision, got.Reason)
	}
	cmd := fake.Find("warden verify")
	if cmd == nil {
		t.Fatalf("warden verify was not run: %s", fake.Transcript())
	}
	line := cmd.String()
	for _, want := range []string{"--commit " + sha, "--require-signed", "--key KEY1"} {
		if !strings.Contains(line, want) {
			t.Errorf("verify command missing %q: %s", want, line)
		}
	}
}

func TestMultipleTrustedKeysArePassedAsAList(t *testing.T) {
	fake := execx.NewFake()
	NewWarden(fake, "warden", []string{"KEY1", "KEY2"}).Verify(t.Context(), "/repo", sha, trusting)

	if !strings.Contains(fake.Find("warden verify").String(), "--key KEY1,KEY2") {
		t.Errorf("keys not joined: %s", fake.Transcript())
	}
}

func TestForkPullRequestNeverConsultsTheNote(t *testing.T) {
	// A fork's note was authored on the same untrusted head. Even a note that
	// verifies proves nothing, so the subprocess must not run at all.
	fake := execx.NewFake()
	got := NewWarden(fake, "warden", []string{"KEY1"}).Verify(t.Context(), "/repo", sha, forkPR)

	if got.Decision != ports.DecisionForbidden {
		t.Errorf("ports.ProvenanceDecision = %s, want forbidden", got.Decision)
	}
	if fake.Ran("warden") {
		t.Errorf("fork PR ran warden anyway: %s", fake.Transcript())
	}
}

func TestNoPinnedKeysMeansNoSkip(t *testing.T) {
	fake := execx.NewFake()
	got := NewWarden(fake, "warden", nil).Verify(t.Context(), "/repo", sha, trusting)

	if got.Decision != ports.DecisionReprove {
		t.Errorf("ports.ProvenanceDecision = %s, want reprove", got.Decision)
	}
	if !strings.Contains(got.Reason, "KILN_TRUSTED_KEYS") {
		t.Errorf("reason should name the missing setting, got %q", got.Reason)
	}
	// Running an unpinned verify would validate a note signed by any key at
	// all, including one the pull request author just generated.
	if fake.Ran("warden") {
		t.Errorf("verified without pinned keys: %s", fake.Transcript())
	}
}

func TestUnsignedOrUntrustedNoteReproves(t *testing.T) {
	fake := execx.NewFake().On("warden verify", execx.Response{ExitCode: 1})
	got := NewWarden(fake, "warden", []string{"KEY1"}).Verify(t.Context(), "/repo", sha, trusting)

	if got.Decision != ports.DecisionReprove {
		t.Fatalf("ports.ProvenanceDecision = %s, want reprove", got.Decision)
	}
	if !strings.Contains(got.Reason, "re-proving") {
		t.Errorf("reason = %q, want an actionable explanation", got.Reason)
	}
}

func TestWardenStderrIsSurfaced(t *testing.T) {
	fake := execx.NewFake().On("warden verify", execx.Response{
		ExitCode: 1, Stderr: "note signed by an untrusted key AAAA",
	})
	got := NewWarden(fake, "warden", []string{"KEY1"}).Verify(t.Context(), "/repo", sha, trusting)

	if !strings.Contains(got.Reason, "untrusted key AAAA") {
		t.Errorf("reason should quote warden, got %q", got.Reason)
	}
}

func TestMissingWardenBinaryReproves(t *testing.T) {
	fake := execx.NewFake().Absent("warden")
	got := NewWarden(fake, "warden", []string{"KEY1"}).Verify(t.Context(), "/repo", sha, trusting)

	// Not a skip. Prove is about to fail on the same missing binary, which is
	// where the operator gets the real message.
	if got.Decision != ports.DecisionReprove {
		t.Errorf("ports.ProvenanceDecision = %s, want reprove", got.Decision)
	}
	if !strings.Contains(got.Reason, "PATH") {
		t.Errorf("reason = %q, want a PATH hint", got.Reason)
	}
}

func TestCustomBinaryNameIsHonoured(t *testing.T) {
	fake := execx.NewFake()
	NewWarden(fake, "warden-next", []string{"KEY1"}).Verify(t.Context(), "/repo", sha, trusting)

	if !fake.Ran("warden-next verify") {
		t.Errorf("KILN_WARDEN not honoured: %s", fake.Transcript())
	}
}

func TestEmptyBinaryDefaultsToWarden(t *testing.T) {
	if got := NewWarden(execx.NewFake(), "", nil).Binary; got != "warden" {
		t.Errorf("Binary = %q, want warden", got)
	}
}

func TestEmptySHANeverSkips(t *testing.T) {
	got := NewWarden(execx.NewFake(), "warden", []string{"KEY1"}).Verify(t.Context(), "/repo", "  ", trusting)

	if got.Decision != ports.DecisionReprove {
		t.Errorf("ports.ProvenanceDecision = %s, want reprove", got.Decision)
	}
}

func TestAlwaysNeverSkips(t *testing.T) {
	got := ports.AlwaysProvenance{}.Verify(t.Context(), "/repo", sha, trusting)

	if got.Skip() {
		t.Error("ports.AlwaysProvenance must never skip")
	}
	if got.Reason == "" {
		t.Error("ports.AlwaysProvenance must give a reason")
	}
}

func TestSameRepoPullRequestMaySkip(t *testing.T) {
	// A same-repo PR may skip: the note it presents was signed by an
	// operator-pinned key that a PR head cannot forge.
	policy := isolation.For(isolation.EventPullRequest, false)
	got := NewWarden(execx.NewFake(), "warden", []string{"KEY1"}).Verify(t.Context(), "/repo", sha, policy)

	if !got.Skip() {
		t.Errorf("ports.ProvenanceDecision = %s (%s), want skipped", got.Decision, got.Reason)
	}
}
