package publish

import (
	"strings"
	"testing"

	"go.klarlabs.de/kiln/internal/config"
)

// Three of senat-os's six images share one Dockerfile and differ only by a
// BIN= build arg. Without this, kiln cannot express that repository's builds
// at all — which is how the gap was found.
func TestBuildPlan_CarriesBuildArgs(t *testing.T) {
	plan, err := BuildPlan(config.Artifact{
		Image: "ghcr.io/acme/senat-api",
		Tags:  []config.Tag{config.TagSHA, config.TagLatest},
		Args:  map[string]string{"BIN": "api"},
	}, "c3f7aca11112222333344445555666677778888", "refs/heads/main")
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if got := plan.Args["BIN"]; got != "api" {
		t.Errorf("Args[BIN] = %q, want api", got)
	}
}

// Docker accepts --build-arg in any order, but the plan is shown to an
// operator and recorded in provenance, so it has to render the same way every
// time or two identical builds look different.
func TestPlan_BuildArgFlagsAreSorted(t *testing.T) {
	p := Plan{Args: map[string]string{"ZED": "3", "ALPHA": "1", "MID": "2"}}

	got := p.BuildArgFlags()
	want := []string{
		"--build-arg", "ALPHA=1",
		"--build-arg", "MID=2",
		"--build-arg", "ZED=3",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("BuildArgFlags() = %v\nwant %v", got, want)
	}
}

func TestPlan_NoArgsMeansNoFlags(t *testing.T) {
	if got := (Plan{}).BuildArgFlags(); len(got) != 0 {
		t.Errorf("BuildArgFlags() = %v, want none", got)
	}
}

// The args change what the image contains, so an operator reading a dry run
// has to see them — two plans differing only by BIN are otherwise identical
// on screen.
func TestPlan_StringShowsBuildArgs(t *testing.T) {
	p := Plan{
		Image: "ghcr.io/acme/senat-api", SHATag: "ghcr.io/acme/senat-api:sha-c3f7aca",
		Dockerfile: "deploy/Dockerfile", Context: ".", Platforms: []string{"linux/amd64"},
		Args: map[string]string{"BIN": "api"},
	}
	if out := p.String(); !strings.Contains(out, "BIN=api") {
		t.Errorf("plan does not show its build args:\n%s", out)
	}
}
