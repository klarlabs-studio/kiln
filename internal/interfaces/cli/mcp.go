package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"go.klarlabs.de/kiln/internal/boot"
	"go.klarlabs.de/kiln/internal/domain/config"
	"go.klarlabs.de/kiln/internal/domain/isolation"
	"go.klarlabs.de/kiln/internal/engine"
	"go.klarlabs.de/kiln/internal/infrastructure/publish"
	"go.klarlabs.de/kiln/internal/infrastructure/store"
	"go.klarlabs.de/kiln/internal/infrastructure/worktree"
	"go.klarlabs.de/kiln/internal/interfaces/mcpsrv"
	"go.klarlabs.de/kiln/internal/version"
)

// runMCP serves the agent surface over stdio.
func runMCP(ctx context.Context, args []string, io IO) error {
	fs := newFlagSet("mcp", io)
	dir := fs.String("dir", "", "repository directory")
	pipelinePath := fs.String("pipeline", "", "pipeline file (default <dir>/.kiln.yaml)")
	if err := fs.Parse(args); err != nil {
		return wrapExit(ExitUsage, err)
	}
	if sub := fs.Arg(0); sub != "serve" {
		return failWith(ExitUsage, "usage: kiln mcp serve")
	}

	// Output is nil on purpose. Subprocess output must not reach stdout, which
	// belongs to the JSON-RPC stream; the structured log on stderr is where an
	// operator watches a run from.
	deps, err := boot.Build(ctx, boot.Options{Dir: *dir, PipelinePath: *pipelinePath})
	if err != nil {
		return wrapExit(ExitConfig, err)
	}

	return mcpsrv.Serve(ctx, &facade{deps: deps}, version.Version)
}

// facade adapts the assembled graph to the MCP surface.
type facade struct{ deps *boot.Deps }

func (f *facade) AllowPrivilegedRun() bool { return f.deps.Env.MCPAllowRun }

func (f *facade) Doctor(ctx context.Context) (mcpsrv.DoctorOutput, error) {
	d := f.deps
	out := mcpsrv.DoctorOutput{
		Directory:      d.Dir,
		PipelineFound:  d.PipelineFound,
		Routing:        map[string][]string{},
		Toolchain:      map[string]bool{},
		ChecksEnabled:  d.ChecksEnabled(),
		ProvenanceSkip: len(d.Env.TrustedKeys) > 0,
		Dry:            d.Dry(),
	}
	if d.RepoErr == nil {
		out.Repository = d.Repo.String()
	} else {
		out.Warnings = append(out.Warnings,
			"github repository unknown: checks are off and every pull request is treated as a fork")
	}

	for _, event := range []string{"pull_request", "push", "tag"} {
		steps := d.Pipeline.Steps(event)
		names := make([]string, len(steps))
		for i, s := range steps {
			names[i] = string(s)
		}
		out.Routing[event] = names
	}

	out.Toolchain[d.Env.Warden] = f.installed(d.Env.Warden)
	if !out.Toolchain[d.Env.Warden] {
		out.Problems = append(out.Problems,
			d.Env.Warden+" is not installed: kiln cannot pass a commit without the gate")
	}
	if d.Pipeline.Prove.Nox {
		out.Toolchain[d.Env.Nox] = f.installed(d.Env.Nox)
		if !out.Toolchain[d.Env.Nox] {
			out.Problems = append(out.Problems, "prove.nox is on but "+d.Env.Nox+" is not installed")
		}
	}

	if d.Pipeline.WantsPublish() && !d.Dry() {
		for _, tool := range f.publishTools() {
			out.Toolchain[tool] = f.installed(tool)
			if !out.Toolchain[tool] {
				out.Problems = append(out.Problems, tool+" is not installed but an event routes to publish")
			}
		}
	}

	for _, artifact := range d.Pipeline.Publish {
		out.Artifacts = append(out.Artifacts, artifactSummary(artifact))
		if artifact.Kind != config.KindImage {
			continue
		}
		if out.Image == "" {
			out.Image = artifact.Image
		}
		if plan, err := publish.BuildPlan(artifact, placeholderSHA, "refs/heads/main"); err == nil {
			out.Tags = append(out.Tags, plan.Refs()...)
		}
	}
	if !d.PipelineFound {
		out.Warnings = append(out.Warnings,
			"no .kiln.yaml: kiln will prove every event and publish nothing")
	}
	if len(d.Env.TrustedKeys) == 0 {
		out.Warnings = append(out.Warnings,
			"no KILN_TRUSTED_KEYS: every run re-proves rather than trusting an existing warden note")
	}
	if d.Pipeline.Wants("pull_request", config.StepPublish) {
		out.Warnings = append(out.Warnings,
			"on.pull_request lists publish: the isolation policy always suppresses it")
	}
	_ = ctx
	return out, nil
}

// publishTools names the binaries the configured artifact kinds need. cosign
// is always in the list: every kind kiln publishes is signed.
func (f *facade) publishTools() []string {
	tools := []string{"cosign"}
	for _, a := range f.deps.Pipeline.Publish {
		switch a.Kind {
		case config.KindBinaries:
			tools = append(tools, "goreleaser")
		default:
			tools = append(tools, "docker")
		}
	}
	slices.Sort(tools)
	return slices.Compact(tools)
}

// artifactSummary describes one pipeline entry for an agent.
func artifactSummary(a config.Artifact) mcpsrv.ArtifactSummary {
	s := mcpsrv.ArtifactSummary{Kind: string(a.Kind), On: a.On}
	if a.Kind == config.KindBinaries {
		s.Target = a.From + " (" + a.Config + ")"
		return s
	}
	s.Target = a.Image
	return s
}

// placeholderSHA lets doctor render a representative tag plan without needing
// a real commit. It is never used for anything that touches a registry.
const placeholderSHA = "0000000000000000000000000000000000000000"

func (f *facade) installed(name string) bool {
	_, err := f.deps.Runner.LookPath(name)
	return err == nil
}

func (f *facade) Status(_ context.Context, id string) (mcpsrv.RunOutput, error) {
	r, err := f.deps.Store.Latest()
	if id != "" {
		r, err = f.deps.Store.Get(id)
	}
	if errors.Is(err, store.ErrNotFound) {
		if id == "" {
			return mcpsrv.RunOutput{}, errors.New("no runs recorded yet")
		}
		return mcpsrv.RunOutput{}, fmt.Errorf("no run %q", id)
	}
	if err != nil {
		return mcpsrv.RunOutput{}, err
	}
	return mcpsrv.FromRun(r), nil
}

func (f *facade) Run(ctx context.Context, in mcpsrv.RunRequest) (mcpsrv.RunOutput, error) {
	d := f.deps

	sha, err := worktree.ResolveSHA(ctx, d.Runner, d.Dir, in.SHA)
	if err != nil {
		return mcpsrv.RunOutput{}, err
	}

	fork := in.Fork
	if !fork && in.Event == isolation.EventPullRequest {
		if in.PR > 0 {
			fork = d.ResolvePullFork(ctx, in.PR)
		} else {
			// An agent that cannot name the pull request cannot vouch for it.
			fork = boot.ForkUnknown
		}
	}

	ref := in.Ref
	if ref == "" && in.Event == isolation.EventPullRequest && in.PR > 0 {
		ref = "refs/pull/" + strconv.Itoa(in.PR) + "/head"
	}

	r, execErr := d.Engine.Execute(ctx, engine.Request{
		SHA:      sha,
		Event:    in.Event,
		Fork:     fork,
		Ref:      ref,
		Repo:     repoName(d),
		Dir:      d.Dir,
		Pipeline: d.Pipeline,
	})
	return mcpsrv.FromRun(r), execErr
}
