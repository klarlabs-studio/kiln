package pipelinefile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"go.klarlabs.de/kiln/internal/domain/config"
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

func TestLoadDirMissingFileIsNotFound(t *testing.T) {
	p, err := LoadDir(t.TempDir())

	if !errors.Is(err, config.ErrNotFound) {
		t.Fatalf("err = %v, want config.ErrNotFound", err)
	}
	// A library repo with no pipeline still proves and still reports a Check.
	if !p.Wants("push", config.StepProve) {
		t.Error("default pipeline must prove")
	}
	if p.WantsPublish() {
		t.Error("default pipeline must not publish")
	}
}

func TestLoadDirReadsFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, config.FileName), []byte(minimal), 0o600); err != nil {
		t.Fatal(err)
	}

	p, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if p.Publish[0].Image != "ghcr.io/klarlabs-studio/kiln" {
		t.Errorf("image = %q", p.Publish[0].Image)
	}
}
