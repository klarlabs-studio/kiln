package boot

import (
	"path/filepath"
	"strings"
	"testing"

	"go.klarlabs.de/kiln/internal/checks"
	"go.klarlabs.de/kiln/internal/envconfig"
	"go.klarlabs.de/kiln/internal/gittest"
	"go.klarlabs.de/kiln/internal/obs"
	"go.klarlabs.de/kiln/internal/publish"
)

const pipeline = `apiVersion: kiln.klarlabs.de/v1
kind: Pipeline
on:
  pull_request: [prove]
  push: [prove, publish]
publish:
  image: ghcr.io/felixgeelhaar/glossa-api
  tags: [sha, latest]
`

func env(t *testing.T) envconfig.Env {
	t.Helper()
	return envconfig.Env{
		DB:       ".kiln/state.json",
		Warden:   "warden",
		Nox:      "nox",
		Addr:     envconfig.DefaultAddr,
		LogLevel: "fatal",
	}
}

func build(t *testing.T, dir string, e envconfig.Env) *Deps {
	t.Helper()
	deps, err := Build(t.Context(), Options{Dir: dir, Env: &e, Log: obs.Discard()})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return deps
}

func TestBuildOnARepositoryWithAPipeline(t *testing.T) {
	repo := gittest.New(t)
	repo.Commit("first", "app.txt", "one\n")
	repo.Write(".kiln.yaml", pipeline)

	deps := build(t, repo.Dir, env(t))

	if !deps.PipelineFound {
		t.Error("PipelineFound = false")
	}
	if deps.Pipeline.Publish.Image != "ghcr.io/felixgeelhaar/glossa-api" {
		t.Errorf("image = %q", deps.Pipeline.Publish.Image)
	}
	if deps.Engine == nil || deps.Store == nil || deps.Checks == nil {
		t.Errorf("graph incomplete: %+v", deps)
	}
}

func TestBuildWithoutAPipelineStillProves(t *testing.T) {
	repo := gittest.New(t)
	repo.Commit("first", "app.txt", "one\n")

	deps := build(t, repo.Dir, env(t))

	if deps.PipelineFound {
		t.Error("PipelineFound = true with no file")
	}
	// A library repository is still gated and still reports a Check.
	if !deps.Pipeline.Wants("push", "prove") {
		t.Error("the default pipeline must prove")
	}
	if deps.Pipeline.WantsPublish() {
		t.Error("the default pipeline must not publish")
	}
}

func TestBuildRejectsAMalformedPipeline(t *testing.T) {
	repo := gittest.New(t)
	repo.Commit("first", "app.txt", "one\n")
	repo.Write(".kiln.yaml", "apiVersion: kiln.klarlabs.de/v1\nkind: Pipeline\ndeploy: {}\n")

	e := env(t)
	_, err := Build(t.Context(), Options{Dir: repo.Dir, Env: &e, Log: obs.Discard()})

	if err == nil || !strings.Contains(err.Error(), "RollOps") {
		t.Errorf("err = %v, want the CD-key rejection", err)
	}
}

func TestBuildRejectsANonRepository(t *testing.T) {
	e := env(t)
	_, err := Build(t.Context(), Options{Dir: t.TempDir(), Env: &e, Log: obs.Discard()})

	if err == nil || !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("err = %v", err)
	}
}

func TestBuildRejectsAMissingExplicitPipeline(t *testing.T) {
	repo := gittest.New(t)
	repo.Commit("first", "app.txt", "one\n")

	e := env(t)
	// An explicitly named pipeline that does not exist is a mistake worth
	// stopping for, unlike a missing default one.
	_, err := Build(t.Context(), Options{
		Dir: repo.Dir, PipelinePath: filepath.Join(repo.Dir, "nope.yaml"), Env: &e, Log: obs.Discard(),
	})
	if err == nil {
		t.Error("Build accepted a --pipeline path that does not exist")
	}
}

func TestRelativeLedgerLandsInsideTheRepository(t *testing.T) {
	repo := gittest.New(t)
	repo.Commit("first", "app.txt", "one\n")

	deps := build(t, repo.Dir, env(t))

	// A cron entry whose working directory differs from the checkout must not
	// keep a second, always-empty ledger and rebuild everything every tick.
	want := filepath.Join(repo.Dir, ".kiln", "state.json")
	if deps.Store.Path() != want {
		t.Errorf("ledger at %q, want %q", deps.Store.Path(), want)
	}
}

func TestAbsoluteLedgerIsRespected(t *testing.T) {
	repo := gittest.New(t)
	repo.Commit("first", "app.txt", "one\n")
	e := env(t)
	e.DB = filepath.Join(t.TempDir(), "shared.json")

	deps := build(t, repo.Dir, e)

	if deps.Store.Path() != e.DB {
		t.Errorf("ledger at %q, want %q", deps.Store.Path(), e.DB)
	}
}

func TestNoTokenMeansNoChecksAndNoClient(t *testing.T) {
	repo := gittest.New(t)
	repo.Commit("first", "app.txt", "one\n")

	deps := build(t, repo.Dir, env(t))

	if deps.ChecksEnabled() {
		t.Error("ChecksEnabled = true without a token")
	}
	if deps.GitHub != nil {
		t.Error("a client was built with no token")
	}
	// Gate the commit, print the result, tell nobody — rather than fail.
	if _, ok := deps.Checks.(checks.Noop); !ok {
		t.Errorf("reporter = %T, want checks.Noop", deps.Checks)
	}
}

func TestTokenAndRepositoryEnableChecks(t *testing.T) {
	repo := gittest.New(t)
	repo.Commit("first", "app.txt", "one\n")
	e := env(t)
	e.Token = "ghp_x"
	e.Repository = "klarlabs-studio/kiln"

	deps := build(t, repo.Dir, e)

	if !deps.ChecksEnabled() {
		t.Fatalf("ChecksEnabled = false (repo=%v err=%v)", deps.Repo, deps.RepoErr)
	}
	if deps.Repo.String() != "klarlabs-studio/kiln" {
		t.Errorf("Repo = %s", deps.Repo)
	}
}

func TestUnidentifiableRepositoryIsRecordedNotFatal(t *testing.T) {
	repo := gittest.New(t)
	repo.Commit("first", "app.txt", "one\n")

	deps := build(t, repo.Dir, env(t))

	// No remote and no GITHUB_REPOSITORY. Checks are simply off.
	if deps.RepoErr == nil {
		t.Error("RepoErr = nil for a repository with no remote")
	}
	if deps.Engine == nil {
		t.Error("Build gave up because it could not name the repository")
	}
}

func TestDryUsesTheRehearsalPublisher(t *testing.T) {
	repo := gittest.New(t)
	repo.Commit("first", "app.txt", "one\n")
	repo.Write(".kiln.yaml", pipeline)
	e := env(t)
	e.Dry = true

	deps := build(t, repo.Dir, e)

	if !deps.Dry() {
		t.Error("Dry = false with KILN_DRY set")
	}
	if _, ok := deps.Engine.Publisher.(*publish.Dry); !ok {
		t.Errorf("publisher = %T, want *publish.Dry", deps.Engine.Publisher)
	}
}

func TestForkUnknownIsUntrusted(t *testing.T) {
	// Without a token Kiln cannot tell a maintainer's branch from a
	// stranger's fork, and the permissive guess hands over credentials.
	if !ForkUnknown {
		t.Error("ForkUnknown must be true: unknown provenance is untrusted provenance")
	}
}

func TestResolvePullForkWithoutATokenAssumesFork(t *testing.T) {
	repo := gittest.New(t)
	repo.Commit("first", "app.txt", "one\n")
	deps := build(t, repo.Dir, env(t))

	if !deps.ResolvePullFork(t.Context(), 7) {
		t.Error("a pull request that cannot be identified must be treated as a fork")
	}
}
