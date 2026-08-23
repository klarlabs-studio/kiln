package task_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.klarlabs.de/kiln/internal/application/ports"

	"gopkg.in/yaml.v3"

	"go.klarlabs.de/kiln/internal/domain/config"
	"go.klarlabs.de/kiln/internal/domain/isolation"
	"go.klarlabs.de/kiln/internal/infrastructure/execx"
	"go.klarlabs.de/kiln/internal/infrastructure/task"
)

// trusted is the policy a push gets: the command may see the environment.
var trusted = isolation.Policy{Secrets: true, Publish: true, Skip: true}

func run(t *testing.T, dir string, def config.Task, policy isolation.Policy) (ports.TaskResult, string) {
	t.Helper()
	var out strings.Builder
	res := task.New(execx.NewSystem()).Run(t.Context(), ports.TaskRequest{
		Name: "check", Task: def, Dir: dir,
		SHA: "8115748887775797df0398ed27080998f4d0c8d7", Ref: "refs/heads/main", Event: "push",
		Policy: policy, Output: &out,
	})
	return res, out.String()
}

func TestATaskRunsInTheWorktree(t *testing.T) {
	dir := t.TempDir()

	res, out := run(t, dir, config.Task{Run: "pwd"}, trusted)
	if res.Err != nil {
		t.Fatalf("Run: %v", res.Err)
	}

	// Not the operator's working copy — the same rule the gate and the build
	// follow, for the same reason.
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, resolved) {
		t.Errorf("ran in %q, want the worktree %q", strings.TrimSpace(out), resolved)
	}
}

func TestAScriptThatFailsHalfwayFails(t *testing.T) {
	// Without -e the shell runs on and exits with the status of the last line,
	// so this would report success having not written the file.
	res, _ := run(t, t.TempDir(), config.Task{Run: "false\necho recovered"}, trusted)

	if res.Err == nil {
		t.Fatal("a script whose first command failed reported success")
	}
	if !errors.Is(res.Err, ports.ErrTaskFailed) {
		t.Errorf("err = %v, want ErrTaskFailed", res.Err)
	}
	if res.OK() {
		t.Error("OK() true for a failed task with no allow_failure")
	}
}

func TestAnUnsetVariableIsAnError(t *testing.T) {
	// -u. The classic version of this bug deletes $PREFIX/ when PREFIX is a
	// typo, having expanded to nothing.
	res, _ := run(t, t.TempDir(), config.Task{Run: `echo "${TYPOED_VAR}"`}, trusted)

	if res.Err == nil {
		t.Error("a typo'd variable expanded to empty and the task passed")
	}
}

func TestAToleratedFailureDoesNotFailTheRun(t *testing.T) {
	res, _ := run(t, t.TempDir(), config.Task{Run: "false", AllowFailure: true}, trusted)

	if res.Err == nil {
		t.Fatal("the failure was swallowed rather than recorded")
	}
	if !res.Tolerated || !res.OK() {
		t.Errorf("tolerated=%v OK=%v, want both true", res.Tolerated, res.OK())
	}
}

func TestTheRunIsDescribedToTheCommand(t *testing.T) {
	res, out := run(t, t.TempDir(), config.Task{
		Run: `echo "$KILN_SHA $KILN_REF $KILN_EVENT $KILN_TASK"`,
	}, trusted)
	if res.Err != nil {
		t.Fatal(res.Err)
	}

	for _, want := range []string{"8115748", "refs/heads/main", "push", "check"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q missing %q", strings.TrimSpace(out), want)
		}
	}
}

func TestAnUntrustedHeadCannotReadSecrets(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "super-secret-value")

	// A fork pull request runs repository-authored commands. If it could read
	// the environment, every secret on the box would be one pull request away.
	fork := isolation.For(isolation.EventPullRequest, true)
	res, out := run(t, t.TempDir(), config.Task{Run: `echo "token=[${GITHUB_TOKEN:-absent}]"`}, fork)
	if res.Err != nil {
		t.Fatal(res.Err)
	}

	if strings.Contains(out, "super-secret-value") {
		t.Error("a fork pull request's task read GITHUB_TOKEN")
	}
	if !strings.Contains(out, "absent") {
		t.Errorf("output = %q, want the variable scrubbed", strings.TrimSpace(out))
	}
}

func TestATrustedRunKeepsItsEnvironment(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "a-real-token")

	// The other half: a push on your own repository is the case where a task
	// legitimately needs the token — to open a pull request, upload a scan.
	res, out := run(t, t.TempDir(), config.Task{Run: `echo "token=[${GITHUB_TOKEN:-absent}]"`}, trusted)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !strings.Contains(out, "a-real-token") {
		t.Errorf("output = %q, want the token available on a trusted event", strings.TrimSpace(out))
	}
}

func TestWorkdirIsRelativeToTheWorktree(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "site"), 0o750); err != nil {
		t.Fatal(err)
	}

	res, out := run(t, dir, config.Task{Run: "pwd", Workdir: "site"}, trusted)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "/site") {
		t.Errorf("ran in %q, want the site subdirectory", strings.TrimSpace(out))
	}
}

func TestDescribeSaysWhenATaskRuns(t *testing.T) {
	var every config.Duration
	if err := yamlDuration(t, "24h", &every); err != nil {
		t.Fatal(err)
	}

	got := task.Describe("remediate", config.Task{
		On: []string{"schedule"}, Every: every, AllowFailure: true,
	})
	// "24h0m0s" is what Go prints and it reads like a broken config file.
	if !strings.Contains(got, "every 24h") || strings.Contains(got, "0m0s") {
		t.Errorf("Describe = %q", got)
	}
	if !strings.Contains(got, "tolerated") {
		t.Errorf("Describe = %q, want the tolerated failure surfaced", got)
	}
}

// yamlDuration builds a config.Duration through the parser, since the type has
// no exported constructor — the parse path is the only way in, deliberately.
func yamlDuration(t *testing.T, s string, d *config.Duration) error {
	t.Helper()
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(s), &node); err != nil {
		return err
	}
	return d.UnmarshalYAML(node.Content[0])
}
