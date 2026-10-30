// Package gocache gives kiln's builds a Go build cache of their own.
//
// Every gate kiln runs is `warden run pre-push`, and for a Go repository that
// is `go test -race` against a fresh worktree. Those children used to inherit
// the operator's cache (~/Library/Caches/go-build on a Mac), and a box
// building a handful of repositories grew it by roughly 35 GB in twelve hours
// — race-instrumented test binaries are large, and every commit produces new
// ones. Go trims its own cache only of entries unused for five days, so
// nothing bounded it, and an external cron script was trimming the operator's
// cache from the outside.
//
// The fix is ownership. Kiln points its children at a directory it created
// (by default <user cache>/kiln/go-build), and trims that directory each
// watch tick. The operator's own cache is never written by a box build and
// never touched by the trimmer.
package gocache

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// marker is the file that makes a directory kiln's. Claim writes it into a
// directory it created or found empty; Trim refuses any directory without it.
// A path check alone is not enough: KILN_GOCACHE can name anything, and a
// typo pointing it at the operator's cache must not turn into a deletion.
const marker = ".kiln-go-cache"

const markerText = "This Go build cache is owned by kiln, which trims it on every watch tick.\n" +
	"Do not point GOCACHE for anything else at this directory.\n"

// ErrNotOwned reports a directory kiln did not create and will not manage.
var ErrNotOwned = errors.New("not a kiln-owned Go build cache")

// Resolve decides which directory, if any, kiln's children use as GOCACHE.
//
// setting is KILN_GOCACHE. Empty means the default under the user cache
// directory; "off" (or 0/false/none) means kiln leaves GOCACHE alone, which
// restores the old behaviour of inheriting the operator's cache; anything
// else is a path, checked by Validate.
//
// An operator who set GOCACHE (non-empty) in kiln's own environment has
// already decided where Go's cache lives, and that wins over everything —
// including KILN_GOCACHE. Kiln then neither injects a GOCACHE nor trims, since
// the directory is not its own.
//
// The empty string means "kiln manages no Go cache".
func Resolve(setting, operatorGOCACHE string, operatorSet bool, userCacheDir func() (string, error)) string {
	if operatorSet && strings.TrimSpace(operatorGOCACHE) != "" {
		return ""
	}
	setting = strings.TrimSpace(setting)
	switch strings.ToLower(setting) {
	case "off", "0", "false", "no", "none":
		return ""
	case "":
		base, err := userCacheDir()
		if err != nil || base == "" {
			// No HOME — a stripped-down service account. Inheriting is the
			// only thing left that cannot be wrong.
			return ""
		}
		return filepath.Join(base, "kiln", "go-build")
	default:
		return setting
	}
}

// Validate refuses a cache directory kiln must not own.
//
// A relative path would resolve against each child's working directory, which
// is a different disposable worktree every run: every build would get a cold
// cache of its own and nothing would ever trim them. Go's own default cache is
// refused by name because trimming it is exactly what this package exists not
// to do; Claim's marker check is the second line behind it.
func Validate(dir string, userCacheDir func() (string, error)) error {
	if dir == "" {
		return nil
	}
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("KILN_GOCACHE %q must be an absolute path, or \"off\"", dir)
	}
	if base, err := userCacheDir(); err == nil && base != "" {
		if filepath.Clean(dir) == filepath.Join(base, "go-build") {
			return fmt.Errorf("KILN_GOCACHE %q is Go's own default cache; "+
				"kiln trims the directory it is given, so it must be one kiln owns", dir)
		}
	}
	return nil
}

// Claim creates dir if needed and marks it as kiln's.
//
// A directory that already has the marker is kiln's from an earlier run. An
// empty one is safe to adopt. One that holds anything else belongs to
// somebody, and is refused with ErrNotOwned rather than adopted — after which
// the caller runs without a kiln cache, which only costs build time.
func Claim(dir string) error {
	if dir == "" {
		return nil
	}
	if Owned(dir) {
		return nil
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("gocache: create %s: %w", dir, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("gocache: read %s: %w", dir, err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("%w: %s is not empty and has no %s marker", ErrNotOwned, dir, marker)
	}
	if err := os.WriteFile(filepath.Join(dir, marker), []byte(markerText), 0o600); err != nil {
		return fmt.Errorf("gocache: mark %s: %w", dir, err)
	}
	return nil
}

// Owned reports whether dir carries kiln's marker.
func Owned(dir string) bool {
	info, err := os.Lstat(filepath.Join(dir, marker))
	return err == nil && info.Mode().IsRegular()
}

// Env returns environ with GOCACHE pointed at dir, or environ unchanged when
// dir is empty.
//
// It always copies: callers pass os.Environ() or a scrubbed copy of it, and
// appending into a shared backing array is how one child's variable ends up in
// another's environment. GOCACHE is a path, not a credential, so it is added
// to a fork's scrubbed environment as readily as to a trusted one — and must
// be, or a fork build would fall back to the operator's cache, which is the
// very growth this exists to stop.
func Env(environ []string, dir string) []string {
	if dir == "" {
		return environ
	}
	out := make([]string, 0, len(environ)+1)
	out = append(out, environ...)
	return append(out, "GOCACHE="+dir)
}
