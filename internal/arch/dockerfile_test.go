package arch

import (
	"bytes"
	"os"
	"testing"
)

// TestDockerfilePinsBaseImages keeps Kiln's own image from floating.
//
// Actions are pinned to commits. A FROM line that names only a tag is the
// same hole: anyone who can move golang:1.25-bookworm or the distroless
// nonroot tag changes what this repository builds without a reviewable diff.
func TestDockerfilePinsBaseImages(t *testing.T) {
	raw, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"golang:1.25-bookworm@sha256:",
		"gcr.io/distroless/static-debian12:nonroot@sha256:",
		"org.opencontainers.image.authors=",
		"HEALTHCHECK",
	} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Errorf("Dockerfile base %q is not digest-pinned", want)
		}
	}
}
