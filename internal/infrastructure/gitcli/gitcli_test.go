package gitcli

import (
	"fmt"
	"strings"
	"testing"

	"go.klarlabs.de/kiln/internal/gittest"
	"go.klarlabs.de/kiln/internal/infrastructure/execx"
)

// A repository with many open pull requests must not put them all on one
// command line, nor pay a round trip each.
func TestFetchBatchesManyRefspecsIntoFewInvocations(t *testing.T) {
	fake := execx.NewFake()
	g := New(fake)

	refspecs := make([]string, 600)
	for i := range refspecs {
		refspecs[i] = fmt.Sprintf("+refs/pull/%d/head:refs/kiln/pr/%d", i+1, i+1)
	}
	if err := g.Fetch(t.Context(), "/repo", "origin", refspecs...); err != nil {
		t.Fatal(err)
	}

	calls := fake.Calls()
	if len(calls) != 3 {
		t.Fatalf("ran %d fetches for 600 refspecs, want 3", len(calls))
	}
	var seen int
	for _, c := range calls {
		got := c.Args[4:]
		if len(got) > fetchBatch {
			t.Errorf("one fetch carried %d refspecs, over the %d bound", len(got), fetchBatch)
		}
		seen += len(got)
	}
	if seen != len(refspecs) {
		t.Errorf("fetched %d refspecs, want all %d", seen, len(refspecs))
	}
}

func TestRemotePullRefsListsOnlyPullHeads(t *testing.T) {
	upstream := gittest.New(t)
	sha := upstream.Commit("first", "a.txt", "x\n")
	upstream.Git("update-ref", "refs/pull/3/head", sha)
	upstream.Git("update-ref", "refs/pull/3/merge", sha)
	upstream.Git("update-ref", "refs/heads/feature/refs/pull/9/head", sha)
	local := upstream.Clone(t)

	refs, err := New(execx.NewSystem()).RemotePullRefs(t.Context(), local.Dir, "origin")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Name != "refs/pull/3/head" || refs[0].SHA != sha {
		t.Errorf("RemotePullRefs = %+v, want only refs/pull/3/head", refs)
	}
}

func TestDropPullRefsRemovesOnlyTheNamedHeads(t *testing.T) {
	repo := gittest.New(t)
	sha := repo.Commit("first", "a.txt", "x\n")
	for _, n := range []int{1, 2, 3} {
		repo.Git("update-ref", fmt.Sprintf("%s%d", PullRefNamespace, n), sha)
	}
	g := New(execx.NewSystem())

	if err := g.DropPullRefs(t.Context(), repo.Dir, []string{"refs/pull/1/head", "refs/pull/3/head"}); err != nil {
		t.Fatal(err)
	}

	left := strings.TrimSpace(repo.Git("for-each-ref", "--format=%(refname)", PullRefNamespace))
	if left != PullRefNamespace+"2" {
		t.Errorf("parked refs left = %q, want only %s2", left, PullRefNamespace)
	}
}

// Anything but a pull head is refused before git runs: the namespace is the
// only thing this may ever delete from.
func TestDropPullRefsRefusesAnythingButAPullHead(t *testing.T) {
	fake := execx.NewFake()
	if err := New(fake).DropPullRefs(t.Context(), "/repo", []string{"refs/heads/main"}); err == nil {
		t.Error("a branch was accepted as a pull ref")
	}
	if len(fake.Calls()) != 0 {
		t.Errorf("git ran despite the refusal: %v", fake.Lines())
	}
}
