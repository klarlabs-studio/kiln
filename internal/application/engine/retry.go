package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"go.klarlabs.de/kiln/internal/application/ports"

	"go.klarlabs.de/kiln/internal/domain/config"
	"go.klarlabs.de/kiln/internal/domain/run"
)

// Verdict is what a watch tick should do with a discovered job.
type Verdict int

const (
	// Build means run it.
	Build Verdict = iota
	// Built means a successful run already covers this commit.
	Built
	// Backoff means it failed recently and is not due for another attempt.
	Backoff
	// Settled means the commit itself failed and nothing that could change
	// the outcome has changed since.
	Settled
)

// SettledRetry is how long a commit that failed on its own merits waits when
// its inputs stay the same. Not forever: the verdict "this is the commit's
// fault" is only as good as the error it was read from, and a full disk can
// surface as an ordinary gate failure.
const SettledRetry = 24 * time.Hour

// RetryBase is the delay before a failed commit is tried again, doubling with
// each further failure.
const RetryBase = 15 * time.Minute

// RetryCap bounds the doubling. A commit that has failed all day is retried
// hourly rather than never: the cause is often outside the commit — a registry
// down, a dependency yanked, a toolchain the box was missing — and those get
// fixed without anybody pushing anything.
const RetryCap = time.Hour

// Decide says whether a tick should build a commit.
//
// This exists because of what a box does that CI does not: CI runs once per
// push, and a watch loop runs every few minutes forever. Without a backoff, a
// pull request whose gate fails is re-gated every tick for the rest of its
// life. Observed on the first real box: 222 runs, 205 of them failures,
// re-running `go test -race` across thirteen open pull requests every five
// minutes on somebody's laptop.
//
// Not "never retry", because a failure is not always about the commit. The
// delay doubles from fifteen minutes to an hour and stays there, so a genuine
// breakage settles into a heartbeat instead of a spin, and a transient one
// recovers on its own.
//
// That heartbeat is still a full re-run every hour for a commit whose own
// tests fail, which on a box with a few stale pull requests is most of its
// work. So a failure the commit caused waits for its inputs — the fingerprint
// from Engine.Inputs — to change, and rechecks daily in case it was misread.
func Decide(s ports.Ledger, sha, ref string, now time.Time, inputs string) (Verdict, time.Duration) {
	if s == nil {
		return Build, 0
	}
	if AlreadyBuilt(s, sha, ref) {
		return Built, 0
	}

	failures, last, latest := failureHistory(s, sha, ref)
	if failures == 0 {
		return Build, 0
	}

	if latest.Cause == run.CauseCommit && latest.Inputs != "" {
		if latest.Inputs != inputs {
			return Build, 0
		}
		if due := last.Add(SettledRetry); now.Before(due) {
			return Settled, due.Sub(now)
		}
		return Build, 0
	}

	delay := backoff(failures)
	if due := last.Add(delay); now.Before(due) {
		return Backoff, due.Sub(now)
	}
	return Build, 0
}

// backoff is 15m, 30m, 1h, 1h, …
func backoff(failures int) time.Duration {
	if failures < 1 {
		return 0
	}
	// 1<<62 nanoseconds is already centuries; the cap makes overflow
	// unreachable, and this keeps it unreachable if the cap ever moves.
	shift := min(failures-1, 20)
	delay := time.Duration(math.MaxInt64)
	if scaled := RetryBase << shift; scaled > 0 {
		delay = scaled
	}
	return min(delay, RetryCap)
}

// failureHistory counts consecutive failed runs for a commit and returns when
// the most recent one finished, and that run.
func failureHistory(s ports.Ledger, sha, ref string) (int, time.Time, *run.Run) {
	runs, err := s.List()
	if err != nil {
		return 0, time.Time{}, nil
	}

	var count int
	var last time.Time
	var latest *run.Run
	for _, r := range runs {
		if r == nil || r.SHA != sha || r.Ref != ref || r.Phase != run.PhaseFailed {
			continue
		}
		count++
		when := r.FinishedAt
		if when.IsZero() {
			when = r.StartedAt
		}
		if when.After(last) {
			last, latest = when, r
		}
	}
	return count, last, latest
}

// causeOf attributes a failure. Only errors that positively say the change was
// judged and rejected count against the commit; anything else, including what
// kiln cannot place, is the box's problem and keeps the hourly retry.
func causeOf(err error) run.Cause {
	switch {
	case errors.Is(err, ErrPhaseTimeout),
		errors.Is(err, ports.ErrToolMissing),
		errors.Is(err, ports.ErrGateUnavailable):
		return run.CauseEnvironment
	case errors.Is(err, ports.ErrGateFailed),
		errors.Is(err, ports.ErrTaskFailed):
		return run.CauseCommit
	default:
		return run.CauseEnvironment
	}
}

// Inputs fingerprints what, besides the commit, decides a run's outcome: the
// kiln build, the tool versions and the pipeline. When it changes, a commit
// that failed on its own merits deserves another look.
func (e *Engine) Inputs(p config.Pipeline) string {
	// encoding/json sorts map keys, so the tool versions hash the same however
	// the map was built.
	b, err := json.Marshal(struct {
		Kiln     string
		Tools    map[string]string
		Pipeline config.Pipeline
	}{e.KilnVersion, e.ToolVersions, p})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// DescribeBackoff renders the wait for a log line, so an operator reading a
// quiet tick can tell "nothing to do" from "waiting before trying again".
func DescribeBackoff(d time.Duration) string {
	return fmt.Sprintf("retrying in %s", d.Round(time.Minute))
}
