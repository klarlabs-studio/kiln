package prove

import (
	"os"
	"path/filepath"
	"testing"

	"go.klarlabs.de/kiln/internal/application/ports"
	"go.klarlabs.de/kiln/internal/domain/isolation"
)

// A box proves inside a disposable worktree, and node_modules is gitignored,
// so it exists in the clone and never in the checkout. Every Node repository's
// gate therefore failed with "vitest: command not found" — while passing by
// hand in the clone, which reads as flakiness rather than a missing file.
func TestMaterializeCarriesGitignoredDepsIntoTheWorktree(t *testing.T) {
	clone, worktree := t.TempDir(), t.TempDir()
	mustFile(t, filepath.Join(clone, "node_modules", ".bin", "vitest"), "#!/bin/sh\n")

	if err := materialize(clone, worktree, []string{"node_modules"}, trustedPolicy()); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(worktree, "node_modules", ".bin", "vitest")); err != nil {
		t.Errorf("node_modules did not reach the worktree: %v", err)
	}
}

// The pipeline is read from the commit being gated, so a fork's author writes
// it. Materialising an arbitrary path would hand them whatever gitignored
// files sit in the operator's clone — .env being the obvious one.
func TestAForkMaterialisesNothing(t *testing.T) {
	clone, worktree := t.TempDir(), t.TempDir()
	mustFile(t, filepath.Join(clone, "node_modules", "x"), "x")
	mustFile(t, filepath.Join(clone, ".env"), "AWS_SECRET_ACCESS_KEY=...")

	if err := materialize(clone, worktree, []string{"node_modules", ".env"}, isolation.For(isolation.EventPullRequest, true)); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(worktree, ".env")); err == nil {
		t.Error("a fork's pipeline materialised the operator's .env into code it is about to run")
	}
	if _, err := os.Stat(filepath.Join(worktree, "node_modules")); err == nil {
		t.Error("a fork materialised at all; the gate failing honestly is the safe outcome")
	}
}

// Containment, checked again at the point of use and not only at load: the
// runtime check is the real one.
func TestMaterializeRefusesToEscapeTheClone(t *testing.T) {
	clone, worktree := t.TempDir(), t.TempDir()

	for _, bad := range []string{"../outside", "/etc", "node_modules/../../elsewhere"} {
		if err := materialize(clone, worktree, []string{bad}, trustedPolicy()); err == nil {
			t.Errorf("materialize(%q) was allowed out of the clone", bad)
		}
	}
}

// A repository that names something it does not have is not an error: a Go
// repo with the key set, or a clone where nobody has run npm install yet.
// The gate then fails on its own terms, which is the honest outcome.
func TestMaterializeSkipsWhatIsNotThere(t *testing.T) {
	clone, worktree := t.TempDir(), t.TempDir()

	if err := materialize(clone, worktree, []string{"node_modules"}, trustedPolicy()); err != nil {
		t.Errorf("a missing directory must not fail the run: %v", err)
	}
}

// Never over the top of something the commit itself provides.
func TestMaterializeDoesNotOverwriteTheCheckout(t *testing.T) {
	clone, worktree := t.TempDir(), t.TempDir()
	mustFile(t, filepath.Join(clone, "vendor", "from-clone"), "clone")
	mustFile(t, filepath.Join(worktree, "vendor", "from-commit"), "commit")

	if err := materialize(clone, worktree, []string{"vendor"}, trustedPolicy()); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(worktree, "vendor", "from-commit")); err != nil {
		t.Error("materialising replaced a directory the commit tracks")
	}
	if _, err := os.Stat(filepath.Join(worktree, "vendor", "from-clone")); err == nil {
		t.Error("materialised over a path the checkout already provided")
	}
}

func trustedPolicy() isolation.Policy { return isolation.For(isolation.EventPush, false) }

func mustFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

var _ = ports.ProveRequest{}
