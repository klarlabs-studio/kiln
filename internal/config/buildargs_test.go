package config

import (
	"strings"
	"testing"
)

const argsBase = `apiVersion: kiln.klarlabs.de/v1
kind: Pipeline
on:
  push: [prove, publish]
publish:
`

// The case this feature exists for: senat-os builds senat-api and
// senat-runtime from one Dockerfile, differing only by BIN=.
func TestArgs_OneDockerfileTwoImages(t *testing.T) {
	p, err := Parse(strings.NewReader(argsBase + `  - kind: image
    image: ghcr.io/acme/senat-api
    dockerfile: deploy/Dockerfile
    tags: [sha, latest]
    args:
      BIN: api
  - kind: image
    image: ghcr.io/acme/senat-runtime
    dockerfile: deploy/Dockerfile
    tags: [sha, latest]
    args:
      BIN: runtime
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(p.Publish) != 2 {
		t.Fatalf("publish entries = %d, want 2", len(p.Publish))
	}
	if got := p.Publish[0].Args["BIN"]; got != "api" {
		t.Errorf("first args BIN = %q", got)
	}
	if got := p.Publish[1].Args["BIN"]; got != "runtime" {
		t.Errorf("second args BIN = %q", got)
	}
}

// A build argument on a binaries artifact is a mistake, and the loader rejects
// fields belonging to the other kind rather than ignoring them.
func TestArgs_RefusedOnBinaries(t *testing.T) {
	_, err := Parse(strings.NewReader(argsBase + `  - kind: binaries
    from: goreleaser
    config: .goreleaser.yaml
    args:
      BIN: api
`))
	if err == nil {
		t.Fatal("accepted build args on a binaries artifact")
	}
	if !strings.Contains(err.Error(), "args") {
		t.Errorf("error does not name the field: %v", err)
	}
}

// No args is the normal case and must stay valid.
func TestArgs_OptionalOnImages(t *testing.T) {
	if _, err := Parse(strings.NewReader(argsBase + `  - kind: image
    image: ghcr.io/acme/app
    tags: [sha, latest]
`)); err != nil {
		t.Fatalf("an image without args should parse: %v", err)
	}
}
