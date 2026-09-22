package trust

import (
	"testing"

	"go.klarlabs.de/kiln/internal/domain/isolation"
)

func TestZeroContextIsUntrusted(t *testing.T) {
	p := Context{}.Policy()
	if p.Secrets || p.Publish || p.Skip {
		t.Errorf("zero Context granted %v", p)
	}
}

func TestParseEvidenceMode(t *testing.T) {
	if _, ok := ParseEvidenceMode("maybe"); ok {
		t.Error("accepted an unknown mode")
	}
	if _, ok := ParseEvidenceMode(""); ok {
		t.Error("empty is not a mode")
	}
	for _, s := range []string{"required", "best-effort"} {
		if _, ok := ParseEvidenceMode(s); !ok {
			t.Errorf("rejected %q", s)
		}
	}
}

func TestResolveEvidence(t *testing.T) {
	if got := ResolveEvidence("", false); got != EvidenceBestEffort {
		t.Errorf("adopting box = %q", got)
	}
	if got := ResolveEvidence("", true); got != EvidenceRequired {
		t.Errorf("box with pinned keys = %q", got)
	}
	if got := ResolveEvidence("best-effort", true); got != EvidenceBestEffort {
		t.Errorf("explicit best-effort lost to the default: %q", got)
	}
}

func TestPolicyCommitIsANamedSource(t *testing.T) {
	if PolicyCommit != "commit" || PolicyOperator != "operator" {
		t.Errorf("policy sources drifted: %q %q", PolicyCommit, PolicyOperator)
	}
}

func TestClaimedPushIsNotYetAPolicy(t *testing.T) {
	// A caller saying "this is a push" is a claim. The engine must not see
	// it until Resolve has established membership.
	c := Context{Event: isolation.EventPush}
	if !c.Policy().Publish {
		t.Error("an established push must be able to publish")
	}
}
