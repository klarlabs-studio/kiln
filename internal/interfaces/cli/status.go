package cli

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"go.klarlabs.de/kiln/internal/application/engine"
	"go.klarlabs.de/kiln/internal/boot"
	"go.klarlabs.de/kiln/internal/domain/run"
	"go.klarlabs.de/kiln/internal/infrastructure/store"
)

// runStatus reads the ledger. It never runs anything, so it is safe on a busy
// box, and it works with no credentials at all.
func runStatus(ctx context.Context, args []string, io IO) error {
	fs := newFlagSet("status", io)
	dir := fs.String("dir", "", "repository directory")
	asJSON := fs.Bool("json", false, "emit the run as JSON")
	list := fs.Int("list", 0, "show the N most recent runs instead of one")
	if err := fs.Parse(args); err != nil {
		return wrapExit(ExitUsage, err)
	}

	deps, err := boot.Build(ctx, boot.Options{Dir: *dir})
	if err != nil {
		return wrapExit(ExitConfig, err)
	}

	if *list > 0 {
		return printList(io, deps, *list, *asJSON)
	}

	r, err := lookup(deps, fs.Arg(0))
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(io, r)
	}
	printStatus(io, r)
	return nil
}

func lookup(deps *boot.Deps, id string) (*run.Run, error) {
	var (
		r   *run.Run
		err error
	)
	if id == "" {
		r, err = deps.Store.Latest()
	} else {
		r, err = deps.Store.Get(id)
	}

	if errors.Is(err, store.ErrNotFound) {
		if id == "" {
			// An empty ledger is not an error. A fresh checkout has simply not
			// built anything yet, and saying so beats a stack of jargon.
			return nil, failWith(ExitError, "no runs recorded in %s yet", deps.Store.Path())
		}
		return nil, failWith(ExitError, "no run %q in %s", id, deps.Store.Path())
	}
	if err != nil {
		return nil, wrapExit(ExitError, err)
	}
	return r, nil
}

func printList(io IO, deps *boot.Deps, limit int, asJSON bool) error {
	runs, err := deps.Store.List()
	if err != nil {
		return wrapExit(ExitError, err)
	}
	if len(runs) > limit {
		runs = runs[:limit]
	}
	if asJSON {
		return writeJSON(io, runs)
	}
	if len(runs) == 0 {
		io.printf("no runs recorded in %s yet\n", deps.Store.Path())
		return nil
	}

	for _, r := range runs {
		io.printf("%-10s %-9s %-12s %-24s %s\n",
			run.ShortSHA(r.SHA), r.Event, phaseLabel(r), truncateRef(r.Ref), r.StartedAt.Format(time.RFC3339))
	}
	return nil
}

func printStatus(io IO, r *run.Run) {
	io.printf("run     %s\n", r.ID)
	io.printf("commit  %s\n", r.SHA)
	if r.Ref != "" {
		io.printf("ref     %s\n", r.Ref)
	}
	if r.Repo != "" {
		io.printf("repo    %s\n", r.Repo)
	}
	io.printf("event   %s", r.Event)
	if r.Fork {
		io.print(" (fork)")
	}
	io.printf("\nphase   %s\n", phaseLabel(r))

	if r.Skipped {
		io.print("prove   satisfied by a trusted warden note\n")
	}
	if r.Digest != "" {
		io.printf("digest  %s\n", r.Digest)
	}
	for _, tag := range r.Tags {
		io.printf("tag     %s\n", tag)
	}
	if r.Error != "" {
		io.printf("error   %s\n", r.Error)
	}
	io.printf("started %s\n", r.StartedAt.Format(time.RFC3339))
	if !r.FinishedAt.IsZero() {
		io.printf("took    %s\n", r.Duration().Round(time.Millisecond))
	}
}

// phaseLabel marks a non-terminal run that has been open too long.
//
// `kiln run` is a one-shot process, so a run still in `proving` hours later
// means the process died — not that a build is taking a long time. Showing it
// as merely "proving" would leave an operator waiting for something that will
// never finish.
func phaseLabel(r *run.Run) string {
	if r.Phase.Terminal() || time.Since(r.StartedAt) < engine.StaleAfter {
		return string(r.Phase)
	}
	return string(r.Phase) + " (abandoned)"
}

func truncateRef(ref string) string {
	const max = 24
	if len(ref) <= max {
		return ref
	}
	return "…" + ref[len(ref)-max+1:]
}

func writeJSON(io IO, v any) error {
	enc := json.NewEncoder(io.Out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return wrapExit(ExitError, err)
	}
	return nil
}
