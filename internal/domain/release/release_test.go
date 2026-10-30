package release_test

import (
	"testing"

	"go.klarlabs.de/kiln/internal/domain/release"
)

func mustParse(t *testing.T, ref string) release.Version {
	t.Helper()
	v, ok := release.ParseTag(ref)
	if !ok {
		t.Fatalf("ParseTag(%q) refused a version", ref)
	}
	return v
}

func TestATagRefOrABareNameParses(t *testing.T) {
	for _, ref := range []string{
		"refs/tags/v1.2.3",
		"v1.2.3",
		"1.2.3",
		"refs/tags/v1.2.3-rc.1",
		"refs/tags/v1.2.3+build.7",
		"refs/tags/v1.2.3-rc.1+build.7",
	} {
		if _, ok := release.ParseTag(ref); !ok {
			t.Errorf("ParseTag(%q) refused a version", ref)
		}
	}
}

// Anything else keeps the behaviour it always had: built when it appears.
// There is no ordering to supersede it by.
func TestANameThatIsNotAVersionIsRefused(t *testing.T) {
	for _, ref := range []string{
		"refs/tags/latest",
		"refs/tags/release-2026-10",
		"refs/tags/v1.2",
		"refs/tags/v1.2.3.4",
		"refs/tags/v01.2.3",
		"refs/tags/v1.2.3-",
		"refs/tags/v1.2.3-rc..1",
		"refs/tags/v1.2.3-01",
		"refs/tags/v1.2.3+",
		"refs/tags/vv1.2.3",
		"refs/heads/v1.2.3",
		"",
	} {
		if v, ok := release.ParseTag(ref); ok {
			t.Errorf("ParseTag(%q) = %v, want refused", ref, v)
		}
	}
}

// Precedence is semver 2.0.0 §11, in the order the specification lists its
// own examples, plus the cases that tend to be got wrong: numeric rather than
// lexical comparison, and a pre-release sorting below its release.
func TestPrecedenceFollowsSemver(t *testing.T) {
	ascending := []string{
		"v0.0.0",
		"v0.1.0",
		"v0.2.0",
		"v0.10.0",
		"v1.0.0-alpha",
		"v1.0.0-alpha.1",
		"v1.0.0-alpha.beta",
		"v1.0.0-beta",
		"v1.0.0-beta.2",
		"v1.0.0-beta.11",
		"v1.0.0-rc.1",
		"v1.0.0",
		"v1.0.1",
		"v1.1.0",
		"v2.0.0",
		"v10.0.0",
	}
	for i := range ascending {
		for j := range ascending {
			a, b := mustParse(t, ascending[i]), mustParse(t, ascending[j])
			want := 0
			switch {
			case i < j:
				want = -1
			case i > j:
				want = 1
			}
			if got := a.Compare(b); got != want {
				t.Errorf("%s vs %s = %d, want %d", ascending[i], ascending[j], got, want)
			}
		}
	}
}

// Build metadata does not take part in precedence, and neither does the "v":
// both name the same release.
func TestBuildMetadataAndThePrefixDoNotChangePrecedence(t *testing.T) {
	a := mustParse(t, "v1.2.3+linux")
	b := mustParse(t, "1.2.3+darwin")
	if a.Compare(b) != 0 {
		t.Errorf("%v vs %v = %d, want equal", a, b, a.Compare(b))
	}
}

func TestSuperseded(t *testing.T) {
	v := func(s string) release.Version { return mustParse(t, s) }
	shipped := func(s string) *release.Version { x := v(s); return &x }

	cases := []struct {
		name    string
		tag     string
		newest  string
		shipped *release.Version
		want    bool
	}{
		// The incident: three tags from a repository's first week, failing
		// on every retry, behind releases that had long since shipped.
		{"older than what shipped", "v0.1.0", "v0.9.0", shipped("v0.8.0"), true},
		{"the release that shipped", "v0.8.0", "v0.9.0", shipped("v0.8.0"), true},
		// Cut since the last success, even while its own build fails.
		{"newer than what shipped", "v0.9.0", "v1.0.0", shipped("v0.8.0"), false},
		{"the newest", "v1.0.0", "v1.0.0", shipped("v0.8.0"), false},
		// Moved and rebuilt: the newest stays current even once it shipped.
		{"the newest, already shipped", "v1.0.0", "v1.0.0", shipped("v1.0.0"), false},
		// Nothing shipped yet: build the head of the line, not its history.
		{"nothing shipped, older", "v0.2.0", "v0.3.0", nil, true},
		{"nothing shipped, newest", "v0.3.0", "v0.3.0", nil, false},
		// A release candidate is overtaken by the release it led up to.
		{"rc once its release shipped", "v1.0.0-rc.1", "v1.0.0", shipped("v1.0.0"), true},
		{"rc ahead of what shipped", "v1.1.0-rc.1", "v1.1.0-rc.1", shipped("v1.0.0"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := release.Superseded(v(c.tag), v(c.newest), c.shipped); got != c.want {
				t.Errorf("Superseded(%s, newest %s) = %v, want %v", c.tag, c.newest, got, c.want)
			}
		})
	}
}

// A patch on an older major line is usually a security backport. It is judged
// against its own line, so a newer major does not overtake it — but a newer
// release on the same line still does.
func TestLinesJudgeEachMajorOnItsOwn(t *testing.T) {
	var l release.Lines
	for _, tag := range []string{"v0.1.0", "v0.2.0", "v0.3.0", "v1.9.4", "v1.9.5", "v2.0.0"} {
		l.Tagged(mustParse(t, tag))
	}
	l.Shipped(mustParse(t, "v2.0.0"))
	l.Shipped(mustParse(t, "v1.9.5"))

	for tag, want := range map[string]bool{
		"v0.1.0": true,  // the incident: overtaken by v0.3.0
		"v0.2.0": true,  //
		"v0.3.0": false, // head of its line
		"v1.9.4": true,  // behind the shipped v1.9.5
		"v1.9.5": false, // head of its line, though v2.0.0 shipped
		"v2.0.0": false,
	} {
		if got := l.Superseded(mustParse(t, tag)); got != want {
			t.Errorf("Superseded(%s) = %v, want %v", tag, got, want)
		}
	}
}

func TestALineWithNoTagsOvertakesNothing(t *testing.T) {
	var l release.Lines
	l.Shipped(mustParse(t, "v3.0.0"))
	if l.Superseded(mustParse(t, "v1.0.0")) {
		t.Error("a line with no recorded tags overtook a tag")
	}
}

func TestStringIsTheTagName(t *testing.T) {
	if got := mustParse(t, "refs/tags/v1.2.3-rc.1").String(); got != "v1.2.3-rc.1" {
		t.Errorf("String() = %q", got)
	}
}
