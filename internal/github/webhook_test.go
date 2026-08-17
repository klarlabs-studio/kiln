package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"go.klarlabs.de/kiln/internal/isolation"
)

const secret = "s3cr3t"

func sign(body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifySignatureAcceptsAGenuineDelivery(t *testing.T) {
	body := []byte(`{"zen":"Keep it logically awesome."}`)

	if err := VerifySignature(secret, body, sign(body)); err != nil {
		t.Errorf("VerifySignature: %v", err)
	}
}

func TestVerifySignatureRejectsATamperedBody(t *testing.T) {
	body := []byte(`{"after":"abc"}`)
	header := sign(body)

	err := VerifySignature(secret, []byte(`{"after":"attacker-chosen"}`), header)
	if !errors.Is(err, ErrBadSignature) {
		t.Errorf("err = %v, want ErrBadSignature", err)
	}
}

func TestVerifySignatureRejectsTheWrongSecret(t *testing.T) {
	body := []byte(`{}`)

	if err := VerifySignature("other", body, sign(body)); !errors.Is(err, ErrBadSignature) {
		t.Errorf("err = %v, want ErrBadSignature", err)
	}
}

func TestMissingSecretIsAsClosedAsAWrongOne(t *testing.T) {
	body := []byte(`{}`)

	// An unsecured endpoint lets anybody on the internet make this machine
	// build and sign a commit of their choosing.
	if err := VerifySignature("", body, sign(body)); !errors.Is(err, ErrBadSignature) {
		t.Errorf("err = %v, want ErrBadSignature for an unconfigured secret", err)
	}
}

func TestOnlySHA256IsAccepted(t *testing.T) {
	body := []byte(`{}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	digest := hex.EncodeToString(mac.Sum(nil))

	for _, header := range []string{
		"sha1=" + digest, // the legacy header GitHub still sends
		digest,           // no scheme at all
		"",
		"sha256=",
	} {
		if err := VerifySignature(secret, body, header); !errors.Is(err, ErrBadSignature) {
			t.Errorf("VerifySignature(%q) = %v, want rejection", header, err)
		}
	}
}

func TestSignatureIsCaseInsensitiveInHex(t *testing.T) {
	body := []byte(`{}`)
	header := strings.ToUpper(strings.TrimPrefix(sign(body), "sha256="))

	if err := VerifySignature(secret, body, "sha256="+header); err != nil {
		t.Errorf("uppercase hex should verify: %v", err)
	}
}

func TestParsePushToABranch(t *testing.T) {
	body := []byte(`{
		"ref": "refs/heads/main", "after": "abc123",
		"repository": {"full_name": "klarlabs-studio/kiln"}
	}`)

	job, err := ParseDelivery("push", body)
	if err != nil {
		t.Fatal(err)
	}
	if job.Event != isolation.EventPush || job.SHA != "abc123" || job.Ref != "refs/heads/main" {
		t.Errorf("Job = %+v", job)
	}
	if job.Fork {
		t.Error("a push to the repository itself is never a fork")
	}
}

func TestParsePushToATagIsATagEvent(t *testing.T) {
	body := []byte(`{"ref": "refs/tags/v1.2.0", "after": "abc123", "repository": {"full_name": "o/r"}}`)

	job, err := ParseDelivery("push", body)
	if err != nil {
		t.Fatal(err)
	}
	if job.Event != isolation.EventTag {
		t.Errorf("Event = %s, want tag", job.Event)
	}
}

func TestDeletedRefIsIgnored(t *testing.T) {
	for _, body := range []string{
		`{"ref":"refs/heads/gone","after":"0000000000000000000000000000000000000000","deleted":true}`,
		`{"ref":"refs/heads/gone","after":"","deleted":false}`,
	} {
		_, err := ParseDelivery("push", []byte(body))
		if !errors.Is(err, ErrIgnored) {
			t.Errorf("deleting a branch should be ignored, got %v", err)
		}
	}
}

func TestPushToANonBuildableRefIsIgnored(t *testing.T) {
	body := []byte(`{"ref":"refs/notes/warden","after":"abc123"}`)

	if _, err := ParseDelivery("push", body); !errors.Is(err, ErrIgnored) {
		t.Errorf("err = %v, want ErrIgnored for a note ref", err)
	}
}

func TestParsePullRequestFromAFork(t *testing.T) {
	body := []byte(`{
		"action": "opened", "number": 7,
		"pull_request": {
			"number": 7,
			"head": {"sha": "deadbeef", "ref": "feature", "repo": {"full_name": "stranger/kiln"}},
			"base": {"repo": {"full_name": "klarlabs-studio/kiln"}}
		},
		"repository": {"full_name": "klarlabs-studio/kiln"}
	}`)

	job, err := ParseDelivery("pull_request", body)
	if err != nil {
		t.Fatal(err)
	}
	if job.Event != isolation.EventPullRequest || !job.Fork {
		t.Errorf("Job = %+v, want a fork pull request", job)
	}
	if job.Ref != "refs/pull/7/head" {
		t.Errorf("Ref = %q", job.Ref)
	}
}

func TestPullRequestActionsThatChangeTheHead(t *testing.T) {
	build := []string{"opened", "synchronize", "reopened", "ready_for_review"}
	ignore := []string{"labeled", "assigned", "closed", "edited", "review_requested"}

	payload := func(action string) []byte {
		return []byte(`{
			"action": "` + action + `", "number": 1,
			"pull_request": {"number": 1,
				"head": {"sha": "abc", "ref": "f", "repo": {"full_name": "o/r"}},
				"base": {"repo": {"full_name": "o/r"}}}
		}`)
	}

	for _, action := range build {
		if _, err := ParseDelivery("pull_request", payload(action)); err != nil {
			t.Errorf("%s should produce a job, got %v", action, err)
		}
	}
	for _, action := range ignore {
		// Rebuilding on `labeled` would burn a build per label.
		if _, err := ParseDelivery("pull_request", payload(action)); !errors.Is(err, ErrIgnored) {
			t.Errorf("%s should be ignored, got %v", action, err)
		}
	}
}

func TestPingIsIgnored(t *testing.T) {
	if _, err := ParseDelivery("ping", []byte(`{"zen":"x"}`)); !errors.Is(err, ErrIgnored) {
		t.Errorf("err = %v, want ErrIgnored", err)
	}
}

func TestUnroutedEventIsIgnored(t *testing.T) {
	if _, err := ParseDelivery("issues", []byte(`{}`)); !errors.Is(err, ErrIgnored) {
		t.Errorf("err = %v, want ErrIgnored", err)
	}
}

func TestMissingEventHeaderIsAnError(t *testing.T) {
	if _, err := ParseDelivery("", []byte(`{}`)); err == nil {
		t.Error("a delivery with no event type must be rejected")
	}
}

func TestMalformedPayloadIsAnError(t *testing.T) {
	if _, err := ParseDelivery("push", []byte(`{not json`)); err == nil {
		t.Error("malformed JSON must be rejected")
	}
	if _, err := ParseDelivery("pull_request", []byte(`{not json`)); err == nil {
		t.Error("malformed JSON must be rejected")
	}
}
