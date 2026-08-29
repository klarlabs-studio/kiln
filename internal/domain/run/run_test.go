package run

import (
	"encoding/json"
	"errors"
	"fmt"
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

// exitErr mimics the shape infrastructure builds for a failed subprocess: a
// detailed rendering for a person reading a terminal, and a summary for
// anything that keeps the text.
type exitErr struct {
	cmd    string
	code   int
	stderr string
}

func (e *exitErr) Error() string   { return e.Summary() + ": " + e.stderr }
func (e *exitErr) Summary() string { return fmt.Sprintf("%s: exit %d", e.cmd, e.code) }

// The ledger is git-tracked and keeps one error string per failed run forever.
// Storing err.Error() put whatever the failing command printed into it — which
// is how cosign key material reached .kiln/state.json (#56).
func TestTheLedgerKeepsNoSubprocessOutput(t *testing.T) {
	// The stderr cosign produced in #56, verbatim, because a fixture that
	// softened it would not prove the thing under test: this exact shape is
	// what used to be written to a git-tracked file.
	// nox:ignore SEC-004 -- fixture: the leaked stderr this test proves is dropped
	leaked := "Error: reading key: open -----BEGIN ENCRYPTED SIGSTORE PRIVATE KEY-----\n" +
		"eyJrZGYiOnsibmFtZSI6InNjcnlwdCJ9fQ==: file name too long"
	err := fmt.Errorf("publish: cosign sign ghcr.io/acme/app: %w",
		&exitErr{cmd: "cosign sign ghcr.io/acme/app", code: 1, stderr: leaked})

	var r Run
	r.Fail(err)

	if strings.Contains(r.Error, "eyJrZGYi") || strings.Contains(r.Error, "BEGIN ENCRYPTED") {
		t.Errorf("the ledger recorded the command's output:\n%s", r.Error)
	}
	// What kiln concluded has to survive, or the record stops explaining the
	// failure it exists to record.
	for _, want := range []string{"publish", "cosign sign", "exit 1"} {
		if !strings.Contains(r.Error, want) {
			t.Errorf("ledger lost %q, leaving nothing to diagnose from:\n%s", want, r.Error)
		}
	}
}

// An error with no subprocess behind it is kiln's own words and is kept whole.
func TestAPlainErrorIsRecordedAsWritten(t *testing.T) {
	var r Run
	r.Fail(errors.New("pipeline: on.push names no artifact to publish"))

	if r.Error != "pipeline: on.push names no artifact to publish" {
		t.Errorf("error = %q, want it kept verbatim", r.Error)
	}
}

// When the wrapping does not embed the detailed form verbatim, the outer text
// cannot be assumed free of the output — so the safe half is kept rather than
// guessing which to trust.
func TestAnUnrecognisedWrappingFallsBackToTheSummary(t *testing.T) {
	inner := &exitErr{cmd: "docker push acme/app", code: 1, stderr: "denied: token abc123"}
	// %v, not %w-at-the-end: the detailed text is reformatted, so a substring
	// replacement cannot find it.
	err := fmt.Errorf("publish: %w (while pushing)", inner)

	var r Run
	r.Fail(err)

	if strings.Contains(r.Error, "abc123") {
		t.Errorf("the ledger recorded a token from the command's output:\n%s", r.Error)
	}
	if !strings.Contains(r.Error, "exit 1") {
		t.Errorf("ledger lost the exit status:\n%s", r.Error)
	}
}

// A nil error still fails the run: a failed run with no explanation would be
// indistinguishable from a successful one.
func TestANilErrorStillFailsTheRun(t *testing.T) {
	var r Run
	r.Fail(nil)

	if r.Phase != PhaseFailed || r.Error == "" {
		t.Errorf("phase=%s error=%q, want a failed run with a placeholder", r.Phase, r.Error)
	}
}
