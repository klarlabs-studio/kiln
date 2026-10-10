package publish

import (
	"strings"
	"testing"

	"go.klarlabs.de/kiln/internal/application/ports"
	"go.klarlabs.de/kiln/internal/domain/config"
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

const placeholderSHAFull = "c3f7aca11112222333344445555666677778888"

func placeholderArtifact() config.Artifact {
	return config.Artifact{
		Image: "ghcr.io/acme/web",
		Tags:  []config.Tag{config.TagSHA, config.TagSemver, config.TagLatest},
		Args: map[string]string{
			"VERSION": "${kiln.version}", "REVISION": "${kiln.sha}", "SHORT": "${kiln.short_sha}",
			"LABEL": "v${kiln.version}-x", "FIXED": "literal",
		},
	}
}

func TestBuildPlan_ResolvesPlaceholders(t *testing.T) {
	plan, err := BuildPlan(placeholderArtifact(), placeholderSHAFull, "refs/tags/v1.2.3")
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	want := map[string]string{
		"VERSION": "1.2.3", "REVISION": placeholderSHAFull, "SHORT": "c3f7aca",
		"LABEL": "v1.2.3-x", "FIXED": "literal",
	}
	for k, w := range want {
		if plan.Args[k] != w {
			t.Errorf("Args[%s] = %q, want %q", k, plan.Args[k], w)
		}
	}
	if flags := strings.Join(plan.BuildArgFlags(), " "); !strings.Contains(flags, "--build-arg VERSION=1.2.3") {
		t.Errorf("flags = %s", flags)
	}
	if !strings.Contains(plan.String(), "VERSION=1.2.3") {
		t.Errorf("plan output does not show resolved value:\n%s", plan)
	}
}

func TestBuildPlan_VersionPlaceholderNeedsATag(t *testing.T) {
	if _, err := BuildPlan(placeholderArtifact(), placeholderSHAFull, "refs/heads/main"); err == nil ||
		!strings.Contains(err.Error(), "kiln.version") {
		t.Fatalf("err = %v, want kiln.version error", err)
	}
	// Without a version placeholder a branch build is unaffected.
	a := placeholderArtifact()
	a.Args = map[string]string{"REVISION": "${kiln.sha}"}
	plan, err := BuildPlan(a, placeholderSHAFull, "refs/heads/main")
	if err != nil || plan.Args["REVISION"] != placeholderSHAFull {
		t.Fatalf("plan = %v, err = %v", plan.Args, err)
	}
}

// The provenance must record what the image was built with, not the template.
func TestImageAttestInput_RecordsResolvedArgs(t *testing.T) {
	req := ports.PublishRequest{Artifact: placeholderArtifact(), SHA: placeholderSHAFull, Ref: "refs/tags/v1.2.3"}
	plan, err := BuildPlan(req.Artifact, req.SHA, req.Ref)
	if err != nil {
		t.Fatal(err)
	}
	in := imageAttestInput(plan, plan.Image, "sha256:abc", req)
	if in.BuildArgs["VERSION"] != "1.2.3" || in.BuildArgs["REVISION"] != placeholderSHAFull {
		t.Errorf("BuildArgs = %v", in.BuildArgs)
	}
	if in.SubjectDigest != "sha256:abc" || in.SubjectName != plan.Image {
		t.Errorf("subject = %q %q", in.SubjectName, in.SubjectDigest)
	}
}
