// Package cli is Kiln's primary surface.
//
// The engine runs in-process and the process exits. There is no daemon to keep
// alive, no runner to register and no queue to drain: `kiln run` is one build,
// `kiln watch --once` is one cron tick. That is a deliberate operational
// choice — the cheapest thing to run reliably is a program that finishes.
//
// Two conventions hold throughout. Structured logs go to stderr, always, so
// `kiln mcp serve` can own stdout for JSON-RPC. And every long-running command
// is bound to a signal-cancelled context, so Ctrl-C tears down worktrees
// instead of leaking them.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"go.klarlabs.de/kiln/internal/version"
)

// ExitCode communicates the outcome to a shell and to cron.
//
// The split matters for automation: a gate that rejected a change (2) is a
// normal, expected outcome that a human must look at, while a misconfigured
// pipeline (3) is something a machine should alert on. Collapsing both into 1
// would make "the build is broken" indistinguishable from "the code is wrong".
const (
	ExitOK     = 0
	ExitError  = 1
	ExitFailed = 2
	ExitConfig = 3
	ExitUsage  = 64
)

// IO is where a command reads and writes. Tests substitute buffers.
type IO struct {
	Out io.Writer
	Err io.Writer
}

// Stdio is the process's own streams.
func Stdio() IO { return IO{Out: os.Stdout, Err: os.Stderr} }

// printf, print and errf write a command's human-readable output.
//
// They drop the write error, deliberately and in one place rather than at
// several dozen call sites. There is nowhere left to report a failed write to
// stdout — and more to the point, a run that built and signed a correct
// artifact must not be recorded as failed because the operator piped it
// through `head`. The artifact is the deliverable; this text is the narration.
func (o IO) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(o.Out, format, args...)
}

func (o IO) print(s string) { _, _ = fmt.Fprint(o.Out, s) }

func (o IO) errf(format string, args ...any) {
	_, _ = fmt.Fprintf(o.Err, format, args...)
}

const usage = `kiln — signed-artifact factory

Warden proves a commit. Kiln turns that commit into a signed container image.
RollOps is the only thing allowed to ship it.

Usage:
  kiln version                          print version, commit and build date
  kiln doctor [--config-only]           validate configuration and toolchain; run nothing
  kiln run --sha S --event E [flags]    build one commit
  kiln watch [--once | --every D]       discover and build new refs
  kiln poll [--once | --every D]        watch, restricted to the tracked branch
  kiln status [run-id]                  show the latest run, or a named one
  kiln verify <ref> [--key k]           check a published artifact's whole chain
  kiln mcp serve                        MCP server over stdio

Run flags:
  --sha S        commit to build; "HEAD" and other commit-ish values are resolved
  --event E      pull_request, push or tag
  --fork         treat the head as untrusted (implied when no GITHUB_TOKEN is set)
  --ref R        ref the commit was found on, e.g. refs/heads/main
  --dir D        repository directory (default: the working directory)
  --pipeline P   pipeline file (default: <dir>/.kiln.yaml)

Environment:
  KILN_DB              run ledger path (default .kiln/state.json)
  KILN_DRY=1           plan tags; call neither docker nor cosign
  KILN_WARDEN          warden binary name
  KILN_NOX             nox binary name
  KILN_TRUSTED_KEYS    signer keys that permit a provenance skip
  GITHUB_TOKEN         checks and pull request lookup
  KILN_LOG_LEVEL       debug, info, warn or error

Verify flags:
  --key K        cosign public key; omit for keyless
  --identity I   certificate identity to require, with --issuer
  --dir D        local clone, so the warden note on the source commit is read

Kiln never deploys. Deployment belongs to RollOps.
`

// Main dispatches one command and returns a process exit code.
func Main(ctx context.Context, args []string, io IO) int {
	if len(args) == 0 {
		io.errf("%s", usage)
		return ExitUsage
	}

	command, rest := args[0], args[1:]
	switch command {
	case "version", "--version", "-v":
		io.print(version.String() + "\n")
		return ExitOK

	case "help", "--help", "-h":
		io.print(usage)
		return ExitOK

	case "doctor":
		return report(io, runDoctor(ctx, rest, io))
	case "run":
		return report(io, runRun(ctx, rest, io))
	case "watch":
		return report(io, runWatch(ctx, rest, io, false))
	case "poll":
		return report(io, runWatch(ctx, rest, io, true))
	case "status":
		return report(io, runStatus(ctx, rest, io))
	case "verify":
		return report(io, runVerify(ctx, rest, io))
	case "mcp":
		return report(io, runMCP(ctx, rest, io))

	default:
		io.errf("kiln: unknown command %q\n\n", command)
		io.errf("%s", usage)
		return ExitUsage
	}
}

// exitError carries a specific exit code out of a command.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func failWith(code int, format string, args ...any) error {
	return &exitError{code: code, err: fmt.Errorf(format, args...)}
}

// wrapExit tags an existing error with an exit code, preserving its chain.
func wrapExit(code int, err error) error {
	if err == nil {
		return nil
	}
	return &exitError{code: code, err: err}
}

// report turns a command's error into an exit code and a message.
//
// Cancellation is not a failure: Ctrl-C on a watch loop is how an operator
// stops it, and printing "error: context canceled" would suggest something
// went wrong.
func report(io IO, err error) int {
	switch {
	case err == nil:
		return ExitOK
	case errors.Is(err, context.Canceled):
		return ExitOK
	}

	io.errf("kiln: %v\n", err)

	var exit *exitError
	if errors.As(err, &exit) {
		return exit.code
	}
	return ExitError
}

// newFlagSet builds a flag set that reports usage errors through the command's
// own stream rather than the global one.
func newFlagSet(name string, io IO) *flag.FlagSet {
	fs := flag.NewFlagSet("kiln "+name, flag.ContinueOnError)
	fs.SetOutput(io.Err)
	return fs
}

// parseInterval reads a --every value.
func parseInterval(s string) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("--every %q is not a duration (try 1m, 30s, 5m): %w", s, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("--every must be positive, got %s", d)
	}
	return d, nil
}
