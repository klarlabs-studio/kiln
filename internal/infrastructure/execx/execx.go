// Package execx is Kiln's subprocess seam.
//
// Kiln is, at heart, a program that runs other programs: git, warden, nox,
// docker, cosign. Routing all of that through one narrow interface buys three
// things — unit tests that never fork a process, one place where a missing
// binary produces the same clear message, and one place where the environment
// handed to an untrusted child is scrubbed.
package execx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Cmd is one subprocess invocation.
type Cmd struct {
	// Name is the binary, resolved through PATH.
	Name string
	Args []string
	// Dir is the working directory. Empty means the caller's own, which is
	// almost always wrong for Kiln — prove and publish both run inside a
	// disposable worktree.
	Dir string
	// Env replaces the child's whole environment when non-nil. Nil inherits
	// the parent's. Fork-PR work passes Scrub(os.Environ()).
	Env []string
	// Stdin, when set, is piped to the child.
	Stdin io.Reader
	// Stdout and Stderr, when set, additionally receive the streams live. The
	// captured copies in Result are produced either way, so a caller can both
	// stream progress to a terminal and inspect the output afterwards.
	Stdout io.Writer
	Stderr io.Writer
}

// String renders the command for logs and error messages. It prints arguments
// verbatim; nothing Kiln passes on a command line is a secret (credentials
// reach docker and cosign through the environment and the keychain), so there
// is nothing here to redact.
func (c Cmd) String() string {
	if len(c.Args) == 0 {
		return c.Name
	}
	args := make([]string, len(c.Args))
	for i, a := range c.Args {
		args[i] = redactKeyMaterial(a)
	}
	return c.Name + " " + strings.Join(args, " ")
}

// redactKeyMaterial replaces an argument that is a private key with a marker.
//
// This rendering is what reaches the retry warnings, the publish error, stderr
// and the `error` field of the run record in .kiln/state.json — a git-tracked
// file. An operator who set KILN_COSIGN_KEY to a PEM body rather than a path
// had the key written to all four, one `git add -A` from being committed (#56).
//
// envconfig.ValidateCosignKey refuses that value before anything runs, which
// is the better fix because it keeps the material out of the process
// entirely. This is the second line: it covers any path that does not go
// through Load — a library caller passing Options.Env, a future flag — and
// costs nothing.
//
// Only material is redacted, not references. A file path, a KMS URI or a
// warden key fingerprint is not secret, and blanking them would remove the
// evidence that the RIGHT key was used, which several tests check and an
// operator debugging a signing failure needs.
func redactKeyMaterial(arg string) string {
	trimmed := strings.TrimSpace(arg)
	for _, p := range []string{"-----BEGIN", "LS0tLS1CRUdJTi"} {
		if strings.HasPrefix(trimmed, p) {
			return "[REDACTED key material]"
		}
	}
	return arg
}

// Result is what a finished subprocess left behind.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Output is the trimmed stdout, which is what every caller that reads a value
// out of a command actually wants.
func (r Result) Output() string { return strings.TrimSpace(r.Stdout) }

// Runner executes commands.
type Runner interface {
	// Run executes c to completion. A non-zero exit is reported as an
	// *ExitError, not as a bare Result: callers that care about the
	// distinction use errors.As, and callers that do not get a failure by
	// default. That is the fail-closed default this codebase wants — a
	// forgotten error check must never read as success.
	Run(ctx context.Context, c Cmd) (Result, error)
	// LookPath reports whether a binary is on PATH.
	LookPath(name string) (string, error)
}

// ExitError reports a command that ran and failed.
type ExitError struct {
	Cmd      string
	Code     int
	Stderr   string
	Combined string
}

func (e *ExitError) Error() string {
	msg := fmt.Sprintf("%s: exit %d", e.Cmd, e.Code)
	if detail := strings.TrimSpace(e.Stderr); detail != "" {
		msg += ": " + lastLines(detail, 5)
	}
	return msg
}

// ExitCode extracts a subprocess exit code from err, reporting ok=false if err
// is not a subprocess failure. Provenance verification uses this: `warden
// verify` communicates its verdict through the exit code, so "exited 1" and
// "could not be executed" must not be confused.
func ExitCode(err error) (int, bool) {
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code, true
	}
	return 0, false
}

// NotFoundError reports a binary that is not installed. Kiln turns this into a
// phase failure rather than a skip: a missing `warden` means the gate did not
// run, and a gate that did not run has not passed.
type NotFoundError struct {
	Name string
	Hint string
}

func (e *NotFoundError) Error() string {
	msg := fmt.Sprintf("%s not found on PATH", e.Name)
	if e.Hint != "" {
		msg += ": " + e.Hint
	}
	return msg
}

// System is the real Runner.
type System struct{}

// NewSystem returns a Runner backed by os/exec.
func NewSystem() System { return System{} }

func (System) LookPath(name string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", &NotFoundError{Name: name}
	}
	return path, nil
}

func (s System) Run(ctx context.Context, c Cmd) (Result, error) {
	if c.Name == "" {
		return Result{}, errors.New("execx: no command")
	}
	if _, err := s.LookPath(c.Name); err != nil {
		return Result{}, err
	}

	cmd := exec.CommandContext(ctx, c.Name, c.Args...) //nolint:gosec // the binary set is fixed by Kiln, not by repo content
	cmd.Dir = c.Dir
	cmd.Env = c.Env
	cmd.Stdin = c.Stdin

	var stdout, stderr bytes.Buffer
	cmd.Stdout = tee(&stdout, c.Stdout)
	cmd.Stderr = tee(&stderr, c.Stderr)

	err := cmd.Run()
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if err == nil {
		return res, nil
	}

	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.ExitCode = ee.ExitCode()
		return res, &ExitError{
			Cmd:      c.String(),
			Code:     res.ExitCode,
			Stderr:   res.Stderr,
			Combined: res.Stdout + res.Stderr,
		}
	}
	// Context cancellation and start failures land here. They are not verdicts
	// about the command's subject, so they must not be reported as one.
	return res, fmt.Errorf("%s: %w", c.String(), err)
}

func tee(capture *bytes.Buffer, live io.Writer) io.Writer {
	if live == nil {
		return capture
	}
	return io.MultiWriter(capture, live)
}

// secretMarkers are substrings that identify a credential-bearing variable by
// convention. A denylist rather than an allowlist is a deliberate trade: an
// allowlist would be tighter, but it would also break every build that needs
// an ordinary variable Kiln has never heard of, and an operator who works
// around the isolation is worse off than one whose unusual secret name slipped
// through. The explicit names below cover the ones Kiln itself introduces.
var secretMarkers = []string{
	"TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL", "APIKEY", "API_KEY",
	"PRIVATE_KEY", "ACCESS_KEY", "SESSION_KEY", "AUTH",
}

// secretNames are dropped whole, either because they do not match a marker or
// because they are worth naming for the reader.
var secretNames = map[string]bool{
	"GITHUB_TOKEN":        true,
	"GH_TOKEN":            true,
	"KILN_TOKEN":          true,
	"KILN_WEBHOOK_SECRET": true,
	// The trusted-key list is not itself a secret, but a fork head that can
	// read it learns exactly which signature to try to forge.
	"KILN_TRUSTED_KEYS": true,
	// Agent forwarding is a live credential, not a value.
	"SSH_AUTH_SOCK": true,
	// Registry and signing material.
	"DOCKER_AUTH_CONFIG":             true,
	"DOCKER_CONFIG":                  true,
	"COSIGN_KEY":                     true,
	"COSIGN_PRIVATE_KEY":             true,
	"REGISTRY_USERNAME":              true,
	"REGISTRY_PASSWORD":              true,
	"AWS_ACCESS_KEY_ID":              true,
	"AWS_SECRET_ACCESS_KEY":          true,
	"AWS_SESSION_TOKEN":              true,
	"GOOGLE_APPLICATION_CREDENTIALS": true,
	"AZURE_CLIENT_SECRET":            true,
	"NPM_TOKEN":                      true,
}

// Scrub removes credential-bearing variables from an environment.
//
// This is what "fork PRs never see secrets" means in practice. The prove phase
// executes code authored on the pull request head; whatever that code does, it
// must not be able to read a registry password out of its own environment and
// exfiltrate it.
func Scrub(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if IsSecretVar(name) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// IsSecretVar reports whether a variable name looks like a credential.
func IsSecretVar(name string) bool {
	upper := strings.ToUpper(name)
	if secretNames[upper] {
		return true
	}
	for _, marker := range secretMarkers {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return false
}

// ScrubbedEnviron is Scrub applied to this process's environment.
func ScrubbedEnviron() []string { return Scrub(os.Environ()) }

// lastLines keeps the tail of a subprocess's output. Build tools are verbose
// and the actionable line is nearly always the last one, so an error that
// quoted everything would bury the answer it is trying to give.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "; ")
	}
	return "… " + strings.Join(lines[len(lines)-n:], "; ")
}
