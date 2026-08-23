package obs

import (
	"bytes"
	"encoding/json"
	"testing"
)

func decode(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &got); err != nil {
		t.Fatalf("log line is not JSON: %v (%q)", err, buf.String())
	}
	return got
}

func TestFieldsAreRendered(t *testing.T) {
	var buf bytes.Buffer
	NewTo(&buf, "debug").Info("built", "sha", "abc123", "attempt", 2, "dry", true)

	got := decode(t, &buf)
	if got["sha"] != "abc123" {
		t.Errorf("sha = %v, want abc123", got["sha"])
	}
	if got["attempt"] != float64(2) {
		t.Errorf("attempt = %v, want 2", got["attempt"])
	}
	if got["dry"] != true {
		t.Errorf("dry = %v, want true", got["dry"])
	}
}

func TestWithBindsFieldsAndCallOverrides(t *testing.T) {
	var buf bytes.Buffer
	log := NewTo(&buf, "debug").With("run", "r-1", "phase", "queued")
	log.Info("advanced", "phase", "proving")

	got := decode(t, &buf)
	if got["run"] != "r-1" {
		t.Errorf("run = %v, want r-1", got["run"])
	}
	// The per-call value must win over the inherited one; a stale phase in the
	// log is worse than no phase at all.
	if got["phase"] != "proving" {
		t.Errorf("phase = %v, want proving", got["phase"])
	}
}

func TestOddKeyIsNotDropped(t *testing.T) {
	var buf bytes.Buffer
	NewTo(&buf, "debug").Info("odd", "dangling")

	if got := decode(t, &buf); got["dangling"] != "" {
		t.Errorf("dangling = %v, want empty string", got["dangling"])
	}
}

func TestDiscardWritesNothing(t *testing.T) {
	// Discard must be safe to chain; a nil-return With would panic on use.
	Discard().With("k", "v").Error("boom", "err", "nope")
}
