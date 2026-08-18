package schedule_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.klarlabs.de/kiln/internal/schedule"
)

var noon = time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

func TestATaskThatHasNeverRunIsDue(t *testing.T) {
	s := schedule.NewStore(t.TempDir())

	// The alternative — waiting a full interval before the first run — is a
	// nightly job that does nothing on the day you configure it, leaving you
	// unable to tell whether it works.
	due, err := s.DueAt("remediate", 24*time.Hour, noon)
	if err != nil {
		t.Fatal(err)
	}
	if !due {
		t.Error("a task that has never run was not due")
	}
}

func TestATaskIsNotDueUntilItsIntervalHasPassed(t *testing.T) {
	s := schedule.NewStore(t.TempDir())
	if err := s.Fired("remediate", noon); err != nil {
		t.Fatal(err)
	}

	for _, at := range []time.Time{
		noon.Add(time.Minute),
		noon.Add(23 * time.Hour),
		noon.Add(24*time.Hour - time.Second),
	} {
		due, err := s.DueAt("remediate", 24*time.Hour, at)
		if err != nil {
			t.Fatal(err)
		}
		if due {
			t.Errorf("due at %s, only %s after the last run", at, at.Sub(noon))
		}
	}

	due, err := s.DueAt("remediate", 24*time.Hour, noon.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !due {
		t.Error("not due a full interval later")
	}
}

func TestAWeekOffFiresOnceNotSevenTimes(t *testing.T) {
	s := schedule.NewStore(t.TempDir())
	if err := s.Fired("remediate", noon); err != nil {
		t.Fatal(err)
	}

	// The box comes back after a week. Cron catch-up is how a nightly
	// remediation job opens seven pull requests at breakfast.
	backOnline := noon.Add(7 * 24 * time.Hour)

	due, err := s.DueAt("remediate", 24*time.Hour, backOnline)
	if err != nil {
		t.Fatal(err)
	}
	if !due {
		t.Fatal("not due after a week")
	}

	// Firing once marks it done; the same tick's neighbours do not each get a
	// turn.
	if err := s.Fired("remediate", backOnline); err != nil {
		t.Fatal(err)
	}
	due, err = s.DueAt("remediate", 24*time.Hour, backOnline.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if due {
		t.Error("due again a minute after firing: a catch-up storm")
	}
}

func TestTasksAreTrackedSeparately(t *testing.T) {
	s := schedule.NewStore(t.TempDir())
	if err := s.Fired("remediate", noon); err != nil {
		t.Fatal(err)
	}

	due, err := s.DueAt("report", 24*time.Hour, noon)
	if err != nil {
		t.Fatal(err)
	}
	if !due {
		t.Error("one task's run marked another as done")
	}
}

func TestStateSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	if err := schedule.NewStore(dir).Fired("remediate", noon); err != nil {
		t.Fatal(err)
	}

	// A new process, the same directory. Without this the interval resets on
	// every restart and a 24h task on a box that redeploys daily runs daily
	// by accident rather than by design.
	due, err := schedule.NewStore(dir).DueAt("remediate", 24*time.Hour, noon.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if due {
		t.Error("the last run was forgotten across processes")
	}
}

func TestAnUnreadableStateFileFiresRatherThanGoesSilent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, schedule.FileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	// One duplicate run is a nuisance; silence that looks like everything
	// being fine is the failure nobody notices.
	due, err := schedule.NewStore(dir).DueAt("remediate", 24*time.Hour, noon)
	if err != nil {
		t.Fatal(err)
	}
	if !due {
		t.Error("a corrupt state file silently disabled the schedule")
	}
}

func TestAKilledWriteLeavesThePreviousState(t *testing.T) {
	dir := t.TempDir()
	s := schedule.NewStore(dir)
	if err := s.Fired("remediate", noon); err != nil {
		t.Fatal(err)
	}

	// Write-and-rename: a stray temp file from a killed process must not be
	// what the next read picks up.
	if err := os.WriteFile(s.Path()+".tmp", []byte("{trunca"), 0o600); err != nil {
		t.Fatal(err)
	}

	due, err := schedule.NewStore(dir).DueAt("remediate", 24*time.Hour, noon.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if due {
		t.Error("a leftover temp file was read as the state")
	}
}
