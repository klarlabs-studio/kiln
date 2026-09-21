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
