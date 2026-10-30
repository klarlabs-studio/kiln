package engine

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"go.klarlabs.de/kiln/internal/application/ports"
	"go.klarlabs.de/kiln/internal/domain/isolation"
	"go.klarlabs.de/kiln/internal/domain/run"
	"go.klarlabs.de/kiln/internal/infrastructure/store"
)

func TestAFailureIsAttributedToItsCause(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want run.Cause
	}{
		{"the gate rejected the commit", ports.ErrGateFailed, run.CauseCommit},
		{"a task exited non-zero", fmt.Errorf("%w: build", ports.ErrTaskFailed), run.CauseCommit},
		{"the toolchain is missing", ports.ErrToolMissing, run.CauseEnvironment},
		{"another process held the lock", ports.ErrGateUnavailable, run.CauseEnvironment},
		// A timeout wraps whatever the phase was doing when it ran out. A
		// hanging gate is about the machine, not the change.
		{"a phase timed out mid-gate", fmt.Errorf("%w: prove: %w", ErrPhaseTimeout, ports.ErrGateFailed), run.CauseEnvironment},
		// Anything kiln cannot place is assumed to be the box's problem, so it
		// keeps being retried rather than parked on a wrong verdict.
		{"an unrecognised failure", errors.New("registry: connection reset"), run.CauseEnvironment},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := causeOf(c.err); got != c.want {
				t.Errorf("causeOf(%v) = %q, want %q", c.err, got, c.want)
			}
		})
	}
}

func TestAFailedRunRecordsItsCauseAndInputs(t *testing.T) {
	h := newHarness(t)
	h.proveErr = ports.ErrGateFailed
	request := req(t, isolation.EventPush, false, "refs/heads/main")

	r, err := h.engine.Execute(t.Context(), request)
	if err == nil {
		t.Fatal("a rejected gate must fail the run")
	}

	if r.Cause != run.CauseCommit {
		t.Errorf("Cause = %q, want %q", r.Cause, run.CauseCommit)
	}
	if r.Inputs == "" || r.Inputs != h.engine.Inputs(request.Pipeline) {
		t.Errorf("Inputs = %q, want the engine's fingerprint %q", r.Inputs, h.engine.Inputs(request.Pipeline))
	}
}

func TestInputsChangeWithWhatCouldFixACommit(t *testing.T) {
	h := newHarness(t)
	p := pipeline(t)
	h.engine.KilnVersion = "v0.8.0"
	h.engine.ToolVersions = map[string]string{"warden": "1.0.0", "nox": "2.0.0"}
	base := h.engine.Inputs(p)

	if again := h.engine.Inputs(p); again != base {
		t.Fatalf("fingerprint is not stable: %q then %q", base, again)
	}

	upgradedKiln := *h.engine
	upgradedKiln.KilnVersion = "v0.9.0"
	upgradedTool := *h.engine
	upgradedTool.ToolVersions = map[string]string{"warden": "1.1.0", "nox": "2.0.0"}
	changedPipeline := pipeline(t)
	changedPipeline.Publish = nil

	for name, got := range map[string]string{
		"kiln upgraded":     upgradedKiln.Inputs(p),
		"warden upgraded":   upgradedTool.Inputs(p),
		"pipeline modified": h.engine.Inputs(changedPipeline),
	} {
		if got == base {
			t.Errorf("%s: fingerprint unchanged", name)
		}
	}
}

func TestACommitFailureWaitsForAChange(t *testing.T) {
	const ref = "refs/heads/main"
	s := store.NewMemory()
	failedAt := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	saveFailure(t, s, ref, run.CauseCommit, "inputs-a", failedAt)

	// Well past the hourly environment cap, and still not retried: nothing
	// that could change the outcome has changed.
	verdict, wait := Decide(s, sha, ref, failedAt.Add(3*time.Hour), "inputs-a")
	if verdict != Settled {
		t.Fatalf("verdict = %v, want Settled", verdict)
	}
	if wait <= 0 {
		t.Errorf("wait = %s, want time until the daily recheck", wait)
	}

	// A changed toolchain, kiln or pipeline is exactly what can turn a red
	// commit green, so it is tried at once.
	if verdict, _ := Decide(s, sha, ref, failedAt.Add(time.Minute), "inputs-b"); verdict != Build {
		t.Errorf("changed inputs: verdict = %v, want Build", verdict)
	}

	// The verdict "this is the commit's fault" can be wrong — a full disk
	// surfaces as a gate failure — so it is rechecked daily, never abandoned.
	if verdict, _ := Decide(s, sha, ref, failedAt.Add(SettledRetry+time.Minute), "inputs-a"); verdict != Build {
		t.Errorf("after a day: verdict = %v, want Build", verdict)
	}
}

func TestAnEnvironmentFailureKeepsTheBackoff(t *testing.T) {
	const ref = "refs/heads/main"
	for name, cause := range map[string]run.Cause{
		"environment":            run.CauseEnvironment,
		"ledger from older kiln": "",
	} {
		t.Run(name, func(t *testing.T) {
			s := store.NewMemory()
			failedAt := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
			saveFailure(t, s, ref, cause, "inputs-a", failedAt)

			if verdict, _ := Decide(s, sha, ref, failedAt.Add(5*time.Minute), "inputs-a"); verdict != Backoff {
				t.Errorf("verdict = %v, want Backoff", verdict)
			}
			if verdict, _ := Decide(s, sha, ref, failedAt.Add(RetryBase+time.Minute), "inputs-a"); verdict != Build {
				t.Errorf("verdict = %v, want Build after the first delay", verdict)
			}
		})
	}
}

func saveFailure(t *testing.T, s ports.Ledger, ref string, cause run.Cause, inputs string, at time.Time) {
	t.Helper()
	r := run.New(sha, ref, "push", false, "o/r")
	r.Fail(errors.New("failed"))
	r.Cause = cause
	r.Inputs = inputs
	r.StartedAt = at.Add(-time.Minute)
	r.FinishedAt = at
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
}
