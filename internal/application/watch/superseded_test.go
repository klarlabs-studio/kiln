package watch

import (
	"slices"
	"testing"
	"time"

	"go.klarlabs.de/kiln/internal/domain/isolation"
	"go.klarlabs.de/kiln/internal/domain/run"
)

// tagRefs is the tags a tick would build, sorted.
func tagRefs(res Result) []string {
	var out []string
	for _, j := range res.Discovered {
		if j.Event == isolation.EventTag {
			out = append(out, j.Ref)
		}
	}
	slices.Sort(out)
	return out
}

// record writes a finished tag run into the ledger, as a tick would have.
func (f *fixture) record(t *testing.T, tag string, phase run.Phase) {
	t.Helper()
	r := &run.Run{
		ID:        "run-" + tag + "-" + string(phase),
		SHA:       f.upstream.Head(),
		Ref:       "refs/tags/" + tag,
		Event:     isolation.EventTag.String(),
		Phase:     phase,
		StartedAt: time.Now().Add(-time.Hour),
	}
	if err := f.store.Save(r); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) tags(names ...string) {
	for _, n := range names {
		f.upstream.Commit(n, "version.txt", n+"\n")
		f.upstream.Tag(n)
	}
}

func (f *fixture) tick(t *testing.T) Result {
	t.Helper()
	res, err := f.watcher.Once(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func assertTags(t *testing.T, res Result, want ...string) {
	t.Helper()
	slices.Sort(want)
	if got := tagRefs(res); !slices.Equal(got, want) {
		t.Errorf("tags built = %v, want %v", got, want)
	}
}

// The incident this exists for: a box with run history retried v0.0.0, v0.1.0
// and v0.2.0 about seventy times each. Every one failed, so no success ever
// silenced them, and the backoff only spaced the retries out — for releases
// that newer ones had long since replaced.
func TestATagOlderThanAShippedReleaseIsNotRetried(t *testing.T) {
	f := newFixture(t)
	f.alreadyWatching(t)
	f.tags("v0.0.0", "v0.1.0", "v0.2.0", "v0.3.0")
	for _, old := range []string{"v0.0.0", "v0.1.0", "v0.2.0"} {
		f.record(t, old, run.PhaseFailed)
	}
	f.record(t, "v0.3.0", run.PhaseSucceeded)

	assertTags(t, f.tick(t), "refs/tags/v0.3.0")
}

// A box that has never shipped a release has no watermark to compare with.
// Building every tag would replay the repository's history; building none
// would leave the box looking broken. The head of the release line is what an
// operator wants published.
func TestWithNothingShippedOnlyTheNewestTagIsBuilt(t *testing.T) {
	f := newFixture(t)
	f.alreadyWatching(t)
	f.tags("v0.0.0", "v0.1.0", "v0.2.0")

	assertTags(t, f.tick(t), "refs/tags/v0.2.0")
}

// A release cut since the last success is new work even when its build keeps
// failing, and a newer tag failing does not bring the older ones back.
func TestAFailingNewTagDoesNotReviveTheOlderOnes(t *testing.T) {
	f := newFixture(t)
	f.alreadyWatching(t)
	f.tags("v1.0.0", "v1.1.0", "v1.2.0", "v1.3.0")
	f.record(t, "v1.0.0", run.PhaseFailed)
	f.record(t, "v1.1.0", run.PhaseSucceeded)
	f.record(t, "v1.2.0", run.PhaseFailed)
	f.record(t, "v1.3.0", run.PhaseFailed)

	assertTags(t, f.tick(t), "refs/tags/v1.2.0", "refs/tags/v1.3.0")
}

// A release candidate is overtaken by the release it led up to, and the next
// one is ahead of everything that shipped.
func TestPreReleasesAreOrderedBelowTheirRelease(t *testing.T) {
	f := newFixture(t)
	f.alreadyWatching(t)
	f.tags("v1.0.0-rc.1", "v1.0.0", "v1.1.0-rc.1")
	f.record(t, "v1.0.0", run.PhaseSucceeded)

	assertTags(t, f.tick(t), "refs/tags/v1.1.0-rc.1")
}

// A tag that is not a version has no place in an order, so nothing can
// supersede it. It is built exactly as it was before.
func TestATagThatIsNotAVersionIsStillBuilt(t *testing.T) {
	f := newFixture(t)
	f.alreadyWatching(t)
	f.tags("nightly", "v1.0.0", "v1.1.0")

	assertTags(t, f.tick(t), "refs/tags/nightly", "refs/tags/v1.1.0")
}

// The baseline is a watermark too: what a new box inherited already shipped,
// so two releases pushed between ticks after it are both built — not just the
// newer of them.
func TestEveryReleaseCutSinceTheBaselineIsBuilt(t *testing.T) {
	f := newFixture(t)
	f.tags("v1.0.0", "v1.1.0")
	if _, err := f.watcher.Once(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	f.tags("v1.2.0", "v1.3.0")

	assertTags(t, f.tick(t), "refs/tags/v1.2.0", "refs/tags/v1.3.0")
}

// The newest release is the newest the repository has, inherited or not. A
// box installed after v1.1.0 treats a later v1.0.1 exactly as a box that had
// built v1.1.0 itself would: overtaken, unless the pipeline asks otherwise.
func TestATagBehindAnInheritedReleaseIsOvertaken(t *testing.T) {
	f := newFixture(t)
	f.tags("v1.0.0", "v1.1.0")
	if _, err := f.watcher.Once(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	f.tags("v1.0.1")

	assertTags(t, f.tick(t))
}

// A patch on an older major line is usually a security backport: v1.9.5 cut
// after v2.0.0 shipped is for whoever still runs v1. Judged against the whole
// repository it would be skipped silently, which is the worse failure.
func TestABackportOnAnOlderMajorLineIsBuilt(t *testing.T) {
	f := newFixture(t)
	f.alreadyWatching(t)
	f.tags("v1.9.4", "v2.0.0", "v1.9.5")
	f.record(t, "v1.9.4", run.PhaseSucceeded)
	f.record(t, "v2.0.0", run.PhaseSucceeded)

	assertTags(t, f.tick(t), "refs/tags/v1.9.5", "refs/tags/v2.0.0")
}

// Within its own line an older patch is still overtaken: v1.9.4 is history
// once v1.9.5 has shipped, whatever v2 is doing.
func TestAnOlderPatchBehindAShippedOneIsSkipped(t *testing.T) {
	f := newFixture(t)
	f.alreadyWatching(t)
	f.tags("v1.9.4", "v1.9.5", "v2.0.0")
	f.record(t, "v1.9.4", run.PhaseFailed)
	f.record(t, "v1.9.5", run.PhaseSucceeded)
	f.record(t, "v2.0.0", run.PhaseSucceeded)

	assertTags(t, f.tick(t), "refs/tags/v1.9.5", "refs/tags/v2.0.0")
}

// Every tag, overtaken or not, when the pipeline says so.
func TestSupersededTagsAreBuiltWhenThePipelineAsks(t *testing.T) {
	f := newFixture(t)
	f.alreadyWatching(t)
	f.tags("v1.0.0", "v2.0.0", "v1.0.1")
	f.record(t, "v2.0.0", run.PhaseSucceeded)
	f.watcher.Pipeline.Watch.SupersededTags = true

	assertTags(t, f.tick(t), "refs/tags/v1.0.0", "refs/tags/v1.0.1", "refs/tags/v2.0.0")
}
