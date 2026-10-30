package gocache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

// claimed returns a kiln-owned cache directory.
func claimed(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "go-build")
	if err := Claim(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func trimmer(dir string, maxSize int64) *Trimmer {
	tr := NewTrimmer(dir, maxSize)
	tr.Now = func() time.Time { return now }
	return tr
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestTrimRemovesEntriesUnusedForADay(t *testing.T) {
	dir := claimed(t)
	stale := writeEntry(t, dir, "00", "aaaa-d", 100, now.Add(-25*time.Hour))
	fresh := writeEntry(t, dir, "01", "bbbb-d", 100, now.Add(-23*time.Hour))

	res, err := trimmer(dir, 0).Trim(t.Context())
	if err != nil {
		t.Fatalf("Trim: %v", err)
	}
	if exists(stale) {
		t.Error("an entry idle for 25h should be gone")
	}
	if !exists(fresh) {
		t.Error("an entry used 23h ago should survive")
	}
	if res.Files != 1 || res.Freed != 100 {
		t.Errorf("result = %+v, want 1 file, 100 bytes", res)
	}
}

func TestTrimEvictsLeastRecentlyUsedUntilUnderTheCap(t *testing.T) {
	dir := claimed(t)
	oldest := writeEntry(t, dir, "00", "aaaa-d", 400, now.Add(-5*time.Hour))
	older := writeEntry(t, dir, "01", "bbbb-d", 400, now.Add(-4*time.Hour))
	newer := writeEntry(t, dir, "02", "cccc-d", 400, now.Add(-3*time.Hour))

	// 1200 bytes against a cap of 500: the two oldest go, the newest stays.
	res, err := trimmer(dir, 500).Trim(t.Context())
	if err != nil {
		t.Fatalf("Trim: %v", err)
	}
	if exists(oldest) || exists(older) {
		t.Error("the least recently used entries should be evicted first")
	}
	if !exists(newer) {
		t.Error("eviction should stop once the cache is under the cap")
	}
	if res.Files != 2 || res.Freed != 800 {
		t.Errorf("result = %+v, want 2 files, 800 bytes", res)
	}
}

// Go refreshes an entry's mtime on use only once it is an hour stale, so an
// mtime inside the last hour says nothing about how recently the entry was
// read — it may be in use by the build running right now.
func TestTrimNeverEvictsEntriesTouchedWithinTheHour(t *testing.T) {
	dir := claimed(t)
	recent := writeEntry(t, dir, "00", "aaaa-d", 1000, now.Add(-30*time.Minute))
	old := writeEntry(t, dir, "01", "bbbb-d", 1000, now.Add(-2*time.Hour))

	res, err := trimmer(dir, 10).Trim(t.Context())
	if err != nil {
		t.Fatalf("Trim: %v", err)
	}
	if !exists(recent) {
		t.Error("an entry touched within the hour must survive even over the cap")
	}
	if exists(old) {
		t.Error("an older entry should still be evicted")
	}
	if res.Files != 1 {
		t.Errorf("result = %+v, want 1 file", res)
	}
}

func TestTrimUnderTheCapEvictsNothingRecent(t *testing.T) {
	dir := claimed(t)
	a := writeEntry(t, dir, "00", "aaaa-d", 100, now.Add(-5*time.Hour))

	res, err := trimmer(dir, 1000).Trim(t.Context())
	if err != nil {
		t.Fatalf("Trim: %v", err)
	}
	if !exists(a) || res.Files != 0 {
		t.Errorf("nothing should go under the cap: %+v", res)
	}
}

func TestTrimLeavesFilesOutsideTheCacheLayoutAlone(t *testing.T) {
	dir := claimed(t)
	old := now.Add(-48 * time.Hour)
	readme := writeEntry(t, dir, ".", "README", 10, old)
	stray := writeEntry(t, dir, "notes", "keep.txt", 10, old)
	upper := writeEntry(t, dir, "AB", "x-d", 10, old)

	if _, err := trimmer(dir, 0).Trim(t.Context()); err != nil {
		t.Fatalf("Trim: %v", err)
	}
	for _, p := range []string{readme, stray, upper, filepath.Join(dir, marker)} {
		if !exists(p) {
			t.Errorf("%s is not a cache entry and should be untouched", p)
		}
	}
}

// The guard that matters most: if the directory is not one kiln claimed —
// an operator's own cache, say — the trimmer touches nothing at all.
func TestTrimRefusesADirectoryKilnDoesNotOwn(t *testing.T) {
	dir := t.TempDir()
	entry := writeEntry(t, dir, "00", "aaaa-d", 100, now.Add(-48*time.Hour))

	_, err := trimmer(dir, 0).Trim(t.Context())
	if !errors.Is(err, ErrNotOwned) {
		t.Fatalf("Trim = %v, want ErrNotOwned", err)
	}
	if !exists(entry) {
		t.Error("an unowned directory must not be trimmed")
	}
}

func TestTrimOfAMissingDirectoryIsNotAnError(t *testing.T) {
	// Claimed at boot, then removed by hand: nothing to trim, nothing to fail.
	dir := claimed(t)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	res, err := trimmer(dir, 0).Trim(t.Context())
	if err != nil || res.Files != 0 {
		t.Errorf("Trim = %+v, %v", res, err)
	}
}

func TestTrimStopsOnACancelledContext(t *testing.T) {
	dir := claimed(t)
	writeEntry(t, dir, "00", "aaaa-d", 100, now.Add(-48*time.Hour))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := trimmer(dir, 0).Trim(ctx); err == nil {
		t.Error("want the cancellation reported")
	}
}
