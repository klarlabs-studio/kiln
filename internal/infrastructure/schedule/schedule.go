// Package schedule remembers when a scheduled task last ran.
//
// A task written as `on: [schedule], every: 24h` has to survive the thing
// running it being restarted, redeployed or switched off for a weekend, and
// the state that makes that possible is small enough to keep in one file
// beside the ledger.
//
// The behaviour that needs stating: a box that was off for a week fires each
// due task **once** when it comes back, not seven times. Cron catch-up storms
// are how a nightly remediation job opens seven pull requests at breakfast,
// and there is no version of that anybody wants.
package schedule

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// FileName is where the state lives, beside the run ledger.
const FileName = "schedule.json"

// State records the last fire time per task, keyed by task name.
type State struct {
	// LastRun is task name → when it last fired, in UTC.
	LastRun map[string]time.Time `json:"last_run"`
}

// Store persists schedule state.
type Store struct {
	path string
	mu   sync.Mutex
}

// NewStore opens the state file beside a ledger path.
func NewStore(dir string) *Store {
	return &Store{path: filepath.Join(dir, FileName)}
}

// Path is where the state is kept, for diagnostics.
func (s *Store) Path() string { return s.path }

// DueAt reports whether a task should fire at the given time.
//
// A task that has never run is due immediately. That is deliberate: the
// alternative is a nightly job that does nothing on the day you configure it
// and leaves you wondering whether it works.
//
// The time is a parameter rather than read from the clock so a test can state
// what "a week later" means instead of sleeping through it.
func (s *Store) DueAt(name string, every time.Duration, now time.Time) (bool, error) {
	state, err := s.load()
	if err != nil {
		return false, err
	}
	last, ran := state.LastRun[name]
	if !ran {
		return true, nil
	}
	return !now.Before(last.Add(every)), nil
}

// Fired records that a task ran.
//
// Written immediately rather than after the task finishes, so a task that
// crashes the process does not re-fire on every restart until it succeeds —
// which for anything that opens a pull request or sends a message would be a
// loop with an audience.
func (s *Store) Fired(name string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := s.loadLocked()
	if err != nil {
		return err
	}
	if state.LastRun == nil {
		state.LastRun = map[string]time.Time{}
	}
	state.LastRun[name] = at.UTC()

	if err := os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		return fmt.Errorf("schedule: create directory: %w", err)
	}
	body, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("schedule: encode: %w", err)
	}

	// Write-and-rename, so a process killed mid-write leaves the previous
	// state rather than a truncated file that reads as "never ran" and fires
	// everything again.
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(body, '\n'), 0o600); err != nil {
		return fmt.Errorf("schedule: write: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("schedule: replace %s: %w", s.path, err)
	}
	return nil
}

func (s *Store) load() (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *Store) loadLocked() (State, error) {
	data, err := os.ReadFile(filepath.Clean(s.path))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return State{LastRun: map[string]time.Time{}}, nil
	case err != nil:
		return State{}, fmt.Errorf("schedule: read %s: %w", s.path, err)
	}

	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		// An unreadable state file means "fire now", not "never fire". The
		// cost of one extra run is a duplicate; the cost of the other choice
		// is silence that looks like everything being fine.
		return State{LastRun: map[string]time.Time{}}, nil
	}
	if state.LastRun == nil {
		state.LastRun = map[string]time.Time{}
	}
	return state, nil
}
