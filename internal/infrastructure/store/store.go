// Package store persists the run ledger.
//
// Two implementations: Memory for tests and the MCP surface, File for the CLI
// and daemon. Both are safe for concurrent use, because `kiln watch` executes
// jobs from a single tick sequentially but kilnd serves webhooks in the
// background, and one storage bug there would be very hard to see.
//
// The ledger is intentionally not a database. It is a JSON array on disk that
// a human can read and an operator can delete. Nothing in Kiln's correctness
// depends on it surviving — see package run.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"go.klarlabs.de/kiln/internal/domain/run"
)

// ErrNotFound reports that no run has the requested id.
var ErrNotFound = errors.New("run not found")

// MaxRuns caps the ledger. Without a cap, a box running `kiln watch --every 1m`
// grows the file forever; with one, the oldest entries fall off and the file
// stays something you can open in an editor.
const MaxRuns = 500

// Store is the run ledger.
type Store interface {
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

// Memory is an in-process ledger.
type Memory struct {
	mu   sync.RWMutex
	runs []*run.Run // newest first
}

// NewMemory returns an empty in-memory ledger.
func NewMemory() *Memory { return &Memory{} }

func (m *Memory) Save(r *run.Run) error {
	if r == nil || r.ID == "" {
		return errors.New("save: run needs an id")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runs = upsert(m.runs, r.Clone())
	return nil
}

func (m *Memory) Get(id string) (*run.Run, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, r := range m.runs {
		if r.ID == id {
			return r.Clone(), nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
}

func (m *Memory) Latest() (*run.Run, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.runs) == 0 {
		return nil, ErrNotFound
	}
	return m.runs[0].Clone(), nil
}

func (m *Memory) List() ([]*run.Run, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return cloneAll(m.runs), nil
}

func (m *Memory) LastSuccess(sha, ref string) (*run.Run, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return lastSuccess(m.runs, sha, ref)
}

// File is a ledger backed by a JSON document, default `.kiln/state.json`.
//
// Every write reads the current file, merges, and renames a temp file over the
// target. That is slower than holding the slice in memory, and deliberately so:
// `kiln run` is a one-shot process, so an in-memory cache would be stale the
// moment a second process touched the file, and rename-over is the only way to
// leave a readable document behind if the machine dies mid-write.
type File struct {
	mu   sync.Mutex
	path string
	// max is the retained-run cap, MaxRuns in production. It is a field rather
	// than a constant reference so tests can exercise trimming without writing
	// five hundred files.
	max int
}

// NewFile returns a file-backed ledger at path. The file and its directory are
// created lazily on first write, so a read-only `kiln status` against a repo
// that has never run does not litter it with an empty `.kiln/`.
func NewFile(path string) *File { return &File{path: path, max: MaxRuns} }

// Path is the ledger's location, for `kiln doctor` output.
func (f *File) Path() string { return f.path }

func (f *File) Save(r *run.Run) error {
	if r == nil || r.ID == "" {
		return errors.New("save: run needs an id")
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	runs, err := f.read()
	if err != nil {
		return err
	}
	runs = upsert(runs, r.Clone())
	if f.max > 0 && len(runs) > f.max {
		runs = runs[:f.max]
	}
	return f.write(runs)
}

func (f *File) Get(id string) (*run.Run, error) {
	runs, err := f.snapshot()
	if err != nil {
		return nil, err
	}
	for _, r := range runs {
		if r.ID == id {
			return r, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
}

func (f *File) Latest() (*run.Run, error) {
	runs, err := f.snapshot()
	if err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return nil, ErrNotFound
	}
	return runs[0], nil
}

func (f *File) List() ([]*run.Run, error) { return f.snapshot() }

func (f *File) LastSuccess(sha, ref string) (*run.Run, error) {
	runs, err := f.snapshot()
	if err != nil {
		return nil, err
	}
	return lastSuccess(runs, sha, ref)
}

func (f *File) snapshot() ([]*run.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.read()
}

// read loads the ledger. A missing file is an empty ledger, not an error: the
// first run on a fresh checkout must not have to initialise anything.
func (f *File) read() ([]*run.Run, error) {
	data, err := os.ReadFile(f.path) //nolint:gosec // operator-configured ledger path
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read ledger %s: %w", f.path, err)
	}
	if len(data) == 0 {
		return nil, nil
	}

	var runs []*run.Run
	if err := json.Unmarshal(data, &runs); err != nil {
		return nil, fmt.Errorf("parse ledger %s: %w (delete it to reset; kiln keeps no state that git does not)", f.path, err)
	}
	sortRuns(runs)
	return runs, nil
}

func (f *File) write(runs []*run.Run) error {
	dir := filepath.Dir(f.path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create ledger dir %s: %w", dir, err)
	}

	data, err := json.MarshalIndent(runs, "", "  ")
	if err != nil {
		return fmt.Errorf("encode ledger: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, ".state-*.json")
	if err != nil {
		return fmt.Errorf("create temp ledger: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup: after a successful rename the path is gone and the
	// Remove is a no-op, but on any early return it removes the debris.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp ledger: %w", err)
	}
	// fsync before rename: a rename is atomic with respect to *ordering*, not
	// to durability. Without this a crash can leave the new name pointing at
	// an empty inode.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp ledger: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp ledger: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("chmod temp ledger: %w", err)
	}
	if err := os.Rename(tmpName, f.path); err != nil {
		return fmt.Errorf("replace ledger %s: %w", f.path, err)
	}
	return nil
}

// upsert replaces a run with the same id, or prepends it, then re-sorts so the
// slice stays newest-first regardless of the order saves arrive in.
func upsert(runs []*run.Run, r *run.Run) []*run.Run {
	for i, existing := range runs {
		if existing.ID == r.ID {
			runs[i] = r
			sortRuns(runs)
			return runs
		}
	}
	runs = append([]*run.Run{r}, runs...)
	sortRuns(runs)
	return runs
}

// sortRuns orders newest-first, breaking ties on id so the order is total and
// two identical timestamps do not shuffle between reads.
func sortRuns(runs []*run.Run) {
	sort.SliceStable(runs, func(i, j int) bool {
		if runs[i].StartedAt.Equal(runs[j].StartedAt) {
			return runs[i].ID > runs[j].ID
		}
		return runs[i].StartedAt.After(runs[j].StartedAt)
	})
}

func lastSuccess(runs []*run.Run, sha, ref string) (*run.Run, error) {
	for _, r := range runs {
		if r.Phase == run.PhaseSucceeded && r.SHA == sha && r.Ref == ref {
			return r.Clone(), nil
		}
	}
	return nil, ErrNotFound
}

func cloneAll(runs []*run.Run) []*run.Run {
	out := make([]*run.Run, len(runs))
	for i, r := range runs {
		out[i] = r.Clone()
	}
	return out
}
