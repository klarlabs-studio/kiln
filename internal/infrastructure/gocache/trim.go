package gocache

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"go.klarlabs.de/kiln/internal/application/ports"
)

const (
	// DefaultMaxSize caps kiln's cache after the idle sweep. Ten GiB holds
	// several repositories' warm race builds; the unbounded cache this replaced
	// reached 35 GB in half a day.
	DefaultMaxSize int64 = 10 << 30

	// MaxIdle is how long an entry may go unused before it is removed.
	// A day, against Go's own five: a box rebuilds its repositories every
	// few hours, so anything untouched for a day belongs to a commit nobody
	// is building any more.
	MaxIdle = 24 * time.Hour

	// MinAge protects entries from size eviction. Go refreshes an entry's
	// mtime on use only once it is an hour old (cmd/go/internal/cache
	// mtimeInterval), so an mtime inside the last hour says nothing about
	// how recently the entry was read — it may belong to the build running
	// right now in another repository's tick.
	MinAge = time.Hour
)

// Trimmer bounds a kiln-owned Go build cache.
type Trimmer struct {
	Dir string
	// MaxSize is the cap applied after the idle sweep. Zero disables it,
	// leaving only the idle sweep.
	MaxSize int64
	// Now is the clock, injectable so a test can state ages exactly.
	Now func() time.Time
}

// NewTrimmer builds a trimmer for dir.
func NewTrimmer(dir string, maxSize int64) *Trimmer {
	return &Trimmer{Dir: dir, MaxSize: maxSize, Now: time.Now}
}

// entry is one cache file.
type entry struct {
	path  string
	size  int64
	mtime time.Time
}

// Trim removes entries idle for longer than MaxIdle, then evicts the least
// recently used ones older than MinAge until the cache is under MaxSize.
//
// It touches only files directly inside the two-hex-digit subdirectories Go
// shards its cache into, and only in a directory carrying kiln's marker.
// Everything else — Go's README and trim.txt, the marker, anything an
// operator left there — is not a cache entry and is not this code's to delete.
//
// A file that cannot be removed is skipped and the sweep carries on; the
// first such error is returned with whatever was freed, because a box that
// stops trimming on one stuck file grows without bound again.
func (t *Trimmer) Trim(ctx context.Context) (ports.CacheTrimResult, error) {
	var res ports.CacheTrimResult
	if _, err := os.Stat(t.Dir); errors.Is(err, fs.ErrNotExist) {
		return res, nil
	}
	if !Owned(t.Dir) {
		return res, fmt.Errorf("%w: %s", ErrNotOwned, t.Dir)
	}

	entries, err := t.scan(ctx)
	if err != nil {
		return res, err
	}

	now := t.now()
	var firstErr error
	remove := func(e entry) {
		if err := os.Remove(e.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			if firstErr == nil {
				firstErr = err
			}
			return
		}
		res.Files++
		res.Freed += e.size
	}

	// Oldest first, so the idle sweep and the eviction share one order.
	sort.Slice(entries, func(i, j int) bool { return entries[i].mtime.Before(entries[j].mtime) })

	var kept []entry
	var total int64
	for _, e := range entries {
		if now.Sub(e.mtime) > MaxIdle {
			remove(e)
			continue
		}
		kept = append(kept, e)
		total += e.size
	}

	if t.MaxSize > 0 {
		for _, e := range kept {
			if total <= t.MaxSize {
				break
			}
			if now.Sub(e.mtime) < MinAge {
				// Sorted oldest first: everything after this is newer still.
				break
			}
			if err := ctx.Err(); err != nil {
				return res, err
			}
			before := res.Files
			remove(e)
			if res.Files > before {
				total -= e.size
			}
		}
	}
	return res, firstErr
}

// scan lists the cache's entries.
func (t *Trimmer) scan(ctx context.Context) ([]entry, error) {
	shards, err := os.ReadDir(t.Dir)
	if err != nil {
		return nil, fmt.Errorf("gocache: read %s: %w", t.Dir, err)
	}
	var out []entry
	for _, shard := range shards {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !shard.IsDir() || !isShard(shard.Name()) {
			continue
		}
		dir := filepath.Join(t.Dir, shard.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if !f.Type().IsRegular() {
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue
			}
			out = append(out, entry{path: filepath.Join(dir, f.Name()), size: info.Size(), mtime: info.ModTime()})
		}
	}
	return out, nil
}

// isShard matches Go's cache subdirectories: two lowercase hex digits.
func isShard(name string) bool {
	if len(name) != 2 {
		return false
	}
	for _, c := range name {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func (t *Trimmer) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}
