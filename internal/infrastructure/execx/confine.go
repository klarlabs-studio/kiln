package execx

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
)

const (
	confineRootEnv = "KILN_INTERNAL_CONFINE_ROOT"
	confineCmdEnv  = "KILN_INTERNAL_CONFINE_CMD"
	confineArgsEnv = "KILN_INTERNAL_CONFINE_ARGS"
	// ConfinedEnv is set on a child that actually entered Landlock.
	// Tests and a repository's own checks can read it. Absence means
	// the kernel restriction was not applied.
	ConfinedEnv      = "KILN_CONFINED"
	ConfinedLandlock = "landlock"
)

func init() {
	if os.Getenv(confineRootEnv) == "" {
		return
	}
	enterConfine()
}

// enterConfine applies Landlock and execs the real command. It never
// returns: the process is either the confined child or it exits.
func enterConfine() {
	// landlock_restrict_self is per-thread. Pin this goroutine so apply
	// and Exec share an OS thread when AllThreadsSyscall is unavailable
	// (cgo binaries return ENOTSUP and only the current thread is confined).
	runtime.LockOSThread()

	root := os.Getenv(confineRootEnv)
	name := os.Getenv(confineCmdEnv)
	var args []string
	if raw := os.Getenv(confineArgsEnv); raw != "" {
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			fmt.Fprintf(os.Stderr, "kiln: confine: decode args: %v\n", err)
			os.Exit(78)
		}
	}
	if err := applyLandlock(root); err != nil {
		fmt.Fprintf(os.Stderr, "kiln: confine: %v\n", err)
		os.Exit(78)
	}
	env := stripConfineEnv(os.Environ())
	env = append(env, ConfinedEnv+"="+ConfinedLandlock)
	argv := append([]string{name}, args...)
	if err := syscall.Exec(name, argv, env); err != nil {
		fmt.Fprintf(os.Stderr, "kiln: confine exec %s: %v\n", name, err)
		os.Exit(78)
	}
}

// overlayEnv copies env, drops any key that extra sets, then appends extra.
// Linux getenv uses the first match; appending a second HOME would leave
// the operator's value in force on a confined child.
func overlayEnv(env []string, extra ...string) []string {
	drop := make(map[string]struct{}, len(extra))
	for _, kv := range extra {
		name, _, _ := strings.Cut(kv, "=")
		drop[name] = struct{}{}
	}
	out := make([]string, 0, len(env)+len(extra))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if _, skip := drop[name]; skip {
			continue
		}
		out = append(out, kv)
	}
	return append(out, extra...)
}

func stripConfineEnv(in []string) []string {
	out := make([]string, 0, len(in))
	for _, kv := range in {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case confineRootEnv, confineCmdEnv, confineArgsEnv:
			continue
		}
		out = append(out, kv)
	}
	return out
}

func confineMode() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("KILN_CONFINE"))) {
	case "0", "off", "false", "no":
		return "off"
	case "required":
		return "required"
	default:
		return "auto"
	}
}

// maybeConfine either runs the command under Landlock or explains why it
// will not. confined is true only when the kernel restriction was used.
func (s System) maybeConfine(ctx context.Context, c Cmd) (confined bool, res Result, err error) {
	switch confineMode() {
	case "off":
		return false, Result{}, nil
	case "required":
		if !LandlockAvailable() {
			return false, Result{}, fmt.Errorf("execx: KILN_CONFINE=required but Landlock is not available")
		}
	default:
		if !LandlockAvailable() {
			return false, Result{}, nil
		}
	}
	res, err = s.runConfined(ctx, c)
	return true, res, err
}

func (s System) runConfined(ctx context.Context, c Cmd) (Result, error) {
	resolved, err := exec.LookPath(c.Name)
	if err != nil {
		return Result{}, &NotFoundError{Name: c.Name}
	}
	exe, err := os.Executable()
	if err != nil {
		return Result{}, fmt.Errorf("execx: confine: resolve this process: %w", err)
	}
	payload, err := json.Marshal(c.Args)
	if err != nil {
		return Result{}, fmt.Errorf("execx: confine: encode args: %w", err)
	}

	env := c.Env
	if env == nil {
		env = os.Environ()
	}
	// Replace, do not append: Linux getenv keeps the first HOME/TMPDIR.
	// GOCOVERDIR belongs in the worktree too — an instrumented trampoline
	// (go test -cover) otherwise writes counters to /tmp after restrict
	// and dies with permission denied. Production kiln is not covered.
	env = overlayEnv(env,
		confineRootEnv+"="+c.Confine,
		confineCmdEnv+"="+resolved,
		confineArgsEnv+"="+string(payload),
		"TMPDIR="+c.Confine,
		"TMP="+c.Confine,
		"HOME="+c.Confine,
		"GOCOVERDIR="+c.Confine,
	)

	inner := Cmd{
		Name:   exe,
		Dir:    c.Dir,
		Env:    env,
		Stdin:  c.Stdin,
		Stdout: c.Stdout,
		Stderr: c.Stderr,
	}
	return s.runUnconfined(ctx, inner)
}

// runUnconfined is the ordinary fork/exec, used by the trampoline spawn
// so we cannot recurse into maybeConfine.
func (s System) runUnconfined(ctx context.Context, c Cmd) (Result, error) {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...) //nolint:gosec // trampoline is this process
	cmd.Dir = c.Dir
	cmd.Env = c.Env
	cmd.Stdin = c.Stdin
	return finish(cmd, c)
}
