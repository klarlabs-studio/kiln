package run

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNewStartsQueued(t *testing.T) {
	r := New("abc1234def", "refs/heads/main", "push", false, "owner/repo")

	if r.Phase != PhaseQueued {
		t.Errorf("phase = %s, want queued", r.Phase)
	}
	if r.ID == "" || r.StartedAt.IsZero() {
		t.Errorf("run not stamped: %+v", r)
	}
	if !r.FinishedAt.IsZero() {
		t.Error("a new run must not be finished")
	}
}

func TestIDsAreUniqueAndSortable(t *testing.T) {
	at := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)

	seen := make(map[string]bool, 1000)
	for range 1000 {
		id := NewID(at)
		if seen[id] {
			t.Fatalf("duplicate id %s within one second", id)
		}
		seen[id] = true
	}

	// Lexical order must track chronological order, so a sorted ledger reads
	// oldest-to-newest without parsing timestamps.
	earlier := NewID(at)
	later := NewID(at.Add(time.Second))
	if earlier >= later {
		t.Errorf("%q should sort before %q", earlier, later)
	}
}

func TestSucceedAndFailAreTerminal(t *testing.T) {
	ok := New("abc", "", "push", false, "")
	ok.Succeed()
	if !ok.Phase.Terminal() || ok.FinishedAt.IsZero() {
		t.Errorf("Succeed left the run open: %+v", ok)
	}

	bad := New("abc", "", "push", false, "")
	bad.Fail(errors.New("docker push refused"))
	if !bad.Phase.Terminal() || bad.Error != "docker push refused" {
		t.Errorf("Fail = %+v", bad)
	}
}

func TestFailWithNilStillFails(t *testing.T) {
	r := New("abc", "", "push", false, "")
	r.Fail(nil)

	if r.Phase != PhaseFailed {
		t.Errorf("phase = %s, want failed", r.Phase)
	}
	// A failed run with an empty message would read as a success in the ledger.
	if r.Error == "" {
		t.Error("a failed run must carry a reason")
	}
}

func TestSucceedClearsAStaleError(t *testing.T) {
	r := New("abc", "", "push", false, "")
	r.Error = "transient publish error, retried"
	r.Succeed()

	if r.Error != "" {
		t.Errorf("Error = %q, want cleared on success", r.Error)
	}
}

func TestShortSHA(t *testing.T) {
	if got := ShortSHA("abc1234def5678"); got != "abc1234" {
		t.Errorf("ShortSHA = %q, want abc1234", got)
	}
	// Short input is returned whole rather than panicking on the slice.
	if got := ShortSHA("abc"); got != "abc" {
		t.Errorf("ShortSHA = %q, want abc", got)
	}
}

func TestCloneIsDeep(t *testing.T) {
	r := New("abc", "", "push", false, "")
	r.Tags = []string{"ghcr.io/x/y:latest"}

	cp := r.Clone()
	cp.Tags[0] = "mutated"
	cp.SHA = "mutated"

	if r.Tags[0] == "mutated" || r.SHA == "mutated" {
		t.Error("Clone shares state with the original")
	}
	if Clone := (*Run)(nil).Clone(); Clone != nil {
		t.Error("nil.Clone() should be nil")
	}
}

func TestJSONOmitsUnsetFinish(t *testing.T) {
	data, err := json.Marshal(New("abc", "", "push", false, ""))
	if err != nil {
		t.Fatal(err)
	}

	// An open run must not serialise a zero timestamp; a reader would show it
	// as finished in the year 1.
	if strings.Contains(string(data), "finished_at") {
		t.Errorf("open run serialised finished_at: %s", data)
	}
}

func TestDurationOfOpenRun(t *testing.T) {
	r := New("abc", "", "push", false, "")
	r.StartedAt = time.Now().Add(-2 * time.Second)

	if r.Duration() < time.Second {
		t.Errorf("Duration = %v, want the elapsed time of an open run", r.Duration())
	}
}
