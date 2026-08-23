package watch

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"go.klarlabs.de/kiln/internal/domain/isolation"
	"go.klarlabs.de/kiln/internal/domain/run"
	"go.klarlabs.de/kiln/internal/github"
	"go.klarlabs.de/kiln/internal/obs"
)

// A tag is a publishing event. A fresh box that builds every tag it finds
// republishes the repository's entire release history — 133 versions on one
// repo here, each pushing images and writing fresh provenance for something
// that was signed long ago.
func TestFirstTick_DoesNotRepublishTheTagsItInherited(t *testing.T) {
	f := newFixture(t)
	f.upstream.Tag("v1.0.0")
	f.upstream.Tag("v1.1.0")

	res, err := f.watcher.Once(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}

	for _, j := range res.Discovered {
		if j.Event == isolation.EventTag {
			t.Errorf("a new box would have published %s", j.Ref)
		}
	}
	// A box that builds nothing at all on install is indistinguishable from a
	// box that is broken.
	if len(res.Discovered) == 0 {
		t.Error("the first tick did nothing, so nothing proves the pipeline runs")
	}
}

func TestFirstTick_RecordsTheBaselineOnDisk(t *testing.T) {
	f := newFixture(t)
	f.upstream.Tag("v1.0.0")

	if _, err := f.watcher.Once(t.Context(), false); err != nil {
		t.Fatal(err)
	}

	got, err := LoadBaseline(f.watcher.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("no baseline written, so the next tick republishes the history")
	}
	if _, ok := got.Tags["refs/tags/v1.0.0"]; !ok {
		t.Errorf("baseline missing the tag that was there: %+v", got.Tags)
	}
	if got.Recorded.IsZero() {
		t.Error("baseline has no timestamp saying when the box started")
	}
}

// The whole point: what happens next is the box's work.
func TestATagPushedAfterTheBoxStartedIsBuilt(t *testing.T) {
	f := newFixture(t)
	f.upstream.Tag("v1.0.0")

	if _, err := f.watcher.Once(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	f.upstream.Tag("v2.0.0")

	res, err := f.watcher.Once(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}

	var built []string
	for _, j := range res.Discovered {
		if j.Event == isolation.EventTag {
			built = append(built, j.Ref)
		}
	}
	if len(built) != 1 || built[0] != "refs/tags/v2.0.0" {
		t.Errorf("tags built = %v, want only the new one", built)
	}
}

// A tag that is moved to a different commit is new work: the artefact it would
// publish is not the one the baseline recorded.
func TestAMovedTagIsBuiltAgain(t *testing.T) {
	f := newFixture(t)
	f.upstream.Tag("v1.0.0")
	if _, err := f.watcher.Once(t.Context(), false); err != nil {
		t.Fatal(err)
	}

	f.upstream.Commit("more work", "next.txt", "x\n")
	f.upstream.Git("tag", "-f", "v1.0.0")

	res, err := f.watcher.Once(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, j := range res.Discovered {
		if j.Ref == "refs/tags/v1.0.0" {
			found = true
		}
	}
	if !found {
		t.Error("a tag moved to a new commit was treated as already published")
	}
}

// An existing box has already built its tags. Writing a baseline underneath it
// would silence a tag that is mid-backoff after a genuine failure.
func TestAnExistingBoxIsNotGivenABaseline(t *testing.T) {
	f := newFixture(t)
	if err := f.store.Save(&run.Run{ID: "run-earlier", SHA: "deadbeef"}); err != nil {
		t.Fatal(err)
	}
	f.upstream.Tag("v1.0.0")

	if _, err := f.watcher.Once(t.Context(), false); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(f.watcher.Dir, BaselineFile)); !os.IsNotExist(err) {
		t.Error("a box with history was given a baseline, which can silence a retry")
	}
}

func TestDryRunDoesNotRecordABaseline(t *testing.T) {
	f := newFixture(t)
	f.upstream.Tag("v1.0.0")

	if _, err := f.watcher.Once(t.Context(), true); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(f.watcher.Dir, BaselineFile)); !os.IsNotExist(err) {
		t.Error("a dry run wrote state")
	}
}

// A box that loses its token must not start gating the repository's whole
// pull request history again.
//
// This is not hypothetical. dispatch squash-merges, so a merged pull request's
// head is never an ancestor of main and `merge-base --is-ancestor` cannot see
// it. With a token the box skipped 42 refs and built none; the moment the
// token went away — a `brew upgrade` moved the binary out of the keychain ACL
// — it started gating #31, which had merged.
func TestATokenlessTickDoesNotReplayTheBaselinedPullRequests(t *testing.T) {
	f := newFixture(t)
	head := f.upstream.Commit("pr work", "feature.txt", "x\n")
	f.upstream.Git("update-ref", "refs/pull/7/head", head)
	// Squash-merge shape: the branch moves on with a *different* commit, so
	// the pull head is not an ancestor of it.
	f.upstream.Git("reset", "--hard", "HEAD~1")
	f.upstream.Commit("the squashed version", "feature.txt", "x\n")

	// First tick records the baseline.
	if _, err := f.watcher.Once(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	f.watcher.GitHub = nil // the token goes away

	res, err := f.watcher.Once(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}

	for _, j := range res.Discovered {
		if j.Ref == "refs/pull/7/head" {
			t.Errorf("a tokenless tick replayed a baselined pull request: %+v", j)
		}
	}
}

// The baseline must not silence a pull request opened after the box started,
// which is the only kind a tokenless box can still usefully gate.
func TestATokenlessTickStillBuildsAPullRequestOpenedSinceTheBaseline(t *testing.T) {
	f := newFixture(t)
	if _, err := f.watcher.Once(t.Context(), false); err != nil {
		t.Fatal(err)
	}

	head := f.upstream.Commit("new pr", "new.txt", "x\n")
	f.upstream.Git("update-ref", "refs/pull/9/head", head)
	f.upstream.Git("reset", "--hard", "HEAD~1")
	f.watcher.GitHub = nil

	res, err := f.watcher.Once(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, j := range res.Discovered {
		if j.Ref == "refs/pull/9/head" {
			found = true
			if !j.Fork {
				t.Error("a pull request that cannot be checked must be treated as a fork")
			}
		}
	}
	if !found {
		t.Error("a pull request opened after the box started was never gated")
	}
}

// With a token the API is the authority, and a pull request open at install
// time must still be gated on every tick — the baseline must not reach it.
func TestAnOpenPullRequestIsNotSilencedByTheBaseline(t *testing.T) {
	f := newFixture(t)
	head := f.upstream.Commit("pr work", "feature.txt", "x\n")
	f.upstream.Git("update-ref", "refs/pull/7/head", head)
	f.upstream.Git("reset", "--hard", "HEAD~1")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[
			{"number": 7, "head": {"sha": "x", "ref": "f", "repo": {"full_name": "klarlabs-studio/kiln"}},
			 "base": {"repo": {"full_name": "klarlabs-studio/kiln"}}}
		]`))
	}))
	defer srv.Close()
	c := github.NewClient("tok", github.Repo{Owner: "klarlabs-studio", Name: "kiln"}, obs.Discard())
	c.BaseURL = srv.URL
	f.watcher.GitHub = c

	if _, err := f.watcher.Once(t.Context(), false); err != nil {
		t.Fatal(err)
	}

	res, err := f.watcher.Once(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}

	if !hasRef(res.Discovered, "refs/pull/7/head") {
		t.Errorf("the baseline silenced a pull request the API vouched for: %+v", res.Discovered)
	}
}

// The baseline records every pull ref present, not the subset a token said was
// open — otherwise the closed ones, which are the bulk, come back the moment
// the token does not answer.
func TestTheBaselineRecordsClosedPullRefsToo(t *testing.T) {
	f := newFixture(t)
	head := f.upstream.Commit("long merged", "old.txt", "x\n")
	f.upstream.Git("update-ref", "refs/pull/3/head", head)
	f.upstream.Git("reset", "--hard", "HEAD~1")

	if _, err := f.watcher.Once(t.Context(), false); err != nil {
		t.Fatal(err)
	}

	got, err := LoadBaseline(f.watcher.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Pulls["refs/pull/3/head"] == "" {
		t.Errorf("baseline did not record the pull ref: %+v", got)
	}
}
