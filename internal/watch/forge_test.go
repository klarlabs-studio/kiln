package watch

import (
	"context"
	"errors"
	"testing"

	"go.klarlabs.de/kiln/internal/github"
)

// fakeForge is what a concrete *github.Client could not be: substitutable.
// Before the Forge port existed, the only way to test the closed-pull-request
// decision was to extract it as a pure function and test that in isolation,
// which left the path through discovery itself uncovered.
type fakeForge struct {
	open    []github.Pull
	err     error
	off     bool
	queried int
}

func (f *fakeForge) Enabled() bool { return !f.off }

func (f *fakeForge) ListOpenPulls(context.Context) ([]github.Pull, error) {
	f.queried++
	if f.err != nil {
		return nil, f.err
	}
	return f.open, nil
}

// The whole path, not just the decision function: a pull ref whose pull
// request the API does not list is closed, and must not be built.
func TestDiscovery_SkipsAPullRequestTheForgeDoesNotList(t *testing.T) {
	f := newFixture(t)
	f.alreadyWatching(t)
	openSHA := f.upstream.Commit("still open", "open.txt", "x\n")
	f.upstream.Git("update-ref", "refs/pull/5/head", openSHA)
	closedSHA := f.upstream.Commit("long merged", "closed.txt", "x\n")
	f.upstream.Git("update-ref", "refs/pull/6/head", closedSHA)
	f.upstream.Git("reset", "--hard", "HEAD~2")

	forge := &fakeForge{open: []github.Pull{{Number: 5, Fork: false}}}
	f.watcher.Forge = forge

	res, err := f.watcher.Once(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}

	if !hasRef(res.Discovered, "refs/pull/5/head") {
		t.Errorf("the open pull request was not built: %+v", res.Discovered)
	}
	if hasRef(res.Discovered, "refs/pull/6/head") {
		t.Errorf("a pull request the forge did not list was built: %+v", res.Discovered)
	}
	if forge.queried != 1 {
		t.Errorf("asked the forge %d times, want exactly 1 per tick", forge.queried)
	}
}

// Fork-ness comes from the forge and decides the isolation policy, so it has
// to survive the whole path, not just pullDecision.
func TestDiscovery_CarriesForkNessFromTheForge(t *testing.T) {
	f := newFixture(t)
	f.alreadyWatching(t)
	sha := f.upstream.Commit("from a stranger", "x.txt", "x\n")
	f.upstream.Git("update-ref", "refs/pull/8/head", sha)
	f.upstream.Git("reset", "--hard", "HEAD~1")

	f.watcher.Forge = &fakeForge{open: []github.Pull{{Number: 8, Fork: true}}}

	res, err := f.watcher.Once(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}

	for _, j := range res.Discovered {
		if j.Ref == "refs/pull/8/head" && !j.Fork {
			// The alternative hands an unknown author the operator's
			// credentials.
			t.Error("a fork pull request reached the engine as same-repo")
		}
	}
}

// A forge that errors is not an authoritative "nothing is open". Treating it
// as one would silently stop gating every pull request the moment GitHub had a
// bad minute.
func TestDiscovery_AForgeErrorIsNotAnEmptyAnswer(t *testing.T) {
	f := newFixture(t)
	f.alreadyWatching(t)
	sha := f.upstream.Commit("pr work", "x.txt", "x\n")
	f.upstream.Git("update-ref", "refs/pull/9/head", sha)
	f.upstream.Git("reset", "--hard", "HEAD~1")

	f.watcher.Forge = &fakeForge{err: errors.New("502 bad gateway")}

	res, err := f.watcher.Once(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, j := range res.Discovered {
		if j.Ref == "refs/pull/9/head" {
			found = true
			if !j.Fork {
				t.Error("an unverifiable pull request must fail closed as a fork")
			}
		}
	}
	if !found {
		t.Error("a forge outage silently stopped gating an unmerged pull request")
	}
}
