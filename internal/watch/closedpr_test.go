package watch

import (
	"testing"

	"go.klarlabs.de/kiln/internal/domain/isolation"
)

// GitHub keeps refs/pull/N/head forever, for every pull request ever opened,
// so a remote's pull refs are the repository's entire history of them — 390
// against 2 still open, measured on one repo here. Building them all re-gates
// that history on a box's first tick, at minutes per gate, and posts a commit
// status on every long-merged commit.
func TestPullDecision_ClosedPullRequestsAreNotBuilt(t *testing.T) {
	open := map[int]bool{12: false} // the API says only #12 is open

	for _, n := range []int{4, 7, 99} {
		if _, build := pullDecision(n, open, true, neverMerged); build {
			t.Errorf("built #%d, which the API did not list as open", n)
		}
	}
	if _, build := pullDecision(12, open, true, neverMerged); !build {
		t.Error("did not build the one open pull request")
	}
}

// Fork-ness as the API reported it has to survive the decision: it is the
// input to the isolation policy.
func TestPullDecision_KeepsForkNessFromTheAPI(t *testing.T) {
	open := map[int]bool{3: false, 5: true}

	if fork, build := pullDecision(3, open, true, neverMerged); !build || fork {
		t.Errorf("same-repo PR: fork=%v build=%v", fork, build)
	}
	if fork, build := pullDecision(5, open, true, neverMerged); !build || !fork {
		t.Errorf("fork PR: fork=%v build=%v", fork, build)
	}
}

// Without a token the open list is not authoritative, so "closed" cannot be
// established that way — but a head already contained in the watched branch
// has certainly been merged, and that costs no API call.
func TestPullDecision_WithoutATokenAMergedHeadIsSkipped(t *testing.T) {
	if _, build := pullDecision(4, nil, false, alwaysMerged); build {
		t.Error("built a pull request whose head is already in the branch")
	}
}

// Unknown and unmerged is built, and fails closed: treating it as same-repo
// would hand an unknown author the operator's credentials.
func TestPullDecision_UnknownAndUnmergedFailsClosed(t *testing.T) {
	fork, build := pullDecision(9, nil, false, neverMerged)
	if !build {
		t.Fatal("skipped a pull request that may well be open")
	}
	if !fork {
		t.Error("an unverifiable pull request must be treated as a fork")
	}
}

// The merge check costs a git call, so it must not be made when the API has
// already answered.
func TestPullDecision_DoesNotPayForTheGitCheckWhenTheAPIAnswered(t *testing.T) {
	called := false
	probe := func() bool { called = true; return false }

	pullDecision(4, map[int]bool{12: false}, true, probe)

	if called {
		t.Error("ran the ancestor check despite an authoritative answer")
	}
}

func neverMerged() bool  { return false }
func alwaysMerged() bool { return true }

// End to end through discovery: a merged pull ref on a tokenless box is not
// discovered, while the branch still is.
func TestDiscovery_SkipsAMergedPullRefWithoutAToken(t *testing.T) {
	f := newFixture(t)
	// A pull ref pointing at a commit that IS on the branch: merged.
	merged := f.upstream.Commit("merged work", "merged.txt", "x\n")
	f.upstream.Git("update-ref", "refs/pull/4/head", merged)
	f.watcher.GitHub = nil

	res, err := f.watcher.Once(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}

	for _, j := range res.Discovered {
		if j.Event == isolation.EventPullRequest && j.Ref == "refs/pull/4/head" {
			t.Errorf("discovered a merged pull request: %+v", j)
		}
	}
	if len(res.Discovered) == 0 {
		t.Error("skipping the merged PR also lost the branch")
	}
}
