// Package version carries the build stamp. The three variables are set at link
// time by the Makefile's -ldflags; the defaults are what a plain `go build`
// produces, so a developer binary is always distinguishable from a release.
package version

// Version, Commit and Date are overwritten via -X at link time.
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// String renders the one-line stamp `kiln version` prints.
func String() string {
	return "kiln " + Version + " (" + Commit + ") built " + Date
}
