package ports

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// The status stays red, deliberately: a commit whose gate never ran has not
// been gated, and ConclusionNeutral posts as a *success* commit status, which
// would show an unverified commit green. What has to change is what it says.
func TestProveSummarySaysTheGateCouldNotRun(t *testing.T) {
	err := fmt.Errorf("%w: exit 78", ErrToolMissing)

	conclusion, title, body := ProveSummary(false, "", err)

	if conclusion != ConclusionFailure {
		t.Errorf("conclusion = %v, want failure: an ungated commit must not read as passing", conclusion)
	}
	if strings.Contains(title, "gate failed") {
		t.Errorf("title = %q — nothing looked at the change, so it did not fail", title)
	}
	if !strings.Contains(title, "could not run") {
		t.Errorf("title = %q, want it to say the gate could not run", title)
	}
	if !strings.Contains(body, "78") {
		t.Errorf("body dropped the detail an operator needs:\n%s", body)
	}
}

func TestProveSummaryStillCallsARejectionAFailure(t *testing.T) {
	conclusion, title, _ := ProveSummary(false, "", errors.New("lint failed"))

	if conclusion != ConclusionFailure || title != "gate failed" {
		t.Errorf("conclusion = %v, title = %q; a real rejection must still say so", conclusion, title)
	}
}
