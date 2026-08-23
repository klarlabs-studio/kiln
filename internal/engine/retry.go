package engine

import (
	"fmt"
	"math"
	"time"

	"go.klarlabs.de/kiln/internal/domain/run"
	"go.klarlabs.de/kiln/internal/store"
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
)

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
func Decide(s store.Store, sha, ref string, now time.Time) (Verdict, time.Duration) {
	if s == nil {
		return Build, 0
	}
	if AlreadyBuilt(s, sha, ref) {
		return Built, 0
	}

	failures, last := failureHistory(s, sha, ref)
	if failures == 0 {
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
// the most recent one finished.
func failureHistory(s store.Store, sha, ref string) (int, time.Time) {
	runs, err := s.List()
	if err != nil {
		return 0, time.Time{}
	}

	var count int
	var latest time.Time
	for _, r := range runs {
		if r == nil || r.SHA != sha || r.Ref != ref || r.Phase != run.PhaseFailed {
			continue
		}
		count++
		when := r.FinishedAt
		if when.IsZero() {
			when = r.StartedAt
		}
		if when.After(latest) {
			latest = when
		}
	}
	return count, latest
}

// DescribeBackoff renders the wait for a log line, so an operator reading a
// quiet tick can tell "nothing to do" from "waiting before trying again".
func DescribeBackoff(d time.Duration) string {
	return fmt.Sprintf("retrying in %s", d.Round(time.Minute))
}
