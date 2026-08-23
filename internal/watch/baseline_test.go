package watch

import (
	"os"
	"path/filepath"
	"testing"

	"go.klarlabs.de/kiln/internal/isolation"
	"go.klarlabs.de/kiln/internal/run"
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
