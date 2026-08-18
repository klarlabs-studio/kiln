// Package version carries the build stamp. The three variables are set at link
// time by the Makefile's -ldflags; the defaults are what a plain `go build`
// produces, so a developer binary is always distinguishable from a release.
package version

import "runtime/debug"

// Version, Commit and Date are overwritten via -X at link time.
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// String renders the one-line stamp `kiln version` prints.
func String() string {
	v, c, d := stamp()
	return "kiln " + v + " (" + c + ") built " + d
}

// stamp resolves the build stamp, falling back to what the toolchain embedded.
//
// `go install go.klarlabs.de/kiln/cmd/kiln@v0.1.0` passes no ldflags, so a
// binary installed that way reported "dev (unknown)" — leaving the operator of
// a provenance tool unable to answer which provenance tool they are running.
// The module version and VCS stamp are in the binary either way; this reads
// them when the link-time values were not supplied.
func stamp() (version, commit, date string) {
	version, commit, date = Version, Commit, Date

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return version, commit, date
	}

	// Only fill gaps. A release's ldflags are authoritative: they carry the
	// tag GoReleaser built, which is the number the release is named after.
	if version == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
		version = info.Main.Version
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if commit == "unknown" && s.Value != "" {
				commit = shortRevision(s.Value)
			}
		case "vcs.time":
			if date == "unknown" && s.Value != "" {
				date = s.Value
			}
		}
	}
	return version, commit, date
}

// shortRevision trims a full object id to the length the ldflags stamp uses,
// so the two paths do not print the same commit two different ways.
func shortRevision(rev string) string {
	const short = 7
	if len(rev) <= short {
		return rev
	}
	return rev[:short]
}
