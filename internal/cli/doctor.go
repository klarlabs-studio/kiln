package cli

import (
	"context"
	"fmt"
	"strings"

	"go.klarlabs.de/kiln/internal/boot"
	"go.klarlabs.de/kiln/internal/config"
	"go.klarlabs.de/kiln/internal/execx"
	"go.klarlabs.de/kiln/internal/publish"
)

// runDoctor validates without executing.
//
// The point of doctor is that it is safe: it runs no gate, builds no image and
// pushes nothing, so an operator can run it on a production box at any time.
// Everything it reports is something that would otherwise be discovered
// halfway through a real run, when a worktree is already checked out and a
// registry is already half-written.
func runDoctor(ctx context.Context, args []string, io IO) error {
	fs := newFlagSet("doctor", io)
	dir := fs.String("dir", "", "repository directory")
	pipelinePath := fs.String("pipeline", "", "pipeline file (default <dir>/.kiln.yaml)")
	sha := fs.String("sha", "HEAD", "commit to plan tags for")
	ref := fs.String("ref", "refs/heads/main", "ref to plan tags for")
	// A pipeline is reviewed on machines that will never build anything — a
	// linter job, a reviewer's laptop. Checking the schema there should not
	// require installing docker, cosign and nox first.
	configOnly := fs.Bool("config-only", false, "validate the pipeline and tag plan; skip toolchain and credentials")
	if err := fs.Parse(args); err != nil {
		return wrapExit(ExitUsage, err)
	}

	deps, err := boot.Build(ctx, boot.Options{Dir: *dir, PipelinePath: *pipelinePath})
	if err != nil {
		return wrapExit(ExitConfig, err)
	}

	report := doctorReport{configOnly: *configOnly}
	report.collect(ctx, deps, *sha, *ref)
	io.print(report.String())

	if report.fatal {
		return failWith(ExitConfig, "doctor found %d problem(s)", report.problems)
	}
	return nil
}

// doctorReport accumulates findings so the output is one coherent document
// rather than a stream of interleaved lines.
type doctorReport struct {
	b strings.Builder
	// configOnly suppresses the toolchain and credential sections.
	configOnly bool
	problems   int
	fatal      bool
}

func (r *doctorReport) ok(format string, args ...any) {
	fmt.Fprintf(&r.b, "  ok    %s\n", fmt.Sprintf(format, args...))
}

// warn is for a degradation the operator may have chosen deliberately: no
// token on a laptop, no trusted keys on a box that never skips.
func (r *doctorReport) warn(format string, args ...any) {
	fmt.Fprintf(&r.b, "  warn  %s\n", fmt.Sprintf(format, args...))
}

// fail is for something that will break a real run.
func (r *doctorReport) fail(format string, args ...any) {
	fmt.Fprintf(&r.b, "  FAIL  %s\n", fmt.Sprintf(format, args...))
	r.problems++
	r.fatal = true
}

func (r *doctorReport) section(name string) { fmt.Fprintf(&r.b, "\n%s\n", name) }

func (r *doctorReport) String() string { return r.b.String() }

func (r *doctorReport) collect(ctx context.Context, deps *boot.Deps, sha, ref string) {
	r.section("repository")
	r.ok("directory %s", deps.Dir)
	if deps.RepoErr != nil {
		r.warn("github repository unknown (%v): checks are off and every PR is a fork", deps.RepoErr)
	} else {
		r.ok("github repository %s", deps.Repo)
	}
	r.ok("run ledger %s", deps.Store.Path())

	r.section("pipeline")
	r.checkPipeline(deps)

	if !r.configOnly {
		r.section("toolchain")
		r.checkToolchain(deps)

		r.section("credentials")
		r.checkCredentials(deps)
	}

	if deps.Pipeline.Publish != nil {
		r.section("tag plan")
		r.checkPlan(ctx, deps, sha, ref)
	}
}

func (r *doctorReport) checkPipeline(deps *boot.Deps) {
	if !deps.PipelineFound {
		// A library repository legitimately has nothing to publish. Kiln still
		// proves it and still posts the Check.
		r.warn("no %s: kiln will prove every event and publish nothing", config.FileName)
		return
	}
	r.ok("%s parsed", config.FileName)

	for _, event := range []string{"pull_request", "push", "tag"} {
		steps := deps.Pipeline.Steps(event)
		if len(steps) == 0 {
			r.warn("on.%s routes to nothing: this event will be ignored", event)
			continue
		}
		names := make([]string, len(steps))
		for i, s := range steps {
			names[i] = string(s)
		}
		r.ok("on.%s → %s", event, strings.Join(names, ", "))
	}

	if deps.Pipeline.Wants("pull_request", config.StepPublish) {
		// The engine will overrule this at run time. Saying so here saves the
		// operator an afternoon wondering where their image went.
		r.warn("on.pull_request lists publish: the isolation policy always suppresses it — " +
			"a pull request head is a proposal, not a release")
	}
	if deps.Pipeline.Prove.Nox {
		r.ok("prove.nox enabled")
	}
}

func (r *doctorReport) checkToolchain(deps *boot.Deps) {
	// warden is required whenever anything proves, which is every sane
	// pipeline. Its absence is the one toolchain gap that is never a warning.
	if path, err := deps.Runner.LookPath(deps.Env.Warden); err != nil {
		r.fail("%s not found: kiln cannot pass a commit without the gate (install warden or set KILN_WARDEN)",
			deps.Env.Warden)
	} else {
		r.ok("%s at %s", deps.Env.Warden, path)
	}

	if deps.Pipeline.Prove.Nox {
		if _, err := deps.Runner.LookPath(deps.Env.Nox); err != nil {
			r.fail("prove.nox is on but %s is not installed (install nox, set KILN_NOX, or set prove.nox: false)",
				deps.Env.Nox)
		} else {
			r.ok("%s installed", deps.Env.Nox)
		}
	}

	r.checkPublishTools(deps)
}

func (r *doctorReport) checkPublishTools(deps *boot.Deps) {
	if !deps.Pipeline.WantsPublish() {
		r.ok("no event publishes: docker and cosign are not needed")
		return
	}
	if deps.Dry() {
		r.warn("KILN_DRY=1: publishes will be planned, not performed")
		return
	}

	for _, tool := range []struct{ name, why string }{
		{"docker", "builds the artifact"},
		{"cosign", "signs the digest RollOps refuses to deploy unsigned"},
	} {
		if _, err := deps.Runner.LookPath(tool.name); err != nil {
			r.fail("%s not found: it %s (install it, or set KILN_DRY=1)", tool.name, tool.why)
		} else {
			r.ok("%s installed (%s)", tool.name, tool.why)
		}
	}
}

func (r *doctorReport) checkCredentials(deps *boot.Deps) {
	if deps.ChecksEnabled() {
		r.ok("GITHUB_TOKEN present: checks will be posted as %q and %q", "Kiln / Prove", "Kiln / Publish")
	} else {
		r.warn("no usable GITHUB_TOKEN: no checks, and every pull request is treated as a fork")
	}

	if len(deps.Env.TrustedKeys) == 0 {
		// Not a failure: a box that always re-proves is correct, just slower.
		r.warn("no KILN_TRUSTED_KEYS: every run re-proves, because kiln only skips for a note " +
			"signed by a key the operator pinned")
	} else {
		r.ok("%d trusted signing key(s) pinned: a matching warden note may skip the re-prove",
			len(deps.Env.TrustedKeys))
	}
}

func (r *doctorReport) checkPlan(ctx context.Context, deps *boot.Deps, commitish, ref string) {
	sha, err := resolveCommit(ctx, deps, commitish)
	if err != nil {
		r.warn("cannot resolve %q, planning against a placeholder: %v", commitish, err)
		sha = strings.Repeat("0", 40)
	}

	plan, err := publish.BuildPlan(*deps.Pipeline.Publish, sha, ref)
	if err != nil {
		r.fail("%v", err)
		return
	}
	for line := range strings.SplitSeq(strings.TrimRight(plan.String(), "\n"), "\n") {
		fmt.Fprintf(&r.b, "  %s\n", line)
	}
}

// resolveCommit turns a commit-ish into an object id via the repository.
func resolveCommit(ctx context.Context, deps *boot.Deps, commitish string) (string, error) {
	res, err := deps.Runner.Run(ctx, execx.Cmd{
		Name: "git", Args: []string{"rev-parse", "--verify", commitish + "^{commit}"}, Dir: deps.Dir,
	})
	if err != nil {
		return "", err
	}
	return res.Output(), nil
}
