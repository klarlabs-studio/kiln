package watch

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"time"

	"go.klarlabs.de/kiln/internal/domain/isolation"
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
	// Pulls maps a pull ref to the head it pointed at. Consulted only when
	// GitHub could not be asked which pull requests are open — see
	// Covers.
	Pulls map[string]string `json:"pulls,omitempty"`
}

// Covers reports that a job is part of the history the box inherited rather
// than work that has happened since.
//
// authoritative says whether GitHub answered which pull requests are open. When
// it did, pull refs are not consulted here at all: the open ones are current
// work and the closed ones are already gone. When it did not — no token, or the
// API failed — the baseline is the only thing standing between the box and the
// repository's entire pull request history.
//
// That fallback matters more than it looks. `merge-base --is-ancestor` only
// recognises a pull request merged by a merge commit; a squash or rebase merge
// writes a new commit, so the head is never an ancestor and every merged pull
// request reads as unmerged. Measured on dispatch, which squash-merges: with a
// token 42 refs were skipped and none built, and the moment the token went
// away it started gating #31, which had merged.
func (b *Baseline) Covers(j Job, authoritative bool) bool {
	if b == nil {
		return false
	}
	switch j.Event {
	case isolation.EventTag:
		sha, ok := b.Tags[j.Ref]
		return ok && sha == j.SHA
	case isolation.EventPullRequest:
		if authoritative {
			return false
		}
		sha, ok := b.Pulls[j.Ref]
		return ok && sha == j.SHA
	default:
		return false
	}
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

// baselineFrom records the tags and pull refs among a freshly discovered job
// list. Pull refs are always recorded, even when the box has a token today:
// the token can go away — an expiry, a revoked scope, a keychain that stops
// answering after an upgrade — and the baseline is what the box falls back on
// when it does.
func baselineFrom(jobs []Job, pulls map[string]string, at time.Time) *Baseline {
	b := &Baseline{Recorded: at, Tags: map[string]string{}, Pulls: map[string]string{}}
	for _, j := range jobs {
		if j.Event == isolation.EventTag {
			b.Tags[j.Ref] = j.SHA
		}
	}
	// Every pull ref, not the subset that survived filtering: with a token
	// only the open ones reach the job list, and those are exactly the ones a
	// later tokenless tick would be right to skip. The closed ones are the
	// bulk, and they are the ones that must not come back.
	maps.Copy(b.Pulls, pulls)
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
