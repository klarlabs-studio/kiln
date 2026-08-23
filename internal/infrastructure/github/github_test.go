package github

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"go.klarlabs.de/kiln/internal/infrastructure/execx"
	"go.klarlabs.de/kiln/internal/infrastructure/obs"
)

func testClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	c := NewClient("tok", Repo{Owner: "klarlabs-studio", Name: "kiln"}, obs.Discard())
	c.BaseURL = srv.URL
	c.Attempts = 2
	return c
}

func TestParseRepo(t *testing.T) {
	want := Repo{Owner: "felixgeelhaar", Name: "glossa"}
	for _, in := range []string{
		"felixgeelhaar/glossa",
		"felixgeelhaar/glossa.git",
		"https://github.com/felixgeelhaar/glossa",
		"https://github.com/felixgeelhaar/glossa.git",
		"git@github.com:felixgeelhaar/glossa.git",
		"ssh://git@github.com/felixgeelhaar/glossa",
	} {
		got, err := ParseRepo(in)
		if err != nil {
			t.Errorf("ParseRepo(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseRepo(%q) = %+v, want %+v", in, got, want)
		}
	}
}

func TestParseRepoRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "justaname", "a/b/c", "/", "owner/"} {
		if _, err := ParseRepo(in); err == nil {
			t.Errorf("ParseRepo(%q) accepted garbage", in)
		}
	}
}

func TestDiscoverRepoPrefersTheRemote(t *testing.T) {
	fake := execx.NewFake().On("git remote get-url origin", execx.Response{
		Stdout: "git@github.com:klarlabs-studio/kiln.git\n",
	})

	got, err := DiscoverRepo(t.Context(), fake, "/repo", "origin", "someone/else")
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "klarlabs-studio/kiln" {
		t.Errorf("Repo = %s, want the remote's answer", got)
	}
}

func TestDiscoverRepoFallsBackToTheEnvironment(t *testing.T) {
	fake := execx.NewFake().On("git remote", execx.Response{ExitCode: 128, Stderr: "no such remote"})

	got, err := DiscoverRepo(t.Context(), fake, "/repo", "origin", "klarlabs-studio/kiln")
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "klarlabs-studio/kiln" {
		t.Errorf("Repo = %s", got)
	}
}

func TestDiscoverRepoWithNothingToGoOn(t *testing.T) {
	fake := execx.NewFake().On("git remote", execx.Response{ExitCode: 128})

	if _, err := DiscoverRepo(t.Context(), fake, "/repo", "origin", ""); err == nil {
		t.Error("want an error when neither source resolves")
	}
}

func TestEnabledRequiresATokenAndRepo(t *testing.T) {
	if NewClient("", Repo{Owner: "a", Name: "b"}, nil).Enabled() {
		t.Error("a tokenless client must not be enabled")
	}
	if NewClient("tok", Repo{}, nil).Enabled() {
		t.Error("a client with no repository must not be enabled")
	}
	if !NewClient("tok", Repo{Owner: "a", Name: "b"}, nil).Enabled() {
		t.Error("a fully configured client should be enabled")
	}
}

func TestCreateCheckRun(t *testing.T) {
	var body map[string]any
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/check-runs") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q", got)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(CheckRun{ID: 42, Name: "Kiln / Prove"})
	})

	got, err := c.CreateCheckRun(t.Context(), "Kiln / Prove", "abc123")
	if err != nil {
		t.Fatalf("CreateCheckRun: %v", err)
	}
	if got.ID != 42 {
		t.Errorf("ID = %d", got.ID)
	}
	if body["status"] != "in_progress" || body["head_sha"] != "abc123" {
		t.Errorf("payload = %v", body)
	}
}

func TestCompleteCheckRunTruncatesTheSummary(t *testing.T) {
	var body map[string]any
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("method = %s, want PATCH", r.Method)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusOK)
	})

	huge := strings.Repeat("x", 100_000)
	if err := c.CompleteCheckRun(t.Context(), 42, "success", "passed", huge); err != nil {
		t.Fatalf("CompleteCheckRun: %v", err)
	}

	output, _ := body["output"].(map[string]any)
	summary, _ := output["summary"].(string)
	// GitHub rejects an over-long summary outright, which would fail the
	// *reporting* of a build that actually succeeded.
	if len(summary) > 65000 {
		t.Errorf("summary is %d chars, want it truncated", len(summary))
	}
}

func TestLookupPullDetectsAFork(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"number": 7, "draft": false,
			"head": {"sha": "deadbeef", "ref": "feature", "repo": {"full_name": "stranger/kiln"}},
			"base": {"repo": {"full_name": "klarlabs-studio/kiln"}}
		}`))
	})

	got, err := c.LookupPull(t.Context(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Fork {
		t.Error("a head in a different repository is a fork")
	}
	if got.HeadSHA != "deadbeef" || got.HeadRef != "feature" {
		t.Errorf("Pull = %+v", got)
	}
}

func TestLookupPullSameRepoIsNotAFork(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"number": 8,
			"head": {"sha": "abc", "ref": "feature", "repo": {"full_name": "klarlabs-studio/kiln"}},
			"base": {"repo": {"full_name": "klarlabs-studio/kiln"}}
		}`))
	})

	got, err := c.LookupPull(t.Context(), 8)
	if err != nil {
		t.Fatal(err)
	}
	if got.Fork {
		t.Error("a same-repo branch is not a fork")
	}
}

func TestLookupPullWithADeletedHeadRepoIsAFork(t *testing.T) {
	// head.repo is null when the fork was deleted. "I cannot tell where this
	// came from" must resolve to the answer that withholds credentials.
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"number": 9,
			"head": {"sha": "abc", "ref": "gone", "repo": null},
			"base": {"repo": {"full_name": "klarlabs-studio/kiln"}}
		}`))
	})

	got, err := c.LookupPull(t.Context(), 9)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Fork {
		t.Error("an unknown head repository must be treated as a fork")
	}
}

func TestListOpenPulls(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "state=open") {
			t.Errorf("query = %q, want open pulls only", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`[
			{"number": 1, "head": {"sha": "a", "ref": "x", "repo": {"full_name": "klarlabs-studio/kiln"}},
			 "base": {"repo": {"full_name": "klarlabs-studio/kiln"}}},
			{"number": 2, "head": {"sha": "b", "ref": "y", "repo": {"full_name": "stranger/kiln"}},
			 "base": {"repo": {"full_name": "klarlabs-studio/kiln"}}}
		]`))
	})

	got, err := c.ListOpenPulls(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Fork || !got[1].Fork {
		t.Errorf("pulls = %+v", got)
	}
}

func TestServerErrorsAreRetried(t *testing.T) {
	var calls atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(CheckRun{ID: 1})
	})

	if _, err := c.CreateCheckRun(t.Context(), "Kiln / Prove", "abc"); err != nil {
		t.Fatalf("a 502 should be retried, got %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want a retry", calls.Load())
	}
}

func TestRateLimitIsRetried(t *testing.T) {
	var calls atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_ = json.NewEncoder(w).Encode(CheckRun{ID: 1})
	})

	if _, err := c.CreateCheckRun(t.Context(), "Kiln / Prove", "abc"); err != nil {
		t.Fatalf("a 429 should be retried, got %v", err)
	}
}

func TestClientErrorsAreNotRetried(t *testing.T) {
	var calls atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
	})

	_, err := c.CreateCheckRun(t.Context(), "Kiln / Prove", "abc")

	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized {
		t.Fatalf("err = %v, want a 401 APIError", err)
	}
	// A bad token says the same thing every time.
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", calls.Load())
	}
	if !strings.Contains(err.Error(), "Bad credentials") {
		t.Errorf("error should quote the API, got %v", err)
	}
}

func TestDisabledClientRefuses(t *testing.T) {
	c := NewClient("", Repo{Owner: "a", Name: "b"}, obs.Discard())

	if _, err := c.CreateCheckRun(t.Context(), "Kiln / Prove", "abc"); err == nil {
		t.Error("a tokenless client must refuse rather than post anonymously")
	}
}
