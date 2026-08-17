package prove

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"go.klarlabs.de/kiln/internal/execx"
	"go.klarlabs.de/kiln/internal/gittest"
	"go.klarlabs.de/kiln/internal/isolation"
)

var (
	trusted   = isolation.For(isolation.EventPush, false)
	untrusted = isolation.For(isolation.EventPullRequest, true)
)

// fakeRepo scripts the git calls a worktree needs so a prove test does not
// have to touch the disk. Tests that care about the checkout itself use a real
// repository instead.
func fakeRepo() *execx.Fake {
	return execx.NewFake()
}

func TestProveRunsTheGateInAWorktree(t *testing.T) {
	repo := gittest.New(t)
	sha := repo.Commit("first", "a.txt", "x\n")

	var ranIn string
	fake := execx.NewFake()
	fake.On("warden run pre-push", execx.Response{Fn: func(c execx.Cmd) (execx.Result, error) {
		ranIn = c.Dir
		return execx.Result{}, nil
	}})
	// git must be real so the worktree actually exists; everything else is
	// scripted. The Fake handles both because it matches on the command line.
	fake.On("git", execx.Response{Fn: func(c execx.Cmd) (execx.Result, error) {
		return execx.NewSystem().Run(t.Context(), c)
	}})

	if err := NewWarden(fake, "warden", "nox").Prove(t.Context(), Request{
		RepoDir: repo.Dir, SHA: sha, Policy: trusted,
	}); err != nil {
		t.Fatalf("Prove: %v", err)
	}

	if ranIn == "" || ranIn == repo.Dir {
		t.Errorf("gate ran in %q, want a disposable worktree, not the operator's checkout", ranIn)
	}
	if _, err := os.Stat(ranIn); !os.IsNotExist(err) {
		t.Error("the worktree survived the run")
	}
}

func TestGateFailureIsDistinguishable(t *testing.T) {
	fake := fakeRepo().On("warden run pre-push", execx.Response{ExitCode: 1, Stderr: "lint failed"})

	err := NewWarden(fake, "warden", "nox").Prove(t.Context(), Request{
		RepoDir: "/repo", SHA: "abc123", Policy: trusted,
	})

	// "your change did not pass" must not read the same as "kiln broke".
	if !errors.Is(err, ErrGateFailed) {
		t.Fatalf("err = %v, want ErrGateFailed", err)
	}
	if !strings.Contains(err.Error(), "lint failed") {
		t.Errorf("error should quote the gate, got %v", err)
	}
}

func TestMissingWardenIsAFailureNotASkip(t *testing.T) {
	fake := fakeRepo().Absent("warden")

	err := NewWarden(fake, "warden", "nox").Prove(t.Context(), Request{
		RepoDir: "/repo", SHA: "abc123", Policy: trusted,
	})

	if !errors.Is(err, ErrToolMissing) {
		t.Fatalf("err = %v, want ErrToolMissing", err)
	}
	// The check must happen before the checkout, so the operator learns
	// immediately rather than after a clone.
	if fake.Ran("git worktree add") {
		t.Errorf("checked out a tree before noticing the missing gate: %s", fake.Transcript())
	}
}

func TestNoxEnabledButMissingIsAFailure(t *testing.T) {
	fake := fakeRepo().Absent("nox")

	err := NewWarden(fake, "warden", "nox").Prove(t.Context(), Request{
		RepoDir: "/repo", SHA: "abc123", Policy: trusted, Nox: true,
	})

	if !errors.Is(err, ErrToolMissing) {
		t.Fatalf("err = %v, want ErrToolMissing", err)
	}
	if !strings.Contains(err.Error(), "prove.nox") {
		t.Errorf("error should name the setting that turns it off, got %v", err)
	}
}

func TestNoxDisabledDoesNotRequireNox(t *testing.T) {
	repo := gittest.New(t)
	sha := repo.Commit("first", "a.txt", "x\n")

	fake := execx.NewFake().Absent("nox")
	fake.On("git", execx.Response{Fn: func(c execx.Cmd) (execx.Result, error) {
		return execx.NewSystem().Run(t.Context(), c)
	}})

	if err := NewWarden(fake, "warden", "nox").Prove(t.Context(), Request{
		RepoDir: repo.Dir, SHA: sha, Policy: trusted, Nox: false,
	}); err != nil {
		t.Fatalf("Prove: %v", err)
	}
	if fake.Ran("nox") {
		t.Errorf("ran the scanner with prove.nox off: %s", fake.Transcript())
	}
}

func TestNoxRunsAfterTheGate(t *testing.T) {
	repo := gittest.New(t)
	sha := repo.Commit("first", "a.txt", "x\n")

	fake := execx.NewFake()
	fake.On("git", execx.Response{Fn: func(c execx.Cmd) (execx.Result, error) {
		return execx.NewSystem().Run(t.Context(), c)
	}})

	if err := NewWarden(fake, "warden", "nox").Prove(t.Context(), Request{
		RepoDir: repo.Dir, SHA: sha, Policy: trusted, Nox: true,
	}); err != nil {
		t.Fatalf("Prove: %v", err)
	}

	lines := fake.Lines()
	gate := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "warden run") })
	scan := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "nox scan") })
	if gate < 0 || scan < 0 || gate > scan {
		t.Errorf("gate must run before the scan: %s", fake.Transcript())
	}
}

func TestNoxFindingsFailTheProve(t *testing.T) {
	fake := fakeRepo().On("nox scan", execx.Response{ExitCode: 1, Stderr: "1 critical finding"})

	err := NewWarden(fake, "warden", "nox").Prove(t.Context(), Request{
		RepoDir: "/repo", SHA: "abc123", Policy: trusted, Nox: true,
	})

	if !errors.Is(err, ErrGateFailed) {
		t.Fatalf("err = %v, want ErrGateFailed", err)
	}
}

func TestTrustedRunInheritsTheEnvironment(t *testing.T) {
	fake := fakeRepo()
	_ = NewWarden(fake, "warden", "nox").Prove(t.Context(), Request{
		RepoDir: "/repo", SHA: "abc123", Policy: trusted,
	})

	cmd := fake.Find("warden run pre-push")
	if cmd == nil {
		t.Fatalf("gate not run: %s", fake.Transcript())
	}
	// nil Env means inherit: warden needs its signing key, and a repository's
	// own checks may legitimately need credentials.
	if cmd.Env != nil {
		t.Errorf("a trusted run should inherit the environment, got %d explicit vars", len(cmd.Env))
	}
}

func TestForkRunGetsAScrubbedEnvironment(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_secret")
	t.Setenv("REGISTRY_PASSWORD", "hunter2")
	t.Setenv("KILN_TRUSTED_KEYS", "AAAA")
	t.Setenv("HOME", "/home/build")

	fake := fakeRepo()
	_ = NewWarden(fake, "warden", "nox").Prove(t.Context(), Request{
		RepoDir: "/repo", SHA: "abc123", Policy: untrusted,
	})

	cmd := fake.Find("warden run pre-push")
	if cmd == nil {
		t.Fatalf("gate not run: %s", fake.Transcript())
	}
	if cmd.Env == nil {
		t.Fatal("a fork run must get an explicit, scrubbed environment")
	}
	for _, kv := range cmd.Env {
		name, _, _ := strings.Cut(kv, "=")
		if execx.IsSecretVar(name) {
			t.Errorf("fork run can see a credential: %s", name)
		}
	}
	if !slices.Contains(cmd.Env, "HOME=/home/build") {
		t.Error("scrubbing removed an ordinary variable the build needs")
	}
	if !slices.Contains(cmd.Env, "KILN_ISOLATED=1") {
		t.Error("an isolated run should be able to tell that it is isolated")
	}
}

func TestForkScanAlsoRunsScrubbed(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_secret")

	fake := fakeRepo()
	_ = NewWarden(fake, "warden", "nox").Prove(t.Context(), Request{
		RepoDir: "/repo", SHA: "abc123", Policy: untrusted, Nox: true,
	})

	cmd := fake.Find("nox scan")
	if cmd == nil {
		t.Fatalf("scan not run: %s", fake.Transcript())
	}
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "GITHUB_TOKEN=") {
			t.Error("the scanner ran with a credential on a fork PR")
		}
	}
}

func TestProveRejectsAnEmptySHA(t *testing.T) {
	if err := NewWarden(fakeRepo(), "warden", "nox").Prove(t.Context(), Request{RepoDir: "/repo"}); err == nil {
		t.Error("Prove accepted an empty commit")
	}
}

func TestCustomBinariesAreHonoured(t *testing.T) {
	fake := fakeRepo()
	_ = NewWarden(fake, "warden-next", "nox-next").Prove(t.Context(), Request{
		RepoDir: "/repo", SHA: "abc123", Policy: trusted, Nox: true,
	})

	if !fake.Ran("warden-next run pre-push") || !fake.Ran("nox-next scan") {
		t.Errorf("KILN_WARDEN/KILN_NOX not honoured: %s", fake.Transcript())
	}
}

func TestEmptyBinaryNamesDefault(t *testing.T) {
	w := NewWarden(fakeRepo(), "", "")
	if w.WardenBin != "warden" || w.NoxBin != "nox" {
		t.Errorf("defaults = %q/%q", w.WardenBin, w.NoxBin)
	}
}

func TestFuncAdapter(t *testing.T) {
	called := false
	var p Prover = Func(func(_ context.Context, _ Request) error {
		called = true
		return nil
	})

	if err := p.Prove(t.Context(), Request{}); err != nil || !called {
		t.Errorf("Func adapter did not delegate (err=%v)", err)
	}
}
