package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"

	"go.klarlabs.de/kiln/internal/boot"
	"go.klarlabs.de/kiln/internal/domain/isolation"
	"go.klarlabs.de/kiln/internal/domain/run"
	"go.klarlabs.de/kiln/internal/engine"
	"go.klarlabs.de/kiln/internal/lock"
	"go.klarlabs.de/kiln/internal/prove"
	"go.klarlabs.de/kiln/internal/publish"
	"go.klarlabs.de/kiln/internal/worktree"
)

// runRun builds one commit.
func runRun(ctx context.Context, args []string, io IO) error {
	fs := newFlagSet("run", io)
	sha := fs.String("sha", "", `commit to build; "HEAD" and other commit-ish values are resolved`)
	event := fs.String("event", "", "pull_request, push or tag")
	fork := fs.Bool("fork", false, "treat the head as untrusted")
	ref := fs.String("ref", "", "ref the commit was found on, e.g. refs/heads/main")
	dir := fs.String("dir", "", "repository directory")
	pipelinePath := fs.String("pipeline", "", "pipeline file (default <dir>/.kiln.yaml)")
	pr := fs.Int("pr", 0, "pull request number; resolves --fork from the api when a token is present")
	quiet := fs.Bool("quiet", false, "do not stream subprocess output")
	if err := fs.Parse(args); err != nil {
		return wrapExit(ExitUsage, err)
	}

	if strings.TrimSpace(*sha) == "" {
		return failWith(ExitUsage, "--sha is required (use --sha HEAD for the current commit)")
	}
	parsedEvent, ok := isolation.ParseEvent(*event)
	if !ok {
		return failWith(ExitUsage, "--event must be pull_request, push or tag, got %q", *event)
	}

	deps, err := boot.Build(ctx, boot.Options{
		Dir:          *dir,
		PipelinePath: *pipelinePath,
		Output:       quietOr(*quiet, os.Stderr),
	})
	if err != nil {
		return wrapExit(ExitConfig, err)
	}

	resolved, err := worktree.ResolveSHA(ctx, deps.Runner, deps.Dir, *sha)
	if err != nil {
		return wrapExit(ExitConfig, err)
	}

	// A one-shot run was asked for explicitly, so a busy repository is a
	// refusal rather than a shrug: the operator wants to know their command
	// did not happen.
	return withRepoLock(deps.Dir, "kiln run --sha "+run.ShortSHA(resolved), busyRefusal,
		func() error {
			return executeRun(ctx, deps, io, parsedEvent, resolved,
				resolveFork(ctx, deps, parsedEvent, *fork, *pr),
				defaultRef(*ref, parsedEvent, *pr))
		})
}

// executeRun is the body of a locked run.
func executeRun(
	ctx context.Context, deps *boot.Deps, io IO,
	event isolation.Event, sha string, fork bool, ref string,
) error {
	r, execErr := deps.Engine.Execute(ctx, engine.Request{
		SHA:      sha,
		Event:    event,
		Fork:     fork,
		Ref:      ref,
		Repo:     repoName(deps),
		Dir:      deps.Dir,
		Pipeline: deps.Pipeline,
		Output:   deps.Output(),
	})

	printRun(io, r)
	return classify(execErr)
}

// busyRefusal is the shared "somebody else has it" answer for the commands
// that were asked for explicitly and must not silently do nothing.
func busyRefusal(h lock.Holder) error {
	return failWith(ExitBusy, "%v: %s", lock.ErrBusy, h)
}

// resolveFork decides the fork flag.
//
// The flag is a floor, never a ceiling: --fork forces untrusted, and nothing —
// not the API, not the absence of a token — can turn it back off. An operator
// who has said "treat this as hostile" must be obeyed.
func resolveFork(ctx context.Context, deps *boot.Deps, event isolation.Event, flagFork bool, pr int) bool {
	if flagFork {
		return true
	}
	if event != isolation.EventPullRequest {
		return false
	}
	if pr > 0 {
		return deps.ResolvePullFork(ctx, pr)
	}
	if !deps.ChecksEnabled() {
		// A pull request that cannot be identified is a pull request that
		// cannot be trusted.
		deps.Log.Warn("no github token: treating this pull request as a fork",
			"effect", "no secrets, no publish, no provenance skip")
		return boot.ForkUnknown
	}
	// A token exists but no number was given, so there is nothing to look up.
	// Same reasoning: unknown means untrusted.
	deps.Log.Warn("no --pr number: treating this pull request as a fork",
		"hint", "pass --pr N to let kiln resolve fork status from the api")
	return boot.ForkUnknown
}

// defaultRef supplies a ref when the caller omitted one. The ref decides the
// semver tag and scopes watch's already-built check, so leaving it empty
// quietly changes behaviour rather than failing.
func defaultRef(ref string, event isolation.Event, pr int) string {
	if ref != "" {
		return ref
	}
	if event == isolation.EventPullRequest && pr > 0 {
		return "refs/pull/" + strconv.Itoa(pr) + "/head"
	}
	return ""
}

func repoName(deps *boot.Deps) string {
	if deps.RepoErr != nil {
		return ""
	}
	return deps.Repo.String()
}

// quietOr returns the stream to forward subprocess output to, or nothing.
//
// The return type is io.Writer, not *os.File, and that matters: a nil
// *os.File assigned into an io.Writer is a *non-nil* interface holding a nil
// pointer, so every "is output configured?" check downstream says yes and the
// first byte written fails with "invalid argument". Returning an untyped nil
// keeps `--quiet` genuinely quiet.
func quietOr(quiet bool, w io.Writer) io.Writer {
	if quiet {
		return nil
	}
	return w
}

// classify maps a run failure onto an exit code.
//
// A rejected change and a broken toolchain are different problems for
// different people: the first needs a developer to fix their code, the second
// needs an operator to fix a machine. Cron can act on the difference.
func classify(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, engine.ErrPhaseTimeout):
		// A machine problem, not a code problem: the same bucket as a missing
		// toolchain, so an operator alerting on 3 hears about it.
		return wrapExit(ExitConfig, err)
	case errors.Is(err, prove.ErrGateFailed):
		return wrapExit(ExitFailed, err)
	case errors.Is(err, prove.ErrToolMissing), errors.Is(err, publish.ErrToolMissing):
		return wrapExit(ExitConfig, err)
	default:
		return wrapExit(ExitError, err)
	}
}

// printRun renders the outcome as a short human summary. Machine-readable
// detail lives in the JSON log on stderr and in the ledger.
func printRun(io IO, r *run.Run) {
	if r == nil {
		return
	}
	io.printf("run     %s\n", r.ID)
	io.printf("commit  %s", run.ShortSHA(r.SHA))
	if r.Ref != "" {
		io.printf(" on %s", r.Ref)
	}
	io.printf("\nevent   %s", r.Event)
	if r.Fork {
		io.print(" (fork: no secrets, no publish)")
	}
	io.printf("\nphase   %s\n", r.Phase)

	if r.Skipped {
		io.print("prove   satisfied by a trusted warden note\n")
	}
	if r.Digest != "" {
		io.printf("digest  %s\n", r.Digest)
		for _, tag := range r.Tags {
			io.printf("tag     %s\n", tag)
		}
	}
	if r.Error != "" {
		io.printf("error   %s\n", r.Error)
	}
	io.printf("took    %s\n", r.Duration().Round(1e6))
}
