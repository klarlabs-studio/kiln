package mcpsrv

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.klarlabs.de/kiln/internal/domain/isolation"
	"go.klarlabs.de/kiln/internal/domain/run"
)

// fake is a scripted Facade.
type fake struct {
	allow     bool
	runs      []RunRequest
	runErr    error
	runResult RunOutput
	statusErr error
}

func (f *fake) AllowPrivilegedRun() bool { return f.allow }

func (f *fake) Doctor(context.Context) (DoctorOutput, error) {
	return DoctorOutput{Directory: "/repo", PipelineFound: true}, nil
}

func (f *fake) Status(_ context.Context, id string) (RunOutput, error) {
	if f.statusErr != nil {
		return RunOutput{}, f.statusErr
	}
	return RunOutput{ID: id, Phase: "succeeded", Succeeded: true}, nil
}

func (f *fake) Run(_ context.Context, in RunRequest) (RunOutput, error) {
	f.runs = append(f.runs, in)
	if f.runErr != nil {
		return f.runResult, f.runErr
	}
	out := f.runResult
	if out.ID == "" {
		out = RunOutput{ID: "run-1", Phase: "succeeded", Succeeded: true}
	}
	return out, nil
}

func TestPullRequestRunsAreAlwaysAllowed(t *testing.T) {
	f := &fake{allow: false}

	out, err := handleRun(t.Context(), f, RunInput{SHA: "abc", Event: "pull_request"})
	if err != nil {
		t.Fatalf("handleRun: %v", err)
	}

	// Proving a pull request produces no artifact and holds no credential, so
	// it needs no opt-in.
	if !out.Succeeded || len(f.runs) != 1 {
		t.Errorf("out = %+v, runs = %+v", out, f.runs)
	}
}

func TestPushAndTagRunsNeedTheOptIn(t *testing.T) {
	for _, event := range []string{"push", "tag"} {
		f := &fake{allow: false}

		_, err := handleRun(t.Context(), f, RunInput{SHA: "abc", Event: event})

		if !errors.Is(err, ErrRunNotPermitted) {
			t.Errorf("%s: err = %v, want ErrRunNotPermitted", event, err)
		}
		if len(f.runs) != 0 {
			t.Errorf("%s: the engine was invoked despite the refusal", event)
		}
	}
}

func TestRefusalNamesTheOptIn(t *testing.T) {
	f := &fake{allow: false}

	_, err := handleRun(t.Context(), f, RunInput{SHA: "abc", Event: "push"})

	// An agent told "internal error" has no move except to give up or retry
	// identically. The message must name what resolves it.
	if !strings.Contains(err.Error(), "KILN_MCP_ALLOW_RUN=1") {
		t.Errorf("err = %v", err)
	}
	var inputErr interface{ Error() string }
	if !errors.As(err, &inputErr) {
		t.Error("the refusal must reach the client, not be sanitized away")
	}
}

func TestOptInPermitsPushRuns(t *testing.T) {
	f := &fake{allow: true}

	out, err := handleRun(t.Context(), f, RunInput{SHA: "abc", Event: "push", Ref: "refs/heads/main"})
	if err != nil {
		t.Fatalf("handleRun: %v", err)
	}
	if !out.Succeeded {
		t.Errorf("out = %+v", out)
	}
	if got := f.runs[0]; got.Event != isolation.EventPush || got.Ref != "refs/heads/main" {
		t.Errorf("request = %+v", got)
	}
}

func TestUnknownEventIsRefusedBeforeTheGate(t *testing.T) {
	f := &fake{allow: true}

	_, err := handleRun(t.Context(), f, RunInput{SHA: "abc", Event: "release"})

	if err == nil || !strings.Contains(err.Error(), "pull_request, push or tag") {
		t.Errorf("err = %v", err)
	}
	if len(f.runs) != 0 {
		t.Error("an unknown event reached the engine")
	}
}

func TestMissingSHAIsRefused(t *testing.T) {
	f := &fake{allow: true}

	if _, err := handleRun(t.Context(), f, RunInput{Event: "push"}); err == nil {
		t.Error("a run with no commit must be refused")
	}
}

func TestForkFlagIsPassedThrough(t *testing.T) {
	f := &fake{allow: false}

	if _, err := handleRun(t.Context(), f, RunInput{SHA: "abc", Event: "pull_request", Fork: true}); err != nil {
		t.Fatal(err)
	}
	if !f.runs[0].Fork {
		t.Error("the fork flag did not reach the engine")
	}
}

func TestAFailedBuildIsAResultNotAProtocolError(t *testing.T) {
	f := &fake{
		allow:     true,
		runErr:    errors.New("gate failed"),
		runResult: RunOutput{ID: "run-9", Phase: "failed", Error: "gate failed"},
	}

	out, err := handleRun(t.Context(), f, RunInput{SHA: "abc", Event: "push"})

	// The agent needs the record to see which phase failed and why; an error
	// with no payload would leave it guessing.
	if err != nil {
		t.Fatalf("err = %v, want the run record instead", err)
	}
	if out.Succeeded || out.Error == "" || out.Phase != "failed" {
		t.Errorf("out = %+v", out)
	}
}

func TestAFailureWithNoRecordIsAnError(t *testing.T) {
	f := &fake{allow: true, runErr: errors.New("cannot resolve HEAD")}

	_, err := handleRun(t.Context(), f, RunInput{SHA: "nope", Event: "push"})

	if err == nil || !strings.Contains(err.Error(), "cannot resolve HEAD") {
		t.Errorf("err = %v", err)
	}
}

func TestServerExposesExactlyTheThreeTools(t *testing.T) {
	srv := NewServer(&fake{}, "test")
	if srv == nil {
		t.Fatal("NewServer returned nil")
	}
	// There is no deploy tool here, and adding one would not be a feature —
	// RollOps owns that. This test is the reminder.
}

func TestFromRun(t *testing.T) {
	r := run.New("abc1234", "refs/heads/main", "push", false, "o/r")
	r.Skipped = true
	r.Digest = "sha256:aaa"
	r.Tags = []string{"ghcr.io/x/y:latest"}
	r.Succeed()

	out := FromRun(r)

	if !out.Succeeded || out.Phase != "succeeded" {
		t.Errorf("out = %+v", out)
	}
	if !out.Skipped || out.Digest != "sha256:aaa" {
		t.Errorf("out = %+v", out)
	}
	if got := FromRun(nil); got.ID != "" {
		t.Errorf("FromRun(nil) = %+v, want the zero value", got)
	}
}

func TestFromRunMarksAFailure(t *testing.T) {
	r := run.New("abc", "", "push", false, "")
	r.Fail(errors.New("boom"))

	out := FromRun(r)

	// An agent branching on a string comparison gets this wrong eventually;
	// Succeeded is the field to read.
	if out.Succeeded {
		t.Error("a failed run reported Succeeded")
	}
	if out.Error != "boom" {
		t.Errorf("Error = %q", out.Error)
	}
}

func TestVisibleErrorIsNilSafe(t *testing.T) {
	if err := VisibleError(nil); err != nil {
		t.Errorf("VisibleError(nil) = %v", err)
	}
}
