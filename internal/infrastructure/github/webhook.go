package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.klarlabs.de/kiln/internal/domain/isolation"
)

// SignatureHeader is the header GitHub signs its deliveries with.
const SignatureHeader = "X-Hub-Signature-256"

// EventHeader names the delivery's event type.
const EventHeader = "X-GitHub-Event"

// ErrBadSignature reports a delivery whose signature does not verify, or a
// server with no secret configured.
//
// Both cases are the same 401 on purpose. A webhook endpoint with no secret is
// an endpoint anybody on the internet can use to make a machine build and sign
// whatever commit they name, so "unconfigured" must be as closed as "wrong".
var ErrBadSignature = errors.New("invalid webhook signature")

// VerifySignature checks a delivery against the shared secret.
//
// Only the sha256 scheme is accepted. GitHub still sends a legacy sha1 header
// for compatibility, and accepting it would let an attacker downgrade to a
// broken MAC by choosing which header to present.
func VerifySignature(secret string, body []byte, header string) error {
	if secret == "" {
		return fmt.Errorf("%w: no KILN_WEBHOOK_SECRET configured", ErrBadSignature)
	}
	scheme, provided, ok := strings.Cut(strings.TrimSpace(header), "=")
	if !ok || !strings.EqualFold(scheme, "sha256") {
		return fmt.Errorf("%w: expected a sha256= signature", ErrBadSignature)
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))

	// Constant time: a byte-by-byte comparison leaks how much of a guessed
	// signature was right, which is enough to forge one.
	if !hmac.Equal([]byte(strings.ToLower(provided)), []byte(expected)) {
		return fmt.Errorf("%w: signature mismatch", ErrBadSignature)
	}
	return nil
}

// Job is a unit of work derived from a delivery.
type Job struct {
	SHA   string
	Ref   string
	Event isolation.Event
	Fork  bool
	Repo  string
	// Reason explains an ignored delivery, for the log and the 202 body.
	Reason string
}

// ErrIgnored reports a delivery Kiln understands but has nothing to do about:
// a ping, a branch deletion, a pull request action that does not change the
// head. It is not a failure — the endpoint answers 202 and moves on.
var ErrIgnored = errors.New("delivery ignored")

// pushPayload is the subset of a push event Kiln reads.
type pushPayload struct {
	Ref     string `json:"ref"`
	After   string `json:"after"`
	Deleted bool   `json:"deleted"`
	Repo    struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// prPayload is the subset of a pull_request event Kiln reads.
type prPayload struct {
	Action      string      `json:"action"`
	Number      int         `json:"number"`
	PullRequest pullPayload `json:"pull_request"`
	Repo        struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// zeroSHA is what GitHub sends as `after` when a ref is deleted.
const zeroSHA = "0000000000000000000000000000000000000000"

// prActions are the pull-request actions that change what should be built. An
// action like `labeled` or `assigned` leaves the head untouched, and rebuilding
// on it would burn a build per label.
var prActions = map[string]bool{
	"opened":           true,
	"synchronize":      true,
	"reopened":         true,
	"ready_for_review": true,
}

// ParseDelivery turns a verified delivery into a job.
//
// Fork status comes from the payload's own head/base comparison rather than a
// follow-up API call: the payload is authenticated by the HMAC, so it is as
// trustworthy as anything the API would say, and it costs no round trip.
func ParseDelivery(eventType string, body []byte) (Job, error) {
	switch eventType {
	case "ping":
		return Job{}, fmt.Errorf("%w: ping", ErrIgnored)

	case "push":
		var p pushPayload
		if err := json.Unmarshal(body, &p); err != nil {
			return Job{}, fmt.Errorf("parse push: %w", err)
		}
		if p.Deleted || p.After == "" || p.After == zeroSHA {
			return Job{}, fmt.Errorf("%w: ref %s was deleted", ErrIgnored, p.Ref)
		}
		event := isolation.EventPush
		if strings.HasPrefix(p.Ref, "refs/tags/") {
			event = isolation.EventTag
		}
		if !strings.HasPrefix(p.Ref, "refs/heads/") && event != isolation.EventTag {
			// A push to refs/pull/* or a note ref is not something to build.
			return Job{}, fmt.Errorf("%w: ref %s is not a branch or tag", ErrIgnored, p.Ref)
		}
		return Job{SHA: p.After, Ref: p.Ref, Event: event, Repo: p.Repo.FullName}, nil

	case "pull_request":
		var p prPayload
		if err := json.Unmarshal(body, &p); err != nil {
			return Job{}, fmt.Errorf("parse pull_request: %w", err)
		}
		if !prActions[p.Action] {
			return Job{}, fmt.Errorf("%w: pull_request %s does not change the head", ErrIgnored, p.Action)
		}
		if p.PullRequest.Head.SHA == "" {
			return Job{}, fmt.Errorf("%w: pull_request %s carried no head sha", ErrIgnored, p.Action)
		}
		return Job{
			SHA:   p.PullRequest.Head.SHA,
			Ref:   fmt.Sprintf("refs/pull/%d/head", p.Number),
			Event: isolation.EventPullRequest,
			Fork:  isFork(p.PullRequest),
			Repo:  p.Repo.FullName,
		}, nil

	case "":
		return Job{}, errors.New("delivery has no " + EventHeader)

	default:
		return Job{}, fmt.Errorf("%w: %s events are not routed", ErrIgnored, eventType)
	}
}
