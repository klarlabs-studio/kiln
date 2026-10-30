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

// The check for a commit that predates the task's script stays red. Skipped and
// neutral both satisfy a required check, so either would let a pull request
// based on an old enough commit merge without the check ever running. What
// changes is the claim: the title names the missing file, and the body says
// what to do about it.
func TestANotApplicableTaskCheckStaysRedAndSaysWhy(t *testing.T) {
	err := &NotApplicableError{Task: "build", Path: "scripts/ci/build.sh"}

	conclusion, title, body := TaskSummary(err, false, "")

	if conclusion != ConclusionFailure {
		t.Errorf("conclusion = %v, want failure: skipped would satisfy a required check", conclusion)
	}
	if title != "not applicable: this commit predates scripts/ci/build.sh" {
		t.Errorf("title = %q, want it to name the missing file", title)
	}
	for _, want := range []string{"Rebase", "remove the task"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}

// An ErrNotApplicable without a path still says not applicable, not "failed".
func TestANotApplicableTaskWithoutAPathStillSaysNotApplicable(t *testing.T) {
	conclusion, title, _ := TaskSummary(fmt.Errorf("%w: build", ErrNotApplicable), false, "")

	if conclusion != ConclusionFailure || !strings.HasPrefix(title, "not applicable") {
		t.Errorf("conclusion = %v, title = %q", conclusion, title)
	}
}

func TestTaskSummaryStillCallsAFailureAFailure(t *testing.T) {
	conclusion, _, _ := TaskSummary(fmt.Errorf("%w: build", ErrTaskFailed), false, "")
	if conclusion != ConclusionFailure {
		t.Errorf("conclusion = %v, want failure", conclusion)
	}
}
