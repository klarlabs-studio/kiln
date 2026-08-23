package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// WhoAmI hand-built a Client literal, which left HTTP nil and BaseURL empty,
// so `kiln login` died on a nil-pointer dereference before reaching the API —
// step two of the three-command quick start, on every path. It shipped because
// this function had no test at all: everything else in the package builds a
// client through a helper that sets those fields.
//
// This test therefore exercises the constructor path rather than injecting a
// client, because the constructor is the thing that was wrong.
func TestWhoAmI_DoesNotPanicAndReadsTheLogin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user" {
			t.Errorf("path = %q, want /user", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"login":"octocat"}`))
	}))
	defer srv.Close()

	got, err := whoAmIAt(context.Background(), "tok", srv.URL)
	if err != nil {
		t.Fatalf("WhoAmI: %v", err)
	}
	if got != "octocat" {
		t.Errorf("login = %q, want octocat", got)
	}
}

// A fine-grained token with no user scope still authenticates, and the caller
// only needs to know the credential works.
func TestWhoAmI_TokenWithNoLoginStillCounts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	got, err := whoAmIAt(context.Background(), "tok", srv.URL)
	if err != nil {
		t.Fatalf("WhoAmI: %v", err)
	}
	if got != "the token" {
		t.Errorf("login = %q, want %q", got, "the token")
	}
}

// A rejected token has to surface as an error the operator can read, not a
// crash and not a silent success.
func TestWhoAmI_RejectedTokenIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	defer srv.Close()

	_, err := whoAmIAt(context.Background(), "bad", srv.URL)
	if err == nil {
		t.Fatal("a 401 was reported as success")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error does not name the status: %v", err)
	}
}
