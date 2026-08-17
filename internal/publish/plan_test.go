package publish

import (
	"slices"
	"strings"
	"testing"

	"go.klarlabs.de/kiln/internal/config"
)

const (
	image = "ghcr.io/felixgeelhaar/glossa-api"
	sha   = "abc1234def5678901234"
)

func cfg(tags ...config.Tag) config.Publish {
	return config.Publish{
		Image: image, Tags: tags, Sign: "cosign",
		Platforms: []string{"linux/amd64"}, Dockerfile: "Dockerfile", Context: ".",
	}
}

func TestPlanProducesSHAAndMovingTags(t *testing.T) {
	plan, err := BuildPlan(cfg(config.TagSHA, config.TagLatest), sha, "refs/heads/main")
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	if plan.SHATag != image+":sha-abc1234" {
		t.Errorf("SHATag = %q", plan.SHATag)
	}
	if !slices.Equal(plan.MovingTags, []string{image + ":latest"}) {
		t.Errorf("MovingTags = %v", plan.MovingTags)
	}
	// The contract with RollOps: pin the sha, follow a moving tag.
	if got := plan.Refs(); len(got) != 2 || got[0] != plan.SHATag {
		t.Errorf("Refs = %v, want the sha tag first", got)
	}
}

func TestPlanSemverFromATagRef(t *testing.T) {
	plan, err := BuildPlan(cfg(config.TagSHA, config.TagSemver), sha, "refs/tags/v0.2.0")
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	if !slices.Contains(plan.MovingTags, image+":v0.2.0") {
		t.Errorf("MovingTags = %v, want the version tag", plan.MovingTags)
	}
}

func TestPlanSemverWithoutTheVPrefix(t *testing.T) {
	plan, err := BuildPlan(cfg(config.TagSHA, config.TagSemver, config.TagLatest), sha, "refs/tags/1.4.2")
	if err != nil {
		t.Fatal(err)
	}

	if !slices.Contains(plan.MovingTags, image+":1.4.2") {
		t.Errorf("MovingTags = %v", plan.MovingTags)
	}
}

func TestPlanSemverPrerelease(t *testing.T) {
	plan, err := BuildPlan(cfg(config.TagSHA, config.TagSemver), sha, "refs/tags/v1.0.0-rc.1")
	if err != nil {
		t.Fatal(err)
	}

	if !slices.Contains(plan.MovingTags, image+":v1.0.0-rc.1") {
		t.Errorf("MovingTags = %v", plan.MovingTags)
	}
}

func TestPlanRewritesSemverBuildMetadata(t *testing.T) {
	// '+' is legal in semver and illegal in an OCI tag. Dropping the metadata
	// would let two different builds collide on one tag, so it is rewritten.
	plan, err := BuildPlan(cfg(config.TagSHA, config.TagSemver), sha, "refs/tags/v1.0.0+build.7")
	if err != nil {
		t.Fatal(err)
	}

	if !slices.Contains(plan.MovingTags, image+":v1.0.0_build.7") {
		t.Errorf("MovingTags = %v, want the rewritten tag", plan.MovingTags)
	}
	if len(plan.Notes) == 0 {
		t.Error("a rewritten tag must be reported to the operator")
	}
}

func TestPlanSemverOnABranchFallsBackToLatest(t *testing.T) {
	plan, err := BuildPlan(cfg(config.TagSHA, config.TagLatest, config.TagSemver), sha, "refs/heads/main")
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	if slices.ContainsFunc(plan.MovingTags, func(s string) bool { return strings.Contains(s, "v") }) {
		t.Errorf("a branch push must not produce a version tag: %v", plan.MovingTags)
	}
	if !slices.Contains(plan.MovingTags, image+":latest") {
		t.Errorf("MovingTags = %v, want latest to carry the build", plan.MovingTags)
	}
	if len(plan.Notes) == 0 {
		t.Error("the operator should be told why no version tag appeared")
	}
}

func TestPlanSemverOnlyOnABranchIsAnError(t *testing.T) {
	// [sha, semver] on a branch push yields no moving tag at all: RollOps'
	// imagePolicy would never discover the build. Fail loudly rather than
	// publish something nothing can find.
	_, err := BuildPlan(cfg(config.TagSHA, config.TagSemver), sha, "refs/heads/main")

	if err == nil {
		t.Fatal("want an error when no moving tag can be produced")
	}
	if !strings.Contains(err.Error(), "imagePolicy") {
		t.Errorf("error should explain the RollOps consequence, got %v", err)
	}
}

func TestPlanNonVersionTagIsNotASemverTag(t *testing.T) {
	_, err := BuildPlan(cfg(config.TagSHA, config.TagSemver), sha, "refs/tags/nightly")

	if err == nil {
		t.Fatal("want an error: 'nightly' is not a version")
	}
}

func TestPlanRejectsMissingImageOrSHA(t *testing.T) {
	if _, err := BuildPlan(config.Publish{Tags: []config.Tag{config.TagSHA}}, sha, "r"); err == nil {
		t.Error("BuildPlan accepted an empty image")
	}
	if _, err := BuildPlan(cfg(config.TagSHA, config.TagLatest), " ", "r"); err == nil {
		t.Error("BuildPlan accepted an empty commit")
	}
}

func TestPlanRejectsAMissingSHATag(t *testing.T) {
	// Defence in depth: the config loader rejects this too, but a plan without
	// an immutable tag leaves RollOps nothing to pin.
	_, err := BuildPlan(cfg(config.TagLatest), sha, "refs/heads/main")

	if err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Errorf("err = %v, want a missing-sha-tag rejection", err)
	}
}

func TestPlanAppliesDefaults(t *testing.T) {
	plan, err := BuildPlan(config.Publish{
		Image: image, Tags: []config.Tag{config.TagSHA, config.TagLatest},
	}, sha, "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}

	if plan.Dockerfile != "Dockerfile" || plan.Context != "." {
		t.Errorf("build defaults = %q/%q", plan.Dockerfile, plan.Context)
	}
	if !slices.Equal(plan.Platforms, []string{"linux/amd64"}) {
		t.Errorf("Platforms = %v", plan.Platforms)
	}
}

func TestPlanStringIsReadable(t *testing.T) {
	plan, _ := BuildPlan(cfg(config.TagSHA, config.TagLatest), sha, "refs/heads/main")

	out := plan.String()
	for _, want := range []string{image, "sha-abc1234", ":latest", "Dockerfile", "linux/amd64"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan output missing %q:\n%s", want, out)
		}
	}
}

func TestPlanIsStableAcrossTagOrder(t *testing.T) {
	a, _ := BuildPlan(cfg(config.TagLatest, config.TagSHA), sha, "refs/heads/main")
	b, _ := BuildPlan(cfg(config.TagSHA, config.TagLatest), sha, "refs/heads/main")

	if !slices.Equal(a.Refs(), b.Refs()) {
		t.Errorf("tag order in the config changed the plan: %v vs %v", a.Refs(), b.Refs())
	}
}
