package cli

import (
	"strings"
	"testing"
)

// `kiln run --ref refs/pull/50/head` already names the pull request, and
// without --pr kiln could not look it up, so a same-repo PR re-run by hand was
// gated as a fork: no dependencies, no skip, exit 78. Found re-running a box's
// PR; the ref alone must be enough.
func TestPRNumberComesFromThePullRef(t *testing.T) {
	tests := []struct {
		ref  string
		flag int
		want int
	}{
		{"refs/pull/50/head", 0, 50},
		{"refs/pull/50/head", 50, 50},
		{"", 7, 7},
		{"refs/heads/main", 0, 0},
		{"refs/pull/50/merge", 0, 0},
		{"refs/pull/0/head", 0, 0},
		{"refs/pull/-3/head", 0, 0},
		{"refs/pull/x/head", 0, 0},
	}
	for _, tt := range tests {
		got, err := prNumber(tt.ref, tt.flag)
		if err != nil {
			t.Errorf("prNumber(%q, %d): %v", tt.ref, tt.flag, err)
			continue
		}
		if got != tt.want {
			t.Errorf("prNumber(%q, %d) = %d, want %d", tt.ref, tt.flag, got, tt.want)
		}
	}
}

// A ref and a --pr that disagree are a typo in one of them, and guessing which
// would mean looking up the wrong pull request's fork status.
func TestPRNumberRefusesADisagreement(t *testing.T) {
	_, err := prNumber("refs/pull/51/head", 50)
	if err == nil || !strings.Contains(err.Error(), "51") || !strings.Contains(err.Error(), "50") {
		t.Errorf("want a refusal naming both numbers, got %v", err)
	}
}
