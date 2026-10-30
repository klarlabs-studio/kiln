package task_test

import (
	"strings"
	"testing"

	"go.klarlabs.de/kiln/internal/application/ports"
	"go.klarlabs.de/kiln/internal/domain/config"
	"go.klarlabs.de/kiln/internal/domain/isolation"
	"go.klarlabs.de/kiln/internal/infrastructure/execx"
	"go.klarlabs.de/kiln/internal/infrastructure/task"
)

func runWithCache(t *testing.T, cache string, policy isolation.Policy) string {
	t.Helper()
	var out strings.Builder
	r := task.New(execx.NewSystem())
	r.GoCache = cache
	res := r.Run(t.Context(), ports.TaskRequest{
		Name: "check", Task: config.Task{Run: `echo "gocache=[${GOCACHE:-unset}] token=[${GITHUB_TOKEN:-absent}]"`},
		Dir: t.TempDir(), Policy: policy, Output: &out,
	})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	return out.String()
}

// A task that runs `go build` is as much a cache writer as the gate.
func TestATaskUsesKilnsGoCache(t *testing.T) {
	t.Setenv("GOCACHE", "")
	out := runWithCache(t, "/cache/kiln/go-build", trusted)
	if !strings.Contains(out, "gocache=[/cache/kiln/go-build]") {
		t.Errorf("output = %q, want kiln's GOCACHE", strings.TrimSpace(out))
	}
}

func TestAForkTaskUsesKilnsGoCacheAndStillCannotReadSecrets(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "super-secret-value")
	out := runWithCache(t, "/cache/kiln/go-build", isolation.For(isolation.EventPullRequest, true))
	if !strings.Contains(out, "gocache=[/cache/kiln/go-build]") {
		t.Errorf("output = %q, want kiln's GOCACHE on a fork too", strings.TrimSpace(out))
	}
	if strings.Contains(out, "super-secret-value") {
		t.Error("a fork task read GITHUB_TOKEN")
	}
}

func TestWithoutAKilnCacheATaskInheritsGOCACHE(t *testing.T) {
	t.Setenv("GOCACHE", "/operator/cache")
	out := runWithCache(t, "", trusted)
	if !strings.Contains(out, "gocache=[/operator/cache]") {
		t.Errorf("output = %q, want the operator's GOCACHE untouched", strings.TrimSpace(out))
	}
}
