package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.klarlabs.de/kiln/internal/application/ports"

	"go.klarlabs.de/kiln/internal/boot"
	"go.klarlabs.de/kiln/internal/domain/config"
	"go.klarlabs.de/kiln/internal/infrastructure/execx"
	"go.klarlabs.de/kiln/internal/infrastructure/policyfile"
	"go.klarlabs.de/kiln/internal/infrastructure/publish"
	"go.klarlabs.de/kiln/internal/infrastructure/task"
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
	policyPath := fs.String("policy", "", "validate a verification policy instead, and print what it would require")
	if err := fs.Parse(args); err != nil {
		return wrapExit(ExitUsage, err)
	}

	// A verification policy is checked on machines that have no pipeline at
	// all — a consumer's repository, where kiln verifies artifacts somebody
	// else built. Requiring a .kiln.yaml to sit beside it would make the
	// policy unreviewable exactly where it matters most.
	if *policyPath != "" {
		return checkPolicy(io, *policyPath)
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

// note is for something kiln could not establish either way. It is not a
// warning: reporting an unknown as a problem is how a doctor teaches people to
// skim past its output.
func (r *doctorReport) note(format string, args ...any) {
	fmt.Fprintf(&r.b, "  ?     %s\n", fmt.Sprintf(format, args...))
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

	if len(deps.Pipeline.Publish) > 0 {
		r.section("artifacts")
		r.checkArtifacts(ctx, deps, sha, ref)
	}

	if len(deps.Pipeline.Services) > 0 {
		r.section("services")
		for _, name := range slices.Sorted(maps.Keys(deps.Pipeline.Services)) {
			svc := deps.Pipeline.Services[name]
			if svc.Port > 0 {
				r.ok("%s from %s, port %d → KILN_SERVICE_%s_HOST/PORT",
					name, svc.Image, svc.Port, strings.ToUpper(strings.ReplaceAll(name, "-", "_")))
			} else {
				r.ok("%s from %s (no port published)", name, svc.Image)
			}
			if svc.Ready == "" {
				// Not an error, but it is the cause of the flake that follows:
				// the gate starts the instant the container does, which is
				// well before a database accepts connections.
				r.warn("  %s has no ready: probe; the gate may start before it accepts connections", name)
			}
		}
	}

	if len(deps.Pipeline.Tasks) > 0 {
		r.section("tasks")
		r.checkTasks(deps)
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
		{"cosign", "signs the digest; verify it at deploy time — nothing downstream checks for you"},
	} {
		if _, err := deps.Runner.LookPath(tool.name); err != nil {
			r.fail("%s not found: it %s (install it, or set KILN_DRY=1)", tool.name, tool.why)
		} else {
			r.ok("%s installed (%s)", tool.name, tool.why)
		}
	}

	r.checkSigningMode(deps)
}

// checkSigningMode reports how publish will sign, and warns when the mode
// cannot work unattended.
//
// This exists because both failure modes are hangs rather than errors. Keyless
// with no ambient OIDC identity drops to the device flow and waits on a browser
// nobody is in front of, until the code expires — which on a multi-image tag
// means some images are signed and some are not, and the tag has to be
// repaired by hand. An encrypted key file with no COSIGN_PASSWORD waits on a
// prompt in the same way. Neither is visible until a release is already
// half-published, so doctor is the right place to say it.
func (r *doctorReport) checkSigningMode(deps *boot.Deps) {
	key := deps.Env.CosignKey
	if key == "" {
		if id, ok := ambientOIDC(); ok {
			r.ok("keyless signing: %s provides the identity cosign proves", id)
			return
		}
		r.warn("keyless signing with no OIDC identity in the environment: cosign will fall back " +
			"to the browser device flow and block until it expires — set KILN_COSIGN_KEY " +
			"(a key file, env://VAR, k8s://ns/name, or a KMS URI) for an unattended box")
		return
	}

	if scheme, _, ok := strings.Cut(key, "://"); ok {
		r.ok("keyed signing: %s, so the private key never sits on this box", scheme+"://")
		return
	}
	r.ok("keyed signing with the key file %s", key)
	// LookupEnv, not Getenv: an unencrypted key is used by setting
	// COSIGN_PASSWORD to the empty string, so "set and empty" is a valid
	// configuration and only "unset" is the one that prompts.
	if _, set := os.LookupEnv("COSIGN_PASSWORD"); !set {
		r.warn("COSIGN_PASSWORD is unset: an encrypted key file makes cosign prompt for the " +
			"passphrase, which an unattended run cannot answer (set it to an empty value for " +
			"an unencrypted key)")
	}
}

// ambientOIDC names the identity provider cosign would prove keylessly against,
// if one is present. Absence is the interesting answer — it is what turns
// keyless from correct into a hang.
func ambientOIDC() (string, bool) {
	for _, p := range []struct{ env, name string }{
		{"ACTIONS_ID_TOKEN_REQUEST_URL", "GitHub Actions"},
		{"SIGSTORE_ID_TOKEN", "SIGSTORE_ID_TOKEN"},
		{"GITLAB_CI", "GitLab CI"},
		{"BUILDKITE_AGENT_ACCESS_TOKEN", "Buildkite"},
		{"GOOGLE_APPLICATION_CREDENTIALS", "Google workload identity"},
	} {
		if os.Getenv(p.env) != "" {
			return p.name, true
		}
	}
	return "", false
}

func (r *doctorReport) checkCredentials(deps *boot.Deps) {
	if deps.ChecksEnabled() {
		// Deliberately not promising check runs. The Checks API only accepts a
		// GitHub App token; a personal access token gets a 403 and kiln falls
		// back to commit statuses. Both work as required contexts, and saying
		// "checks will be posted" flatly would be false half the time.
		r.ok("GITHUB_TOKEN present: results post as %q, %q and one per task",
			"Kiln / Prove", "Kiln / Publish")
		r.note("a personal access token posts commit statuses; check runs need a GitHub App")
	} else {
		r.warn("no usable GITHUB_TOKEN: no checks, and every pull request is treated as a fork")
	}

	r.checkRegistries(deps)

	if len(deps.Env.TrustedKeys) == 0 {
		// Not a failure: a box that always re-proves is correct, just slower.
		r.warn("no KILN_TRUSTED_KEYS: every run re-proves, because kiln only skips for a note " +
			"signed by a key the operator pinned")
	} else {
		r.ok("%d trusted signing key(s) pinned: a matching warden note may skip the re-prove",
			len(deps.Env.TrustedKeys))
	}
}

// checkRegistries reports whether the box can authenticate to the registries
// this pipeline pushes to.
//
// The push is the last thing a publish does, so a missing login is discovered
// after the gate has run and the image has been built — the most expensive
// possible moment to learn it, and on an unattended box one that repeats every
// tick. This reads docker's own configuration and says so up front.
//
// It deliberately does not contact the registry. Whether the credentials are
// *valid* is a question only the registry can answer, and asking it here would
// turn `kiln doctor` into something that needs the network and a rate-limit
// budget to run.
func (r *doctorReport) checkRegistries(deps *boot.Deps) {
	seen := map[string]bool{}
	for _, a := range deps.Pipeline.Publish {
		if a.Kind != config.KindImage || a.Image == "" {
			continue
		}
		registry := publish.RegistryOf(a.Image)
		if seen[registry] {
			continue
		}
		seen[registry] = true

		switch publish.CheckRegistryCredentials(registry) {
		case publish.CredentialsPresent:
			r.ok("docker credentials for %s", registry)
		case publish.CredentialsMissing:
			r.warn("no docker credentials for %s: `docker login %s` before publishing, "+
				"or the push will fail after the build", registry, registry)
		case publish.CredentialsUnknown:
			// A CI runner may be injecting credentials in a way this cannot
			// see. Saying "not logged in" there would be a false alarm, and a
			// doctor that cries wolf is a doctor nobody reads.
			r.note("could not tell whether %s has credentials; docker's config says nothing either way", registry)
		}
	}
}

// checkTasks lists what will run and when.
//
// A task routed to an event the pipeline ignores is the quiet failure this
// section exists to catch: it parses, it validates, and it never runs, and
// nothing anywhere would have said so.
func (r *doctorReport) checkTasks(deps *boot.Deps) {
	for _, name := range slices.Sorted(maps.Keys(deps.Pipeline.Tasks)) {
		t := deps.Pipeline.Tasks[name]
		r.ok("%s", task.Describe(name, t))

		for _, event := range t.On {
			if event == config.ScheduleEvent {
				continue
			}
			if len(deps.Pipeline.Steps(event)) == 0 {
				r.warn("  tasks.%s runs on %s, which this pipeline routes to nothing",
					name, event)
			}
		}
		r.ok("  posts check %q", ports.TaskName(name))
	}
}

func (r *doctorReport) checkArtifacts(ctx context.Context, deps *boot.Deps, commitish, ref string) {
	sha, err := resolveCommit(ctx, deps, commitish)
	if err != nil {
		r.warn("cannot resolve %q, planning against a placeholder: %v", commitish, err)
		sha = strings.Repeat("0", 40)
	}

	for i, artifact := range deps.Pipeline.Publish {
		events := "every publishing event"
		if len(artifact.On) > 0 {
			events = strings.Join(artifact.On, ", ")
		}
		r.ok("publish[%d] %s on %s", i, artifact.Kind, events)

		switch artifact.Kind {
		case config.KindBinaries:
			r.checkRelease(deps, artifact)
		default:
			r.checkImagePlan(artifact, sha, ref)
		}
	}
}

// planRef picks the ref to plan an artifact against.
//
// Doctor runs on whatever is checked out, which is usually a branch. An
// artifact published only `on: [tag]` can never be built from a branch, so
// planning it there describes a build that cannot happen — and for the common
// `tags: [sha, semver]` it reports "no moving tag" and advises publishing
// semver only on tag events, which is exactly what the config already says. Six
// of those made doctor exit non-zero on a correct pipeline, which is worse than
// no check: the one signal before a release was all noise.
//
// So a tag-only artifact is planned against a tag. The current one if doctor is
// standing on it, otherwise a representative version, labelled as hypothetical.
func planRef(artifact config.Artifact, ref string) (planned string, hypothetical bool) {
	if !artifact.TagOnly() || strings.HasPrefix(ref, "refs/tags/") {
		return ref, false
	}
	return "refs/tags/v0.0.0", true
}

func (r *doctorReport) checkImagePlan(artifact config.Artifact, sha, ref string) {
	planned, hypothetical := planRef(artifact, ref)
	plan, err := publish.BuildPlan(artifact, sha, planned)
	if err != nil {
		r.fail("%v", err)
		return
	}
	if hypothetical {
		// Say the plan is for a tag that does not exist. A version number in
		// the output that nobody recognises is otherwise alarming.
		fmt.Fprintf(&r.b, "    (planned against %s — this artifact only publishes on a tag)\n", planned)
	}
	for line := range strings.SplitSeq(strings.TrimRight(plan.String(), "\n"), "\n") {
		fmt.Fprintf(&r.b, "    %s\n", line)
	}
}

// checkRelease reports what a binary release would need, without running one.
// The signing check is the valuable half: it is the difference between a
// release anyone can verify and one nobody can.
func (r *doctorReport) checkRelease(deps *boot.Deps, artifact config.Artifact) {
	fmt.Fprintf(&r.b, "    from %s (%s)\n", artifact.From, artifact.Config)

	path := filepath.Join(deps.Dir, artifact.Config)
	switch err := publish.CheckReleaseSigning(path); {
	case err == nil:
		fmt.Fprintf(&r.b, "    sign %s signs its checksum manifest\n", artifact.Config)
	case errors.Is(err, publish.ErrUnsignedRelease):
		r.fail("%v", err)
	default:
		r.warn("cannot read %s: %v", artifact.Config, err)
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

// checkPolicy loads a verification policy and prints what it would require.
//
// "It parses" is the smaller half. The value is the list: a policy author can
// see that the rule they thought they wrote is the rule that will run, which
// is the failure a silently-ignored field would otherwise cause at the worst
// possible time.
func checkPolicy(io IO, path string) error {
	p, err := policyfile.Load(path)
	if err != nil {
		return wrapExit(ExitConfig, err)
	}
	io.print(path + " requires:\n")
	for _, c := range p.Checks() {
		io.print("  - " + c + "\n")
	}
	return nil
}
