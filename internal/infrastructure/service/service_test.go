package service_test

import (
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"go.klarlabs.de/kiln/internal/config"
	"go.klarlabs.de/kiln/internal/infrastructure/execx"
	"go.klarlabs.de/kiln/internal/infrastructure/service"
)

func postgres() map[string]config.Service {
	return map[string]config.Service{
		"postgres": {
			Image: "postgres:16",
			Port:  5432,
			Env:   map[string]string{"POSTGRES_PASSWORD": "test"},
			Ready: "pg_isready -U postgres",
		},
	}
}

// up scripts a docker that starts cleanly and publishes an ephemeral port.
func up(t *testing.T) *execx.Fake {
	t.Helper()
	f := execx.NewFake()
	f.On("docker run", execx.Response{Stdout: "c0ffee\n"})
	f.On("docker port", execx.Response{Stdout: "127.0.0.1:54190\n"})
	f.On("docker exec", execx.Response{})
	f.On("docker rm", execx.Response{})
	return f
}

func TestTheAddressDockerChoseIsExported(t *testing.T) {
	f := up(t)

	set, err := service.New(f, nil).Start(t.Context(), postgres(), "run-1")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer set.Stop()

	env := strings.Join(set.Env(), " ")
	// The port is docker's, read back — never the one in the config. A box
	// runs many repositories, and two pipelines both binding 5432 would fail
	// in a way that reads as a flaky test.
	if !strings.Contains(env, "KILN_SERVICE_POSTGRES_PORT=54190") {
		t.Errorf("env = %q, want the published port", env)
	}
	if !strings.Contains(env, "KILN_SERVICE_POSTGRES_HOST=127.0.0.1") {
		t.Errorf("env = %q, want the host", env)
	}
}

func TestThePortIsPublishedOnAnEphemeralHostPort(t *testing.T) {
	f := up(t)

	set, err := service.New(f, nil).Start(t.Context(), postgres(), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	defer set.Stop()

	run := findCall(t, f, "docker run")
	// 127.0.0.1:: — bound to loopback, port chosen by the kernel. A fixed host
	// port is the collision; a wildcard bind would expose a test database to
	// the network.
	if !strings.Contains(run, "127.0.0.1::5432") {
		t.Errorf("docker run = %q, want an ephemeral loopback publish", run)
	}
	if !strings.Contains(run, "--env POSTGRES_PASSWORD=test") {
		t.Errorf("docker run = %q, want the configured environment", run)
	}
}

func TestReadinessIsWaitedFor(t *testing.T) {
	f := up(t)

	set, err := service.New(f, nil).Start(t.Context(), postgres(), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	defer set.Stop()

	// A gate that starts before the database accepts connections fails in a
	// way nobody debugs twice: the error is in the test output and the cause
	// is a race in the harness.
	probe := findCall(t, f, "docker exec")
	if !strings.Contains(probe, "pg_isready") {
		t.Errorf("probe = %q", probe)
	}
}

func TestAServiceThatNeverBecomesReadyFails(t *testing.T) {
	f := up(t)
	f.On("docker exec", execx.Response{ExitCode: 1, Stderr: "connection refused"})

	timeout := postgres()
	svc := timeout["postgres"]
	svc.ReadyTimeout = shortTimeout(t)
	timeout["postgres"] = svc

	_, err := service.New(f, nil).Start(t.Context(), timeout, "run-1")

	if !errors.Is(err, service.ErrNotReady) {
		t.Fatalf("err = %v, want ErrNotReady", err)
	}
	// And the container it started is gone: a half-started set left behind
	// holds a port on a box that is about to try the whole thing again.
	if !called(f, "docker rm") {
		t.Error("the container was leaked after a readiness failure")
	}
}

func TestAFailedStartTearsDownWhatWasAlreadyUp(t *testing.T) {
	f := up(t)
	// Two services; the second image does not exist.
	f.On("docker run --detach --rm --name kiln-1-redis", execx.Response{
		ExitCode: 125, Stderr: "no such image",
	})

	services := postgres()
	services["redis"] = config.Service{Image: "redis:doesnotexist", Port: 6379}

	_, err := service.New(f, nil).Start(t.Context(), services, "run-1")
	if err == nil {
		t.Fatal("a failed start reported success")
	}
	if !called(f, "docker rm") {
		t.Error("postgres was left running after redis failed to start")
	}
}

func TestStopIsIdempotent(t *testing.T) {
	f := up(t)
	set, err := service.New(f, nil).Start(t.Context(), postgres(), "run-1")
	if err != nil {
		t.Fatal(err)
	}

	// The engine defers Stop and the error path may call it too. A double
	// teardown must not double-remove or panic.
	set.Stop()
	before := len(f.Calls())
	set.Stop()
	if len(f.Calls()) != before {
		t.Error("the second Stop issued more docker commands")
	}
}

func TestNoServicesIsNotAnError(t *testing.T) {
	f := execx.NewFake()

	set, err := service.New(f, nil).Start(t.Context(), nil, "run-1")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(set.Env()) != 0 {
		t.Errorf("env = %v, want nothing", set.Env())
	}
	if len(f.Calls()) != 0 {
		t.Errorf("docker was called for a pipeline with no services: %v", f.Lines())
	}
}

func TestADashedNameBecomesAValidVariable(t *testing.T) {
	f := up(t)
	services := map[string]config.Service{
		"my-cache": {Image: "redis:7", Port: 6379},
	}

	set, err := service.New(f, nil).Start(t.Context(), services, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	defer set.Stop()

	// KILN_SERVICE_MY-CACHE_PORT is not a variable a shell can read.
	env := strings.Join(set.Env(), " ")
	if !strings.Contains(env, "KILN_SERVICE_MY_CACHE_PORT=") {
		t.Errorf("env = %q, want the dash normalised", env)
	}
}

func findCall(t *testing.T, f *execx.Fake, prefix string) string {
	t.Helper()
	for _, line := range f.Lines() {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	t.Fatalf("no call matching %q in %v", prefix, f.Lines())
	return ""
}

func called(f *execx.Fake, prefix string) bool {
	for _, line := range f.Lines() {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

// shortTimeout builds a Duration through the YAML parser, which is the only
// way in — the type deliberately has no exported constructor.
func shortTimeout(t *testing.T) config.Duration {
	t.Helper()
	var node yaml.Node
	if err := yaml.Unmarshal([]byte("1s"), &node); err != nil {
		t.Fatal(err)
	}
	var d config.Duration
	if err := d.UnmarshalYAML(node.Content[0]); err != nil {
		t.Fatal(err)
	}
	return d
}
