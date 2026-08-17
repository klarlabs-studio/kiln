package config

import (
	"errors"
	"os"
	"path/filepath"
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
	if p.Publish.Dockerfile != "Dockerfile" || p.Publish.Context != "." || p.Publish.Sign != "cosign" {
		t.Errorf("publish defaults not applied: %+v", *p.Publish)
	}
	if got := p.Publish.Platforms; len(got) != 1 || got[0] != "linux/amd64" {
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
publish: {image: ghcr.io/x/y, tags: [sha, latest]}
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
publish: {image: ghcr.io/x/y, tags: [sha, latest]}
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
		t.Errorf("want missing-publish-block rejection, got %v", err)
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

func TestLoadDirMissingFileIsNotFound(t *testing.T) {
	p, err := LoadDir(t.TempDir())

	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	// A library repo with no pipeline still proves and still reports a Check.
	if !p.Wants("push", StepProve) {
		t.Error("default pipeline must prove")
	}
	if p.WantsPublish() {
		t.Error("default pipeline must not publish")
	}
}

func TestLoadDirReadsFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(minimal), 0o600); err != nil {
		t.Fatal(err)
	}

	p, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if p.Publish.Image != "ghcr.io/klarlabs-studio/kiln" {
		t.Errorf("image = %q", p.Publish.Image)
	}
}

func TestTagKindsIsStable(t *testing.T) {
	doc := strings.Replace(minimal, "tags: [sha, latest]", "tags: [semver, sha, latest]", 1)
	p := parse(t, doc)

	first := p.Publish.TagKinds()
	second := p.Publish.TagKinds()
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("TagKinds not stable: %v vs %v", first, second)
		}
	}
	// The caller-visible slice must not alias the config's own backing array.
	first[0] = "mutated"
	if p.Publish.Tags[0] == "mutated" {
		t.Error("TagKinds returned an aliasing slice")
	}
}
