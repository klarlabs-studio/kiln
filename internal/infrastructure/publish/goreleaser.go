package publish

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"go.klarlabs.de/kiln/internal/config"
	"go.klarlabs.de/kiln/internal/infrastructure/attest"
	"go.klarlabs.de/kiln/internal/infrastructure/execx"
	"go.klarlabs.de/kiln/internal/infrastructure/obs"
	"go.klarlabs.de/kiln/internal/infrastructure/worktree"
)

// Goreleaser publishes a binary release.
//
// It delegates the whole of cross-compilation, archiving, checksums, changelog
// and the GitHub Release to goreleaser, for the same reason prove delegates to
// warden: `.goreleaser.yaml` is already the release language of every repo
// here, and Kiln does not invent a second one any more than it invents a
// second check language.
//
// What Kiln adds is the part goreleaser leaves optional. Of the sibling repos,
// warden's config signs its checksum manifest with keyless cosign and the
// others do not — so whether a release is verifiable depends on whether
// somebody remembered. Kiln refuses to publish an unsigned release, which
// turns that from a habit into a property.
type Goreleaser struct {
	Runner execx.Runner
	Log    obs.Logger
	// Binary is the goreleaser executable (KILN_GORELEASER).
	Binary string
	// Cosign is checked for presence; the signing itself is configured in
	// `.goreleaser.yaml` and performed by goreleaser mid-pipeline, before it
	// uploads anything.
	Cosign string
	// Token is the forge credential goreleaser needs to create the release.
	Token string
	// Uploader attaches the provenance bundle to the release goreleaser made.
	// Nil disables the attachment, which the dry path relies on.
	Uploader AssetUploader
	// Dry stops short of uploading.
	Dry bool
	// SigningKey selects keyed signing for the provenance attestation
	// (KILN_COSIGN_KEY), matching the image path. goreleaser signs the
	// artifacts themselves from `.goreleaser.yaml`, so this covers only the
	// attestation kiln attaches.
	SigningKey string
}

// AssetUploader attaches a file to an existing release. It is an interface so
// this package does not depend on the forge client, and so the attachment can
// be exercised without a network.
type AssetUploader interface {
	UploadReleaseAssetByTag(ctx context.Context, tag, name string, body []byte) error
}

// signingArgs prefixes a cosign signing invocation with the configured key,
// exactly as the image publisher does.
func (g *Goreleaser) signingArgs(args ...string) []string {
	if g.SigningKey == "" {
		return args
	}
	return append([]string{args[0], "--key", g.SigningKey}, args[1:]...)
}

// ProvenanceAsset is the file name the release provenance is attached under.
// A verifier looks for it by name, so it is a contract.
const ProvenanceAsset = "provenance.intoto.jsonl"

// NewGoreleaser builds a release publisher.
func NewGoreleaser(r execx.Runner, log obs.Logger, token string, dry bool) *Goreleaser {
	if log == nil {
		log = obs.Discard()
	}
	return &Goreleaser{Runner: r, Log: log, Binary: "goreleaser", Cosign: "cosign", Token: token, Dry: dry}
}

// ErrUnsignedRelease reports a `.goreleaser.yaml` with no signing configured.
//
// This is a refusal, not a warning. An unsigned release is one nobody
// downstream can verify — the same defect that makes RollOps reject an
// unsigned image digest, in the artifact kind humans actually download.
var ErrUnsignedRelease = errors.New("release config does not sign its artifacts")

// ErrNotATag reports a binary release attempted from something other than a
// tag. goreleaser derives the version from the tag; without one there is
// nothing to call the release.
var ErrNotATag = errors.New("a binary release needs a tag")

func (g *Goreleaser) Publish(ctx context.Context, req Request) (Result, error) {
	if err := g.preflight(req); err != nil {
		return Result{}, err
	}

	var result Result
	err := worktree.With(ctx, g.Runner, req.RepoDir, req.SHA, func(dir string) error {
		var inner error
		result, inner = g.release(ctx, req, dir)
		return inner
	})
	return result, err
}

func (g *Goreleaser) preflight(req Request) error {
	if !strings.HasPrefix(req.Ref, "refs/tags/") {
		return fmt.Errorf("%w: ref %q is not one (route binaries to tag events, or push a tag)",
			ErrNotATag, req.Ref)
	}
	if _, err := g.Runner.LookPath(g.Binary); err != nil {
		return fmt.Errorf("%w: %s builds the release archives "+
			"(install goreleaser, or set KILN_GORELEASER): %w", ErrToolMissing, g.Binary, err)
	}
	if _, err := g.Runner.LookPath(g.Cosign); err != nil {
		return fmt.Errorf("%w: %s signs the release checksum manifest "+
			"(install cosign, or set KILN_DRY=1): %w", ErrToolMissing, g.Cosign, err)
	}
	// goreleaser cannot create a GitHub Release without a credential, and
	// discovering that after a five-minute cross-compile is a poor way to
	// learn it.
	if !g.Dry && g.Token == "" {
		return errors.New("publish: a binary release needs GITHUB_TOKEN to create the release " +
			"(export one, or set KILN_DRY=1 to build without uploading)")
	}
	return nil
}

func (g *Goreleaser) release(ctx context.Context, req Request, dir string) (Result, error) {
	cfgPath := req.Artifact.Config
	if cfgPath == "" {
		cfgPath = ".goreleaser.yaml"
	}
	if err := CheckReleaseSigning(filepath.Join(dir, cfgPath)); err != nil {
		return Result{}, err
	}

	args := []string{"release", "--clean", "--config", cfgPath}
	if g.Dry {
		// The cross-compile and the archives still happen — a rehearsal that
		// skipped the build would rehearse nothing — but the upload and the
		// signing do not.
		//
		// Signing is skipped because keyless cosign needs an ambient OIDC
		// identity, which a laptop running `KILN_DRY=1` does not have; leaving
		// it in would make every local rehearsal fail on an interactive
		// browser prompt. Nothing is lost: whether the config *can* sign was
		// already established by CheckReleaseSigning before the build started,
		// and that static check is the guarantee, not this run.
		args = append(args, "--skip=publish,announce,sign")
	}

	env := os.Environ()
	if g.Token != "" {
		env = append(env, "GITHUB_TOKEN="+g.Token)
	}

	if _, err := g.Runner.Run(ctx, execx.Cmd{
		Name: g.Binary, Args: args, Dir: dir, Env: env,
		Stdout: req.Output, Stderr: req.Output,
	}); err != nil {
		return Result{}, fmt.Errorf("publish: goreleaser release: %w", err)
	}

	return g.collect(ctx, dir, req)
}

// CheckReleaseSigning refuses a release config that would produce artifacts
// nobody can verify. `kiln doctor` calls it too, so an operator learns before
// a tag is pushed rather than after a five-minute build.
//
// Reading the file rather than trusting the operator is the point: this is the
// check that makes "kiln releases are signed" true of every repository, not
// only the ones whose config happened to include a signs: block.
func CheckReleaseSigning(path string) error {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return fmt.Errorf("publish: read %s: %w", filepath.Base(path), err)
	}

	var cfg struct {
		Signs []yaml.Node `yaml:"signs"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("publish: parse %s: %w", filepath.Base(path), err)
	}
	if len(cfg.Signs) == 0 {
		return fmt.Errorf(
			"%w: %s has no signs: block, so the checksum manifest would ship unverifiable. "+
				"Add a cosign sign-blob signer (see warden's .goreleaser.yaml for the keyless form)",
			ErrUnsignedRelease, filepath.Base(path))
	}
	return nil
}

// artifact is the subset of a goreleaser dist/artifacts.json entry Kiln reads.
type artifact struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// publishedTypes are the artifact types that end up in the release. goreleaser
// records intermediates too — the raw Binary before archiving, for one — and
// listing those in the Check would misrepresent what a user can download.
var publishedTypes = []string{"Archive", "Checksum", "Signature", "Certificate", "Linux Package", "SBOM"}

// collect reads what goreleaser produced and derives the release's identity.
func (g *Goreleaser) collect(ctx context.Context, dir string, req Request) (Result, error) {
	distDir := filepath.Join(dir, "dist")

	raw, err := os.ReadFile(filepath.Join(distDir, "artifacts.json")) //nolint:gosec // path is derived from kiln's own worktree
	if err != nil {
		return Result{}, fmt.Errorf("publish: goreleaser produced no artifacts.json: %w", err)
	}
	var entries []artifact
	if err := json.Unmarshal(raw, &entries); err != nil {
		return Result{}, fmt.Errorf("publish: parse artifacts.json: %w", err)
	}

	names := make([]string, 0, len(entries))
	var checksumFile string
	for _, e := range entries {
		if !slices.Contains(publishedTypes, e.Type) {
			continue
		}
		names = append(names, e.Name)
		if e.Type == "Checksum" {
			checksumFile = e.Name
		}
	}
	slices.Sort(names)

	if checksumFile == "" {
		return Result{}, errors.New(
			"publish: the release has no checksum manifest, so there is nothing for the signature to cover")
	}

	// The manifest's own digest is the release's content identity: it changes
	// if any file in it changes, which makes it the one value worth recording
	// in the ledger and quoting in the Check.
	digest, err := fileDigest(filepath.Join(distDir, checksumFile))
	if err != nil {
		return Result{}, err
	}

	tag := strings.TrimPrefix(req.Ref, "refs/tags/")

	attested, err := g.attest(ctx, distDir, checksumFile, digest, tag, req)
	if err != nil {
		return Result{}, err
	}
	if attested {
		names = append(names, ProvenanceAsset)
		slices.Sort(names)
	}

	g.Log.Info("released binaries", "tag", tag, "files", len(names), "checksums", digest)

	return Result{
		Kind:      config.KindBinaries,
		Digest:    digest,
		Reference: tag,
		Tags:      names,
		// Signing was verified as configured before the run and performed by
		// goreleaser before it uploaded anything.
		Signed:   !g.Dry,
		Attested: attested,
	}, nil
}

// attest signs SLSA provenance over the checksum manifest and attaches it to
// the release.
//
// The manifest is the right subject: it names every archive by digest, so one
// statement covers the whole release the same way one signature does. A
// per-archive attestation would be four statements saying the same thing.
//
// Returns false without error when the run is dry or no uploader is wired —
// there is no release to attach to in either case, and failing would make a
// rehearsal impossible.
func (g *Goreleaser) attest(
	ctx context.Context, distDir, checksumFile, digest, tag string, req Request,
) (bool, error) {
	if g.Dry || g.Uploader == nil {
		return false, nil
	}

	in := req.Provenance
	in.SubjectName = checksumFile
	in.SubjectDigest = digest
	in.ArtifactKind = string(config.KindBinaries)
	if in.Config == "" {
		in.Config = req.Artifact.Config
	}

	stmt, err := attest.Build(in)
	if err != nil {
		return false, fmt.Errorf("publish: %w", err)
	}
	predicate, cleanup, err := writePredicate(stmt)
	if err != nil {
		return false, err
	}
	defer cleanup()

	bundle := filepath.Join(distDir, ProvenanceAsset)
	if _, err := g.Runner.Run(ctx, execx.Cmd{
		Name: g.Cosign,
		Args: g.signingArgs(
			"attest-blob", "--yes",
			"--type", attest.CosignType,
			"--predicate", predicate,
			"--new-bundle-format",
			"--bundle", bundle,
			filepath.Join(distDir, checksumFile),
		),
		Dir:    distDir,
		Stdout: req.Output, Stderr: req.Output,
	}); err != nil {
		return false, fmt.Errorf("publish: cosign attest-blob %s: %w", checksumFile, err)
	}

	body, err := os.ReadFile(filepath.Clean(bundle)) //nolint:gosec // path is kiln's own dist dir
	if err != nil {
		return false, fmt.Errorf("publish: read provenance bundle: %w", err)
	}
	if err := g.Uploader.UploadReleaseAssetByTag(ctx, tag, ProvenanceAsset, body); err != nil {
		return false, fmt.Errorf("publish: attach provenance to release %s: %w", tag, err)
	}

	g.Log.Info("provenance attached", "release", tag, "asset", ProvenanceAsset, "commit", in.SHA)
	return true, nil
}

func fileDigest(path string) (string, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("publish: read %s: %w", filepath.Base(path), err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
