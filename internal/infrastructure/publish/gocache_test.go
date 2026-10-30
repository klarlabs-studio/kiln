package publish

import (
	"slices"
	"strings"
	"testing"

	"go.klarlabs.de/kiln/internal/application/ports"
	"go.klarlabs.de/kiln/internal/infrastructure/obs"
)

// A release cross-compiles every target, which is as much cache as a week of
// gates; it belongs in kiln's bounded cache like everything else a box builds.
func TestAReleaseUsesKilnsGoCache(t *testing.T) {
	repo, head, fake := releaseRepo(t, signingConfig)
	g := NewGoreleaser(fake, obs.Discard(), "tok", false)
	g.GoCache = "/cache/kiln/go-build"

	if _, err := g.Publish(t.Context(), ports.PublishRequest{
		RepoDir: repo.Dir, SHA: head, Ref: "refs/tags/v1.4.0", Artifact: binariesArtifact(),
	}); err != nil {
		t.Fatal(err)
	}

	env := fake.Find("goreleaser release").Env
	if !slices.Contains(env, "GOCACHE=/cache/kiln/go-build") {
		t.Error("goreleaser did not get kiln's GOCACHE")
	}
	if !slices.Contains(env, "GITHUB_TOKEN=tok") {
		t.Error("adding GOCACHE must not cost the release its credential")
	}
}

func TestWithoutAKilnCacheAReleaseSetsNoGOCACHE(t *testing.T) {
	t.Setenv("GOCACHE", "")
	repo, head, fake := releaseRepo(t, signingConfig)

	if _, err := NewGoreleaser(fake, obs.Discard(), "tok", false).Publish(t.Context(), ports.PublishRequest{
		RepoDir: repo.Dir, SHA: head, Ref: "refs/tags/v1.4.0", Artifact: binariesArtifact(),
	}); err != nil {
		t.Fatal(err)
	}
	for _, kv := range fake.Find("goreleaser release").Env {
		if strings.HasPrefix(kv, "GOCACHE=") && kv != "GOCACHE=" {
			t.Errorf("unexpected %s", kv)
		}
	}
}
