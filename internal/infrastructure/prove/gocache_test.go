package prove

import (
	"slices"
	"strings"
	"testing"

	"go.klarlabs.de/kiln/internal/application/ports"
	"go.klarlabs.de/kiln/internal/infrastructure/execx"
)

// `go test -race` under the gate is what grew the operator's Go cache by
// ~35 GB in twelve hours. With a kiln cache configured, every child the gate
// starts writes there instead — on a trusted run, which otherwise inherits.
func TestATrustedGateUsesKilnsGoCache(t *testing.T) {
	t.Setenv("HOME", "/home/build")
	fake := fakeRepo()
	w := NewWarden(fake, "warden", "nox")
	w.GoCache = "/cache/kiln/go-build"

	_ = w.Prove(t.Context(), ports.ProveRequest{RepoDir: "/repo", SHA: "abc123", Policy: trusted, Nox: true})

	for _, name := range []string{"warden run pre-push", "nox scan"} {
		cmd := fake.Find(name)
		if cmd == nil {
			t.Fatalf("%s not run: %s", name, fake.Transcript())
		}
		if !slices.Contains(cmd.Env, "GOCACHE=/cache/kiln/go-build") {
			t.Errorf("%s did not get kiln's GOCACHE", name)
		}
		// Rebuilding the environment must not drop the inheritance.
		if !slices.Contains(cmd.Env, "HOME=/home/build") {
			t.Errorf("%s lost the inherited environment", name)
		}
	}
}

// A fork's scrubbed environment gets the same cache, and still no secrets:
// without it a fork build would fall back to the operator's cache.
func TestAForkGateUsesKilnsGoCacheWithoutSecrets(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_secret")
	fake := fakeRepo()
	w := NewWarden(fake, "warden", "nox")
	w.GoCache = "/cache/kiln/go-build"

	_ = w.Prove(t.Context(), ports.ProveRequest{RepoDir: "/repo", SHA: "abc123", Policy: untrusted})

	cmd := fake.Find("warden run pre-push")
	if cmd == nil {
		t.Fatalf("gate not run: %s", fake.Transcript())
	}
	if !slices.Contains(cmd.Env, "GOCACHE=/cache/kiln/go-build") {
		t.Error("a fork gate did not get kiln's GOCACHE")
	}
	for _, kv := range cmd.Env {
		name, _, _ := strings.Cut(kv, "=")
		if execx.IsSecretVar(name) {
			t.Errorf("fork run can see a credential: %s", name)
		}
	}
}

func TestWithoutAKilnCacheATrustedGateStillInherits(t *testing.T) {
	fake := fakeRepo()
	_ = NewWarden(fake, "warden", "nox").Prove(t.Context(), ports.ProveRequest{
		RepoDir: "/repo", SHA: "abc123", Policy: trusted,
	})
	if cmd := fake.Find("warden run pre-push"); cmd == nil || cmd.Env != nil {
		t.Errorf("with no kiln cache the gate should inherit unchanged: %v", cmd)
	}
}
