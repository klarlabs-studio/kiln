// Package health answers whether a box can build at all.
//
// The two things that took real boxes down were outside every commit: a disk
// full enough that the linker could not map its output, and a docker daemon
// that had stopped answering. Both surfaced as failed runs against whatever
// commit happened to be next, hundreds of them, each one a build somebody
// would have to read to learn it was not their fault.
package health

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.klarlabs.de/kiln/internal/infrastructure/execx"
)

// DockerTimeout bounds the daemon probe.
//
// A wedged Docker Desktop does not refuse the connection, it hangs; a probe
// without a bound would hang the tick with it, holding the repository lock.
const DockerTimeout = 15 * time.Second

// Checker satisfies ports.BoxHealth.
type Checker struct {
	Runner execx.Runner
	Docker string
}

// New builds a checker.
func New(r execx.Runner) *Checker {
	return &Checker{Runner: r, Docker: "docker"}
}

// FreeBytes reports the space available to an unprivileged writer.
func (c *Checker) FreeBytes(path string) (uint64, error) {
	return freeBytes(path)
}

// DockerReady asks the daemon for its version, which needs a live daemon and
// nothing else.
func (c *Checker) DockerReady(ctx context.Context) error {
	if _, err := c.Runner.LookPath(c.Docker); err != nil {
		return fmt.Errorf("docker is not installed: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, DockerTimeout)
	defer cancel()

	_, err := c.Runner.Run(ctx, execx.Cmd{
		Name: c.Docker,
		Args: []string{"version", "--format", "{{.Server.Version}}"},
	})
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("docker daemon did not answer within %s", DockerTimeout)
	}
	var exit *execx.ExitError
	if errors.As(err, &exit) {
		// The daemon's own explanation ("Docker Desktop is unable to start")
		// is the useful part; the exit code is not.
		if detail := lastLine(exit.Stderr); detail != "" {
			return fmt.Errorf("docker daemon is not answering: %s", detail)
		}
	}
	if err != nil {
		return fmt.Errorf("docker daemon is not answering: %w", err)
	}
	return nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
