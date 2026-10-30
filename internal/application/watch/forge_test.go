package watch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go.klarlabs.de/kiln/internal/domain/forge"
	"go.klarlabs.de/kiln/internal/infrastructure/execx"
	"go.klarlabs.de/kiln/internal/infrastructure/gitcli"
)

// fakeForge is what a concrete *github.Client could not be: substitutable.
// Before the Forge port existed, the only way to test the closed-pull-request
// decision was to extract it as a pure function and test that in isolation,
// which left the path through discovery itself uncovered.
type fakeForge struct {
	open    []forge.Pull
	err     error
	off     bool
	queried int
}

func (f *fakeForge) Enabled() bool { return !f.off }

func (f *fakeForge) ListOpenPulls(context.Context) ([]forge.Pull, error) {
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

	forge := &fakeForge{open: []forge.Pull{{Number: 5, Fork: false}}}
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

	f.watcher.Forge = &fakeForge{open: []forge.Pull{{Number: 8, Fork: true}}}

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

// parked lists the pull request heads parked in the box's own clone, by the
// local ref name — what the fetch actually brought down, as opposed to what
// discovery decided to build.
func (f *fixture) parked(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for line := range strings.SplitSeq(f.local.Git("for-each-ref", "--format=%(refname)", PRRefNamespace), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out[line] = true
		}
	}
	return out
}

// pullRef parks a commit upstream as refs/pull/N/head, which is how GitHub
// publishes a pull request head, without moving the upstream branch.
func (f *fixture) pullRef(n int, file string) {
	sha := f.upstream.Commit(fmt.Sprintf("pr %d", n), file, "x\n")
	f.upstream.Git("update-ref", fmt.Sprintf("refs/pull/%d/head", n), sha)
	f.upstream.Git("reset", "--hard", "HEAD~1")
}

// GitHub keeps refs/pull/N/head for every pull request ever opened, and a
// wildcard fetch brought all of them down on every tick only for discovery to
// throw them away: 372 skipped against one build, every thirty minutes, on one
// box here. With an authoritative answer the closed ones are never fetched.
func TestWithATokenOnlyTheOpenPullRequestsAreFetched(t *testing.T) {
	f := newFixture(t)
	f.alreadyWatching(t)
	f.pullRef(5, "open.txt")
	f.pullRef(6, "closed.txt")
	f.watcher.Forge = &fakeForge{open: []forge.Pull{{Number: 5}}}
	// Real git, recorded: the final state alone cannot tell "never fetched"
	// from "fetched everything, then pruned", and only one of those is the fix.
	recorder := execx.NewFake()
	recorder.On("git", execx.Response{Fn: func(c execx.Cmd) (execx.Result, error) {
		return execx.NewSystem().Run(t.Context(), c)
	}})
	f.watcher.Git = gitcli.New(recorder)

	res, err := f.watcher.Once(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}

	for _, line := range recorder.Lines() {
		if strings.HasPrefix(line, "git fetch") && strings.Contains(line, "refs/pull/*") {
			t.Errorf("fetched every pull ref despite an authoritative answer: %s", line)
		}
	}
	parked := f.parked(t)
	if !parked[PRRefNamespace+"5"] {
		t.Errorf("the open pull request was not fetched: %v", parked)
	}
	if parked[PRRefNamespace+"6"] {
		t.Errorf("a closed pull request was fetched: %v", parked)
	}
	if !hasRef(res.Discovered, "refs/pull/5/head") {
		t.Errorf("the open pull request was not built: %+v", res.Discovered)
	}
}

// The parked namespace must stop growing. A box that ran the wildcard fetch —
// every box before this change, or one whose token lapsed for a while — holds
// a ref for every pull request ever opened, and nothing else ever removes them.
func TestAuthoritativeTickPrunesParkedRefsOfClosedPullRequests(t *testing.T) {
	f := newFixture(t)
	f.alreadyWatching(t)
	f.pullRef(5, "open.txt")
	f.pullRef(6, "closed.txt")

	// Tokenless: the wildcard parks both.
	if _, err := f.watcher.Once(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if parked := f.parked(t); !parked[PRRefNamespace+"6"] {
		t.Fatalf("setup: the wildcard fetch did not park #6: %v", parked)
	}

	f.watcher.Forge = &fakeForge{open: []forge.Pull{{Number: 5}}}
	if _, err := f.watcher.Once(t.Context(), true); err != nil {
		t.Fatal(err)
	}

	parked := f.parked(t)
	if parked[PRRefNamespace+"6"] {
		t.Errorf("the closed pull request's parked ref survived: %v", parked)
	}
	if !parked[PRRefNamespace+"5"] {
		t.Errorf("the open pull request's parked ref was pruned: %v", parked)
	}
}

// When GitHub cannot answer, nothing is known to be closed, so nothing may be
// left unfetched or pruned: the wildcard is the only way to see the pull
// request a tokenless box can still usefully gate.
func TestAForgeErrorFetchesEveryPullRef(t *testing.T) {
	f := newFixture(t)
	f.alreadyWatching(t)
	f.pullRef(5, "a.txt")
	f.pullRef(6, "b.txt")
	f.watcher.Forge = &fakeForge{err: errors.New("502 bad gateway")}

	if _, err := f.watcher.Once(t.Context(), true); err != nil {
		t.Fatal(err)
	}

	parked := f.parked(t)
	if !parked[PRRefNamespace+"5"] || !parked[PRRefNamespace+"6"] {
		t.Errorf("a forge outage narrowed the fetch: %v", parked)
	}
}

// A fetch by number fails whole when any one ref is missing — a remote that
// is not the repository the token answers for, or a pull request GitHub has
// not finished publishing. That must cost the narrowing, not the pull requests.
func TestATargetedFetchFailureFallsBackToTheWildcard(t *testing.T) {
	f := newFixture(t)
	f.alreadyWatching(t)
	f.pullRef(5, "open.txt")
	f.watcher.Forge = &fakeForge{open: []forge.Pull{{Number: 5}, {Number: 42}}}

	res, err := f.watcher.Once(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	if !hasRef(res.Discovered, "refs/pull/5/head") {
		t.Errorf("one unfetchable ref stopped an open pull request being gated: %+v", res.Discovered)
	}
}

// With nothing open there is nothing to fetch, and everything parked is stale.
func TestNoOpenPullRequestsPrunesEveryParkedRef(t *testing.T) {
	f := newFixture(t)
	f.alreadyWatching(t)
	f.pullRef(5, "a.txt")
	if _, err := f.watcher.Once(t.Context(), true); err != nil {
		t.Fatal(err)
	}

	f.watcher.Forge = &fakeForge{open: nil}
	res, err := f.watcher.Once(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	if parked := f.parked(t); len(parked) != 0 {
		t.Errorf("parked refs survived an empty open list: %v", parked)
	}
	if hasRef(res.Discovered, "refs/pull/5/head") {
		t.Errorf("a closed pull request was built: %+v", res.Discovered)
	}
}

// `kiln poll` is the subset of watch that needs no token, so it must not ask
// GitHub anything — moving the question ahead of the fetch must not change that.
func TestBranchesOnlyNeverAsksTheForge(t *testing.T) {
	f := newFixture(t)
	f.watcher.BranchesOnly = true
	fake := &fakeForge{}
	f.watcher.Forge = fake

	if _, err := f.watcher.Once(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if fake.queried != 0 {
		t.Errorf("asked the forge %d times on a branches-only tick", fake.queried)
	}
}
