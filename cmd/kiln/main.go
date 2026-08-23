// Command kiln is the signed-artifact factory's primary surface.
//
// Warden proves a commit. Kiln turns that commit into a signed container
// image. RollOps is the only thing allowed to ship it.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"go.klarlabs.de/kiln/internal/interfaces/cli"
)

// main is a thin shell around run so that every deferred cleanup — including
// the signal handler's — actually runs before os.Exit.
func main() { os.Exit(run()) }

func run() int {
	// NotifyContext rather than a bare signal handler: every long-running
	// phase already takes a context, so one cancellation unwinds the whole
	// stack — including the deferred worktree cleanups. A Ctrl-C that left
	// checkouts behind would fill a watch box's disk over a few weeks.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return cli.Main(ctx, os.Args[1:], cli.Stdio())
}
