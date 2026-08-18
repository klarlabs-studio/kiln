package prune

import (
	"slices"
	"strings"
	"testing"
	"time"

	"go.klarlabs.de/kiln/internal/execx"
	"go.klarlabs.de/kiln/internal/obs"
)

const repo = "ghcr.io/felixgeelhaar/glossa-api"

// listing renders what `docker image ls` would print for a repository.
func listing(rows ...[3]string) string {
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(strings.Join(r[:], "\t") + "\n")
	}
	return b.String()
}

// row builds one listing line: tag, image id, and an age in days.
func row(tag, id string, ageDays int) [3]string {
	when := time.Now().Add(-time.Duration(ageDays) * 24 * time.Hour)
	return [3]string{tag, id, when.Format(dockerTimeLayout)}
}

func fakeDocker(rows ...[3]string) *execx.Fake {
	f := execx.NewFake()
	f.On("docker image ls", execx.Response{Stdout: listing(rows...)})
	return f
}

func prune(t *testing.T, f *execx.Fake, opts Options) Result {
	t.Helper()
	if opts.Repos == nil {
		opts.Repos = []string{repo}
	}
	res, err := New(f, obs.Discard()).Prune(t.Context(), opts)
	if err != nil {
		t.Fatalf("Prune: %v\n%s", err, f.Transcript())
	}
	return res
}

func TestOldBuildsAreRemovedAndRecentOnesKept(t *testing.T) {
	f := fakeDocker(
		row("sha-aaaaaaa", "id1", 1),
		row("sha-bbbbbbb", "id2", 2),
		row("sha-ccccccc", "id3", 3),
		row("sha-ddddddd", "id4", 4),
	)

	res := prune(t, f, Options{Keep: 2})

	if len(res.Removed) != 2 {
		t.Fatalf("removed %v, want the two oldest", res.Removed)
	}
	for _, want := range []string{repo + ":sha-ccccccc", repo + ":sha-ddddddd"} {
		if !slices.Contains(res.Removed, want) {
			t.Errorf("did not remove %s: %v", want, res.Removed)
		}
	}
	// Retention is about being able to roll back; the newest must survive.
	for _, keep := range []string{"sha-aaaaaaa", "sha-bbbbbbb"} {
		if f.Ran("docker image rm " + repo + ":" + keep) {
			t.Errorf("removed a recent build: %s", keep)
		}
	}
}

func TestMovingTagsAreNeverRemoved(t *testing.T) {
	f := fakeDocker(
		row("latest", "id1", 1),
		row("v1.2.0", "id1", 1),
		row("sha-aaaaaaa", "id1", 1),
		row("sha-bbbbbbb", "id2", 9),
		row("sha-ccccccc", "id3", 10),
	)

	res := prune(t, f, Options{Keep: 1})

	for _, ref := range res.Removed {
		if strings.HasSuffix(ref, ":latest") || strings.HasSuffix(ref, ":v1.2.0") {
			t.Errorf("removed a moving tag RollOps follows: %s", ref)
		}
	}
	// sha-aaaaaaa shares an image with :latest. Removing it by the sha name
	// would take the moving tag with it.
	if slices.Contains(res.Removed, repo+":sha-aaaaaaa") {
		t.Errorf("removed the image :latest points at: %v", res.Removed)
	}
}

func TestNothingOutsideThePipelinesRepositories(t *testing.T) {
	f := fakeDocker(row("sha-aaaaaaa", "id1", 30))

	prune(t, f, Options{Keep: 0})

	// Keep: 0 disables image pruning, and nothing may reach for the blunt
	// instrument that would take every unused image on a shared daemon.
	for _, line := range f.Lines() {
		if strings.Contains(line, "image prune") || strings.Contains(line, "-a") {
			t.Errorf("used a daemon-wide prune: %s", line)
		}
	}
	if f.Ran("docker image rm") {
		t.Errorf("pruned with Keep=0: %s", f.Transcript())
	}
}

func TestNoReposMeansNoImagesTouched(t *testing.T) {
	f := fakeDocker(row("sha-aaaaaaa", "id1", 30))

	prune(t, f, Options{Repos: []string{}, Keep: 10})

	if f.Ran("docker image rm") {
		t.Errorf("kiln prunes what it made, and nothing else: %s", f.Transcript())
	}
}

func TestFewerBuildsThanTheLimitRemovesNothing(t *testing.T) {
	f := fakeDocker(row("sha-aaaaaaa", "id1", 1), row("sha-bbbbbbb", "id2", 2))

	res := prune(t, f, Options{Keep: 10})

	if len(res.Removed) != 0 {
		t.Errorf("removed %v from a repository under the limit", res.Removed)
	}
	if res.Kept != 2 {
		t.Errorf("Kept = %d, want 2", res.Kept)
	}
}

func TestDryRunReportsWithoutRemoving(t *testing.T) {
	f := fakeDocker(row("sha-aaaaaaa", "id1", 1), row("sha-bbbbbbb", "id2", 2))

	res := prune(t, f, Options{Keep: 1, DryRun: true, BuildCacheMaxAge: time.Hour})

	if len(res.Removed) != 1 {
		t.Errorf("Removed = %v, want the plan", res.Removed)
	}
	if f.Ran("docker image rm") || f.Ran("docker builder prune") {
		t.Errorf("a dry run deleted something: %s", f.Transcript())
	}
	if !strings.Contains(res.CacheFreed, "would prune") {
		t.Errorf("CacheFreed = %q, want the plan", res.CacheFreed)
	}
}

func TestBuildCacheIsPrunedByAge(t *testing.T) {
	f := fakeDocker(row("sha-aaaaaaa", "id1", 1))
	f.On("docker builder prune", execx.Response{
		Stdout: "deleted: abc\ndeleted: def\nTotal reclaimed space: 11.34GB\n",
	})

	res := prune(t, f, Options{Keep: 10, BuildCacheMaxAge: 7 * 24 * time.Hour})

	cmd := f.Find("docker builder prune")
	if cmd == nil {
		t.Fatalf("cache not pruned: %s", f.Transcript())
	}
	if !strings.Contains(cmd.String(), "until=168h") {
		t.Errorf("age filter missing: %s", cmd.String())
	}
	// docker's own summary is the honest number to report.
	if res.CacheFreed != "Total reclaimed space: 11.34GB" {
		t.Errorf("CacheFreed = %q", res.CacheFreed)
	}
}

func TestZeroCacheAgeLeavesTheCacheAlone(t *testing.T) {
	f := fakeDocker(row("sha-aaaaaaa", "id1", 1))

	prune(t, f, Options{Keep: 10, BuildCacheMaxAge: 0})

	// The cache is shared with every other build on the box; an operator who
	// did not ask must not lose it.
	if f.Ran("docker builder prune") {
		t.Errorf("pruned a shared cache unasked: %s", f.Transcript())
	}
}

func TestMissingDockerIsNotAFailure(t *testing.T) {
	f := fakeDocker().Absent("docker")

	res, err := New(f, obs.Discard()).Prune(t.Context(), Options{Repos: []string{repo}, Keep: 10})

	// A prove-only box has nothing to collect and should not fail
	// housekeeping over a tool it never needed.
	if err != nil {
		t.Errorf("err = %v", err)
	}
	if len(res.Removed) != 0 {
		t.Errorf("Removed = %v", res.Removed)
	}
}

func TestAnUnreadableRepositoryDoesNotStopTheCache(t *testing.T) {
	f := execx.NewFake()
	f.On("docker image ls", execx.Response{ExitCode: 1, Stderr: "cannot connect"})
	f.On("docker builder prune", execx.Response{Stdout: "Total reclaimed space: 1GB"})

	res := prune(t, f, Options{Keep: 10, BuildCacheMaxAge: time.Hour})

	// The cache is the bigger reclaim; losing it because one image listing
	// failed would forfeit most of the point.
	if res.CacheFreed == "" {
		t.Errorf("cache prune skipped after an image failure: %s", f.Transcript())
	}
}

func TestAnImageInUseIsSkippedNotFatal(t *testing.T) {
	f := fakeDocker(
		row("sha-aaaaaaa", "id1", 1),
		row("sha-bbbbbbb", "id2", 2),
		row("sha-ccccccc", "id3", 3),
	)
	f.On("docker image rm "+repo+":sha-ccccccc", execx.Response{
		ExitCode: 1, Stderr: "image is being used by running container",
	})

	res := prune(t, f, Options{Keep: 1})

	// docker refusing is docker protecting something in use.
	if slices.Contains(res.Removed, repo+":sha-ccccccc") {
		t.Error("reported removing an image docker refused to delete")
	}
	if !slices.Contains(res.Removed, repo+":sha-bbbbbbb") {
		t.Errorf("one stuck image stopped the sweep: %v", res.Removed)
	}
}

func TestNonKilnTagsAreIgnoredEntirely(t *testing.T) {
	f := fakeDocker(
		row("dev", "id1", 40),
		row("someone-elses-tag", "id2", 40),
		row("sha-aaaaaaa", "id3", 40),
		row("sha-bbbbbbb", "id4", 41),
	)

	res := prune(t, f, Options{Keep: 1})

	for _, ref := range res.Removed {
		if !strings.Contains(ref, ":"+shaTagPrefix) {
			t.Errorf("removed a tag kiln did not create: %s", ref)
		}
	}
}

func TestRetentionIsRepeatableWhenTimestampsTie(t *testing.T) {
	// Every reproducible image reports 1970 — FROM scratch, or anything built
	// with SOURCE_DATE_EPOCH — so a whole repository can tie.
	epoch := time.Unix(0, 0).Format(dockerTimeLayout)
	rows := [][3]string{
		{"sha-ddddddd", "id4", epoch},
		{"sha-aaaaaaa", "id1", epoch},
		{"sha-ccccccc", "id3", epoch},
		{"sha-bbbbbbb", "id2", epoch},
	}

	first := prune(t, fakeDocker(rows...), Options{Keep: 2, DryRun: true})
	// Docker's listing order is not stable between calls, so the second sweep
	// sees the same images in a different order.
	slices.Reverse(rows)
	second := prune(t, fakeDocker(rows...), Options{Keep: 2, DryRun: true})

	if !slices.Equal(first.Removed, second.Removed) {
		t.Errorf("retention is not repeatable: %v then %v", first.Removed, second.Removed)
	}
}

func TestADryRunPredictsTheRealRun(t *testing.T) {
	epoch := time.Unix(0, 0).Format(dockerTimeLayout)
	rows := [][3]string{
		{"sha-ccccccc", "id3", epoch},
		{"sha-aaaaaaa", "id1", epoch},
		{"sha-bbbbbbb", "id2", epoch},
	}

	planned := prune(t, fakeDocker(rows...), Options{Keep: 1, DryRun: true})

	f := fakeDocker(rows...)
	actual := prune(t, f, Options{Keep: 1})

	// An operator reads the plan, approves it, and expects that to be what
	// happens. A dry run that names different images is worse than none.
	if !slices.Equal(planned.Removed, actual.Removed) {
		t.Errorf("plan %v, actual %v", planned.Removed, actual.Removed)
	}
}

func TestNewestSurvivesWhenTimestampsDiffer(t *testing.T) {
	f := fakeDocker(
		row("sha-zzzzzzz", "id1", 1),
		row("sha-aaaaaaa", "id2", 30),
	)

	res := prune(t, f, Options{Keep: 1})

	// Time wins over the tiebreak: the day-old build survives even though its
	// tag sorts last.
	if !slices.Contains(res.Removed, repo+":sha-aaaaaaa") || len(res.Removed) != 1 {
		t.Errorf("Removed = %v, want the month-old build", res.Removed)
	}
}
