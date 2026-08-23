// Package boot wires a running Kiln out of the operator's environment.
//
// Four surfaces — CLI, MCP, kilnd and the webhook — all need the same object
// graph, and each one assembling it separately is how surfaces drift apart:
// one forgets to honour KILN_DRY, another builds a GitHub client where it
// should have built a Noop, and suddenly `kiln run` and `POST /v1/run` behave
// differently on the same commit. Assembling it once means that cannot happen.
package boot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.klarlabs.de/kiln/internal/domain/config"
	"go.klarlabs.de/kiln/internal/engine"
	"go.klarlabs.de/kiln/internal/infrastructure/checks"
	"go.klarlabs.de/kiln/internal/infrastructure/credstore"
	"go.klarlabs.de/kiln/internal/infrastructure/envconfig"
	"go.klarlabs.de/kiln/internal/infrastructure/execx"
	"go.klarlabs.de/kiln/internal/infrastructure/github"
	"go.klarlabs.de/kiln/internal/infrastructure/obs"
	"go.klarlabs.de/kiln/internal/infrastructure/pipelinefile"
	"go.klarlabs.de/kiln/internal/infrastructure/prove"
	"go.klarlabs.de/kiln/internal/infrastructure/provenance"
	"go.klarlabs.de/kiln/internal/infrastructure/publish"
	"go.klarlabs.de/kiln/internal/infrastructure/service"
	"go.klarlabs.de/kiln/internal/infrastructure/store"
	"go.klarlabs.de/kiln/internal/infrastructure/task"
	"go.klarlabs.de/kiln/internal/infrastructure/worktree"
)

// Options are the per-invocation inputs boot cannot read from the environment.
type Options struct {
	// Dir is the repository. Empty means the process's working directory.
	Dir string
	// PipelinePath overrides the default `<Dir>/.kiln.yaml`.
	PipelinePath string
	// Output receives subprocess output. Nil keeps builds quiet, which is what
	// the MCP and HTTP surfaces want; the CLI passes os.Stderr.
	Output io.Writer
	// Env overrides the process environment, for tests.
	Env *envconfig.Env
	// Log overrides the logger.
	Log obs.Logger
}

// Deps is the assembled graph.
type Deps struct {
	Env      envconfig.Env
	Dir      string
	Pipeline config.Pipeline
	// PipelineFound is false when the repository has no `.kiln.yaml`. That is
	// not an error — a library still gets proven — but a surface reporting a
	// publish plan needs to know why there isn't one.
	PipelineFound bool
	Repo          github.Repo
	// RepoErr records why the repository could not be identified. Checks are
	// simply off in that case.
	RepoErr error

	Runner execx.Runner
	Store  *store.File
	GitHub *github.Client
	Checks checks.Reporter
	Engine *engine.Engine
	Log    obs.Logger

	// output is where subprocess output goes for requests built from this
	// graph. Unexported so surfaces read it through Output() rather than
	// setting it after the fact.
	output io.Writer
}

// Dry reports whether this process will only plan a publish.
func (d *Deps) Dry() bool { return d.Env.Dry }

// ChecksEnabled reports whether Kiln can post to GitHub Checks.
func (d *Deps) ChecksEnabled() bool { return d.GitHub != nil && d.GitHub.Enabled() }

// Build assembles everything.
//
// It fails only on things that make every command meaningless — an
// unreadable directory, a malformed `.kiln.yaml`. A missing token, an
// unidentifiable repository and an absent pipeline are all degradations that
// get recorded on Deps and reported by `kiln doctor`, because a developer
// running `kiln run` on a laptop should still be able to gate a commit.
func Build(ctx context.Context, opts Options) (*Deps, error) {
	env := envconfig.Load()
	if opts.Env != nil {
		env = *opts.Env
	}

	log := opts.Log
	if log == nil {
		log = obs.New(env.LogLevel)
	}

	dir, err := resolveDir(opts.Dir, env.Dir)
	if err != nil {
		return nil, err
	}

	runner := execx.NewSystem()
	if !worktree.IsRepo(ctx, runner, dir) {
		return nil, fmt.Errorf("boot: %s is not a git repository — kiln builds commits, so it needs one", dir)
	}

	pipeline, found, err := loadPipeline(dir, opts.PipelinePath)
	if err != nil {
		return nil, err
	}

	// A token from the environment wins — CI sets one, and an operator
	// exporting GITHUB_TOKEN for one command means it. Otherwise the stored
	// one, so a schedule needs no credential in its unit file and no token in
	// plaintext next to it.
	//
	// Before deps is built, not after: Env is copied by value, and a token
	// added afterwards would reach the API client while ChecksEnabled() — and
	// therefore doctor, and the fork default — still read empty.
	if env.Token == "" {
		if token, kind, err := credstore.New(runner).Get(ctx); err == nil {
			env.Token = token
			log.Debug("using the stored github token", "from", string(kind))
		}
	}

	deps := &Deps{
		Env:           env,
		Dir:           dir,
		Pipeline:      pipeline,
		PipelineFound: found,
		Runner:        runner,
		Store:         store.NewFile(resolveDB(env.DB, dir)),
		Log:           log,
	}

	deps.Repo, deps.RepoErr = github.DiscoverRepo(ctx, runner, dir, pipeline.Watch.Remote, env.Repository)
	deps.GitHub = buildClient(env, deps.Repo, log)
	deps.Checks = buildReporter(deps.GitHub, log)

	// One warden binding serves both roles: deciding whether a re-prove can be
	// skipped, and carrying warden's verdict onto the artifact.
	wardenProvenance := provenance.NewWarden(runner, env.Warden, env.TrustedKeys)

	deps.Engine = engine.New(engine.Engine{
		Prover:           prove.NewWarden(runner, env.Warden, env.Nox),
		Publisher:        buildPublisher(env, runner, log),
		ReleasePublisher: buildReleasePublisher(ctx, env, runner, deps.GitHub, log),
		ToolVersions:     toolVersions(ctx, runner, env),
		PhaseTimeout:     env.PhaseTimeout,
		Provenance:       wardenProvenance,
		SourceAttester:   wardenProvenance,
		Tasks:            task.New(runner),
		GitHub:           deps.GitHub,
		KeepRoot:         filepath.Dir(deps.Store.Path()),
		Services:         service.New(runner, log),
		Checks:           deps.Checks,
		Store:            deps.Store,
		Log:              log,
	})
	// The engine does not know about Options.Output; surfaces attach it per
	// request. Recording it here keeps the plumbing in one place.
	deps.output = opts.Output

	return deps, nil
}

// Output is where subprocess output should be streamed for requests built
// from this graph.
func (d *Deps) Output() io.Writer { return d.output }

func resolveDir(optDir, envDir string) (string, error) {
	dir := optDir
	if dir == "" {
		dir = envDir
	}
	if dir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("boot: no directory given and the working directory is unreadable: %w", err)
		}
		dir = cwd
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("boot: resolve %s: %w", dir, err)
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		return "", fmt.Errorf("boot: %s is not a directory", abs)
	}
	return abs, nil
}

// resolveDB keeps a relative KILN_DB inside the repository. Otherwise a cron
// entry whose working directory differs from the checkout would silently keep
// a second, always-empty ledger and rebuild everything on every tick.
func resolveDB(db, dir string) string {
	if db == "" {
		db = envconfig.DefaultDB
	}
	if filepath.IsAbs(db) {
		return db
	}
	return filepath.Join(dir, db)
}

func loadPipeline(dir, explicit string) (config.Pipeline, bool, error) {
	if explicit != "" {
		// An explicitly named pipeline that does not exist is a mistake worth
		// stopping for; a missing default one is not.
		p, err := pipelinefile.LoadFile(explicit)
		if err != nil {
			return config.Pipeline{}, false, err
		}
		return p, true, nil
	}

	p, err := pipelinefile.LoadDir(dir)
	switch {
	case errors.Is(err, config.ErrNotFound):
		return p, false, nil
	case err != nil:
		return config.Pipeline{}, false, err
	default:
		return p, true, nil
	}
}

func buildClient(env envconfig.Env, repo github.Repo, log obs.Logger) *github.Client {
	if env.Token == "" || !repo.Valid() {
		return nil
	}
	return github.NewClient(env.Token, repo, log)
}

func buildReporter(c *github.Client, log obs.Logger) checks.Reporter {
	if c == nil || !c.Enabled() {
		// No token: gate the commit, print the result, tell nobody. Failing
		// here would make a laptop run impossible.
		return checks.Noop{}
	}
	return checks.NewGitHub(c, log)
}

// buildPublisher honours KILN_DRY. The dry publisher is a rehearsal that
// reports a placeholder digest and Signed=false, so nothing downstream can
// mistake it for a real artifact.
func buildPublisher(env envconfig.Env, runner execx.Runner, log obs.Logger) publish.Publisher {
	if env.Dry {
		return publish.NewDry(log)
	}
	d := publish.NewDocker(runner, log)
	d.SigningKey = env.CosignKey
	return d
}

// buildReleasePublisher wires the binary-release path.
//
// KILN_DRY still runs goreleaser — a rehearsal that skipped the cross-compile
// would rehearse nothing — but withholds the upload, so the dry publisher is
// not substituted here the way it is for images.
func buildReleasePublisher(
	ctx context.Context, env envconfig.Env, runner execx.Runner, gh *github.Client, log obs.Logger,
) publish.Publisher {
	_ = ctx
	g := publish.NewGoreleaser(runner, log, env.Token, env.Dry)
	g.SigningKey = env.CosignKey
	if env.Goreleaser != "" {
		g.Binary = env.Goreleaser
	}
	if gh != nil && gh.Enabled() {
		g.Uploader = releaseUploader{gh}
	}
	return g
}

// releaseUploader adapts the forge client to the narrow interface the release
// publisher needs, so that package keeps no dependency on the client.
type releaseUploader struct{ c *github.Client }

func (u releaseUploader) UploadReleaseAssetByTag(ctx context.Context, tag, name string, body []byte) error {
	rel, err := u.c.ReleaseByTag(ctx, tag)
	if err != nil {
		return err
	}
	return u.c.UploadReleaseAsset(ctx, rel, name, body)
}

// toolVersions records what the gate and the builders are, for the provenance
// predicate.
//
// Best-effort by design: a tool whose version cannot be read is simply absent
// from the record. Guessing would put a false claim inside something signed,
// which is worse than an incomplete one.
func toolVersions(ctx context.Context, runner execx.Runner, env envconfig.Env) map[string]string {
	probes := map[string][]string{
		env.Warden: {"version"},
		"cosign":   {"version", "--json"},
	}
	out := make(map[string]string, len(probes))
	for bin, args := range probes {
		res, err := runner.Run(ctx, execx.Cmd{Name: bin, Args: args})
		if err != nil {
			continue
		}
		if v := firstVersionToken(res.Output()); v != "" {
			out[bin] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// firstVersionToken pulls a version out of a tool's banner. Tools disagree
// wildly on format, so this takes the first token that looks like one and
// gives up otherwise rather than recording a line of ASCII art.
func firstVersionToken(s string) string {
	for _, field := range strings.Fields(s) {
		trimmed := strings.TrimPrefix(field, "v")
		if trimmed == "" {
			continue
		}
		if trimmed[0] >= '0' && trimmed[0] <= '9' && strings.Contains(trimmed, ".") {
			return trimmed
		}
	}
	return ""
}

// ForkUnknown is the fork status to assume when GitHub cannot be asked.
//
// True, always. Without a token Kiln cannot distinguish a maintainer's branch
// from a stranger's fork, and the permissive guess hands a stranger the
// operator's registry credentials. The conservative guess only costs a
// re-prove.
const ForkUnknown = true

// ResolvePullFork answers "is this pull request from a fork" as well as the
// available credentials allow.
func (d *Deps) ResolvePullFork(ctx context.Context, number int) bool {
	if d.GitHub == nil || !d.GitHub.Enabled() {
		d.Log.Warn("no github token: treating pull request as a fork",
			"pr", number, "effect", "no secrets, no publish, no provenance skip")
		return ForkUnknown
	}
	pull, err := d.GitHub.LookupPull(ctx, number)
	if err != nil {
		d.Log.Warn("could not look up pull request: treating it as a fork", "pr", number, "err", err)
		return ForkUnknown
	}
	return pull.Fork
}
