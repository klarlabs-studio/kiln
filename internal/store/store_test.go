package store

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.klarlabs.de/kiln/internal/run"
)

// Both implementations satisfy the same contract, so both run the same suite.
func each(t *testing.T, fn func(t *testing.T, s Store)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) { fn(t, NewMemory()) })
	t.Run("file", func(t *testing.T) {
		fn(t, NewFile(filepath.Join(t.TempDir(), ".kiln", "state.json")))
	})
}

func mkRun(id, sha, ref string, phase run.Phase, at time.Time) *run.Run {
	return &run.Run{
		ID: id, SHA: sha, Ref: ref, Event: "push",
		Phase: phase, StartedAt: at,
	}
}

func TestSaveAndGet(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		want := mkRun("run-1", "abc", "refs/heads/main", run.PhaseSucceeded, time.Now())
		if err := s.Save(want); err != nil {
			t.Fatalf("Save: %v", err)
		}

		got, err := s.Get("run-1")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.SHA != "abc" || got.Phase != run.PhaseSucceeded {
			t.Errorf("Get = %+v", got)
		}
	})
}

func TestGetUnknownIsNotFound(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		if _, err := s.Get("nope"); !errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})
}

func TestLatestOnEmptyLedger(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		if _, err := s.Latest(); !errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})
}

func TestLatestIsNewestByStartTime(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		base := time.Now().Add(-time.Hour)
		// Saved out of order on purpose: the ledger orders by start time, not
		// by arrival.
		mustSave(t, s, mkRun("run-old", "aaa", "r", run.PhaseSucceeded, base))
		mustSave(t, s, mkRun("run-new", "bbb", "r", run.PhaseFailed, base.Add(time.Minute)))
		mustSave(t, s, mkRun("run-mid", "ccc", "r", run.PhaseSucceeded, base.Add(30*time.Second)))

		got, err := s.Latest()
		if err != nil {
			t.Fatalf("Latest: %v", err)
		}
		if got.ID != "run-new" {
			t.Errorf("Latest = %s, want run-new", got.ID)
		}
	})
}

func TestSaveReplacesSameID(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		at := time.Now()
		mustSave(t, s, mkRun("run-1", "abc", "r", run.PhaseProving, at))
		mustSave(t, s, mkRun("run-1", "abc", "r", run.PhaseSucceeded, at))

		all, err := s.List()
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(all) != 1 {
			t.Fatalf("List has %d runs, want 1 (a phase update must not append)", len(all))
		}
		if all[0].Phase != run.PhaseSucceeded {
			t.Errorf("phase = %s, want succeeded", all[0].Phase)
		}
	})
}

func TestLastSuccessIgnoresFailures(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		at := time.Now()
		mustSave(t, s, mkRun("run-fail", "abc", "refs/heads/main", run.PhaseFailed, at))

		// A failed run must never suppress a retry on the next watch tick.
		if _, err := s.LastSuccess("abc", "refs/heads/main"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}

		mustSave(t, s, mkRun("run-ok", "abc", "refs/heads/main", run.PhaseSucceeded, at.Add(time.Second)))
		if _, err := s.LastSuccess("abc", "refs/heads/main"); err != nil {
			t.Errorf("LastSuccess after a success: %v", err)
		}
	})
}

func TestLastSuccessIsRefScoped(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		mustSave(t, s, mkRun("run-1", "abc", "refs/heads/main", run.PhaseSucceeded, time.Now()))

		// Same commit, different ref: a tag pointing at an already-built branch
		// head still needs its own run, because the tag routes to different
		// steps and produces a different moving tag.
		if _, err := s.LastSuccess("abc", "refs/tags/v1.0.0"); !errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})
}

func TestSaveRejectsRunWithoutID(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		if err := s.Save(&run.Run{SHA: "abc"}); err == nil {
			t.Error("Save accepted a run with no id")
		}
		if err := s.Save(nil); err == nil {
			t.Error("Save accepted nil")
		}
	})
}

func TestStoreHandsOutClones(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		original := mkRun("run-1", "abc", "r", run.PhaseSucceeded, time.Now())
		original.Tags = []string{"ghcr.io/x/y:latest"}
		mustSave(t, s, original)

		// Mutating the saved value must not reach into the ledger...
		original.SHA = "mutated"
		original.Tags[0] = "mutated"

		got, err := s.Get("run-1")
		if err != nil {
			t.Fatal(err)
		}
		if got.SHA != "abc" || got.Tags[0] != "ghcr.io/x/y:latest" {
			t.Errorf("ledger was mutated through the caller's pointer: %+v", got)
		}

		// ...nor must mutating a returned value.
		got.SHA = "mutated-again"
		again, _ := s.Get("run-1")
		if again.SHA != "abc" {
			t.Errorf("ledger was mutated through a returned pointer: %+v", again)
		}
	})
}

func TestFileCreatesDirectoryLazily(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".kiln", "state.json")
	s := NewFile(path)

	// A read against a repo that has never run must leave no trace.
	if _, err := s.Latest(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Latest: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".kiln")); !os.IsNotExist(err) {
		t.Error("a read created the ledger directory")
	}

	mustSave(t, s, mkRun("run-1", "abc", "r", run.PhaseSucceeded, time.Now()))
	if _, err := os.Stat(path); err != nil {
		t.Errorf("ledger not written: %v", err)
	}
}

func TestFilePersistsAcrossInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	mustSave(t, NewFile(path), mkRun("run-1", "abc", "r", run.PhaseSucceeded, time.Now()))

	// A second process — modelled here as a second File — must see it. This is
	// why File re-reads on every operation instead of caching.
	got, err := NewFile(path).Get("run-1")
	if err != nil {
		t.Fatalf("Get from a fresh instance: %v", err)
	}
	if got.SHA != "abc" {
		t.Errorf("SHA = %q", got.SHA)
	}
}

func TestFileCorruptLedgerIsAClearError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := NewFile(path).Latest()
	if err == nil {
		t.Fatal("want a parse error")
	}
	if got := err.Error(); !contains(got, "delete it to reset") {
		t.Errorf("error should tell the operator how to recover, got %q", got)
	}
}

func TestFileTrimsToCap(t *testing.T) {
	s := NewFile(filepath.Join(t.TempDir(), "state.json"))
	s.max = 5

	base := time.Now().Add(-2 * time.Hour)
	for i := range s.max + 4 {
		mustSave(t, s, mkRun(
			"run-"+itoa(i), "sha", "r", run.PhaseSucceeded, base.Add(time.Duration(i)*time.Second)))
	}

	all, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 5 {
		t.Fatalf("ledger holds %d runs, want 5", len(all))
	}
	// Trimming must drop the oldest, never the newest.
	if all[0].ID != "run-8" {
		t.Errorf("newest run = %s, want run-8", all[0].ID)
	}
	if all[len(all)-1].ID != "run-4" {
		t.Errorf("oldest retained run = %s, want run-4", all[len(all)-1].ID)
	}
}

func TestNewFileUsesTheProductionCap(t *testing.T) {
	if got := NewFile("x").max; got != MaxRuns {
		t.Errorf("cap = %d, want %d", got, MaxRuns)
	}
}

func TestConcurrentSaves(t *testing.T) {
	each(t, func(t *testing.T, s Store) {
		const n = 24
		var wg sync.WaitGroup
		base := time.Now()
		for i := range n {
			wg.Go(func() {
				_ = s.Save(mkRun("run-"+itoa(i), "sha", "r", run.PhaseSucceeded,
					base.Add(time.Duration(i)*time.Millisecond)))
			})
		}
		wg.Wait()

		all, err := s.List()
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != n {
			t.Errorf("List has %d runs, want %d", len(all), n)
		}
	})
}

func mustSave(t *testing.T, s Store, r *run.Run) {
	t.Helper()
	if err := s.Save(r); err != nil {
		t.Fatalf("Save %s: %v", r.ID, err)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func itoa(i int) string { return strconv.Itoa(i) }
