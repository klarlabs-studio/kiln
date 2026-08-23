package config

import (
	"strings"
	"testing"
)

const minimal = `
apiVersion: kiln.klarlabs.de/v1
kind: Pipeline
on:
  pull_request: [prove]
  push: [prove, publish]
prove:
  from: warden
publish:
  - kind: image
    image: ghcr.io/klarlabs-studio/kiln
    tags: [sha, latest]
`

func parse(t *testing.T, doc string) Pipeline {
	t.Helper()
	p, err := Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return p
}

func parseErr(t *testing.T, doc string) error {
	t.Helper()
	if _, err := Parse(strings.NewReader(doc)); err != nil {
		return err
	}
	t.Fatal("Parse: want error, got nil")
	return nil
}

func TestParseMinimal(t *testing.T) {
	p := parse(t, minimal)

	if !p.Wants("pull_request", StepProve) || p.Wants("pull_request", StepPublish) {
		t.Errorf("pull_request steps = %v", p.On.PullRequest)
	}
	if !p.Wants("push", StepPublish) {
		t.Errorf("push steps = %v", p.On.Push)
	}
	if p.Publish[0].Dockerfile != "Dockerfile" || p.Publish[0].Context != "." || p.Publish[0].Sign != "cosign" {
		t.Errorf("publish defaults not applied: %+v", p.Publish[0])
	}
	if got := p.Publish[0].Platforms; len(got) != 1 || got[0] != "linux/amd64" {
		t.Errorf("platforms = %v, want [linux/amd64]", got)
	}
	if p.Watch.Remote != "origin" || p.Watch.Ref != "main" {
		t.Errorf("watch defaults = %+v", p.Watch)
	}
	if !p.WatchPullRequests() || !p.WatchTags() {
		t.Error("watch discovery toggles must default to true")
	}
}

func TestTagEventInheritsPush(t *testing.T) {
	p := parse(t, minimal)

	if !p.Wants("tag", StepPublish) {
		t.Errorf("tag steps = %v, want to inherit push", p.On.Tag)
	}
}

func TestExplicitTagEventWins(t *testing.T) {
	p := parse(t, minimal+`
`)
	_ = p

	doc := `
apiVersion: kiln.klarlabs.de/v1
kind: Pipeline
on:
  push: [prove, publish]
  tag: [prove]
prove: {from: warden}
publish: [{kind: image, image: ghcr.io/x/y, tags: [sha, latest]}]
`
	got := parse(t, doc)
	if got.Wants("tag", StepPublish) {
		t.Errorf("explicit tag list must not inherit push: %v", got.On.Tag)
	}
}

func TestWatchFalseIsHonoured(t *testing.T) {
	doc := minimal + `
watch:
  pull_requests: false
  tags: false
`
	p := parse(t, doc)
	if p.WatchPullRequests() || p.WatchTags() {
		t.Error("explicit false must not be overwritten by the true default")
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	err := parseErr(t, minimal+"\nbogus: 1\n")
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error should name the unknown field, got %v", err)
	}
}

func TestCDKeysRejectedByName(t *testing.T) {
	for _, key := range cdKeys {
		doc := minimal + "\n" + key + ":\n  target: prod\n"
		err := parseErr(t, doc)
		if !strings.Contains(err.Error(), "RollOps") {
			t.Errorf("%s: error should point at RollOps, got %v", key, err)
		}
	}
}

func TestSHAOnlyTagsRejected(t *testing.T) {
	doc := `
apiVersion: kiln.klarlabs.de/v1
kind: Pipeline
on: {push: [prove, publish]}
publish:
  - kind: image
    image: ghcr.io/x/y
    tags: [sha]
`
	err := parseErr(t, doc)
	if !strings.Contains(err.Error(), "sha-only") {
		t.Errorf("want sha-only rejection, got %v", err)
	}
}

func TestTagsMustIncludeSHA(t *testing.T) {
	doc := `
apiVersion: kiln.klarlabs.de/v1
kind: Pipeline
on: {push: [prove, publish]}
publish:
  - kind: image
    image: ghcr.io/x/y
    tags: [latest]
`
	err := parseErr(t, doc)
	if !strings.Contains(err.Error(), `"sha"`) {
		t.Errorf("want missing-sha rejection, got %v", err)
	}
}

func TestProveFromMustBeWarden(t *testing.T) {
	doc := strings.Replace(minimal, "from: warden", "from: github-actions", 1)

	err := parseErr(t, doc)
	if !strings.Contains(err.Error(), "warden") {
		t.Errorf("want prove.from rejection, got %v", err)
	}
}

func TestPublishWithoutProveRejected(t *testing.T) {
	doc := `
apiVersion: kiln.klarlabs.de/v1
kind: Pipeline
on: {push: [publish]}
publish: [{kind: image, image: ghcr.io/x/y, tags: [sha, latest]}]
`
	err := parseErr(t, doc)
	if !strings.Contains(err.Error(), "ungated") {
		t.Errorf("want publish-without-prove rejection, got %v", err)
	}
}

func TestPublishRoutedButNotConfigured(t *testing.T) {
	doc := `
apiVersion: kiln.klarlabs.de/v1
kind: Pipeline
on: {push: [prove, publish]}
`
	err := parseErr(t, doc)
	if !strings.Contains(err.Error(), "publish:") {
		t.Errorf("want empty-publish-list rejection, got %v", err)
	}
}

func TestUnknownStepRejected(t *testing.T) {
	doc := `
apiVersion: kiln.klarlabs.de/v1
kind: Pipeline
on: {push: [prove, deploy]}
`
	err := parseErr(t, doc)
	if !strings.Contains(err.Error(), "unknown step") {
		t.Errorf("want unknown-step rejection, got %v", err)
	}
}

func TestWrongAPIVersionRejected(t *testing.T) {
	doc := strings.Replace(minimal, APIVersion, "kiln.klarlabs.de/v2", 1)

	if err := parseErr(t, doc); !strings.Contains(err.Error(), "apiVersion") {
		t.Errorf("want apiVersion rejection, got %v", err)
	}
}

func TestEmptyDocumentRejected(t *testing.T) {
	if err := parseErr(t, ""); !strings.Contains(err.Error(), "empty pipeline") {
		t.Errorf("want empty rejection, got %v", err)
	}
}

func TestTagKindsIsStable(t *testing.T) {
	doc := strings.Replace(minimal, "tags: [sha, latest]", "tags: [semver, sha, latest]", 1)
	p := parse(t, doc)

	first := p.Publish[0].TagKinds()
	second := p.Publish[0].TagKinds()
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("TagKinds not stable: %v vs %v", first, second)
		}
	}
	// The caller-visible slice must not alias the config's own backing array.
	first[0] = "mutated"
	if p.Publish[0].Tags[0] == "mutated" {
		t.Error("TagKinds returned an aliasing slice")
	}
}

const bothKinds = `
apiVersion: kiln.klarlabs.de/v1
kind: Pipeline
on:
  pull_request: [prove]
  push: [prove, publish]
  tag: [prove, publish]
publish:
  - kind: image
    image: ghcr.io/klarlabs-studio/kiln
    tags: [sha, latest, semver]
  - kind: binaries
`

func TestOneCommitCanYieldSeveralArtifacts(t *testing.T) {
	p := parse(t, bothKinds)

	if len(p.Publish) != 2 {
		t.Fatalf("publish has %d entries, want 2", len(p.Publish))
	}
	if p.Publish[0].Kind != KindImage || p.Publish[1].Kind != KindBinaries {
		t.Errorf("kinds = %q, %q", p.Publish[0].Kind, p.Publish[1].Kind)
	}
}

func TestBinariesDefaults(t *testing.T) {
	p := parse(t, bothKinds)
	bin := p.Publish[1]

	// goreleaser owns the release language, the way warden owns the check
	// language; both get named in the file rather than assumed.
	if bin.From != "goreleaser" || bin.Config != ".goreleaser.yaml" {
		t.Errorf("binaries defaults = %+v", bin)
	}
	if bin.Sign != "cosign" {
		t.Errorf("sign = %q, want cosign", bin.Sign)
	}
}

func TestBinariesDefaultToTagsOnly(t *testing.T) {
	p := parse(t, bothKinds)

	// goreleaser derives the version from the tag. A binary release on a branch
	// push would publish something with a version nobody can ask for.
	if got := p.ArtifactsFor("push"); len(got) != 1 || got[0].Kind != KindImage {
		t.Errorf("push artifacts = %+v, want the image alone", got)
	}
	got := p.ArtifactsFor("tag")
	if len(got) != 2 {
		t.Fatalf("tag artifacts = %+v, want both", got)
	}
}

func TestExplicitArtifactOnWins(t *testing.T) {
	doc := `
apiVersion: kiln.klarlabs.de/v1
kind: Pipeline
on:
  push: [prove, publish]
  tag: [prove, publish]
publish:
  - kind: image
    image: ghcr.io/x/y
    tags: [sha, latest]
    on: [tag]
  - kind: binaries
    on: [push, tag]
`
	p := parse(t, doc)

	if got := p.ArtifactsFor("push"); len(got) != 1 || got[0].Kind != KindBinaries {
		t.Errorf("push artifacts = %+v", got)
	}
	if got := p.ArtifactsFor("tag"); len(got) != 2 {
		t.Errorf("tag artifacts = %+v, want both", got)
	}
}

func TestArtifactsForAnUnroutedEventIsEmpty(t *testing.T) {
	p := parse(t, bothKinds)

	// pull_request routes to prove only, so nothing publishes however
	// permissive an individual artifact's `on` list is.
	if got := p.ArtifactsFor("pull_request"); len(got) != 0 {
		t.Errorf("pull_request artifacts = %+v, want none", got)
	}
}

func TestKindDefaultsToImage(t *testing.T) {
	doc := `
apiVersion: kiln.klarlabs.de/v1
kind: Pipeline
on: {push: [prove, publish]}
publish:
  - image: ghcr.io/x/y
    tags: [sha, latest]
`
	p := parse(t, doc)

	if p.Publish[0].Kind != KindImage {
		t.Errorf("kind = %q, want the image shorthand to work", p.Publish[0].Kind)
	}
}

func TestImageFieldOnBinariesIsRejected(t *testing.T) {
	doc := `
apiVersion: kiln.klarlabs.de/v1
kind: Pipeline
on: {tag: [prove, publish]}
publish:
  - kind: binaries
    image: ghcr.io/x/y
`
	err := parseErr(t, doc)

	// Silently ignoring it would let an operator write a setting that never
	// takes effect — the failure KnownFields exists to prevent, one level down.
	if !strings.Contains(err.Error(), "would be ignored") {
		t.Errorf("want a misplaced-field rejection, got %v", err)
	}
}

func TestBinariesFieldOnImageIsRejected(t *testing.T) {
	doc := `
apiVersion: kiln.klarlabs.de/v1
kind: Pipeline
on: {push: [prove, publish]}
publish:
  - kind: image
    image: ghcr.io/x/y
    tags: [sha, latest]
    config: .goreleaser.yaml
`
	if err := parseErr(t, doc); !strings.Contains(err.Error(), "would be ignored") {
		t.Errorf("want a misplaced-field rejection, got %v", err)
	}
}

func TestUnknownArtifactKindRejected(t *testing.T) {
	doc := `
apiVersion: kiln.klarlabs.de/v1
kind: Pipeline
on: {push: [prove, publish]}
publish:
  - kind: helm-chart
`
	if err := parseErr(t, doc); !strings.Contains(err.Error(), "unknown artifact kind") {
		t.Errorf("want an unknown-kind rejection, got %v", err)
	}
}

func TestBinariesFromMustBeGoreleaser(t *testing.T) {
	doc := `
apiVersion: kiln.klarlabs.de/v1
kind: Pipeline
on: {tag: [prove, publish]}
publish:
  - kind: binaries
    from: ko
`
	err := parseErr(t, doc)
	if !strings.Contains(err.Error(), ".goreleaser.yaml") {
		t.Errorf("error should name the release language, got %v", err)
	}
}

func TestUnknownArtifactEventRejected(t *testing.T) {
	doc := `
apiVersion: kiln.klarlabs.de/v1
kind: Pipeline
on: {push: [prove, publish]}
publish:
  - kind: binaries
    on: [release]
`
	if err := parseErr(t, doc); !strings.Contains(err.Error(), "unknown event") {
		t.Errorf("want an unknown-event rejection, got %v", err)
	}
}

func TestDuplicateImagesRejected(t *testing.T) {
	doc := `
apiVersion: kiln.klarlabs.de/v1
kind: Pipeline
on: {push: [prove, publish]}
publish:
  - kind: image
    image: ghcr.io/x/y
    tags: [sha, latest]
  - kind: image
    image: ghcr.io/x/y
    tags: [sha, semver]
`
	err := parseErr(t, doc)

	// Two entries racing each other onto :latest, with the winner decided by
	// list order nobody wrote down.
	if !strings.Contains(err.Error(), "already published") {
		t.Errorf("want a duplicate-image rejection, got %v", err)
	}
}

func TestTwoDistinctImagesAreFine(t *testing.T) {
	doc := `
apiVersion: kiln.klarlabs.de/v1
kind: Pipeline
on: {push: [prove, publish]}
publish:
  - kind: image
    image: ghcr.io/x/api
    tags: [sha, latest]
  - kind: image
    image: ghcr.io/x/worker
    tags: [sha, latest]
`
	if p := parse(t, doc); len(p.Publish) != 2 {
		t.Errorf("a monorepo publishing two images must be allowed: %+v", p.Publish)
	}
}

func TestKeepDefaultsAndIsHonoured(t *testing.T) {
	p := parse(t, minimal)
	if got := p.Publish[0].Keep; got == nil || *got != 10 {
		t.Errorf("Keep = %v, want the default", got)
	}

	explicit := parse(t, strings.Replace(minimal,
		"    tags: [sha, latest]", "    tags: [sha, latest]\n    keep: 3", 1))
	if got := explicit.Publish[0].Keep; got == nil || *got != 3 {
		t.Errorf("Keep = %v, want 3", got)
	}
}

func TestKeepZeroDisablesPruning(t *testing.T) {
	p := parse(t, strings.Replace(minimal,
		"    tags: [sha, latest]", "    tags: [sha, latest]\n    keep: 0", 1))

	// An operator who wrote 0 meant "never prune this image", not "give me
	// the default" — which is why it is a pointer.
	if got := p.PrunableImages()["ghcr.io/klarlabs-studio/kiln"]; got != 0 {
		t.Errorf("keep = %d, want 0 to survive", got)
	}
}

func TestKeepOnBinariesIsRejected(t *testing.T) {
	doc := `
apiVersion: kiln.klarlabs.de/v1
kind: Pipeline
on: {tag: [prove, publish]}
publish:
  - kind: binaries
    keep: 5
`
	if err := parseErr(t, doc); !strings.Contains(err.Error(), "would be ignored") {
		t.Errorf("want a misplaced-field rejection, got %v", err)
	}
}

func TestPrunableImagesListsOnlyImages(t *testing.T) {
	p := parse(t, bothKinds)

	got := p.PrunableImages()
	if len(got) != 1 {
		t.Fatalf("PrunableImages = %v, want just the image", got)
	}
	if _, ok := got["ghcr.io/klarlabs-studio/kiln"]; !ok {
		t.Errorf("PrunableImages = %v", got)
	}
}
