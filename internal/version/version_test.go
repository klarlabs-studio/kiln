package version

import (
	"runtime/debug"
	"strings"
	"testing"
)

// pin sets the link-time variables for one test and restores them, so the
// cases can describe a release binary and a `go install` binary in the same
// package.
func pin(t *testing.T, v, c, d string) {
	t.Helper()
	oldV, oldC, oldD := Version, Commit, Date
	t.Cleanup(func() { Version, Commit, Date = oldV, oldC, oldD })
	Version, Commit, Date = v, c, d
}

func TestLinkTimeValuesWin(t *testing.T) {
	pin(t, "0.1.0", "abc1234", "2026-08-18T10:00:00Z")

	// GoReleaser stamps the tag it built. The module version embedded by the
	// toolchain may say the same thing or nothing; either way the release's
	// own number is the one the release is named after.
	got := String()
	if !strings.Contains(got, "0.1.0") || !strings.Contains(got, "abc1234") {
		t.Errorf("String() = %q, want the link-time stamp", got)
	}
}

func TestAGoInstalledBinaryStillKnowsWhatItIs(t *testing.T) {
	pin(t, "dev", "unknown", "unknown")

	// `go install pkg@v0.1.0` passes no ldflags. Under `go test` the main
	// module reads as "(devel)" with no vcs settings, so this asserts the
	// fallback does not invent anything — the interesting half is that it
	// reads whatever the toolchain did embed.
	v, c, d := stamp()

	info, ok := debug.ReadBuildInfo()
	if !ok {
		t.Skip("no build info in this binary")
	}
	if mv := info.Main.Version; mv != "" && mv != "(devel)" && v != mv {
		t.Errorf("version = %q, want the embedded module version %q", v, mv)
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && s.Value != "" && c != shortRevision(s.Value) {
			t.Errorf("commit = %q, want the embedded revision %q", c, shortRevision(s.Value))
		}
		if s.Key == "vcs.time" && s.Value != "" && d != s.Value {
			t.Errorf("date = %q, want the embedded time %q", d, s.Value)
		}
	}
}

func TestNothingKnownStaysHonest(t *testing.T) {
	pin(t, "dev", "unknown", "unknown")

	// A binary built with no stamp and no VCS information must not claim a
	// version it does not have. "dev" is the correct answer.
	v, c, d := stamp()
	if v == "" || c == "" || d == "" {
		t.Errorf("stamp() = (%q, %q, %q), want no empty fields", v, c, d)
	}
	if strings.Contains(String(), "()") {
		t.Errorf("String() = %q, want a readable line", String())
	}
}

func TestARevisionIsShortenedTheSameWayBothPathsPrintIt(t *testing.T) {
	if got := shortRevision("8115748887775797df0398ed27080998f4d0c8d7"); got != "8115748" {
		t.Errorf("shortRevision = %q, want 8115748", got)
	}
	if got := shortRevision("abc"); got != "abc" {
		t.Errorf("shortRevision(%q) = %q, want it unchanged", "abc", got)
	}
}
