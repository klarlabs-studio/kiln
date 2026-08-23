package checks

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"go.klarlabs.de/kiln/internal/domain/run"
	"go.klarlabs.de/kiln/internal/infrastructure/github"
	"go.klarlabs.de/kiln/internal/infrastructure/obs"
)

// The names are a contract with branch protection and with RollOps' PR
// writeback. This test exists so that renaming one is a deliberate act.
func TestCheckNamesAreStable(t *testing.T) {
	if NameProve != "Kiln / Prove" {
		t.Errorf("NameProve = %q — renaming this unblocks every protected branch waiting on it", NameProve)
	}
	if NamePublish != "Kiln / Publish" {
		t.Errorf("NamePublish = %q — renaming this needs a migration note", NamePublish)
	}
}

func testReporter(t *testing.T, h http.HandlerFunc) *GitHub {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	c := github.NewClient("tok", github.Repo{Owner: "o", Name: "r"}, obs.Discard())
	c.BaseURL = srv.URL
	c.Attempts = 1
	return NewGitHub(c, obs.Discard())
}

func TestStartThenCompleteUpdatesTheSameRun(t *testing.T) {
	var created, patched atomic.Int32
	var patchedID string
	r := testReporter(t, func(w http.ResponseWriter, req *http.Request) {
		switch req.Method {
		case http.MethodPost:
			created.Add(1)
			_ = json.NewEncoder(w).Encode(github.CheckRun{ID: 99})
		case http.MethodPatch:
			patched.Add(1)
			patchedID = req.URL.Path
			w.WriteHeader(http.StatusOK)
		}
	})

	if err := r.Start(t.Context(), NameProve, "abc"); err != nil {
		t.Fatal(err)
	}
	if err := r.Complete(t.Context(), NameProve, "abc", Success, "gate passed", "all good"); err != nil {
		t.Fatal(err)
	}

	if created.Load() != 1 || patched.Load() != 1 {
		t.Errorf("created=%d patched=%d, want one of each", created.Load(), patched.Load())
	}
	if !strings.HasSuffix(patchedID, "/99") {
		t.Errorf("patched %q, want the run opened by Start", patchedID)
	}
}

func TestCompleteWithoutStartStillReportsTheVerdict(t *testing.T) {
	var created, patched atomic.Int32
	r := testReporter(t, func(w http.ResponseWriter, req *http.Request) {
		switch req.Method {
		case http.MethodPost:
			created.Add(1)
			_ = json.NewEncoder(w).Encode(github.CheckRun{ID: 7})
		case http.MethodPatch:
			patched.Add(1)
			w.WriteHeader(http.StatusOK)
		}
	})

	// Start failed earlier (a transient API blip). Losing the verdict entirely
	// is worse than opening a run just to conclude it.
	if err := r.Complete(t.Context(), NamePublish, "abc", Failure, "publish failed", "boom"); err != nil {
		t.Fatal(err)
	}
	if created.Load() != 1 || patched.Load() != 1 {
		t.Errorf("created=%d patched=%d", created.Load(), patched.Load())
	}
}

func TestTwoPhasesTrackSeparateRuns(t *testing.T) {
	var next atomic.Int64
	patched := map[string]bool{}
	r := testReporter(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost {
			_ = json.NewEncoder(w).Encode(github.CheckRun{ID: next.Add(1)})
			return
		}
		patched[req.URL.Path] = true
		w.WriteHeader(http.StatusOK)
	})

	_ = r.Start(t.Context(), NameProve, "abc")
	_ = r.Start(t.Context(), NamePublish, "abc")
	_ = r.Complete(t.Context(), NameProve, "abc", Success, "", "")
	_ = r.Complete(t.Context(), NamePublish, "abc", Success, "", "")

	if len(patched) != 2 {
		t.Errorf("patched %v, want two distinct check runs", patched)
	}
}

func TestReporterWithoutATokenIsSilent(t *testing.T) {
	c := github.NewClient("", github.Repo{Owner: "o", Name: "r"}, obs.Discard())
	r := NewGitHub(c, obs.Discard())

	// `kiln run` on a laptop should gate a commit and print the result, not
	// fail because it could not tell GitHub about it.
	if err := r.Start(t.Context(), NameProve, "abc"); err != nil {
		t.Errorf("Start without a token: %v", err)
	}
	if err := r.Complete(t.Context(), NameProve, "abc", Success, "", ""); err != nil {
		t.Errorf("Complete without a token: %v", err)
	}
}

func TestAPIFailureIsReportedToTheCaller(t *testing.T) {
	r := testReporter(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})

	// The engine treats this as best-effort, but it must be able to see it.
	if err := r.Start(t.Context(), NameProve, "abc"); err == nil {
		t.Error("a 401 should surface to the caller")
	}
}

func TestNoopIsAlwaysHappy(t *testing.T) {
	var n Noop
	if err := n.Start(t.Context(), NameProve, "abc"); err != nil {
		t.Error(err)
	}
	if err := n.Complete(t.Context(), NameProve, "abc", Success, "", ""); err != nil {
		t.Error(err)
	}
}

func TestRecordingCapturesTheSequence(t *testing.T) {
	var rec Recording

	_ = rec.Start(t.Context(), NameProve, "abc")
	_ = rec.Complete(t.Context(), NameProve, "abc", Success, "gate passed", "details")

	if !rec.Started(NameProve) {
		t.Error("Started = false")
	}
	got, ok := rec.Conclusions(NameProve)
	if !ok || got != Success {
		t.Errorf("Conclusion = (%s, %v)", got, ok)
	}
	if rec.Summary(NameProve) != "details" {
		t.Errorf("Summary = %q", rec.Summary(NameProve))
	}
	if _, ok := rec.Conclusions(NamePublish); ok {
		t.Error("reported a conclusion for a check that never ran")
	}
}

func TestFailingReporter(t *testing.T) {
	want := errors.New("network down")
	f := Failing{Err: want}

	if err := f.Start(t.Context(), NameProve, "abc"); !errors.Is(err, want) {
		t.Errorf("err = %v", err)
	}
	if err := (Failing{}).Complete(t.Context(), NameProve, "abc", Success, "", ""); err == nil {
		t.Error("the zero Failing must still fail")
	}
}

func TestProveSummaryForAPass(t *testing.T) {
	got, title, _ := ProveSummary(false, "checks ran", nil)

	if got != Success || !strings.Contains(title, "passed") {
		t.Errorf("(%s, %q)", got, title)
	}
}

func TestSkippedProveStillConcludesSuccess(t *testing.T) {
	got, title, summary := ProveSummary(true, "warden note on abc1234 is signed by a trusted key", nil)

	// The commit IS gated — by the note. A branch protection rule waiting on
	// this check must be satisfied, or every provenance skip blocks a merge.
	if got != Success {
		t.Errorf("Conclusion = %s, want success", got)
	}
	if !strings.Contains(title, "provenance") {
		t.Errorf("title = %q, want it to say the gate was satisfied by provenance", title)
	}
	if !strings.Contains(summary, "trusted key") {
		t.Errorf("summary should justify the skip, got %q", summary)
	}
}

func TestProveSummaryForAFailure(t *testing.T) {
	got, title, summary := ProveSummary(false, "", errors.New("lint failed on main.go"))

	if got != Failure || !strings.Contains(title, "failed") {
		t.Errorf("(%s, %q)", got, title)
	}
	if !strings.Contains(summary, "lint failed on main.go") {
		t.Errorf("summary should quote the failure, got %q", summary)
	}
}

func TestPublishSummaryForASignedImage(t *testing.T) {
	artifacts := []run.Artifact{{
		Kind: "image", Reference: "ghcr.io/x/y@sha256:aaa", Digest: "sha256:aaa",
		Names: []string{"ghcr.io/x/y:sha-abc1234", "ghcr.io/x/y:latest"}, Signed: true,
	}}

	got, title, summary := PublishSummary(artifacts, nil)

	if got != Success || !strings.Contains(title, "image") {
		t.Errorf("(%s, %q)", got, title)
	}
	for _, want := range []string{"ghcr.io/x/y:latest", "ghcr.io/x/y@sha256:aaa", "RollOps"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary missing %q:\n%s", want, summary)
		}
	}
}

func TestPublishSummaryListsBothKinds(t *testing.T) {
	artifacts := []run.Artifact{
		{Kind: "image", Reference: "ghcr.io/x/y@sha256:aaa", Names: []string{"ghcr.io/x/y:latest"}, Signed: true},
		{Kind: "binaries", Reference: "v1.4.0", Digest: "sha256:bbb",
			Names: []string{"checksums.txt", "checksums.txt.sig", "x_1.4.0_linux_amd64.tar.gz"}, Signed: true},
	}

	got, title, summary := PublishSummary(artifacts, nil)

	if got != Success {
		t.Errorf("Conclusion = %s", got)
	}
	// One event produced both, so one check reports both. Splitting them would
	// make branch protection wait on a name that does not always exist.
	if !strings.Contains(title, "image") || !strings.Contains(title, "binaries") {
		t.Errorf("title = %q, want both kinds named", title)
	}
	for _, want := range []string{"v1.4.0", "checksums.txt.sig", "x_1.4.0_linux_amd64.tar.gz", "sha256:bbb"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary missing %q:\n%s", want, summary)
		}
	}
}

func TestDryRunPublishIsNeutralNotSuccess(t *testing.T) {
	artifacts := []run.Artifact{{
		Kind: "image", Reference: "ghcr.io/x/y@sha256:000",
		Names: []string{"ghcr.io/x/y:latest"}, Signed: false,
	}}

	got, title, summary := PublishSummary(artifacts, nil)

	// A rehearsal on a pull request page must not read as a real artifact.
	if got != Neutral {
		t.Errorf("Conclusion = %s, want neutral for a dry run", got)
	}
	if !strings.Contains(strings.ToLower(title+summary), "dry") {
		t.Errorf("a dry run must say so: %q / %q", title, summary)
	}
}

func TestOneUnsignedArtifactMakesTheWholeCheckNeutral(t *testing.T) {
	artifacts := []run.Artifact{
		{Kind: "image", Reference: "ghcr.io/x/y@sha256:aaa", Signed: true},
		{Kind: "binaries", Reference: "v1.4.0", Signed: false},
	}

	// "Mostly signed" is not a claim worth making on a check anyone reads as
	// a guarantee.
	if got, _, _ := PublishSummary(artifacts, nil); got != Neutral {
		t.Errorf("Conclusion = %s, want neutral when any artifact is unsigned", got)
	}
}

func TestPublishSummaryWithNothingRouted(t *testing.T) {
	got, title, _ := PublishSummary(nil, nil)

	if got != Neutral || !strings.Contains(title, "nothing") {
		t.Errorf("(%s, %q)", got, title)
	}
}

func TestPublishSummaryForAFailure(t *testing.T) {
	got, _, summary := PublishSummary(nil, errors.New("cosign sign refused"))

	if got != Failure {
		t.Errorf("Conclusion = %s", got)
	}
	if !strings.Contains(summary, "cosign sign refused") {
		t.Errorf("summary = %q", summary)
	}
}
