// Command kilnd serves Kiln over HTTP.
//
// It is optional. Cron plus `kiln watch --once` is the daemon-less default and
// nothing depends on this binary; kilnd exists for operators who already run a
// process and would rather receive GitHub deliveries than poll for them.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.klarlabs.de/kiln/internal/boot"
	"go.klarlabs.de/kiln/internal/interfaces/daemon"
	"go.klarlabs.de/kiln/internal/version"
)

// shutdownGrace bounds the wait for in-flight work on SIGTERM. Long enough for
// a build to finish, short enough that a container runtime does not resort to
// SIGKILL without warning.
const shutdownGrace = 30 * time.Second

// main is a thin shell around start so that every deferred cleanup — including
// the signal handler's — actually runs before os.Exit.
func main() { os.Exit(start()) }

func start() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "kilnd: %v\n", err)
		return 1
	}
	return 0
}

func run(ctx context.Context) error {
	// Output is nil: a daemon's subprocess output belongs in the structured
	// log, not interleaved on its stdout.
	deps, err := boot.Build(ctx, boot.Options{})
	if err != nil {
		return err
	}

	srv, err := daemon.New(deps, deps.Env.DaemonToken, deps.Env.WebhookSecret, deps.Log)
	if err != nil {
		return err
	}

	httpSrv := &http.Server{
		Addr:    deps.Env.Addr,
		Handler: srv.Handler(),
		// A slow-loris client must not be able to hold a connection open
		// indefinitely. The write timeout is generous because /v1/run is
		// synchronous and a real build takes minutes.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}

	deps.Log.Info("kilnd listening",
		"addr", deps.Env.Addr, "dir", deps.Dir, "version", version.Version,
		"webhook", deps.Env.WebhookSecret != "", "dry", deps.Dry())

	errs := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}

	deps.Log.Info("shutting down", "grace", shutdownGrace.String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()

	// Stop accepting first, then wait for background builds. The other order
	// would let a delivery arriving during shutdown start work nobody waits
	// for.
	httpErr := httpSrv.Shutdown(shutdownCtx)
	if err := srv.Shutdown(shutdownCtx); err != nil {
		// A build killed midway leaves a half-written registry and an
		// in-progress check that never concludes. Worth saying out loud.
		deps.Log.Warn("shut down with work still running", "err", err)
	}
	return httpErr
}
