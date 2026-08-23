package watch

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"go.klarlabs.de/kiln/internal/isolation"
	"go.klarlabs.de/kiln/internal/store"
)

// BaselineFile is where a box records what already existed when it first
// ticked, beside the run ledger rather than inside it.
const BaselineFile = ".kiln/baseline.json"

// Baseline is the set of tags a repository already had when a box started
// watching it.
//
// A tag is a publishing event, and a tag does not go stale: v2.1.0 is exactly
// as valid a tag today as the day it was pushed. Nothing about the tag itself
// says it is not this box's work — only the fact that it predates the box. So
// unlike a closed pull request, which can be filtered on its own state, this
// needs a watermark.
//
// Without one the first tick of a new box rebuilds and *republishes* every
// release the repository ever cut: 133 of them on one repo here, each pushing
// images and writing fresh provenance for a version that was signed long ago.
type Baseline struct {
	// Recorded is when the box first looked.
	Recorded time.Time `json:"recorded"`
	// Tags maps a tag ref to the commit it pointed at. The SHA is kept so a
	// moved tag is treated as new work, which it is.
	Tags map[string]string `json:"tags"`
}

// Covers reports that a job is part of the history the box inherited rather
// than work that has happened since.
func (b *Baseline) Covers(j Job) bool {
	if b == nil || j.Event != isolation.EventTag {
		return false
	}
	sha, ok := b.Tags[j.Ref]
	return ok && sha == j.SHA
}

// LoadBaseline reads a box's baseline. A missing file is not an error: it
// means this box has never ticked, and the caller records one.
func LoadBaseline(dir string) (*Baseline, error) {
	raw, err := os.ReadFile(filepath.Join(dir, BaselineFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("watch: read baseline: %w", err)
	}
	var b Baseline
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("watch: parse baseline: %w", err)
	}
	return &b, nil
}

// SaveBaseline writes a box's baseline.
func SaveBaseline(dir string, b *Baseline) error {
	path := filepath.Join(dir, BaselineFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("watch: create state directory: %w", err)
	}
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return fmt.Errorf("watch: encode baseline: %w", err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("watch: write baseline: %w", err)
	}
	return nil
}

// baselineFrom records the tags among a freshly discovered job list.
func baselineFrom(jobs []Job, at time.Time) *Baseline {
	b := &Baseline{Recorded: at, Tags: map[string]string{}}
	for _, j := range jobs {
		if j.Event == isolation.EventTag {
			b.Tags[j.Ref] = j.SHA
		}
	}
	return b
}

// ledgerIsEmpty reports that a box has never recorded a run.
//
// Only a genuinely new box gets a baseline. An existing one has already built
// its tags, and writing a baseline underneath it would silence a tag that is
// mid-backoff after a real failure.
func ledgerIsEmpty(s store.Store) bool {
	if s == nil {
		return false
	}
	runs, err := s.List()
	return err == nil && len(runs) == 0
}
