package execx

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfinedChildCannotReadOutsideTheWorktree(t *testing.T) {
	if !LandlockAvailable() {
		t.Skip("Landlock is not available on this kernel")
	}

	work := t.TempDir()
	inside := filepath.Join(work, "inside.txt")
	if err := os.WriteFile(inside, []byte("ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A sibling under /tmp can share an overlay with the worktree on
	// some boxes. Put the secret on a different hierarchy.
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "secret.txt")
	if home, err := os.UserHomeDir(); err == nil && home != "" && !strings.HasPrefix(work, home) {
		outside = filepath.Join(home, "kiln-confine-secret.txt")
		t.Cleanup(func() { _ = os.Remove(outside) })
	}
	if err := os.WriteFile(outside, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Landlock denies open, not stat or access(2). `test -r` only looks
	// at Unix mode bits, so a confined child can still "see" a secret
	// that it cannot actually read. cat opens the file. Write the
	// copies into the worktree so a RO /dev cannot hide a grant bug.
	script := `echo confined=$KILN_CONFINED
if cat "$1" >inside.out 2>inside.err; then echo inside=yes; else echo inside=no; fi
if cat "$2" >outside.out 2>outside.err; then echo outside=yes; else echo outside=no; fi`

	res, err := NewSystem().Run(t.Context(), Cmd{
		Name:    "sh",
		Args:    []string{"-c", script, "confine-test", inside, outside},
		Dir:     work,
		Confine: work,
		Env:     []string{"PATH=" + os.Getenv("PATH")},
	})
	if err != nil {
		t.Fatalf("Run: %v\n%s\nwork=%s outside=%s", err, res.Stderr, work, outside)
	}
	insideErr, _ := os.ReadFile(filepath.Join(work, "inside.err"))
	t.Logf("work=%s outside=%s out=%q inside.err=%q", work, outside, res.Stdout, insideErr)
	if !strings.Contains(res.Stdout, "confined="+ConfinedLandlock) {
		t.Errorf("child must record that Landlock applied: %q", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "inside=yes") {
		t.Errorf("confined child could not read the worktree: %q err=%q", res.Stdout, insideErr)
	}
	if !strings.Contains(res.Stdout, "outside=no") {
		t.Errorf("confined child read a path outside the worktree: %q", res.Stdout)
	}
}

func TestApplyLandlockDeniesAForeignPath(t *testing.T) {
	if !LandlockAvailable() {
		t.Skip("Landlock is not available on this kernel")
	}
	if os.Getenv("KILN_LL_PROBE") == "1" {
		if err := applyLandlock(os.Getenv("KILN_LL_ROOT")); err != nil {
			t.Fatalf("apply: %v", err)
		}
		if _, err := os.ReadFile(os.Getenv("KILN_LL_INSIDE")); err != nil {
			t.Fatalf("inside: %v", err)
		}
		if _, err := os.ReadFile(os.Getenv("KILN_LL_SECRET")); err == nil {
			t.Fatal("secret was readable after Landlock")
		}
		return
	}

	root := t.TempDir()
	inside := filepath.Join(root, "in.txt")
	if err := os.WriteFile(inside, []byte("ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("no\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestApplyLandlockDeniesAForeignPath", "-test.v")
	// Coverage counters and MkdirTemp must land in the granted tree. The
	// parent `go test -cover` points both at /tmp; after restrict those
	// writes are permission denied and the probe exits 2.
	cmd.Env = overlayEnv(os.Environ(),
		"KILN_LL_PROBE=1",
		"KILN_LL_ROOT="+root,
		"KILN_LL_INSIDE="+inside,
		"KILN_LL_SECRET="+secret,
		"TMPDIR="+root,
		"TMP="+root,
		"GOCOVERDIR="+root,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("probe: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "PASS") {
		t.Fatalf("probe output:\n%s", out)
	}
}

func TestOverlayEnvReplacesRatherThanAppends(t *testing.T) {
	got := overlayEnv([]string{"HOME=/op", "PATH=/bin", "HOME=/dup"}, "HOME=/work", "TMPDIR=/work")
	want := []string{"PATH=/bin", "HOME=/work", "TMPDIR=/work"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("overlayEnv = %q, want %q", got, want)
	}
}

func TestConfineOffRunsUnconfined(t *testing.T) {
	t.Setenv("KILN_CONFINE", "off")
	outside := filepath.Join(t.TempDir(), "visible.txt")
	if err := os.WriteFile(outside, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()

	res, err := NewSystem().Run(t.Context(), Cmd{
		Name:    "sh",
		Args:    []string{"-c", "test -r " + outside + " && printf yes"},
		Dir:     work,
		Confine: work,
		Env:     []string{"PATH=" + os.Getenv("PATH")},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Stdout != "yes" {
		t.Errorf("KILN_CONFINE=off must not restrict the child: %q", res.Stdout)
	}
}

func TestRequiredConfineAppliesWhenAvailable(t *testing.T) {
	if !LandlockAvailable() {
		t.Skip("needs a Landlock kernel")
	}
	t.Setenv("KILN_CONFINE", "required")
	work := t.TempDir()
	res, err := NewSystem().Run(t.Context(), Cmd{
		Name:    "sh",
		Args:    []string{"-c", "printf %s \"$KILN_CONFINED\""},
		Dir:     work,
		Confine: work,
		Env:     []string{"PATH=" + os.Getenv("PATH")},
	})
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, res.Stderr)
	}
	if res.Stdout != ConfinedLandlock {
		t.Errorf("KILN_CONFINE=required produced %q, want %s", res.Stdout, ConfinedLandlock)
	}
}

func TestRequiredConfineFailsWhenUnavailable(t *testing.T) {
	if LandlockAvailable() {
		t.Skip("this kernel has Landlock; the refusal path needs one that does not")
	}
	t.Setenv("KILN_CONFINE", "required")
	_, err := NewSystem().Run(t.Context(), Cmd{
		Name: "true", Confine: t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("err = %v, want a required-but-unavailable refusal", err)
	}
}
