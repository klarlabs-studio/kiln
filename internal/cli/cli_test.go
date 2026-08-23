package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.klarlabs.de/kiln/internal/gittest"
	"go.klarlabs.de/kiln/internal/lock"
)

// capture runs a command against buffers and returns its output and exit code.
func capture(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errb bytes.Buffer
	code = Main(t.Context(), args, IO{Out: &out, Err: &errb})
	return out.String(), errb.String(), code
}

// repoWith builds a repository with an optional pipeline and chdirs into it,
// so commands run with no --dir behave the way an operator experiences them.
func repoWith(t *testing.T, pipeline string) *gittest.Repo {
	t.Helper()
	repo := gittest.New(t)
	repo.Commit("first", "app.txt", "one\n")
	if pipeline != "" {
		repo.Write(".kiln.yaml", pipeline)
		repo.Git("add", "-A")
		repo.Git("commit", "-q", "-m", "add pipeline")
	}
	t.Chdir(repo.Dir)

	// A hermetic environment: an operator's real token or trusted keys must
	// not change what these tests observe.
	for _, k := range []string{
		"GITHUB_TOKEN", "GH_TOKEN", "KILN_TRUSTED_KEYS", "KILN_DRY",
		"KILN_DB", "KILN_DIR", "GITHUB_REPOSITORY", "KILN_MCP_ALLOW_RUN",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("KILN_LOG_LEVEL", "fatal")
	// Pin a stub gate. Otherwise every doctor and run test quietly depends on
	// whether the developer happens to have warden installed, and passes on a
	// laptop while failing on a runner. Tests that need a *missing* gate
	// override KILN_WARDEN after calling this.
	t.Setenv("KILN_WARDEN", fakeBin(t, "warden-stub", "exit 0"))
	return repo
}

const publishingPipeline = `apiVersion: kiln.klarlabs.de/v1
kind: Pipeline
on:
  pull_request: [prove]
  push: [prove, publish]
publish:
  - kind: image
    image: ghcr.io/felixgeelhaar/glossa-api
    tags: [sha, latest]
`

// tagOnlySemverPipeline is the shape every real repository here uses: images
// cut from a version tag, tagged sha + semver. Doctor runs on a branch, where
// that plan has no semver — which is correct and must not be reported as a
// misconfiguration.
const tagOnlySemverPipeline = `apiVersion: kiln.klarlabs.de/v1
kind: Pipeline
on:
  push: [prove]
  tag: [prove, publish]
publish:
  - kind: image
    image: ghcr.io/felixgeelhaar/glossa-api
    tags: [sha, semver]
    on: [tag]
`

// TestDoctorDoesNotFailATagOnlyPipelineOnABranch covers a false positive that
// made doctor useless where it mattered most: six of these, all advising the
// operator to do the thing the config already did, and a non-zero exit on a
// correct pipeline.
func TestDoctorDoesNotFailATagOnlyPipelineOnABranch(t *testing.T) {
	repoWith(t, tagOnlySemverPipeline)

	out, _, code := capture(t, "doctor")

	if strings.Contains(out, "no moving tag") {
		t.Errorf("a tag-only artifact cannot be built from a branch:\n%s", out)
	}
	if code != ExitOK {
		t.Errorf("code = %d, want ExitOK on a correct pipeline:\n%s", code, out)
	}
	// The plan shown is for a tag that does not exist, so it has to say so —
	// an unrecognised version number in the output is otherwise alarming.
	if !strings.Contains(out, "only publishes on a tag") {
		t.Errorf("doctor should label the hypothetical plan:\n%s", out)
	}
}

// TestDoctorStillFailsAnUnpinnableBranchBuild keeps the check that the false
// positive was hiding: an artifact that really is built on a branch, with no
// moving tag, is one RollOps' imagePolicy would never see.
func TestDoctorStillFailsAnUnpinnableBranchBuild(t *testing.T) {
	repoWith(t, `apiVersion: kiln.klarlabs.de/v1
kind: Pipeline
on:
  push: [prove, publish]
publish:
  - kind: image
    image: ghcr.io/felixgeelhaar/glossa-api
    tags: [sha, semver]
`)

	out, _, code := capture(t, "doctor")

	if !strings.Contains(out, "no moving tag") {
		t.Errorf("a branch build with no moving tag is still a real problem:\n%s", out)
	}
	if code == ExitOK {
		t.Errorf("code = %d, want a failure:\n%s", code, out)
	}
}

func TestVersionPrintsTheStamp(t *testing.T) {
	out, _, code := capture(t, "version")

	if code != ExitOK {
		t.Errorf("code = %d", code)
	}
	if !strings.HasPrefix(out, "kiln ") {
		t.Errorf("out = %q", out)
	}
}

func TestNoArgumentsIsAUsageError(t *testing.T) {
	_, errOut, code := capture(t)

	if code != ExitUsage {
		t.Errorf("code = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(errOut, "kiln — signed-artifact factory") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestUnknownCommandNamesItself(t *testing.T) {
	_, errOut, code := capture(t, "deploy")

	if code != ExitUsage {
		t.Errorf("code = %d", code)
	}
	// "deploy" is the command people will reach for. The usage text tells them
	// where deployment actually lives.
	if !strings.Contains(errOut, `unknown command "deploy"`) {
		t.Errorf("stderr = %q", errOut)
	}
	if !strings.Contains(errOut, "RollOps") {
		t.Error("usage should say where deployment belongs")
	}
}

func TestHelpGoesToStdout(t *testing.T) {
	out, _, code := capture(t, "help")

	if code != ExitOK || !strings.Contains(out, "kiln doctor") {
		t.Errorf("code = %d, out = %q", code, out)
	}
}

func TestDoctorOnAPublishingRepo(t *testing.T) {
	repoWith(t, publishingPipeline)

	out, _, _ := capture(t, "doctor")

	for _, want := range []string{
		"pipeline", ".kiln.yaml parsed",
		"on.push → prove, publish",
		"toolchain", "artifacts",
		"ghcr.io/felixgeelhaar/glossa-api:latest",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output missing %q:\n%s", want, out)
		}
	}
}

func TestDoctorWarnsAboutTheMissingToken(t *testing.T) {
	repoWith(t, publishingPipeline)

	out, _, _ := capture(t, "doctor")

	if !strings.Contains(out, "no usable GITHUB_TOKEN") {
		t.Errorf("doctor should name the missing token:\n%s", out)
	}
	if !strings.Contains(out, "no KILN_TRUSTED_KEYS") {
		t.Errorf("doctor should explain why every run re-proves:\n%s", out)
	}
}

// TestDoctorWarnsAboutUnattendedKeylessSigning covers the mode that cost a
// release: keyless signing on a box with no OIDC identity does not fail, it
// blocks on a browser device flow until the code expires, part-way through a
// multi-image tag.
func TestDoctorWarnsAboutUnattendedKeylessSigning(t *testing.T) {
	repoWith(t, publishingPipeline)

	out, _, _ := capture(t, "doctor")

	if !strings.Contains(out, "keyless signing with no OIDC identity") {
		t.Errorf("doctor should warn that keyless cannot run unattended here:\n%s", out)
	}
	if !strings.Contains(out, "KILN_COSIGN_KEY") {
		t.Errorf("doctor should name the variable that fixes it:\n%s", out)
	}
}

func TestDoctorReportsKeyedSigning(t *testing.T) {
	repoWith(t, publishingPipeline)
	t.Setenv("KILN_COSIGN_KEY", "awskms://alias/kiln")

	out, _, _ := capture(t, "doctor")

	if !strings.Contains(out, "keyed signing") || !strings.Contains(out, "awskms://") {
		t.Errorf("doctor should report the signing mode in effect:\n%s", out)
	}
	if strings.Contains(out, "COSIGN_PASSWORD") {
		t.Errorf("a KMS key needs no passphrase; doctor should not ask for one:\n%s", out)
	}
}

// TestDoctorWarnsAboutAnUnpasswordedKeyFile covers the second hang: cosign
// prompts for an encrypted key's passphrase, and an unattended run cannot
// answer.
func TestDoctorWarnsAboutAnUnpasswordedKeyFile(t *testing.T) {
	repoWith(t, publishingPipeline)
	t.Setenv("KILN_COSIGN_KEY", "cosign.key")
	// t.Setenv registers the restore; unsetting after it is how a test reaches
	// the genuinely-absent case, which is the only one that prompts.
	t.Setenv("COSIGN_PASSWORD", "")
	if err := os.Unsetenv("COSIGN_PASSWORD"); err != nil {
		t.Fatal(err)
	}

	out, _, _ := capture(t, "doctor")

	if !strings.Contains(out, "COSIGN_PASSWORD is unset") {
		t.Errorf("doctor should warn about the passphrase prompt:\n%s", out)
	}
}

// TestDoctorAcceptsAnEmptyCosignPassword pins the distinction the warning
// depends on: an unencrypted key is configured by setting the variable empty,
// and warning there would send the operator to fix a working setup.
func TestDoctorAcceptsAnEmptyCosignPassword(t *testing.T) {
	repoWith(t, publishingPipeline)
	t.Setenv("KILN_COSIGN_KEY", "cosign.key")
	t.Setenv("COSIGN_PASSWORD", "")

	out, _, _ := capture(t, "doctor")

	if strings.Contains(out, "COSIGN_PASSWORD is unset") {
		t.Errorf("an empty passphrase is set, not unset:\n%s", out)
	}
}

func TestDoctorWithoutAPipelineIsAWarningNotAFailure(t *testing.T) {
	repoWith(t, "")

	out, _, code := capture(t, "doctor")

	// A library repository legitimately has nothing to publish.
	if !strings.Contains(out, "no .kiln.yaml") {
		t.Errorf("doctor output:\n%s", out)
	}
	if code == ExitConfig {
		t.Error("a missing pipeline must not be a configuration failure")
	}
}

func TestDoctorRejectsABrokenPipeline(t *testing.T) {
	repoWith(t, "apiVersion: kiln.klarlabs.de/v1\nkind: Pipeline\ndeploy:\n  target: prod\n")

	_, errOut, code := capture(t, "doctor")

	if code != ExitConfig {
		t.Errorf("code = %d, want %d", code, ExitConfig)
	}
	if !strings.Contains(errOut, "RollOps") {
		t.Errorf("stderr should point at RollOps: %q", errOut)
	}
}

func TestDoctorFlagsASHAOnlyTagPlan(t *testing.T) {
	// The loader catches this at parse time, so doctor reports a config error
	// rather than a plan finding. Either way the operator learns before a
	// build wastes their time.
	repoWith(t, `apiVersion: kiln.klarlabs.de/v1
kind: Pipeline
on: {push: [prove, publish]}
publish:
  - kind: image
    image: ghcr.io/x/y
    tags: [sha]
`)

	_, errOut, code := capture(t, "doctor")

	if code != ExitConfig {
		t.Errorf("code = %d", code)
	}
	if !strings.Contains(errOut, "imagePolicy") {
		t.Errorf("stderr should explain the RollOps consequence: %q", errOut)
	}
}

func TestDoctorWarnsWhenPullRequestsAskToPublish(t *testing.T) {
	repoWith(t, `apiVersion: kiln.klarlabs.de/v1
kind: Pipeline
on:
  pull_request: [prove, publish]
  push: [prove, publish]
publish:
  - kind: image
    image: ghcr.io/x/y
    tags: [sha, latest]
`)

	out, _, _ := capture(t, "doctor")

	// The engine suppresses it at run time; saying so here saves an afternoon.
	if !strings.Contains(out, "isolation policy always suppresses it") {
		t.Errorf("doctor should warn about the suppressed publish:\n%s", out)
	}
}

func TestDoctorOutsideARepository(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("KILN_DIR", "")

	_, errOut, code := capture(t, "doctor")

	if code != ExitConfig {
		t.Errorf("code = %d", code)
	}
	if !strings.Contains(errOut, "not a git repository") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestRunRequiresSHAAndEvent(t *testing.T) {
	repoWith(t, publishingPipeline)

	_, errOut, code := capture(t, "run", "--event", "push")
	if code != ExitUsage || !strings.Contains(errOut, "--sha is required") {
		t.Errorf("code = %d, stderr = %q", code, errOut)
	}

	_, errOut, code = capture(t, "run", "--sha", "HEAD")
	if code != ExitUsage || !strings.Contains(errOut, "--event must be") {
		t.Errorf("code = %d, stderr = %q", code, errOut)
	}

	_, errOut, code = capture(t, "run", "--sha", "HEAD", "--event", "release")
	if code != ExitUsage || !strings.Contains(errOut, "--event must be") {
		t.Errorf("code = %d, stderr = %q", code, errOut)
	}
}

func TestRunWithoutWardenIsAConfigFailure(t *testing.T) {
	repoWith(t, "")
	// A missing gate means the checks did not run, and a commit whose checks
	// did not run has not passed them.
	t.Setenv("KILN_WARDEN", "kiln-no-such-warden")

	_, errOut, code := capture(t, "run", "--sha", "HEAD", "--event", "push", "--quiet")

	if code != ExitConfig {
		t.Errorf("code = %d, want %d (a broken machine, not a rejected change)", code, ExitConfig)
	}
	if !strings.Contains(errOut, "kiln-no-such-warden") {
		t.Errorf("stderr should name the missing binary: %q", errOut)
	}
}

func TestRunGatePassing(t *testing.T) {
	repoWith(t, "")
	t.Setenv("KILN_WARDEN", fakeBin(t, "warden-pass", "exit 0"))

	out, _, code := capture(t, "run", "--sha", "HEAD", "--event", "push", "--quiet")

	if code != ExitOK {
		t.Fatalf("code = %d\n%s", code, out)
	}
	for _, want := range []string{"phase   succeeded", "commit  "} {
		if !strings.Contains(out, want) {
			t.Errorf("run output missing %q:\n%s", want, out)
		}
	}
}

func TestRunGateFailingUsesADistinctExitCode(t *testing.T) {
	repoWith(t, "")
	t.Setenv("KILN_WARDEN", fakeBin(t, "warden-fail", "echo 'lint failed' >&2; exit 1"))

	out, _, code := capture(t, "run", "--sha", "HEAD", "--event", "push", "--quiet")

	// A rejected change (2) and a broken machine (3) are different problems
	// for different people, and cron should be able to tell them apart.
	if code != ExitFailed {
		t.Errorf("code = %d, want %d\n%s", code, ExitFailed, out)
	}
	if !strings.Contains(out, "phase   failed") {
		t.Errorf("run output:\n%s", out)
	}
}

func TestRunResolvesHEAD(t *testing.T) {
	repo := repoWith(t, "")
	t.Setenv("KILN_WARDEN", fakeBin(t, "warden-pass", "exit 0"))

	out, _, _ := capture(t, "run", "--sha", "HEAD", "--event", "push", "--quiet")

	// The ledger and the image tag must record the real commit, not a name
	// that will mean something else tomorrow.
	if !strings.Contains(out, repo.Head()[:7]) {
		t.Errorf("run did not resolve HEAD to %s:\n%s", repo.Head()[:7], out)
	}
}

func TestRunOnAPullRequestWithoutATokenIsTreatedAsAFork(t *testing.T) {
	repoWith(t, publishingPipeline)
	t.Setenv("KILN_WARDEN", fakeBin(t, "warden-pass", "exit 0"))

	out, _, code := capture(t, "run", "--sha", "HEAD", "--event", "pull_request", "--quiet")

	if code != ExitOK {
		t.Fatalf("code = %d\n%s", code, out)
	}
	if !strings.Contains(out, "fork") {
		t.Errorf("an unidentifiable pull request must be treated as a fork:\n%s", out)
	}
	// And a fork never publishes, whatever the pipeline says.
	if strings.Contains(out, "digest") {
		t.Errorf("a fork pull request published:\n%s", out)
	}
}

func TestRunDryPlansWithoutDocker(t *testing.T) {
	repoWith(t, publishingPipeline)
	t.Setenv("KILN_WARDEN", fakeBin(t, "warden-pass", "exit 0"))
	t.Setenv("KILN_DRY", "1")

	out, _, code := capture(t, "run", "--sha", "HEAD", "--event", "push", "--quiet")

	if code != ExitOK {
		t.Fatalf("code = %d\n%s", code, out)
	}
	if !strings.Contains(out, "ghcr.io/felixgeelhaar/glossa-api:latest") {
		t.Errorf("a dry run should still report the tag plan:\n%s", out)
	}
}

func TestStatusOnAnEmptyLedger(t *testing.T) {
	repoWith(t, "")

	_, errOut, code := capture(t, "status")

	if code != ExitError {
		t.Errorf("code = %d", code)
	}
	// A fresh checkout has simply not built anything yet.
	if !strings.Contains(errOut, "no runs recorded") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestStatusAfterARun(t *testing.T) {
	repo := repoWith(t, "")
	t.Setenv("KILN_WARDEN", fakeBin(t, "warden-pass", "exit 0"))
	if _, _, code := capture(t, "run", "--sha", "HEAD", "--event", "push", "--quiet"); code != ExitOK {
		t.Fatalf("run failed with %d", code)
	}

	out, _, code := capture(t, "status")

	if code != ExitOK {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(out, repo.Head()) || !strings.Contains(out, "succeeded") {
		t.Errorf("status output:\n%s", out)
	}
}

func TestStatusJSON(t *testing.T) {
	repoWith(t, "")
	t.Setenv("KILN_WARDEN", fakeBin(t, "warden-pass", "exit 0"))
	_, _, _ = capture(t, "run", "--sha", "HEAD", "--event", "push", "--quiet")

	out, _, code := capture(t, "status", "--json")

	if code != ExitOK {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(out, `"phase": "succeeded"`) {
		t.Errorf("json output:\n%s", out)
	}
}

func TestStatusUnknownRunID(t *testing.T) {
	repoWith(t, "")

	_, errOut, code := capture(t, "status", "run-does-not-exist")

	if code != ExitError || !strings.Contains(errOut, "run-does-not-exist") {
		t.Errorf("code = %d, stderr = %q", code, errOut)
	}
}

func TestLedgerLandsInsideTheRepository(t *testing.T) {
	repo := repoWith(t, "")
	t.Setenv("KILN_WARDEN", fakeBin(t, "warden-pass", "exit 0"))
	// A cron entry whose working directory differs from the checkout must not
	// keep a second, always-empty ledger.
	t.Chdir(t.TempDir())

	if _, _, code := capture(t, "run", "--sha", "HEAD", "--event", "push", "--dir", repo.Dir, "--quiet"); code != ExitOK {
		t.Fatalf("run failed with %d", code)
	}

	if _, err := os.Stat(filepath.Join(repo.Dir, ".kiln", "state.json")); err != nil {
		t.Errorf("ledger not written inside the repository: %v", err)
	}
}

func TestWatchOnceOnAFreshClone(t *testing.T) {
	upstream := gittest.New(t)
	upstream.Commit("first", "app.txt", "one\n")
	local := upstream.Clone(t)
	t.Chdir(local.Dir)
	for _, k := range []string{"GITHUB_TOKEN", "GH_TOKEN", "KILN_DB", "KILN_DIR"} {
		t.Setenv(k, "")
	}
	t.Setenv("KILN_LOG_LEVEL", "fatal")
	t.Setenv("KILN_WARDEN", fakeBin(t, "warden-pass", "exit 0"))

	out, _, code := capture(t, "watch", "--once")

	if code != ExitOK {
		t.Fatalf("code = %d\n%s", code, out)
	}
	if !strings.Contains(out, "built") {
		t.Errorf("watch output:\n%s", out)
	}

	// The second tick must be a no-op, or every cron minute costs a build.
	out, _, code = capture(t, "watch", "--once")
	if code != ExitOK {
		t.Fatalf("second tick: code = %d", code)
	}
	if !strings.Contains(out, "skip") {
		t.Errorf("second tick rebuilt an unchanged head:\n%s", out)
	}
}

func TestWatchDryRunRunsNothing(t *testing.T) {
	upstream := gittest.New(t)
	upstream.Commit("first", "app.txt", "one\n")
	local := upstream.Clone(t)
	t.Chdir(local.Dir)
	t.Setenv("KILN_LOG_LEVEL", "fatal")
	t.Setenv("KILN_WARDEN", "kiln-no-such-warden")

	out, _, code := capture(t, "watch", "--once", "--dry-run")

	// Even with no gate installed, a dry run must succeed: it runs nothing.
	if code != ExitOK {
		t.Fatalf("code = %d\n%s", code, out)
	}
	if !strings.Contains(out, "plan") {
		t.Errorf("dry-run output:\n%s", out)
	}
}

func TestWatchRejectsOnceWithEvery(t *testing.T) {
	repoWith(t, "")

	_, errOut, code := capture(t, "watch", "--once", "--every", "1m")

	if code != ExitUsage || !strings.Contains(errOut, "mutually exclusive") {
		t.Errorf("code = %d, stderr = %q", code, errOut)
	}
}

func TestWatchRejectsABadInterval(t *testing.T) {
	repoWith(t, "")

	_, errOut, code := capture(t, "watch", "--every", "soon")

	if code != ExitUsage || !strings.Contains(errOut, "not a duration") {
		t.Errorf("code = %d, stderr = %q", code, errOut)
	}
}

func TestMCPRequiresTheServeSubcommand(t *testing.T) {
	repoWith(t, "")

	_, errOut, code := capture(t, "mcp")

	if code != ExitUsage || !strings.Contains(errOut, "kiln mcp serve") {
		t.Errorf("code = %d, stderr = %q", code, errOut)
	}
}

// stubTool puts a do-nothing executable of the given name on PATH.
//
// Any test whose subject branches on whether a binary exists must say which
// answer it wants. Reading the developer's PATH instead is how a test passes
// on a laptop with the whole toolchain installed and fails on a bare runner.
func stubTool(t *testing.T, name string) {
	t.Helper()
	fakeBin(t, name, "exit 0")
}

// fakeBin writes an executable shell script and returns its name, adding its
// directory to PATH. It stands in for warden so the CLI tests can exercise
// both verdicts without installing anything.
func fakeBin(t *testing.T, name, body string) string {
	t.Helper()
	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(binDir, name)
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return name
}

func TestDoctorConfigOnlySkipsTheToolchain(t *testing.T) {
	repoWith(t, publishingPipeline)
	// A reviewer's laptop has no cosign. Checking a pipeline's schema there
	// must not require installing the world.
	t.Setenv("KILN_WARDEN", "kiln-no-such-warden")

	out, _, code := capture(t, "doctor", "--config-only")

	if code != ExitOK {
		t.Fatalf("code = %d\n%s", code, out)
	}
	if strings.Contains(out, "toolchain") || strings.Contains(out, "credentials") {
		t.Errorf("--config-only still checked the environment:\n%s", out)
	}
	// It must still validate the thing it is for.
	if !strings.Contains(out, "artifacts") {
		t.Errorf("--config-only dropped the artifact plan:\n%s", out)
	}
}

func TestDoctorConfigOnlyStillRejectsABadPipeline(t *testing.T) {
	repoWith(t, `apiVersion: kiln.klarlabs.de/v1
kind: Pipeline
on: {push: [prove, publish]}
publish:
  - kind: image
    image: ghcr.io/x/y
    tags: [sha]
`)

	_, errOut, code := capture(t, "doctor", "--config-only")

	if code != ExitConfig || !strings.Contains(errOut, "imagePolicy") {
		t.Errorf("code = %d, stderr = %q", code, errOut)
	}
}

func TestQuietDoesNotBreakACommandThatPrints(t *testing.T) {
	repoWith(t, "")
	// A gate that writes to stderr is the common case, and it is what exposes
	// a typed-nil writer: a nil *os.File inside an io.Writer reads as "output
	// configured" and then fails on the first byte with "invalid argument".
	t.Setenv("KILN_WARDEN", fakeBin(t, "warden-chatty", "echo running checks >&2; echo done; exit 0"))

	out, _, code := capture(t, "run", "--sha", "HEAD", "--event", "push", "--quiet")

	if code != ExitOK {
		t.Fatalf("code = %d\n%s", code, out)
	}
	if !strings.Contains(out, "phase   succeeded") {
		t.Errorf("run output:\n%s", out)
	}
}

func TestNonQuietRunAlsoWorksWithAChattyGate(t *testing.T) {
	repoWith(t, "")
	t.Setenv("KILN_WARDEN", fakeBin(t, "warden-chatty", "echo running checks >&2; echo done; exit 0"))

	out, _, code := capture(t, "run", "--sha", "HEAD", "--event", "push")

	if code != ExitOK {
		t.Fatalf("code = %d\n%s", code, out)
	}
}

func TestQuietWatchDoesNotBreakACommandThatPrints(t *testing.T) {
	upstream := gittest.New(t)
	upstream.Commit("first", "app.txt", "one\n")
	local := upstream.Clone(t)
	t.Chdir(local.Dir)
	for _, k := range []string{"GITHUB_TOKEN", "GH_TOKEN", "KILN_DB", "KILN_DIR"} {
		t.Setenv(k, "")
	}
	t.Setenv("KILN_LOG_LEVEL", "fatal")
	t.Setenv("KILN_WARDEN", fakeBin(t, "warden-chatty", "echo running checks >&2; exit 0"))

	out, _, code := capture(t, "watch", "--once", "--quiet")

	if code != ExitOK {
		t.Fatalf("code = %d\n%s", code, out)
	}
	if !strings.Contains(out, "built") {
		t.Errorf("watch output:\n%s", out)
	}
}

func TestVerifyRequiresAReference(t *testing.T) {
	repoWith(t, "")

	_, errOut, code := capture(t, "verify")

	if code != ExitUsage || !strings.Contains(errOut, "kiln verify <image-ref>") {
		t.Errorf("code = %d, stderr = %q", code, errOut)
	}
}

func TestVerifyRefusesAnUnpinnedKeylessCheck(t *testing.T) {
	repoWith(t, "")
	// verify short-circuits when cosign is absent, so a test about what it
	// does *with* cosign has to supply one. Leaving this to the developer's
	// PATH is how the last two CI failures happened.
	stubTool(t, "cosign")

	out, _, code := capture(t, "verify", "ghcr.io/x/y@sha256:aaa")

	// "Signed by somebody" is not a security property, and cosign would refuse
	// this anyway — better to say why than to pass the refusal through.
	if code == ExitOK {
		t.Errorf("an unpinned keyless verification must not succeed:\n%s", out)
	}
	if !strings.Contains(out, "proves nothing") {
		t.Errorf("output should explain the refusal:\n%s", out)
	}
}

func TestVerifyWithoutCosignChecksNothing(t *testing.T) {
	repoWith(t, "")
	// An empty PATH, asserted rather than assumed: this is the "nothing could
	// be checked" path.
	t.Setenv("PATH", t.TempDir())

	out, _, code := capture(t, "verify", "ghcr.io/x/y@sha256:aaa", "--key", "cosign.pub")

	if code == ExitOK {
		t.Error("a verification that checked nothing must not exit zero")
	}
	if !strings.Contains(out, "cosign is not installed") {
		t.Errorf("output should name what was missing:\n%s", out)
	}
}

// holdRepoLock takes the repository lock from another process, which is the
// only way to model the cron overlap this exists for: flock is per open file
// description, so locking twice inside one process proves nothing.
//
// It re-execs this test binary rather than `go run`-ing a helper, because the
// tests chdir into throwaway git repositories and `go run` needs a module.
func holdRepoLock(t *testing.T, dir string) {
	t.Helper()

	cmd := exec.Command(os.Args[0], "-test.run=TestRepoLockHolderHelper", "-test.timeout=10m")
	cmd.Env = append(os.Environ(),
		"KILN_CLI_LOCK_HELPER=1",
		"KILN_CLI_LOCK_DIR="+dir,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the lock holder: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if lock.ReadHolder(lock.PathFor(dir)).PID != 0 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("the holder never took the lock")
}

// TestRepoLockHolderHelper is the body of the process holdRepoLock starts. It
// returns immediately under a normal run.
func TestRepoLockHolderHelper(t *testing.T) {
	if os.Getenv("KILN_CLI_LOCK_HELPER") == "" {
		t.Skip("helper process entry point")
	}
	l, err := lock.TryAcquire(lock.PathFor(os.Getenv("KILN_CLI_LOCK_DIR")), "kiln run --sha deadbee")
	if err != nil {
		t.Fatalf("helper could not take the lock: %v", err)
	}
	defer func() { _ = l.Release() }()
	// A timer keeps Go's deadlock detector from killing us and freeing the
	// lock, which would make every caller's assertion meaningless.
	time.Sleep(5 * time.Minute)
}

func TestABusyRepositoryRefusesARun(t *testing.T) {
	repo := repoWith(t, "")
	holdRepoLock(t, repo.Dir)

	out, errOut, code := capture(t, "run", "--sha", "HEAD", "--event", "push", "--quiet")

	// An explicit command must not silently do nothing.
	if code != ExitBusy {
		t.Errorf("code = %d, want %d\n%s%s", code, ExitBusy, out, errOut)
	}
	if !strings.Contains(errOut, "holds this repository") {
		t.Errorf("stderr = %q", errOut)
	}
	// The message should name who, so the operator can go look.
	if !strings.Contains(errOut, "kiln run --sha deadbee") {
		t.Errorf("stderr should name the holder: %q", errOut)
	}
}

func TestABusyRepositoryIsNotAWatchFailure(t *testing.T) {
	upstream := gittest.New(t)
	upstream.Commit("first", "app.txt", "one\n")
	local := upstream.Clone(t)
	t.Chdir(local.Dir)
	for _, k := range []string{"GITHUB_TOKEN", "GH_TOKEN", "KILN_DB", "KILN_DIR"} {
		t.Setenv(k, "")
	}
	t.Setenv("KILN_LOG_LEVEL", "fatal")
	t.Setenv("KILN_WARDEN", fakeBin(t, "warden-pass", "exit 0"))
	holdRepoLock(t, local.Dir)

	out, _, code := capture(t, "watch", "--once")

	// Under cron an overlap is expected. Exiting non-zero would page somebody
	// every time a build outran the schedule.
	if code != ExitOK {
		t.Errorf("code = %d, want 0 for an expected overlap\n%s", code, out)
	}
	if !strings.Contains(out, "busy") {
		t.Errorf("output should say why nothing happened:\n%s", out)
	}
}

func TestADryRunIgnoresTheLock(t *testing.T) {
	upstream := gittest.New(t)
	upstream.Commit("first", "app.txt", "one\n")
	local := upstream.Clone(t)
	t.Chdir(local.Dir)
	t.Setenv("KILN_LOG_LEVEL", "fatal")
	t.Setenv("KILN_WARDEN", fakeBin(t, "warden-pass", "exit 0"))
	holdRepoLock(t, local.Dir)

	out, _, code := capture(t, "watch", "--once", "--dry-run")

	// Refusing to show an operator the plan because a build is in flight would
	// be obstructive at exactly the moment they want to look.
	if code != ExitOK || !strings.Contains(out, "plan") {
		t.Errorf("code = %d, out = %q", code, out)
	}
}

func TestReadOnlyCommandsIgnoreTheLock(t *testing.T) {
	repo := repoWith(t, publishingPipeline)
	holdRepoLock(t, repo.Dir)

	// status, doctor and verify must stay usable during a build — that is when
	// an operator most wants them.
	if _, _, code := capture(t, "doctor"); code == ExitBusy {
		t.Error("doctor blocked on the lock")
	}
	if _, _, code := capture(t, "status"); code == ExitBusy {
		t.Error("status blocked on the lock")
	}
}

// fleet builds n clones sharing an upstream, the shape a build box watching
// several services actually has.
func fleet(t *testing.T, n int) (root string, dirs []string) {
	t.Helper()
	root = t.TempDir()
	for i := range n {
		upstream := gittest.New(t)
		upstream.Commit("first", "app.txt", "one\n")

		dir := filepath.Join(root, "svc"+strconv.Itoa(i))
		cmd := exec.Command("git", "clone", "-q", upstream.Dir, dir)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("clone: %v\n%s", err, out)
		}
		dirs = append(dirs, dir)
	}
	for _, k := range []string{"GITHUB_TOKEN", "GH_TOKEN", "KILN_DB", "KILN_DIR"} {
		t.Setenv(k, "")
	}
	t.Setenv("KILN_LOG_LEVEL", "fatal")
	t.Setenv("KILN_WARDEN", fakeBin(t, "warden-pass", "exit 0"))
	return root, dirs
}

func TestWatchAFleetFromOneProcess(t *testing.T) {
	root, dirs := fleet(t, 3)

	out, _, code := capture(t, "watch", "--once", "--repos", filepath.Join(root, "svc*"))

	if code != ExitOK {
		t.Fatalf("code = %d\n%s", code, out)
	}
	for _, dir := range dirs {
		if !strings.Contains(out, dir) {
			t.Errorf("output does not mention %s:\n%s", dir, out)
		}
	}
	if strings.Count(out, "built") != len(dirs) {
		t.Errorf("expected every repository to build:\n%s", out)
	}
}

func TestACommaSeparatedFleet(t *testing.T) {
	_, dirs := fleet(t, 2)

	out, _, code := capture(t, "watch", "--once", "--repos", strings.Join(dirs, ","))

	if code != ExitOK {
		t.Fatalf("code = %d\n%s", code, out)
	}
	if strings.Count(out, "==") != 2 {
		t.Errorf("expected both repositories:\n%s", out)
	}
}

func TestOneBadRepositoryDoesNotStopTheFleet(t *testing.T) {
	root, dirs := fleet(t, 2)
	// A directory that is not a repository at all — the realistic version is
	// somebody's stray folder matching the glob.
	broken := filepath.Join(root, "svc9")
	if err := os.MkdirAll(broken, 0o750); err != nil {
		t.Fatal(err)
	}

	out, _, code := capture(t, "watch", "--once", "--repos", filepath.Join(root, "svc*"))

	// The point of a fleet in one process is that it is not a single point of
	// failure.
	if code != ExitFailed {
		t.Errorf("code = %d, want the failure counted", code)
	}
	if strings.Count(out, "built") != len(dirs) {
		t.Errorf("the healthy repositories should still have built:\n%s", out)
	}
	if !strings.Contains(out, "not a git repository") {
		t.Errorf("output should say what was wrong:\n%s", out)
	}
}

func TestFleetSkipsABusyRepository(t *testing.T) {
	root, dirs := fleet(t, 2)
	holdRepoLock(t, dirs[0])

	out, _, code := capture(t, "watch", "--once", "--repos", filepath.Join(root, "svc*"))

	if code != ExitOK {
		t.Errorf("code = %d, a busy member is not a fleet failure\n%s", code, out)
	}
	if !strings.Contains(out, "busy") {
		t.Errorf("output should name the busy repository:\n%s", out)
	}
	// Each repository has its own lock, so the others carry on.
	if strings.Count(out, "built") != 1 {
		t.Errorf("the unlocked repository should still have built:\n%s", out)
	}
}

func TestReposAndDirAreMutuallyExclusive(t *testing.T) {
	root, _ := fleet(t, 1)

	_, errOut, code := capture(t, "watch", "--once", "--repos", root, "--dir", root)

	if code != ExitUsage || !strings.Contains(errOut, "mutually exclusive") {
		t.Errorf("code = %d, stderr = %q", code, errOut)
	}
}

func TestAGlobThatMatchesNothingIsAUsageError(t *testing.T) {
	repoWith(t, "")

	_, errOut, code := capture(t, "watch", "--once", "--repos", filepath.Join(t.TempDir(), "nothing*"))

	// Silently watching zero repositories is how somebody discovers a typo
	// three weeks later.
	if code != ExitUsage || !strings.Contains(errOut, "matched no directories") {
		t.Errorf("code = %d, stderr = %q", code, errOut)
	}
}
