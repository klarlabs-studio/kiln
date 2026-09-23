package gitea

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"go.klarlabs.de/kiln/internal/infrastructure/obs"
)

func testClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	c := NewClient("tok", Repo{Owner: "klarlabs-studio", Name: "kiln"}, srv.URL, obs.Discard())
	c.BaseURL = srv.URL
	c.Attempts = 2
	return c
}

func TestNormalizeBaseURL(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"https://gitea.example.com", "https://gitea.example.com/api/v1"},
		{"https://gitea.example.com/", "https://gitea.example.com/api/v1"},
		{"https://gitea.example.com/api/v1", "https://gitea.example.com/api/v1"},
		{"https://gitea.example.com/api/v1/", "https://gitea.example.com/api/v1"},
	}
	for _, tc := range cases {
		if got := NormalizeBaseURL(tc.in); got != tc.want {
			t.Errorf("NormalizeBaseURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestEnabledRequiresTokenRepoAndURL(t *testing.T) {
	if NewClient("", Repo{Owner: "a", Name: "b"}, "https://gitea.example", nil).Enabled() {
		t.Error("a tokenless client must not be enabled")
	}
	if NewClient("tok", Repo{}, "https://gitea.example", nil).Enabled() {
		t.Error("a client with no repository must not be enabled")
	}
	if NewClient("tok", Repo{Owner: "a", Name: "b"}, "", nil).Enabled() {
		t.Error("a client with no instance URL must not be enabled")
	}
	if !NewClient("tok", Repo{Owner: "a", Name: "b"}, "https://gitea.example", nil).Enabled() {
		t.Error("a fully configured client should be enabled")
	}
}

func TestLookupPullDetectsAFork(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "token tok" {
			t.Errorf("Authorization = %q, want token auth", got)
		}
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("Accept = %q", r.Header.Get("Accept"))
		}
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

func TestCreateStatus(t *testing.T) {
	var body map[string]any
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.Contains(r.URL.Path, "/statuses/") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusCreated)
	})

	if err := c.CreateStatus(t.Context(), "abc123", "success", "Kiln / Prove", "gate passed"); err != nil {
		t.Fatalf("CreateStatus: %v", err)
	}
	if body["state"] != "success" || body["context"] != "Kiln / Prove" {
		t.Errorf("payload = %v", body)
	}
}

func TestOpenPullRequestIsIdempotentByHead(t *testing.T) {
	var posts atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`[
				{"number": 4, "head": {"sha": "abc", "ref": "kiln/deps", "repo": {"full_name": "klarlabs-studio/kiln"}},
				 "base": {"repo": {"full_name": "klarlabs-studio/kiln"}}}
			]`))
			return
		}
		posts.Add(1)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"number": 99}`))
	})

	got, opened, err := c.OpenPullRequest(t.Context(), "kiln/deps", "main", "deps", "body")
	if err != nil {
		t.Fatal(err)
	}
	if opened || got.Number != 4 {
		t.Errorf("opened=%v pull=%+v, want the existing #4", opened, got)
	}
	if posts.Load() != 0 {
		t.Error("an existing head must not open another pull request")
	}
}

func TestOpenPullRequestCreatesWhenMissing(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["head"] != "kiln/deps" || body["base"] != "main" {
			t.Errorf("payload = %v", body)
		}
		_, _ = w.Write([]byte(`{
			"number": 11,
			"head": {"sha": "abc", "ref": "kiln/deps", "repo": {"full_name": "klarlabs-studio/kiln"}},
			"base": {"repo": {"full_name": "klarlabs-studio/kiln"}}
		}`))
	})

	got, opened, err := c.OpenPullRequest(t.Context(), "kiln/deps", "main", "deps", "body")
	if err != nil {
		t.Fatal(err)
	}
	if !opened || got.Number != 11 {
		t.Errorf("opened=%v pull=%+v", opened, got)
	}
}

func TestLabelPull(t *testing.T) {
	var path string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.WriteHeader(http.StatusOK)
	})

	if err := c.LabelPull(t.Context(), 3, []string{"kiln"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(path, "/issues/3/labels") {
		t.Errorf("path = %q, want the issues labels endpoint", path)
	}
}

func TestLabelPullNoopWhenEmpty(t *testing.T) {
	c := testClient(t, func(http.ResponseWriter, *http.Request) {
		t.Error("empty labels must not hit the API")
	})
	if err := c.LabelPull(t.Context(), 3, nil); err != nil {
		t.Fatal(err)
	}
}

func TestWhoAmIReadsTheLogin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user" && r.URL.Path != "/api/v1/user" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "token tok" {
			t.Errorf("Authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"login":"gitea-admin"}`))
	}))
	t.Cleanup(srv.Close)

	got, err := WhoAmI(t.Context(), "tok", srv.URL)
	if err != nil {
		t.Fatalf("WhoAmI: %v", err)
	}
	if got != "gitea-admin" {
		t.Errorf("login = %q", got)
	}
}

func TestWhoAmIRejectedTokenIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"token is invalid"}`))
	}))
	t.Cleanup(srv.Close)

	_, err := WhoAmI(t.Context(), "bad", srv.URL)
	if err == nil {
		t.Fatal("a 401 was reported as success")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized {
		t.Fatalf("err = %v, want a 401 APIError", err)
	}
}

func TestServerErrorsAreRetried(t *testing.T) {
	var calls atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{
			"number": 1,
			"head": {"sha": "a", "ref": "x", "repo": {"full_name": "klarlabs-studio/kiln"}},
			"base": {"repo": {"full_name": "klarlabs-studio/kiln"}}
		}`))
	})

	if _, err := c.LookupPull(t.Context(), 1); err != nil {
		t.Fatalf("a 502 should be retried, got %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want a retry", calls.Load())
	}
}

func TestDisabledClientRefuses(t *testing.T) {
	c := NewClient("", Repo{Owner: "a", Name: "b"}, "https://gitea.example", obs.Discard())
	if _, err := c.LookupPull(t.Context(), 1); err == nil {
		t.Error("a tokenless client must refuse rather than post anonymously")
	}
}

func TestNewProposerNilWhenDisabled(t *testing.T) {
	if NewProposer(nil) != nil {
		t.Error("nil client must yield a nil proposer")
	}
	if NewProposer(NewClient("", Repo{Owner: "a", Name: "b"}, "https://gitea.example", nil)) != nil {
		t.Error("disabled client must yield a nil proposer")
	}
}

func TestProposerOpensAndLabels(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[]`))
		case strings.Contains(r.URL.Path, "/labels"):
			w.WriteHeader(http.StatusOK)
		default:
			_, _ = w.Write([]byte(`{
				"number": 12,
				"head": {"sha": "abc", "ref": "kiln/deps", "repo": {"full_name": "klarlabs-studio/kiln"}},
				"base": {"repo": {"full_name": "klarlabs-studio/kiln"}}
			}`))
		}
	})

	p := NewProposer(c)
	if p == nil {
		t.Fatal("an enabled client must yield a proposer")
	}
	n, opened, err := p.OpenPullRequest(t.Context(), "kiln/deps", "main", "deps", "body")
	if err != nil || !opened || n != 12 {
		t.Fatalf("OpenPullRequest = %d opened=%v err=%v", n, opened, err)
	}
	if err := p.LabelPull(t.Context(), n, []string{"kiln"}); err != nil {
		t.Fatal(err)
	}
}
