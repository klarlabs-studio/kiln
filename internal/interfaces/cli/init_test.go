package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.klarlabs.de/kiln/internal/domain/config"
	"go.klarlabs.de/kiln/internal/gittest"
	"go.klarlabs.de/kiln/internal/infrastructure/execx"
)

// initIn runs `kiln init` in a repository and returns what it printed and
// what it wrote.
func initIn(t *testing.T, dir string, args ...string) (string, string) {
	t.Helper()
	var out strings.Builder
	io := IO{Out: &out, Err: &out}

	if err := runInit(t.Context(), append([]string{"--dir", dir}, args...), io); err != nil {
		t.Fatalf("init: %v\n%s", err, out.String())
	}
	body, err := os.ReadFile(filepath.Join(dir, config.FileName))
	if err != nil {
		t.Fatalf("no pipeline written: %v", err)
	}
	return out.String(), string(body)
}

func TestInitWritesAPipelineKilnCanLoad(t *testing.T) {
	repo := gittest.New(t)
	repo.Commit("first", "main.go", "package main\n")

	_, body := initIn(t, repo.Dir)

	// The generator's own loader is the only judge worth having. A file kiln
	// emits and then refuses is worse than no generator at all.
	if _, err := config.Parse(strings.NewReader(body)); err != nil {
		t.Fatalf("kiln cannot load what kiln wrote: %v\n%s", err, body)
	}
}

func TestARepositoryWithNothingToPublishGetsNoPublishBlock(t *testing.T) {
	repo := gittest.New(t)
	repo.Commit("first", "main.go", "package main\n")

	out, body := initIn(t, repo.Dir)

	if strings.Contains(body, "publish:") {
		t.Errorf("invented something to publish:\n%s", body)
	}
	if !strings.Contains(out, "publish nothing") {
		t.Errorf("output = %q, want it to say so plainly", out)
	}
}

func TestADockerfileBecomesAnImageArtifact(t *testing.T) {
	repo := gittest.New(t)
	repo.Write("Dockerfile", "FROM scratch\n")
	repo.Commit("first", "main.go", "package main\n")

	_, body := initIn(t, repo.Dir)

	if !strings.Contains(body, "kind: image") {
		t.Errorf("a Dockerfile did not produce an image artifact:\n%s", body)
	}
	// sha alone is refused by the loader — RollOps needs a moving tag to
	// follow — so the generator must not emit it.
	if !strings.Contains(body, "tags: [sha, latest]") {
		t.Errorf("tags = %q, want a moving tag beside the immutable one", body)
	}
	if !strings.Contains(body, "push: [prove, publish]") {
		t.Errorf("a repository with an artifact should route push to publish:\n%s", body)
	}
}

func TestAGoreleaserConfigBecomesABinaryRelease(t *testing.T) {
	repo := gittest.New(t)
	repo.Write(".goreleaser.yaml", "version: 2\n")
	repo.Commit("first", "main.go", "package main\n")

	_, body := initIn(t, repo.Dir)

	if !strings.Contains(body, "kind: binaries") || !strings.Contains(body, "from: goreleaser") {
		t.Errorf("goreleaser was not picked up:\n%s", body)
	}
}

func TestAMissingGateIsSaidOutLoud(t *testing.T) {
	repo := gittest.New(t)
	repo.Commit("first", "main.go", "package main\n")

	out, _ := initIn(t, repo.Dir)

	// kiln runs no checks of its own. A pipeline with no .warden.yaml proves
	// nothing, and finding that out later — from a green run that checked
	// nothing — is the worst version of it.
	if !strings.Contains(out, "no .warden.yaml") {
		t.Errorf("output = %q, want the missing gate named", out)
	}
}

func TestAnExistingPipelineIsNotClobbered(t *testing.T) {
	repo := gittest.New(t)
	repo.Write(config.FileName, "# hand written\n")
	repo.Commit("first", "main.go", "package main\n")

	var out strings.Builder
	err := runInit(t.Context(), []string{"--dir", repo.Dir}, IO{Out: &out, Err: &out})
	if err == nil {
		t.Fatal("an existing pipeline was overwritten without --force")
	}

	body, readErr := os.ReadFile(filepath.Join(repo.Dir, config.FileName))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(body) != "# hand written\n" {
		t.Errorf("the file changed anyway: %q", body)
	}
}

func TestForceReplacesIt(t *testing.T) {
	repo := gittest.New(t)
	repo.Write(config.FileName, "# hand written\n")
	repo.Commit("first", "main.go", "package main\n")

	_, body := initIn(t, repo.Dir, "--force")
	if strings.Contains(body, "hand written") {
		t.Error("--force did not replace the file")
	}
}

func TestSomethingThatIsNotARepositoryIsRefused(t *testing.T) {
	var out strings.Builder
	err := runInit(t.Context(), []string{"--dir", t.TempDir()}, IO{Out: &out, Err: &out})

	// Every phase checks a commit out of a repository. Writing a pipeline into
	// a directory that has none would produce a file that can never run.
	if err == nil {
		t.Fatal("wrote a pipeline into a directory that is not a git repository")
	}
	if !strings.Contains(err.Error(), "git repository") {
		t.Errorf("err = %v", err)
	}
}

func TestTheImageNameComesFromTheRemote(t *testing.T) {
	repo := gittest.New(t)
	repo.Write("Dockerfile", "FROM scratch\n")
	repo.Commit("first", "main.go", "package main\n")

	if _, err := execx.NewSystem().Run(t.Context(), execx.Cmd{
		Name: "git", Args: []string{"remote", "add", "origin", "git@github.com:acme/widget.git"}, Dir: repo.Dir,
	}); err != nil {
		t.Fatal(err)
	}

	_, body := initIn(t, repo.Dir, "--force")

	// Guessed from the remote rather than invented: a wrong image name fails
	// at the push, after the gate has run and the image has been built.
	if !strings.Contains(body, "ghcr.io/acme/widget") {
		t.Errorf("image not derived from the remote:\n%s", body)
	}
}

func TestSlugsAreReadFromBothRemoteForms(t *testing.T) {
	for url, want := range map[string]string{
		"git@github.com:acme/widget.git":       "acme/widget",
		"https://github.com/acme/widget.git":   "acme/widget",
		"https://github.com/acme/widget":       "acme/widget",
		"ssh://git@github.com/acme/widget.git": "acme/widget",
		"git@gitlab.com:team/sub/widget.git":   "team/sub/widget",
		"":                                     "",
	} {
		if got := repoSlug(url); got != want {
			t.Errorf("repoSlug(%q) = %q, want %q", url, got, want)
		}
	}
}
