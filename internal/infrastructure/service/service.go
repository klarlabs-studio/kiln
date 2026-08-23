// Package service runs the containers a gate needs beside it.
//
// A test suite that talks to postgres is not an unusual pipeline; it is most
// of them. GitHub Actions answers this with `services:`, and the first
// repository kiln tried to migrate stopped there — the database was the only
// thing standing between it and leaving Actions.
//
// The design constraint that shapes everything here: a kiln box runs many
// repositories. Two pipelines that both want port 5432 would collide, and the
// symptom would be a test failing for reasons that have nothing to do with the
// commit. So ports are allocated by docker and read back, never fixed, and the
// address is handed to the gate through the environment.
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.klarlabs.de/kiln/internal/application/ports"
	"go.klarlabs.de/kiln/internal/domain/config"
	"go.klarlabs.de/kiln/internal/infrastructure/execx"
	"go.klarlabs.de/kiln/internal/infrastructure/obs"
)

// Running is a started service.
type Running struct {
	// Name is the key from `.kiln.yaml`.
	Name string
	// Container is the docker name, unique to this run.
	Container string
	// Host and Port are where the service actually listens, as docker
	// allocated it.
	Host string
	Port string
}

// Env renders the variables the gate and tasks read.
//
// KILN_SERVICE_POSTGRES_PORT rather than POSTGRES_PORT: a name that generic
// would collide with whatever the image itself sets, and a test reading it
// would work by accident until the day the image changed.
func (r Running) Env() []string {
	prefix := "KILN_SERVICE_" + strings.ToUpper(strings.ReplaceAll(r.Name, "-", "_"))
	return []string{
		prefix + "_HOST=" + r.Host,
		prefix + "_PORT=" + r.Port,
	}
}

// Set is every service started for one run.
type Set struct {
	Running []Running
	stop    func()
}

// Env is the environment for all of them.
func (s *Set) Env() []string {
	if s == nil {
		return nil
	}
	var out []string
	for _, r := range s.Running {
		out = append(out, r.Env()...)
	}
	return out
}

// Stop tears every container down. Safe to call twice.
func (s *Set) Stop() {
	if s == nil || s.stop == nil {
		return
	}
	s.stop()
	s.stop = nil
}

// Runner starts and stops services.
type Runner struct {
	Exec   execx.Runner
	Docker string
	Log    ports.Logger
}

// New builds a runner.
func New(r execx.Runner, log ports.Logger) *Runner {
	if log == nil {
		log = obs.Discard()
	}
	return &Runner{Exec: r, Docker: "docker", Log: log}
}

// ErrNotReady reports a service that never became usable.
var ErrNotReady = errors.New("service never became ready")

// Start brings up every service and waits for it to be ready.
//
// If any service fails, the ones already started are torn down before
// returning. A half-started set left behind would hold ports and memory on a
// box that is about to try the whole thing again on the next tick.
func (s *Runner) Start(ctx context.Context, services map[string]config.Service, runID string) (*Set, error) {
	if len(services) == 0 {
		return &Set{}, nil
	}

	set := &Set{}
	set.stop = func() { s.stopAll(set.Running) }

	for _, name := range sortedNames(services) {
		running, err := s.start(ctx, name, services[name], runID)
		if err != nil {
			set.Stop()
			return nil, err
		}
		set.Running = append(set.Running, running)

		if err := s.waitReady(ctx, running, services[name]); err != nil {
			set.Stop()
			return nil, err
		}
		s.Log.Info("service ready", "service", name, "port", running.Port)
	}
	return set, nil
}

func (s *Runner) start(ctx context.Context, name string, spec config.Service, runID string) (Running, error) {
	container := fmt.Sprintf("kiln-%s-%s", shortID(runID), name)

	args := []string{"run", "--detach", "--rm", "--name", container}
	for _, kv := range sortedEnv(spec.Env) {
		args = append(args, "--env", kv)
	}
	if spec.Port > 0 {
		// Published to an ephemeral host port, chosen by the kernel. Fixed
		// ports are how two repositories on one box break each other.
		args = append(args, "--publish", fmt.Sprintf("127.0.0.1::%d", spec.Port))
	}
	args = append(args, spec.Image)
	args = append(args, spec.Command...)

	if _, err := s.Exec.Run(ctx, execx.Cmd{Name: s.Docker, Args: args}); err != nil {
		return Running{}, fmt.Errorf("service %s: start %s: %w", name, spec.Image, err)
	}

	running := Running{Name: name, Container: container, Host: "127.0.0.1"}
	if spec.Port > 0 {
		port, err := s.publishedPort(ctx, container, spec.Port)
		if err != nil {
			return running, err
		}
		running.Port = port
	}
	return running, nil
}

// publishedPort reads back the host port docker chose.
func (s *Runner) publishedPort(ctx context.Context, container string, port int) (string, error) {
	res, err := s.Exec.Run(ctx, execx.Cmd{
		Name: s.Docker,
		Args: []string{"port", container, fmt.Sprintf("%d/tcp", port)},
	})
	if err != nil {
		return "", fmt.Errorf("service: read the port docker chose for %s: %w", container, err)
	}

	// `docker port` prints "127.0.0.1:49154", possibly several lines.
	line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(res.Stdout), "\n", 2)[0])
	idx := strings.LastIndex(line, ":")
	if idx < 0 || idx == len(line)-1 {
		return "", fmt.Errorf("service: cannot read a port from %q", line)
	}
	return line[idx+1:], nil
}

// waitReady polls the readiness command until it succeeds.
//
// A gate that starts before the database accepts connections fails in a way
// nobody debugs twice: the error is in the test output, the cause is a race in
// the harness, and the usual response is to add a sleep somewhere.
func (s *Runner) waitReady(ctx context.Context, running Running, spec config.Service) error {
	if strings.TrimSpace(spec.Ready) == "" {
		return nil
	}

	timeout := spec.ReadyTimeout.Std()
	if timeout <= 0 {
		timeout = DefaultReadyTimeout
	}
	deadline := time.Now().Add(timeout)

	var last error
	for attempt := 0; time.Now().Before(deadline); attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_, err := s.Exec.Run(ctx, execx.Cmd{
			Name: s.Docker,
			Args: append([]string{"exec", running.Container, "sh", "-c"}, spec.Ready),
		})
		if err == nil {
			return nil
		}
		last = err

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(readyInterval):
		}
	}
	return fmt.Errorf("%w: %s after %s: %w", ErrNotReady, running.Name, timeout, last)
}

// DefaultReadyTimeout is how long a service has to come up.
const DefaultReadyTimeout = 60 * time.Second

// readyInterval is how often readiness is retried. Frequent enough that a fast
// service does not add measurable time, slow enough not to spin.
const readyInterval = 500 * time.Millisecond

// stopAll tears containers down, reporting failures without stopping.
//
// Uses a context detached from the run: teardown happens when the run is
// finishing, including when it was cancelled, and one that inherited a
// cancelled context would refuse to run at precisely the moment it is needed.
func (s *Runner) stopAll(running []Running) {
	for _, r := range running {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 30*time.Second)
		_, err := s.Exec.Run(ctx, execx.Cmd{Name: s.Docker, Args: []string{"rm", "--force", r.Container}})
		cancel()
		if err != nil {
			// A leaked container holds a port and memory on a box that will
			// try again next tick, so this is worth saying loudly even though
			// there is nothing left to do about it here.
			s.Log.Warn("could not remove service container",
				"service", r.Name, "container", r.Container, "err", err)
			continue
		}
		s.Log.Debug("service stopped", "service", r.Name)
	}
}

func sortedNames(services map[string]config.Service) []string {
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	// Deterministic start order, so a log from two runs of the same pipeline
	// can be compared line by line.
	sortStrings(names)
	return names
}

func sortedEnv(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// shortID trims a run id down to something a container name can carry.
func shortID(runID string) string {
	if idx := strings.LastIndex(runID, "-"); idx >= 0 && idx < len(runID)-1 {
		return runID[idx+1:]
	}
	if len(runID) > 8 {
		return runID[len(runID)-8:]
	}
	return runID
}
