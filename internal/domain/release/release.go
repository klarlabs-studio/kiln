// Package release is what a version tag means: whether a tag names a release
// at all, and which of two releases is the newer.
//
// It exists because a tag does not go stale on its own. v0.1.0 is as valid a
// tag the day v0.9.0 ships as the day it was pushed, so nothing about the tag
// says it is history — only its place in the order of releases does. Without
// that order a box retried three failing tags from a repository's first week,
// v0.0.0 to v0.2.0, about seventy times each, long after newer releases had
// shipped and nobody would ever deploy them.
package release

import (
	"cmp"
	"strconv"
	"strings"
)

// Version is a parsed semver 2.0.0 tag.
type Version struct {
	// name is the tag as written, without refs/tags/, so a log line names the
	// tag the operator pushed rather than a normalised form of it.
	name                string
	major, minor, patch uint64
	pre                 []string
}

// ParseTag reads a tag ref, or a bare tag name, as a version.
//
// It accepts what the semver tag kind publishes — an optional "v", three
// numeric parts, an optional pre-release and build metadata — and refuses
// everything else, so a tag like "latest" or "release-2026-10" keeps being
// built the way it always was. Leading zeros and empty identifiers are
// refused, as the specification refuses them: guessing what "v01.2.3" means
// is how two tags end up comparing equal that the author thought were not.
func ParseTag(ref string) (Version, bool) {
	name := ref
	if strings.HasPrefix(ref, "refs/") {
		var ok bool
		if name, ok = strings.CutPrefix(ref, "refs/tags/"); !ok {
			return Version{}, false
		}
	}
	rest := strings.TrimPrefix(name, "v")

	// Build metadata carries no precedence; it only has to be well formed.
	if core, build, found := strings.Cut(rest, "+"); found {
		if !identifiers(build, false) {
			return Version{}, false
		}
		rest = core
	}
	var pre []string
	if core, p, found := strings.Cut(rest, "-"); found {
		if !identifiers(p, true) {
			return Version{}, false
		}
		pre = strings.Split(p, ".")
		rest = core
	}

	parts := strings.Split(rest, ".")
	if len(parts) != 3 {
		return Version{}, false
	}
	nums := make([]uint64, 3)
	for i, part := range parts {
		n, ok := numeric(part)
		if !ok {
			return Version{}, false
		}
		nums[i] = n
	}
	return Version{name: name, major: nums[0], minor: nums[1], patch: nums[2], pre: pre}, true
}

// String is the tag name as it was pushed.
func (v Version) String() string { return v.name }

// Compare orders two versions by semver precedence: -1 when v is older than o,
// 1 when it is newer, 0 when they name the same release.
func (v Version) Compare(o Version) int {
	for _, d := range [][2]uint64{{v.major, o.major}, {v.minor, o.minor}, {v.patch, o.patch}} {
		if d[0] != d[1] {
			return cmp.Compare(d[0], d[1])
		}
	}
	// A pre-release sorts below the release it leads up to.
	switch {
	case len(v.pre) == 0 && len(o.pre) == 0:
		return 0
	case len(v.pre) == 0:
		return 1
	case len(o.pre) == 0:
		return -1
	}
	for i := 0; i < len(v.pre) && i < len(o.pre); i++ {
		if c := comparePre(v.pre[i], o.pre[i]); c != 0 {
			return c
		}
	}
	return cmp.Compare(len(v.pre), len(o.pre))
}

// Lines judges tags one major release line at a time.
//
// Per line rather than across the repository, because a patch cut on an older
// line is not history: v1.9.5 tagged after v2.0.0 shipped is usually a
// security backport for whoever still runs v1, and skipping it silently is a
// worse failure than building an old tag once too often. Within a line the
// order is unambiguous — v0.2.0 is overtaken by v0.3.0 whatever else exists.
//
// The zero value is ready to use.
type Lines struct {
	newest, shipped map[uint64]Version
}

// Tagged records a version tag the repository has.
func (l *Lines) Tagged(v Version) { l.newest = keepNewer(l.newest, v) }

// Shipped records a release that has already been published.
func (l *Lines) Shipped(v Version) { l.shipped = keepNewer(l.shipped, v) }

// Superseded reports that v has been overtaken within its own major line.
// A line nothing was Tagged on has no newest, so nothing in it is overtaken.
func (l *Lines) Superseded(v Version) bool {
	newest, ok := l.newest[v.major]
	if !ok {
		return false
	}
	var shipped *Version
	if s, ok := l.shipped[v.major]; ok {
		shipped = &s
	}
	return Superseded(v, newest, shipped)
}

func keepNewer(m map[uint64]Version, v Version) map[uint64]Version {
	if m == nil {
		m = map[uint64]Version{}
	}
	if cur, ok := m[v.major]; !ok || v.Compare(cur) > 0 {
		m[v.major] = v
	}
	return m
}

// Superseded reports that a release is not worth building any more, within
// one release line — Lines applies it per major.
//
// shipped is the newest release of the line already published, if any;
// newest is the newest release the line has. A tag is current while it
// is newer than what shipped — a release cut since, including one whose build
// is still failing — or while it is the newest there is. Everything else has
// been overtaken: an older tag that never built is a release nobody will
// deploy now, and retrying it is work with no reader.
//
// The newest is never superseded, which is what keeps a box with nothing
// shipped yet from building nothing at all: it builds the head of the
// release line rather than the line's whole history.
func Superseded(v, newest Version, shipped *Version) bool {
	if v.Compare(newest) >= 0 {
		return false
	}
	return shipped == nil || v.Compare(*shipped) <= 0
}

// comparePre orders one pre-release identifier pair: numeric identifiers
// numerically and below alphanumeric ones, alphanumeric ones in ASCII order.
func comparePre(a, b string) int {
	an, aNum := numeric(a)
	bn, bNum := numeric(b)
	switch {
	case aNum && bNum:
		return cmp.Compare(an, bn)
	case aNum:
		return -1
	case bNum:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

// identifiers checks a dot-separated pre-release or build list. Pre-release
// numeric identifiers may not carry leading zeros; build ones may.
func identifiers(s string, pre bool) bool {
	if s == "" {
		return false
	}
	for id := range strings.SplitSeq(s, ".") {
		if id == "" {
			return false
		}
		for _, r := range id {
			if !isAlnum(r) && r != '-' {
				return false
			}
		}
		if pre && isDigits(id) && len(id) > 1 && id[0] == '0' {
			return false
		}
	}
	return true
}

// numeric parses a version number: digits only, no leading zero.
func numeric(s string) (uint64, bool) {
	if !isDigits(s) || (len(s) > 1 && s[0] == '0') {
		return 0, false
	}
	n, err := strconv.ParseUint(s, 10, 64)
	return n, err == nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isAlnum(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}
