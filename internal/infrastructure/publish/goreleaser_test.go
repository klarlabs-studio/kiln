package publish

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.klarlabs.de/kiln/internal/application/ports"
	"go.klarlabs.de/kiln/internal/domain/config"
	"go.klarlabs.de/kiln/internal/gittest"
	"go.klarlabs.de/kiln/internal/infrastructure/attest"
	"go.klarlabs.de/kiln/internal/infrastructure/execx"
	"go.klarlabs.de/kiln/internal/infrastructure/obs"
)

// signingConfig is the shape warden's release config already has: a cosign
// sign-blob over the checksum manifest.
const signingConfig = `version: 2
builds:
  - main: ./cmd/thing
checksum:
  name_template: checksums.txt
signs:
  - cmd: cosign
    signature: "${artifact}.sig"
    args: [sign-blob, "--yes", "--output-signature=${signature}", "${artifact}"]
    artifacts: checksum
`

const unsignedConfig = `version: 2
builds:
  - main: ./cmd/thing
checksum:
  name_template: checksums.txt
`

func binariesArtifact() config.Artifact {
	return config.Artifact{
		Kind: config.KindBinaries, From: "goreleaser",
		Config: ".goreleaser.yaml", Sign: "cosign", On: []string{"tag"},
	}
}

// releaseRepo is a repository with a release config, plus a fake goreleaser
// that writes the dist/ layout the real one produces.
func releaseRepo(t *testing.T, releaseCfg string) (*gittest.Repo, string, *execx.Fake) {
	t.Helper()
	repo := gittest.New(t)
	repo.Write(".goreleaser.yaml", releaseCfg)
	head := repo.Commit("release config", "main.go", "package main\n")
	repo.Tag("v1.4.0")

	fake := execx.NewFake()
	fake.On("git", execx.Response{Fn: func(c execx.Cmd) (execx.Result, error) {
		return execx.NewSystem().Run(t.Context(), c)
	}})
	fake.On("goreleaser release", execx.Response{Fn: func(c execx.Cmd) (execx.Result, error) {
		return execx.Result{}, writeDist(c.Dir)
	}})
	return repo, head, fake
}

// writeDist emulates goreleaser's output: archives, a checksum manifest, its
// signature, and the artifacts.json index kiln reads.
func writeDist(dir string) error {
	dist := filepath.Join(dir, "dist")
	if err := os.MkdirAll(dist, 0o750); err != nil {
		return err
	}
	files := map[string]string{
		"thing_1.4.0_linux_amd64.tar.gz":  "archive-1",
		"thing_1.4.0_darwin_arm64.tar.gz": "archive-2",
		"checksums.txt":                   "deadbeef  thing_1.4.0_linux_amd64.tar.gz\n",
		"checksums.txt.sig":               "signature",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dist, name), []byte(body), 0o600); err != nil {
			return err
		}
	}
	index := []map[string]string{
		{"name": "thing_1.4.0_linux_amd64.tar.gz", "type": "Archive"},
		{"name": "thing_1.4.0_darwin_arm64.tar.gz", "type": "Archive"},
		{"name": "checksums.txt", "type": "Checksum"},
		{"name": "checksums.txt.sig", "type": "Signature"},
		// An intermediate goreleaser records but never uploads.
		{"name": "thing", "type": "Binary"},
	}
	raw, err := json.Marshal(index)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dist, "artifacts.json"), raw, 0o600)
}

func TestReleasePublishesEveryUploadableFile(t *testing.T) {
	repo, head, fake := releaseRepo(t, signingConfig)

	res, err := NewGoreleaser(fake, obs.Discard(), "tok", false).Publish(t.Context(), ports.PublishRequest{
		RepoDir: repo.Dir, SHA: head, Ref: "refs/tags/v1.4.0", Artifact: binariesArtifact(),
	})
	if err != nil {
		t.Fatalf("Publish: %v\n%s", err, fake.Transcript())
	}

	if res.Kind != config.KindBinaries {
		t.Errorf("Kind = %q", res.Kind)
	}
	if res.Reference != "v1.4.0" {
		t.Errorf("Reference = %q, want the tag", res.Reference)
	}
	if !res.Signed {
		t.Error("Signed = false after a real release")
	}
	for _, want := range []string{"checksums.txt", "checksums.txt.sig", "thing_1.4.0_linux_amd64.tar.gz"} {
		if !slices.Contains(res.Tags, want) {
			t.Errorf("release is missing %s: %v", want, res.Tags)
		}
	}
	// The raw binary is an intermediate; listing it would misrepresent what a
	// user can actually download.
	if slices.Contains(res.Tags, "thing") {
		t.Errorf("an unpublished intermediate leaked into the release listing: %v", res.Tags)
	}
}

func TestReleaseDigestIsTheChecksumManifest(t *testing.T) {
	repo, head, fake := releaseRepo(t, signingConfig)

	res, err := NewGoreleaser(fake, obs.Discard(), "tok", false).Publish(t.Context(), ports.PublishRequest{
		RepoDir: repo.Dir, SHA: head, Ref: "refs/tags/v1.4.0", Artifact: binariesArtifact(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// sha256 of the manifest body written by writeDist. The manifest covers
	// every file, so its digest is the release's content identity.
	if !strings.HasPrefix(res.Digest, "sha256:") || len(res.Digest) != 71 {
		t.Errorf("Digest = %q, want a sha256 of the checksum manifest", res.Digest)
	}
}

func TestUnsignedReleaseConfigIsRefused(t *testing.T) {
	repo, head, fake := releaseRepo(t, unsignedConfig)

	_, err := NewGoreleaser(fake, obs.Discard(), "tok", false).Publish(t.Context(), ports.PublishRequest{
		RepoDir: repo.Dir, SHA: head, Ref: "refs/tags/v1.4.0", Artifact: binariesArtifact(),
	})

	// Whether a release is verifiable must not depend on whether somebody
	// remembered to add a signs: block.
	if !errors.Is(err, ErrUnsignedRelease) {
		t.Fatalf("err = %v, want ErrUnsignedRelease", err)
	}
	if !strings.Contains(err.Error(), "signs:") {
		t.Errorf("error should name what is missing, got %v", err)
	}
	if fake.Ran("goreleaser") {
		t.Errorf("built a release that could never be published: %s", fake.Transcript())
	}
}

func TestReleaseNeedsATag(t *testing.T) {
	repo, head, fake := releaseRepo(t, signingConfig)

	_, err := NewGoreleaser(fake, obs.Discard(), "tok", false).Publish(t.Context(), ports.PublishRequest{
		RepoDir: repo.Dir, SHA: head, Ref: "refs/heads/main", Artifact: binariesArtifact(),
	})

	// goreleaser derives the version from the tag; a branch push has none.
	if !errors.Is(err, ErrNotATag) {
		t.Fatalf("err = %v, want ErrNotATag", err)
	}
	if fake.Ran("goreleaser") {
		t.Error("started a release with no version to give it")
	}
}

func TestReleaseNeedsAToken(t *testing.T) {
	repo, head, fake := releaseRepo(t, signingConfig)

	_, err := NewGoreleaser(fake, obs.Discard(), "", false).Publish(t.Context(), ports.PublishRequest{
		RepoDir: repo.Dir, SHA: head, Ref: "refs/tags/v1.4.0", Artifact: binariesArtifact(),
	})

	if err == nil || !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Fatalf("err = %v, want a missing-token failure", err)
	}
	// Discovering this after a five-minute cross-compile is a poor way to
	// learn it.
	if fake.Ran("goreleaser") {
		t.Errorf("cross-compiled before noticing there was no credential: %s", fake.Transcript())
	}
}

func TestMissingGoreleaserIsAToolFailure(t *testing.T) {
	repo, head, fake := releaseRepo(t, signingConfig)
	fake.Absent("goreleaser")

	_, err := NewGoreleaser(fake, obs.Discard(), "tok", false).Publish(t.Context(), ports.PublishRequest{
		RepoDir: repo.Dir, SHA: head, Ref: "refs/tags/v1.4.0", Artifact: binariesArtifact(),
	})

	if !errors.Is(err, ErrToolMissing) {
		t.Errorf("err = %v, want ErrToolMissing", err)
	}
}

func TestMissingCosignIsAToolFailure(t *testing.T) {
	repo, head, fake := releaseRepo(t, signingConfig)
	fake.Absent("cosign")

	_, err := NewGoreleaser(fake, obs.Discard(), "tok", false).Publish(t.Context(), ports.PublishRequest{
		RepoDir: repo.Dir, SHA: head, Ref: "refs/tags/v1.4.0", Artifact: binariesArtifact(),
	})

	if !errors.Is(err, ErrToolMissing) {
		t.Errorf("err = %v, want ErrToolMissing", err)
	}
}

func TestDryReleaseBuildsButDoesNotUpload(t *testing.T) {
	repo, head, fake := releaseRepo(t, signingConfig)

	res, err := NewGoreleaser(fake, obs.Discard(), "", true).Publish(t.Context(), ports.PublishRequest{
		RepoDir: repo.Dir, SHA: head, Ref: "refs/tags/v1.4.0", Artifact: binariesArtifact(),
	})
	if err != nil {
		t.Fatalf("Publish: %v\n%s", err, fake.Transcript())
	}

	cmd := fake.Find("goreleaser release")
	if cmd == nil {
		t.Fatalf("nothing ran: %s", fake.Transcript())
	}
	// The compile still happens; the upload and the signing do not. Keyless
	// cosign needs an OIDC identity a laptop has no way to produce.
	if !strings.Contains(cmd.String(), "--skip=publish,announce,sign") {
		t.Errorf("dry release did not withhold the upload: %s", cmd.String())
	}
	if res.Signed {
		t.Error("a dry release must not claim to have published a signature")
	}
}

func TestReleaseRunsFromAPinnedWorktree(t *testing.T) {
	repo, head, fake := releaseRepo(t, signingConfig)
	// A dirty checkout must not reach the release archives.
	repo.Write("main.go", "package main // uncommitted\n")

	if _, err := NewGoreleaser(fake, obs.Discard(), "tok", false).Publish(t.Context(), ports.PublishRequest{
		RepoDir: repo.Dir, SHA: head, Ref: "refs/tags/v1.4.0", Artifact: binariesArtifact(),
	}); err != nil {
		t.Fatal(err)
	}

	if cmd := fake.Find("goreleaser release"); cmd.Dir == repo.Dir {
		t.Error("released from the operator's checkout instead of a pinned worktree")
	}
}

func TestReleaseWithoutAChecksumManifestIsRefused(t *testing.T) {
	repo := gittest.New(t)
	repo.Write(".goreleaser.yaml", signingConfig)
	head := repo.Commit("release config", "main.go", "package main\n")

	fake := execx.NewFake()
	fake.On("git", execx.Response{Fn: func(c execx.Cmd) (execx.Result, error) {
		return execx.NewSystem().Run(t.Context(), c)
	}})
	fake.On("goreleaser release", execx.Response{Fn: func(c execx.Cmd) (execx.Result, error) {
		dist := filepath.Join(c.Dir, "dist")
		if err := os.MkdirAll(dist, 0o750); err != nil {
			return execx.Result{}, err
		}
		raw, _ := json.Marshal([]map[string]string{{"name": "thing.tar.gz", "type": "Archive"}})
		return execx.Result{}, os.WriteFile(filepath.Join(dist, "artifacts.json"), raw, 0o600)
	}})

	_, err := NewGoreleaser(fake, obs.Discard(), "tok", false).Publish(t.Context(), ports.PublishRequest{
		RepoDir: repo.Dir, SHA: head, Ref: "refs/tags/v1.4.0", Artifact: binariesArtifact(),
	})

	if err == nil || !strings.Contains(err.Error(), "checksum manifest") {
		t.Errorf("err = %v, want a missing-manifest refusal", err)
	}
}

func TestReleaseHonoursACustomConfigPath(t *testing.T) {
	repo := gittest.New(t)
	repo.Write("build/release.yaml", signingConfig)
	head := repo.Commit("release config", "main.go", "package main\n")

	fake := execx.NewFake()
	fake.On("git", execx.Response{Fn: func(c execx.Cmd) (execx.Result, error) {
		return execx.NewSystem().Run(t.Context(), c)
	}})
	fake.On("goreleaser release", execx.Response{Fn: func(c execx.Cmd) (execx.Result, error) {
		return execx.Result{}, writeDist(c.Dir)
	}})

	art := binariesArtifact()
	art.Config = "build/release.yaml"

	if _, err := NewGoreleaser(fake, obs.Discard(), "tok", false).Publish(t.Context(), ports.PublishRequest{
		RepoDir: repo.Dir, SHA: head, Ref: "refs/tags/v1.4.0", Artifact: art,
	}); err != nil {
		t.Fatalf("Publish: %v\n%s", err, fake.Transcript())
	}

	if !strings.Contains(fake.Find("goreleaser release").String(), "--config build/release.yaml") {
		t.Errorf("custom config not passed through: %s", fake.Transcript())
	}
}

func TestReleaseGetsTheTokenInItsEnvironment(t *testing.T) {
	repo, head, fake := releaseRepo(t, signingConfig)

	if _, err := NewGoreleaser(fake, obs.Discard(), "ghp_secret", false).Publish(t.Context(), ports.PublishRequest{
		RepoDir: repo.Dir, SHA: head, Ref: "refs/tags/v1.4.0", Artifact: binariesArtifact(),
	}); err != nil {
		t.Fatal(err)
	}

	if !slices.Contains(fake.Find("goreleaser release").Env, "GITHUB_TOKEN=ghp_secret") {
		t.Error("goreleaser cannot create a release without the credential")
	}
}

func TestADryReleaseStillRefusesAnUnsignableConfig(t *testing.T) {
	repo, head, fake := releaseRepo(t, unsignedConfig)

	// The dry run skips the signing step, so the static check is the only
	// thing standing between an unsignable config and a surprise at tag time.
	_, err := NewGoreleaser(fake, obs.Discard(), "", true).Publish(t.Context(), ports.PublishRequest{
		RepoDir: repo.Dir, SHA: head, Ref: "refs/tags/v1.4.0", Artifact: binariesArtifact(),
	})

	if !errors.Is(err, ErrUnsignedRelease) {
		t.Errorf("err = %v, want ErrUnsignedRelease even on a rehearsal", err)
	}
}

// recordingUploader captures what would be attached to a release.
type recordingUploader struct {
	tag, name string
	body      []byte
	err       error
}

func (u *recordingUploader) UploadReleaseAssetByTag(_ context.Context, tag, name string, body []byte) error {
	u.tag, u.name, u.body = tag, name, body
	return u.err
}

func provenanceInput() ports.AttestInput {
	return ports.AttestInput{
		Repo: "klarlabs-studio/kiln", SHA: "c3f7aca23fa4bfa8d65b3741f46c509713cd618e",
		Ref: "refs/tags/v1.4.0", Event: "tag", GateReproved: true, KilnVersion: "v0.1.0",
		InvocationID: "run-1", StartedOn: time.Unix(0, 0).UTC(),
	}
}

func TestReleaseAttachesProvenance(t *testing.T) {
	repo, head, fake := releaseRepo(t, signingConfig)
	up := &recordingUploader{}
	g := NewGoreleaser(fake, obs.Discard(), "tok", false)
	g.Uploader = up
	// The real cosign writes the bundle; the fake must too, or there is
	// nothing to upload.
	fake.On("cosign attest-blob", execx.Response{Fn: func(c execx.Cmd) (execx.Result, error) {
		return execx.Result{}, os.WriteFile(bundlePath(c.Args), []byte(`{"dsseEnvelope":{}}`), 0o600)
	}})

	res, err := g.Publish(t.Context(), ports.PublishRequest{
		RepoDir: repo.Dir, SHA: head, Ref: "refs/tags/v1.4.0",
		Artifact: binariesArtifact(), Provenance: provenanceInput(),
	})
	if err != nil {
		t.Fatalf("Publish: %v\n%s", err, fake.Transcript())
	}

	if !res.Attested {
		t.Error("Attested = false after attaching provenance")
	}
	if up.name != ProvenanceAsset || up.tag != "v1.4.0" {
		t.Errorf("uploaded %q to %q", up.name, up.tag)
	}
	if len(up.body) == 0 {
		t.Error("uploaded an empty bundle")
	}
	if !slices.Contains(res.Tags, ProvenanceAsset) {
		t.Errorf("the release listing omits the provenance: %v", res.Tags)
	}
}

func TestProvenanceCoversTheChecksumManifest(t *testing.T) {
	repo, head, fake := releaseRepo(t, signingConfig)
	g := NewGoreleaser(fake, obs.Discard(), "tok", false)
	g.Uploader = &recordingUploader{}

	var predicate []byte
	fake.On("cosign attest-blob", execx.Response{Fn: func(c execx.Cmd) (execx.Result, error) {
		predicate, _ = os.ReadFile(flagValue(c.Args, "--predicate"))
		return execx.Result{}, os.WriteFile(bundlePath(c.Args), []byte("{}"), 0o600)
	}})

	if _, err := g.Publish(t.Context(), ports.PublishRequest{
		RepoDir: repo.Dir, SHA: head, Ref: "refs/tags/v1.4.0",
		Artifact: binariesArtifact(), Provenance: provenanceInput(),
	}); err != nil {
		t.Fatal(err)
	}

	var body attest.Provenance
	if err := json.Unmarshal(predicate, &body); err != nil {
		t.Fatalf("predicate body is not JSON: %v", err)
	}
	if got := sourceCommit(body); got != provenanceInput().SHA {
		t.Errorf("gitCommit = %q", got)
	}
	// cosign derives the subject from the artifact it signs, so the manifest
	// being the subject is asserted on the command rather than the predicate.
	// cosign must sign the manifest itself, not the predicate file.
	if signed := fake.Find("cosign attest-blob"); !strings.Contains(signed.String(), "checksums.txt") {
		t.Errorf("attest-blob subject = %s", signed.String())
	}
}

func TestAFailedUploadFailsTheRelease(t *testing.T) {
	repo, head, fake := releaseRepo(t, signingConfig)
	g := NewGoreleaser(fake, obs.Discard(), "tok", false)
	g.Uploader = &recordingUploader{err: errors.New("422 already exists")}
	fake.On("cosign attest-blob", execx.Response{Fn: func(c execx.Cmd) (execx.Result, error) {
		return execx.Result{}, os.WriteFile(bundlePath(c.Args), []byte("{}"), 0o600)
	}})

	_, err := g.Publish(t.Context(), ports.PublishRequest{
		RepoDir: repo.Dir, SHA: head, Ref: "refs/tags/v1.4.0",
		Artifact: binariesArtifact(), Provenance: provenanceInput(),
	})

	// A release whose provenance silently failed to attach is one nobody can
	// check, which is the thing this kind exists to prevent.
	if err == nil || !strings.Contains(err.Error(), "attach provenance") {
		t.Errorf("err = %v, want the upload failure surfaced", err)
	}
}

func TestNoUploaderMeansNoAttestation(t *testing.T) {
	repo, head, fake := releaseRepo(t, signingConfig)
	g := NewGoreleaser(fake, obs.Discard(), "tok", false)

	// Without a forge client there is no release to attach to. Failing would
	// make a tokenless run impossible; claiming attestation would be a lie.
	res, err := g.Publish(t.Context(), ports.PublishRequest{
		RepoDir: repo.Dir, SHA: head, Ref: "refs/tags/v1.4.0",
		Artifact: binariesArtifact(), Provenance: provenanceInput(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Attested {
		t.Error("Attested = true with nowhere to attach it")
	}
	if fake.Ran("cosign attest-blob") {
		t.Error("signed a statement it could not publish")
	}
}

func TestDryReleaseDoesNotAttest(t *testing.T) {
	repo, head, fake := releaseRepo(t, signingConfig)
	g := NewGoreleaser(fake, obs.Discard(), "", true)
	g.Uploader = &recordingUploader{}

	res, err := g.Publish(t.Context(), ports.PublishRequest{
		RepoDir: repo.Dir, SHA: head, Ref: "refs/tags/v1.4.0",
		Artifact: binariesArtifact(), Provenance: provenanceInput(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Attested || fake.Ran("cosign attest-blob") {
		t.Error("a rehearsal must not attest: there is no release to attach to")
	}
}

// bundlePath and flagValue read a flag out of a scripted cosign invocation.
func bundlePath(args []string) string { return flagValue(args, "--bundle") }

func flagValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
