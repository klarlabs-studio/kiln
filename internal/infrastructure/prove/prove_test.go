package prove

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"go.klarlabs.de/kiln/internal/application/ports"

	"go.klarlabs.de/kiln/internal/domain/isolation"
	"go.klarlabs.de/kiln/internal/gittest"
	"go.klarlabs.de/kiln/internal/infrastructure/execx"
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

	if err := NewWarden(fake, "warden", "nox").Prove(t.Context(), ports.ProveRequest{
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

	err := NewWarden(fake, "warden", "nox").Prove(t.Context(), ports.ProveRequest{
		RepoDir: "/repo", SHA: "abc123", Policy: trusted,
	})

	// "your change did not pass" must not read the same as "kiln broke".
	if !errors.Is(err, ports.ErrGateFailed) {
		t.Fatalf("err = %v, want ports.ErrGateFailed", err)
	}
	if !strings.Contains(err.Error(), "lint failed") {
		t.Errorf("error should quote the gate, got %v", err)
	}
}

func TestMissingWardenIsAFailureNotASkip(t *testing.T) {
	fake := fakeRepo().Absent("warden")

	err := NewWarden(fake, "warden", "nox").Prove(t.Context(), ports.ProveRequest{
		RepoDir: "/repo", SHA: "abc123", Policy: trusted,
	})

	if !errors.Is(err, ports.ErrToolMissing) {
		t.Fatalf("err = %v, want ports.ErrToolMissing", err)
	}
	// The check must happen before the checkout, so the operator learns
	// immediately rather than after a clone.
	if fake.Ran("git worktree add") {
		t.Errorf("checked out a tree before noticing the missing gate: %s", fake.Transcript())
	}
}

func TestNoxEnabledButMissingIsAFailure(t *testing.T) {
	fake := fakeRepo().Absent("nox")

	err := NewWarden(fake, "warden", "nox").Prove(t.Context(), ports.ProveRequest{
		RepoDir: "/repo", SHA: "abc123", Policy: trusted, Nox: true,
	})

	if !errors.Is(err, ports.ErrToolMissing) {
		t.Fatalf("err = %v, want ports.ErrToolMissing", err)
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

	if err := NewWarden(fake, "warden", "nox").Prove(t.Context(), ports.ProveRequest{
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

	if err := NewWarden(fake, "warden", "nox").Prove(t.Context(), ports.ProveRequest{
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

	err := NewWarden(fake, "warden", "nox").Prove(t.Context(), ports.ProveRequest{
		RepoDir: "/repo", SHA: "abc123", Policy: trusted, Nox: true,
	})

	if !errors.Is(err, ports.ErrGateFailed) {
		t.Fatalf("err = %v, want ports.ErrGateFailed", err)
	}
}

func TestTrustedRunInheritsTheEnvironment(t *testing.T) {
	fake := fakeRepo()
	_ = NewWarden(fake, "warden", "nox").Prove(t.Context(), ports.ProveRequest{
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
	_ = NewWarden(fake, "warden", "nox").Prove(t.Context(), ports.ProveRequest{
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
	_ = NewWarden(fake, "warden", "nox").Prove(t.Context(), ports.ProveRequest{
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
	if err := NewWarden(fakeRepo(), "warden", "nox").Prove(t.Context(), ports.ProveRequest{RepoDir: "/repo"}); err == nil {
		t.Error("Prove accepted an empty commit")
	}
}

func TestCustomBinariesAreHonoured(t *testing.T) {
	fake := fakeRepo()
	_ = NewWarden(fake, "warden-next", "nox-next").Prove(t.Context(), ports.ProveRequest{
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
	var p ports.Prover = ports.ProveFunc(func(_ context.Context, _ ports.ProveRequest) error {
		called = true
		return nil
	})

	if err := p.Prove(t.Context(), ports.ProveRequest{}); err != nil || !called {
		t.Errorf("ports.ProveFunc adapter did not delegate (err=%v)", err)
	}
}

// The gate must never be invoked in its pushing form. `warden run pre-push`
// without --attest-only fast-forwards and pushes the branch; from a build box
// that is a write nobody asked for, and in a detached worktree it aborts with
// "branch changed mid-run". Warden's CI mode is the only correct invocation.
func TestGateIsInvokedInAttestOnlyMode(t *testing.T) {
	fake := fakeRepo()
	_ = NewWarden(fake, "warden", "nox").Prove(t.Context(), ports.ProveRequest{
		RepoDir: "/repo", SHA: "abc123", Policy: trusted,
	})

	cmd := fake.Find("warden run pre-push")
	if cmd == nil {
		t.Fatalf("gate not run: %s", fake.Transcript())
	}
	if !slices.Contains(cmd.Args, "--attest-only") {
		t.Errorf("kiln invoked the pushing form of the gate: %s", cmd.String())
	}
}

func TestGateRunsWithNoStdin(t *testing.T) {
	// warden's pre-push path reads a push payload from stdin and decides
	// whether there is anything to gate. Kiln gives it none; warden treats
	// "no refs at all" as gatable, so this must stay a real run rather than a
	// silent pass. Asserting kiln sends nothing keeps that contract visible.
	fake := fakeRepo()
	_ = NewWarden(fake, "warden", "nox").Prove(t.Context(), ports.ProveRequest{
		RepoDir: "/repo", SHA: "abc123", Policy: trusted,
	})

	if cmd := fake.Find("warden run pre-push"); cmd.Stdin != nil {
		t.Error("kiln fed the gate a push payload it did not construct")
	}
}

// Warden's exit codes are a sysexits(3) contract, not a boolean. Collapsing
// them cost a real box a red "gate failed" on a healthy main: the gate had not
// rejected anything, it had never run.
func TestAnEnvironmentFailureIsNotAGateFailure(t *testing.T) {
	fake := fakeRepo().On("warden run pre-push", execx.Response{
		ExitCode: 78,
		Stderr:   "step test could not run: its toolchain or dependencies are not installed",
	})

	err := NewWarden(fake, "warden", "nox").Prove(t.Context(), ports.ProveRequest{
		RepoDir: "/repo", SHA: "abc123", Policy: trusted,
	})

	if errors.Is(err, ports.ErrGateFailed) {
		// This is the bug: it tells the author their change is bad when
		// nothing looked at it.
		t.Error("exit 78 reported as a gate failure; warden said it could not run")
	}
	if !errors.Is(err, ports.ErrToolMissing) {
		t.Fatalf("err = %v, want ports.ErrToolMissing", err)
	}
}

// 75 is EX_TEMPFAIL: another process held a machine-global lock. Nothing is
// wrong with the change and nothing is missing from the box.
func TestContentionIsNotAGateFailure(t *testing.T) {
	fake := fakeRepo().On("warden run pre-push", execx.Response{
		ExitCode: 75,
		Stderr:   "another process holds the lock",
	})

	err := NewWarden(fake, "warden", "nox").Prove(t.Context(), ports.ProveRequest{
		RepoDir: "/repo", SHA: "abc123", Policy: trusted,
	})

	if errors.Is(err, ports.ErrGateFailed) {
		t.Error("exit 75 reported as a gate failure; the gate never ran")
	}
	if !errors.Is(err, ports.ErrGateUnavailable) {
		t.Fatalf("err = %v, want ports.ErrGateUnavailable", err)
	}
}

// The ordinary rejection must keep reading as one.
func TestAGateRejectionStillReadsAsOne(t *testing.T) {
	fake := fakeRepo().On("warden run pre-push", execx.Response{ExitCode: 1, Stderr: "lint failed"})

	err := NewWarden(fake, "warden", "nox").Prove(t.Context(), ports.ProveRequest{
		RepoDir: "/repo", SHA: "abc123", Policy: trusted,
	})

	if !errors.Is(err, ports.ErrGateFailed) {
		t.Fatalf("err = %v, want ports.ErrGateFailed", err)
	}
	if errors.Is(err, ports.ErrToolMissing) || errors.Is(err, ports.ErrGateUnavailable) {
		t.Error("a genuine rejection was excused as an environment problem")
	}
}
