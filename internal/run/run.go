// Package run is the record of one Kiln invocation.
//
// Git is the desired state; this record is runtime only. Nothing here is
// authoritative about the repository — it exists so `kiln status` can answer
// "what happened last time" and so `kiln watch` can tell an already-built
// SHA from a new one. Losing the ledger costs a duplicate build, never
// correctness.
package run

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// Phase is where a run got to. The sequence is queued → isolating → proving →
// publishing → succeeded|failed; a run that skips publishing goes straight
// from proving to succeeded.
type Phase string

const (
	PhaseQueued     Phase = "queued"
	PhaseIsolating  Phase = "isolating"
	PhaseProving    Phase = "proving"
	PhasePublishing Phase = "publishing"
	PhaseSucceeded  Phase = "succeeded"
	PhaseFailed     Phase = "failed"
)

// Terminal reports whether the phase is an end state.
func (p Phase) Terminal() bool { return p == PhaseSucceeded || p == PhaseFailed }

// Run is one execution of the engine.
//
// The JSON tags are a stored format: `.kiln/state.json` written by one version
// is read by the next, so fields are added, never renamed.
type Run struct {
	ID    string `json:"id"`
	SHA   string `json:"sha"`
	Ref   string `json:"ref,omitempty"`
	Event string `json:"event"`
	Fork  bool   `json:"fork"`
	Repo  string `json:"repo,omitempty"`
	Phase Phase  `json:"phase"`

	// Skipped records that a trusted warden note stood in for the re-prove.
	// It is part of the provenance story, not a performance note: an auditor
	// reading the ledger needs to know which runs executed the checks and
	// which inherited them.
	Skipped bool `json:"skipped"`

	// Digest and Tags are the publish hand-off to RollOps. Digest is the
	// immutable `sha256:…` the registry assigned; Tags are the fully qualified
	// references that now point at it.
	Digest string   `json:"digest,omitempty"`
	Tags   []string `json:"tags,omitempty"`

	// Error is the failure message, empty on success. It is a string rather
	// than an error so the record round-trips through JSON unchanged.
	Error string `json:"error,omitempty"`

	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
}

// New starts a run in the queued phase with a fresh id.
func New(sha, ref, event string, fork bool, repo string) *Run {
	return &Run{
		ID:        NewID(time.Now().UTC()),
		SHA:       sha,
		Ref:       ref,
		Event:     event,
		Fork:      fork,
		Repo:      repo,
		Phase:     PhaseQueued,
		StartedAt: time.Now().UTC(),
	}
}

// NewID builds a sortable, collision-resistant run id: a UTC timestamp so ids
// order lexically the way runs order in time, plus random bytes so two runs
// started in the same second on the same box do not collide.
func NewID(at time.Time) string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice, and a run id is not a secret;
		// a degraded but unique-enough id beats aborting a build.
		return fmt.Sprintf("run-%s-%09d", at.Format("20060102T150405Z"), at.Nanosecond())
	}
	return fmt.Sprintf("run-%s-%s", at.Format("20060102T150405Z"), hex.EncodeToString(b[:]))
}

// Succeed closes the run as successful.
func (r *Run) Succeed() {
	r.Phase = PhaseSucceeded
	r.Error = ""
	r.FinishedAt = time.Now().UTC()
}

// Fail closes the run with a reason. A nil error still fails the run — with a
// placeholder message — because a failed run with no explanation would be
// indistinguishable from a successful one in the ledger.
func (r *Run) Fail(err error) {
	r.Phase = PhaseFailed
	if err != nil {
		r.Error = err.Error()
	} else if r.Error == "" {
		r.Error = "failed"
	}
	r.FinishedAt = time.Now().UTC()
}

// Duration is how long the run took; for an open run, how long it has been
// running.
func (r *Run) Duration() time.Duration {
	if r.FinishedAt.IsZero() {
		return time.Since(r.StartedAt)
	}
	return r.FinishedAt.Sub(r.StartedAt)
}

// ShortSHA is the seven-character form used in tags and log lines.
func ShortSHA(sha string) string {
	if len(sha) <= 7 {
		return sha
	}
	return sha[:7]
}

// Clone returns a deep copy. Stores hand out clones so a caller mutating a
// returned run cannot corrupt the ledger it came from.
func (r *Run) Clone() *Run {
	if r == nil {
		return nil
	}
	cp := *r
	if r.Tags != nil {
		cp.Tags = append([]string(nil), r.Tags...)
	}
	return &cp
}
