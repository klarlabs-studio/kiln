package ports

import (
	"go.klarlabs.de/kiln/internal/domain/run"
)

// Ledger is the run ledger.
type Ledger interface {
	// Save inserts or replaces a run by id.
	Save(r *run.Run) error
	// Get returns one run by id, or ErrNotFound.
	Get(id string) (*run.Run, error)
	// Latest returns the most recently started run, or ErrNotFound when the
	// ledger is empty.
	Latest() (*run.Run, error)
	// List returns every run, newest first.
	List() ([]*run.Run, error)
	// LastSuccess returns the most recent succeeded run for a SHA on a ref.
	// This is what makes `kiln watch` idempotent: a ref whose head already
	// built successfully is not rebuilt. Only *succeeded* runs count — a
	// failure must always be retried on the next tick.
	LastSuccess(sha, ref string) (*run.Run, error)
}
