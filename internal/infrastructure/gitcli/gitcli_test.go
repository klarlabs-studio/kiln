package gitcli

import (
	"testing"

	"go.klarlabs.de/kiln/internal/gittest"
	"go.klarlabs.de/kiln/internal/infrastructure/execx"
)

func TestResolveTurnsARefIntoACommit(t *testing.T) {
	repo := gittest.New(t)
	sha := repo.Commit("first", "app.txt", "one\n")
	g := New(execx.NewSystem())

	got, err := g.Resolve(t.Context(), repo.Dir, "HEAD")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != sha {
		t.Errorf("Resolve(HEAD) = %s, want %s", got, sha)
	}
}

func TestContainsIsAncestryNotEquality(t *testing.T) {
	repo := gittest.New(t)
	first := repo.Commit("first", "app.txt", "one\n")
	second := repo.Commit("second", "app.txt", "two\n")
	g := New(execx.NewSystem())

	ok, err := g.Contains(t.Context(), repo.Dir, first, second)
	if err != nil || !ok {
		t.Fatalf("first should be an ancestor of second: ok=%v err=%v", ok, err)
	}
	ok, err = g.Contains(t.Context(), repo.Dir, second, first)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("a descendant is not an ancestor")
	}
}

func TestTagsPeelsAnAnnotatedTag(t *testing.T) {
	repo := gittest.New(t)
	sha := repo.Commit("first", "app.txt", "one\n")
	repo.Tag("v1.0.0")
	g := New(execx.NewSystem())

	tags, err := g.Tags(t.Context(), repo.Dir)
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}
	if len(tags) != 1 || tags[0].SHA != sha {
		t.Fatalf("tags = %+v, want the peeled commit %s", tags, sha)
	}
}

func TestContainsRejectsAForeignSHA(t *testing.T) {
	repo := gittest.New(t)
	repo.Commit("first", "app.txt", "one\n")
	g := New(execx.NewSystem())

	ok, err := g.Contains(t.Context(), repo.Dir, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("an unknown SHA must not be treated as on the tip")
	}
}

func TestHeadSHAReadsTheRemoteTrackingBranch(t *testing.T) {
	upstream := gittest.New(t)
	sha := upstream.Commit("first", "app.txt", "one\n")
	clone := upstream.Clone(t)
	g := New(execx.NewSystem())

	got, err := g.HeadSHA(t.Context(), clone.Dir, "origin", "main")
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}
	if got != sha {
		t.Errorf("HeadSHA = %s, want %s", got, sha)
	}
}

func TestPullRefsMapsTheParkingNamespace(t *testing.T) {
	repo := gittest.New(t)
	sha := repo.Commit("first", "app.txt", "one\n")
	repo.Git("update-ref", PullRefNamespace+"7", sha)
	g := New(execx.NewSystem())

	refs, err := g.PullRefs(t.Context(), repo.Dir)
	if err != nil {
		t.Fatalf("PullRefs: %v", err)
	}
	if len(refs) != 1 || refs[0].Name != "refs/pull/7/head" || refs[0].SHA != sha {
		t.Fatalf("refs = %+v", refs)
	}
}

func TestResolveRejectsAnEmptyRef(t *testing.T) {
	g := New(execx.NewSystem())
	if _, err := g.Resolve(t.Context(), t.TempDir(), "  "); err == nil {
		t.Fatal("empty ref must fail")
	}
}
