package execx

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestSystemRunCapturesOutput(t *testing.T) {
	res, err := NewSystem().Run(t.Context(), Cmd{
		Name: "sh", Args: []string{"-c", "printf out; printf err >&2"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Stdout != "out" || res.Stderr != "err" {
		t.Errorf("Result = %+v", res)
	}
}

func TestSystemRunNonZeroIsAnExitError(t *testing.T) {
	_, err := NewSystem().Run(t.Context(), Cmd{
		Name: "sh", Args: []string{"-c", "echo boom >&2; exit 3"},
	})

	code, ok := ExitCode(err)
	if !ok || code != 3 {
		t.Fatalf("ExitCode = (%d, %v), want (3, true)", code, ok)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error should quote stderr, got %v", err)
	}
}

func TestSystemRunMissingBinary(t *testing.T) {
	_, err := NewSystem().Run(t.Context(), Cmd{Name: "kiln-no-such-binary-xyz"})

	var nf *NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("err = %v, want NotFoundError", err)
	}
	// A missing binary is not an exit code; conflating the two would let a
	// caller read "warden is not installed" as "warden said no".
	if _, ok := ExitCode(err); ok {
		t.Error("a missing binary must not report an exit code")
	}
}

func TestSystemRunHonoursDirAndEnv(t *testing.T) {
	dir := t.TempDir()
	res, err := NewSystem().Run(t.Context(), Cmd{
		Name: "sh", Args: []string{"-c", "pwd; printf %s \"$KILN_PROBE\""},
		Dir: dir, Env: []string{"KILN_PROBE=set", "PATH=" + pathEnv(t)},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Stdout, "set") {
		t.Errorf("env not applied: %q", res.Stdout)
	}
}

func TestSystemRunRespectsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := NewSystem().Run(ctx, Cmd{Name: "sh", Args: []string{"-c", "sleep 5"}})
	if err == nil {
		t.Fatal("want an error from a cancelled context")
	}
	// Cancellation is not a verdict about the command's subject.
	if _, ok := ExitCode(err); ok {
		t.Errorf("cancellation reported as an exit code: %v", err)
	}
}

func TestScrubRemovesCredentials(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"HOME=/home/build",
		"GITHUB_TOKEN=ghp_xxx",
		"KILN_TRUSTED_KEYS=AAAA",
		"MY_APP_SECRET=hunter2",
		"REGISTRY_PASSWORD=hunter2",
		"AWS_SECRET_ACCESS_KEY=xxx",
		"SSH_AUTH_SOCK=/tmp/agent.1",
		"CI=true",
		"malformed-no-equals",
	}

	got := Scrub(in)

	for _, keep := range []string{"PATH=/usr/bin", "HOME=/home/build", "CI=true"} {
		if !slices.Contains(got, keep) {
			t.Errorf("Scrub dropped an ordinary variable: %s", keep)
		}
	}
	for _, kv := range got {
		name, _, _ := strings.Cut(kv, "=")
		if IsSecretVar(name) {
			t.Errorf("Scrub kept a credential: %s", kv)
		}
	}
	if slices.Contains(got, "malformed-no-equals") {
		t.Error("Scrub kept a malformed entry")
	}
}

func TestIsSecretVarIsCaseInsensitive(t *testing.T) {
	for _, name := range []string{"github_token", "Api_Key", "my_password", "DB_CREDENTIALS"} {
		if !IsSecretVar(name) {
			t.Errorf("IsSecretVar(%q) = false", name)
		}
	}
	for _, name := range []string{"PATH", "HOME", "GOFLAGS", "KILN_DB", "KILN_DRY"} {
		if IsSecretVar(name) {
			t.Errorf("IsSecretVar(%q) = true", name)
		}
	}
}

func TestExitErrorTailsLongOutput(t *testing.T) {
	var b strings.Builder
	for i := range 40 {
		b.WriteString("line ")
		b.WriteByte(byte('a' + i%26))
		b.WriteByte('\n')
	}
	err := &ExitError{Cmd: "docker build", Code: 1, Stderr: b.String()}

	msg := err.Error()
	if !strings.HasPrefix(msg, "docker build: exit 1") {
		t.Errorf("message = %q", msg)
	}
	if !strings.Contains(msg, "…") {
		t.Error("long output should be elided, not quoted whole")
	}
}

func TestFakeMatchesLongestPrefix(t *testing.T) {
	f := NewFake().
		On("docker", Response{Stdout: "generic"}).
		On("docker push", Response{Stdout: "pushed"})

	res, err := f.Run(t.Context(), Cmd{Name: "docker", Args: []string{"push", "img"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Output() != "pushed" {
		t.Errorf("Output = %q, want pushed (the more specific prefix)", res.Output())
	}

	res, _ = f.Run(t.Context(), Cmd{Name: "docker", Args: []string{"build", "."}})
	if res.Output() != "generic" {
		t.Errorf("Output = %q, want generic", res.Output())
	}
}

func TestFakeNonZeroLooksLikeARealFailure(t *testing.T) {
	f := NewFake().On("warden verify", Response{ExitCode: 1, Stderr: "no note"})

	_, err := f.Run(t.Context(), Cmd{Name: "warden", Args: []string{"verify"}})
	code, ok := ExitCode(err)
	if !ok || code != 1 {
		t.Fatalf("ExitCode = (%d, %v), want (1, true)", code, ok)
	}
}

func TestFakeRecordsCalls(t *testing.T) {
	f := NewFake()
	_, _ = f.Run(t.Context(), Cmd{Name: "git", Args: []string{"fetch", "origin"}})
	_, _ = f.Run(t.Context(), Cmd{Name: "git", Args: []string{"rev-parse", "HEAD"}})

	if !f.Ran("git fetch") || f.Count("git") != 2 {
		t.Errorf("call recording wrong: %s", f.Transcript())
	}
	if c := f.Find("git rev-parse"); c == nil || c.Name != "git" {
		t.Errorf("Find missed the call: %s", f.Transcript())
	}
}

func TestFakeAbsentBinary(t *testing.T) {
	f := NewFake().Absent("cosign")

	_, err := f.Run(t.Context(), Cmd{Name: "cosign", Args: []string{"sign"}})
	var nf *NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("err = %v, want NotFoundError", err)
	}
}

func pathEnv(t *testing.T) string {
	t.Helper()
	return "/usr/bin:/bin:/usr/local/bin"
}
