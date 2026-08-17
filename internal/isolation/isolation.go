// Package isolation decides what a run is allowed to touch.
//
// This is the smallest package in Kiln and the one that matters most. Every
// other component asks it the same three questions — may this run see secrets,
// may it publish, may it skip the re-prove — and the answers depend on exactly
// two inputs: the event, and whether the head came from a fork.
//
// The rules are a pure function with no I/O, so they can be exhaustively
// tested, and the engine consults them *after* the caller has stated its
// intent, so a caller asking to publish on a fork pull request is overruled
// rather than obeyed.
package isolation

// Event is the trigger shape a run was created from.
type Event string

const (
	// EventPullRequest is a proposed change. Untrusted by default.
	EventPullRequest Event = "pull_request"
	// EventPush is a commit landing on a branch of the repository itself.
	EventPush Event = "push"
	// EventTag is a tag ref. Trusted like a push; it is how releases happen.
	EventTag Event = "tag"
)

// ParseEvent validates an event name from a CLI flag, HTTP body or MCP call.
// An unrecognised value is refused rather than coerced: guessing here would
// mean guessing at a trust boundary.
func ParseEvent(s string) (Event, bool) {
	switch Event(s) {
	case EventPullRequest, EventPush, EventTag:
		return Event(s), true
	default:
		return "", false
	}
}

// Valid reports whether e is one of the three known events.
func (e Event) Valid() bool {
	_, ok := ParseEvent(string(e))
	return ok
}

// String renders the event for logs and stored runs.
func (e Event) String() string { return string(e) }

// Policy is the decision. All three fields default to the safe answer, so a
// zero Policy — the value a caller gets if it forgets to call For — denies
// everything rather than permitting it.
type Policy struct {
	// Secrets permits the run to see registry, signing and forge-write
	// credentials.
	Secrets bool
	// Publish permits the build/push/sign phase to run at all.
	Publish bool
	// Skip permits a trusted warden note to stand in for a re-prove.
	Skip bool
}

// For resolves the policy for an event and fork status.
//
//	event         fork  secrets  publish  skip
//	pull_request  yes   no       no       no
//	pull_request  no    no       no       yes
//	push / tag    —     yes      yes      yes
//
// Two rows deserve their reasoning written down.
//
// A same-repo pull request may skip but still may not publish or see secrets.
// Skipping is safe because the note being verified was signed by a key the
// operator pinned in their own environment — the PR head cannot forge it.
// Publishing is not safe, and more importantly is not *useful*: a pull request
// is a proposal, and RollOps deploys from branches and tags, so an image built
// from an unmerged head is an artifact nobody should ever be able to ship.
//
// A fork pull request gets nothing. Its head contains attacker-authored code
// that Kiln is about to execute (that is what proving *is*), so the run must
// not hold a credential worth stealing, and its warden note — authored on the
// same untrusted head — is not evidence of anything.
func For(event Event, fork bool) Policy {
	switch event {
	case EventPush, EventTag:
		return Policy{Secrets: true, Publish: true, Skip: true}
	case EventPullRequest:
		if fork {
			return Policy{}
		}
		return Policy{Skip: true}
	default:
		// An unknown event is treated as maximally untrusted. Reaching here
		// means validation was bypassed upstream; denying is the only answer
		// that cannot be exploited.
		return Policy{}
	}
}
