// Package publish turns a proven commit into a signed image.
//
// The output is a contract, not a convenience: an immutable `sha-<short>` tag
// for RollOps to pin, at least one moving tag for its imagePolicy to follow, a
// digest, and a cosign signature over that digest. Anything less is a failure,
// because RollOps enforces the signature at apply time and an unsigned image
// is one it will refuse to ship.
//
// Kiln produces; it never enforces at apply time and it never deploys.
package publish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.klarlabs.de/fortify/retry"

	"go.klarlabs.de/kiln/internal/application/ports"
	"go.klarlabs.de/kiln/internal/domain/config"
	"go.klarlabs.de/kiln/internal/infrastructure/attest"
	"go.klarlabs.de/kiln/internal/infrastructure/execx"
	"go.klarlabs.de/kiln/internal/infrastructure/obs"
	"go.klarlabs.de/kiln/internal/infrastructure/worktree"
)

// ErrToolMissing reports that docker or cosign is not installed. This is a
// publish failure, never a silent skip: a pipeline that quietly stopped
// signing would hand RollOps an artifact it will refuse, and the operator
// would learn about it at deploy time instead of build time.
var ErrToolMissing = errors.New("required tool missing")

// Docker is the real publisher.
type Docker struct {
	Runner execx.Runner
	Log    ports.Logger
	// Docker and Cosign are the binary names, overridable for testing and for
	// podman-compatible shims.
	Docker string
	Cosign string
	// PushRetry and SignRetry bound the transient-failure retries. Zero uses
	// the defaults, which are tuned for a registry having a bad minute rather
	// than for a registry that is down.
	PushRetries int
	// SigningKey selects keyed signing (KILN_COSIGN_KEY). Empty stays keyless.
	// See envconfig.Env.CosignKey for what the value may be and why a
	// self-hosted builder needs one.
	SigningKey string
	// Nox is the scanner used to build an SBOM when an artifact asks for one
	// (KILN_NOX). Only read when artifact.sbom is set.
	Nox string
}

// NewDocker builds a publisher.
func NewDocker(r execx.Runner, log ports.Logger) *Docker {
	if log == nil {
		log = obs.Discard()
	}
	return &Docker{Runner: r, Log: log, Docker: "docker", Cosign: "cosign", PushRetries: 3}
}

// signingArgs prefixes a cosign signing invocation with the configured key.
//
// Keyed and keyless are the same command with one flag between them, so the
// two paths share every retry, every error and every test — a keyed signature
// cannot silently take a different route through this package than the keyless
// one it replaced.
func (d *Docker) signingArgs(args ...string) []string {
	if d.SigningKey == "" {
		return args
	}
	// After the subcommand, before the reference: cosign accepts flags in any
	// position, but keeping them adjacent to the subcommand keeps the logged
	// command readable.
	return append([]string{args[0], "--key", d.SigningKey}, args[1:]...)
}

// Publish builds the image from a fresh checkout, pushes every tag, resolves
// the digest and signs it.
func (d *Docker) Publish(ctx context.Context, req ports.PublishRequest) (ports.PublishResult, error) {
	if err := d.preflight(); err != nil {
		return ports.PublishResult{}, err
	}

	plan, err := BuildPlan(req.Artifact, req.SHA, req.Ref)
	if err != nil {
		return ports.PublishResult{}, err
	}

	var result ports.PublishResult
	err = worktree.With(ctx, d.Runner, req.RepoDir, req.SHA, func(dir string) error {
		var inner error
		result, inner = d.build(ctx, req, plan, dir)
		return inner
	})
	return result, err
}

// preflight fails before any work happens when the toolchain is incomplete.
// Discovering a missing cosign *after* pushing would leave an unsigned image
// in the registry that RollOps cannot deploy and nobody remembers to clean up.
func (d *Docker) preflight() error {
	if _, err := d.Runner.LookPath(d.Docker); err != nil {
		return fmt.Errorf("%w: %s is required to build the artifact "+
			"(install it, or set KILN_DRY=1 to print the tag plan instead): %w", ErrToolMissing, d.Docker, err)
	}
	if _, err := d.Runner.LookPath(d.Cosign); err != nil {
		return fmt.Errorf("%w: %s is required because RollOps refuses to deploy an unsigned digest "+
			"(install cosign, or set KILN_DRY=1): %w", ErrToolMissing, d.Cosign, err)
	}
	return nil
}

func (d *Docker) build(ctx context.Context, req ports.PublishRequest, plan Plan, dir string) (ports.PublishResult, error) {
	multiArch := len(plan.Platforms) > 1

	var digest string
	var err error
	if multiArch {
		digest, err = d.buildxPush(ctx, req, plan, dir)
	} else {
		digest, err = d.classicBuildPush(ctx, req, plan, dir)
	}
	if err != nil {
		return ports.PublishResult{}, err
	}

	reference := plan.Image + "@" + digest
	if err := d.sign(ctx, reference, req); err != nil {
		return ports.PublishResult{}, err
	}
	if err := d.attest(ctx, plan.Image, digest, reference, req); err != nil {
		return ports.PublishResult{}, err
	}
	if err := d.attachSBOM(ctx, reference, req); err != nil {
		return ports.PublishResult{}, err
	}
	if err := d.attachSourceSummary(ctx, reference, req); err != nil {
		return ports.PublishResult{}, err
	}

	return ports.PublishResult{
		Kind:      config.KindImage,
		Digest:    digest,
		Reference: reference,
		Tags:      plan.Refs(),
		Signed:    true,
		Attested:  true,
	}, nil
}

// attest attaches SLSA build provenance to the digest.
//
// It runs after signing and before the result is reported, and a failure
// fails the publish. That is deliberate: an image in the registry carrying a
// signature but no provenance is one whose origin cannot be checked, and
// "signed by someone" is a much weaker claim than the one kiln exists to
// make. Better to fail a build than to quietly downgrade what shipped.
func (d *Docker) attest(ctx context.Context, image, digest, reference string, req ports.PublishRequest) error {
	in := req.Provenance
	in.SubjectName = image
	in.SubjectDigest = digest
	in.ArtifactKind = string(config.KindImage)
	if in.Config == "" {
		in.Config = req.Artifact.Dockerfile
	}
	// Two images can share a commit and a Dockerfile and differ only by their
	// build arguments; without recording them the attestations are identical
	// while the images are not.
	in.BuildArgs = req.Artifact.Args

	stmt, err := attest.Build(in)
	if err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	path, cleanup, err := writePredicate(stmt)
	if err != nil {
		return err
	}
	defer cleanup()

	err = d.withRetry(ctx, "cosign attest", func(ctx context.Context) error {
		_, e := d.Runner.Run(ctx, execx.Cmd{
			Name: d.Cosign,
			Args: d.signingArgs(
				"attest", "--yes",
				"--type", attest.CosignType,
				"--predicate", path,
				reference,
			),
			Dir:    req.RepoDir,
			Stdout: req.Output, Stderr: req.Output,
		})
		return e
	})
	if err != nil {
		return fmt.Errorf("publish: cosign attest %s: %w", reference, err)
	}
	d.Log.Info("provenance attached", "reference", reference, "commit", in.SHA)
	return nil
}

// attachSBOM scans the checked-out source and attaches a CycloneDX inventory
// to the same digest the provenance hangs off.
//
// Provenance answers "where did this come from". It cannot answer "what is
// inside it", which is the question an incident opens with, and which is
// otherwise reconstructed by hand from a commit. Both attestations live on the
// digest, so a consumer joins them with cosign and nothing else — no clone, no
// build system, no kiln.
//
// The scan runs against req.RepoDir, the worktree this publish already built
// from, so the inventory describes the commit that produced the image rather
// than whatever is checked out on the box.
//
// A failure here fails the publish. The alternative — attaching provenance and
// quietly skipping the inventory — produces an artifact that looks complete
// and is missing the half somebody will later assume is there, which is the
// failure mode this whole chain exists to avoid.
func (d *Docker) attachSBOM(ctx context.Context, reference string, req ports.PublishRequest) error {
	if !req.Artifact.SBOM {
		return nil
	}

	dir, err := os.MkdirTemp("", "kiln-sbom-*")
	if err != nil {
		return fmt.Errorf("publish: sbom workspace: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	// --offline because a publish already knows what it built: an inventory
	// that varies with a vulnerability feed's availability is not a property
	// of the artifact, and this attestation is meant to be a fact about it.
	if _, err := d.Runner.Run(ctx, execx.Cmd{
		Name:   d.nox(),
		Args:   []string{"scan", ".", "--format", "cdx", "--output", dir, "--offline"},
		Dir:    req.RepoDir,
		Stdout: req.Output, Stderr: req.Output,
	}); err != nil {
		return fmt.Errorf("publish: nox scan for sbom: %w", err)
	}

	path := filepath.Join(dir, "sbom.cdx.json")
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("publish: nox produced no sbom at %s: %w", path, err)
	}

	if err := d.withRetry(ctx, "cosign attest sbom", func(ctx context.Context) error {
		_, e := d.Runner.Run(ctx, execx.Cmd{
			Name: d.Cosign,
			Args: d.signingArgs(
				"attest", "--yes",
				"--type", "cyclonedx",
				"--predicate", path,
				reference,
			),
			Dir:    req.RepoDir,
			Stdout: req.Output, Stderr: req.Output,
		})
		return e
	}); err != nil {
		return fmt.Errorf("publish: cosign attest sbom %s: %w", reference, err)
	}
	d.Log.Info("sbom attached", "reference", reference)
	return nil
}

// nox is the scanner binary, defaulting to the name on PATH.
func (d *Docker) nox() string {
	if d.Nox == "" {
		return "nox"
	}
	return d.Nox
}

// attachSourceSummary republishes Warden's verification summary against the
// artifact.
//
// A second attestation rather than a field inside the first, because they are
// different claims by different authorities: kiln's provenance says where the
// artifact came from, warden's summary says the source was gated. Keeping them
// separate lets a consumer require each on its own terms, and lets it check
// that the commit warden verified is the commit kiln built — which is the join
// that stops a gated commit's summary being attached to an artifact built from
// an ungated one.
//
// Absent is not fatal. A repository still adopting warden publishes artifacts
// with build provenance and no source summary; refusing would make adoption
// all-or-nothing.
func (d *Docker) attachSourceSummary(ctx context.Context, reference string, req ports.PublishRequest) error {
	if len(req.SourceVSA) == 0 {
		return nil
	}

	envelope, vsa, err := attest.ParseEnvelope(req.SourceVSA)
	if err != nil {
		// Something was produced but is not a signed summary. Attaching it
		// would put a claim in the registry nobody can check, and signing it
		// here would make it kiln's claim rather than warden's.
		d.Log.Warn("skipping the source summary", "err", err)
		return nil
	}
	if !vsa.Passed() {
		// Kiln does not publish a failing verdict as though it were a pass.
		// Reaching here at all means prove succeeded, so this is a
		// contradiction worth failing on rather than papering over.
		return fmt.Errorf("publish: warden reports %q for this commit", vsa.Predicate.VerificationResult)
	}
	if commit := vsa.SourceCommit(); commit != "" && req.SHA != "" && commit != req.SHA {
		// The summary is about a different commit than the one being built.
		// Attaching it would manufacture exactly the mismatch a consumer's
		// join is there to catch.
		return fmt.Errorf("publish: the source summary is for %s but this build is of %s",
			shortSHA(commit), shortSHA(req.SHA))
	}

	path, cleanup, err := writeBytes(req.SourceVSA)
	if err != nil {
		return err
	}
	defer cleanup()

	// `attach`, not `attest`. attest would sign the payload with kiln's key and
	// discard warden's signature, turning warden's claim into kiln's account of
	// it. attach uploads the envelope byte for byte, so what a consumer
	// verifies is the signature warden made.
	if err := d.withRetry(ctx, "cosign attach attestation", func(ctx context.Context) error {
		_, e := d.Runner.Run(ctx, execx.Cmd{
			Name:   d.Cosign,
			Args:   []string{"attach", "attestation", "--attestation", path, reference},
			Dir:    req.RepoDir,
			Stdout: req.Output, Stderr: req.Output,
		})
		return e
	}); err != nil {
		return fmt.Errorf("publish: cosign attach source summary: %w", err)
	}

	d.Log.Info("source summary attached",
		"reference", reference, "verifier", vsa.Predicate.Verifier.ID,
		"signed_by", envelope.KeyID(), "levels", vsa.Predicate.VerifiedLevels)
	return nil
}

// writePredicate spills the statement to a file for cosign to read. cosign
// takes a path, not stdin, so there is no way to avoid the temp file.
func writePredicate(stmt attest.Statement) (path string, cleanup func(), err error) {
	// The predicate body, not the statement: cosign builds the statement
	// around it and derives the subject from the artifact itself.
	data, err := stmt.PredicateJSON()
	if err != nil {
		return "", func() {}, err
	}
	return writeBytes(data)
}

// writeBytes spills a predicate body to a file for cosign to read. cosign
// takes a path, not stdin, so there is no way to avoid the temp file.
func writeBytes(data []byte) (path string, cleanup func(), err error) {
	f, err := os.CreateTemp("", "kiln-predicate-*.json")
	if err != nil {
		return "", func() {}, fmt.Errorf("publish: create predicate file: %w", err)
	}
	name := f.Name()
	cleanup = func() { _ = os.Remove(name) }

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("publish: write predicate: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("publish: close predicate: %w", err)
	}
	return name, cleanup, nil
}

// classicBuildPush is the single-platform path: plain `docker build`, one push
// per tag, then read the digest the registry assigned.
func (d *Docker) classicBuildPush(ctx context.Context, req ports.PublishRequest, plan Plan, dir string) (string, error) {
	args := []string{"build", "--platform", plan.Platforms[0], "-f", plan.Dockerfile}
	args = append(args, plan.BuildArgFlags()...)
	for _, ref := range plan.Refs() {
		args = append(args, "-t", ref)
	}
	// A build arg rather than a label so a Dockerfile can stamp the commit into
	// the binary it produces; the OCI label below records it either way.
	args = append(args,
		"--label", "org.opencontainers.image.revision="+req.SHA,
		"--label", "org.opencontainers.image.source="+plan.Image,
		plan.Context)

	if _, err := d.Runner.Run(ctx, execx.Cmd{
		Name: d.Docker, Args: args, Dir: dir,
		Stdout: req.Output, Stderr: req.Output,
	}); err != nil {
		return "", fmt.Errorf("publish: docker build: %w", err)
	}

	for _, ref := range plan.Refs() {
		if err := d.push(ctx, ref, dir, req); err != nil {
			return "", err
		}
	}
	return d.resolveDigest(ctx, plan.Image, plan.SHATag, dir)
}

// buildxPush is the multi-platform path. A multi-arch image cannot exist in
// the local daemon's image store, so buildx builds and pushes in one step and
// reports the manifest-list digest through a metadata file.
func (d *Docker) buildxPush(ctx context.Context, req ports.PublishRequest, plan Plan, dir string) (string, error) {
	metaFile, err := os.CreateTemp("", "kiln-buildx-*.json")
	if err != nil {
		return "", fmt.Errorf("publish: create metadata file: %w", err)
	}
	metaPath := metaFile.Name()
	_ = metaFile.Close()
	defer func() { _ = os.Remove(metaPath) }()

	args := []string{
		"buildx", "build",
		"--platform", strings.Join(plan.Platforms, ","),
		"-f", plan.Dockerfile,
		"--label", "org.opencontainers.image.revision=" + req.SHA,
		"--metadata-file", metaPath,
		"--push",
	}
	args = append(args, plan.BuildArgFlags()...)
	for _, ref := range plan.Refs() {
		args = append(args, "-t", ref)
	}
	args = append(args, plan.Context)

	if err := d.withRetry(ctx, "docker buildx build", func(ctx context.Context) error {
		_, err := d.Runner.Run(ctx, execx.Cmd{
			Name: d.Docker, Args: args, Dir: dir,
			Stdout: req.Output, Stderr: req.Output,
		})
		return err
	}); err != nil {
		return "", fmt.Errorf("publish: docker buildx build (multi-platform builds need a docker-container builder: "+
			"`docker buildx create --use`): %w", err)
	}

	return readBuildxDigest(metaPath)
}

func readBuildxDigest(path string) (string, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("publish: read buildx metadata: %w", err)
	}
	var meta struct {
		Digest string `json:"containerimage.digest"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return "", fmt.Errorf("publish: parse buildx metadata: %w", err)
	}
	if meta.Digest == "" {
		return "", errors.New("publish: buildx reported no image digest")
	}
	return meta.Digest, nil
}

// push sends one tag, retrying transient registry failures. A registry 500 or
// a dropped connection mid-layer is common enough that failing the whole run
// on the first one would make unattended watch unusable.
func (d *Docker) push(ctx context.Context, ref, dir string, req ports.PublishRequest) error {
	err := d.withRetry(ctx, "docker push "+ref, func(ctx context.Context) error {
		_, err := d.Runner.Run(ctx, execx.Cmd{
			Name: d.Docker, Args: []string{"push", ref}, Dir: dir,
			Stdout: req.Output, Stderr: req.Output,
		})
		return err
	})
	if err != nil {
		return fmt.Errorf("publish: push %s: %w", ref, err)
	}
	return nil
}

// resolveDigest reads back what the registry actually stored.
//
// The digest is taken from the registry's answer rather than computed locally,
// because the digest RollOps will pin is the one the registry knows. An image
// can carry several RepoDigests when it has been pushed to more than one
// repository, so the entry is matched against the image being published.
func (d *Docker) resolveDigest(ctx context.Context, image, ref, dir string) (string, error) {
	res, err := d.Runner.Run(ctx, execx.Cmd{
		Name: d.Docker,
		Args: []string{"image", "inspect", "--format", "{{json .RepoDigests}}", ref},
		Dir:  dir,
	})
	if err != nil {
		return "", fmt.Errorf("publish: inspect %s: %w", ref, err)
	}

	var repoDigests []string
	if err := json.Unmarshal([]byte(res.Output()), &repoDigests); err != nil {
		return "", fmt.Errorf("publish: parse RepoDigests for %s: %w", ref, err)
	}
	for _, rd := range repoDigests {
		repo, digest, ok := strings.Cut(rd, "@")
		if ok && repo == image {
			return digest, nil
		}
	}
	if len(repoDigests) == 0 {
		return "", fmt.Errorf("publish: %s has no registry digest — the push did not take effect", ref)
	}
	return "", fmt.Errorf("publish: none of %v belongs to %s", repoDigests, image)
}

// sign signs the digest, not a tag. A tag is mutable; signing one would attest
// to whatever that tag points at when someone checks, which is not a claim
// worth making.
func (d *Docker) sign(ctx context.Context, reference string, req ports.PublishRequest) error {
	err := d.withRetry(ctx, "cosign sign", func(ctx context.Context) error {
		_, err := d.Runner.Run(ctx, execx.Cmd{
			Name: d.Cosign,
			// --yes suppresses the keyless confirmation prompt; an unattended
			// watch tick has no terminal to answer it on.
			Args:   d.signingArgs("sign", "--yes", reference),
			Dir:    req.RepoDir,
			Stdout: req.Output, Stderr: req.Output,
		})
		return err
	})
	if err != nil {
		return fmt.Errorf("publish: cosign sign %s: %w", reference, err)
	}
	return nil
}

// withRetry wraps a transient-failure-prone step. Exponential backoff with
// jitter, bounded attempts; a genuinely broken registry still fails the run
// rather than looping.
func (d *Docker) withRetry(ctx context.Context, label string, fn func(context.Context) error) error {
	attempts := d.PushRetries
	if attempts <= 0 {
		attempts = 3
	}

	r := retry.New[struct{}](retry.Config{
		MaxAttempts:   attempts,
		InitialDelay:  2 * time.Second,
		MaxDelay:      20 * time.Second,
		Multiplier:    2,
		BackoffPolicy: retry.BackoffExponential,
		Jitter:        true,
		IsRetryable:   retryableRegistryError,
		OnRetry: func(attempt int, err error) {
			d.Log.Warn("retrying", "step", label, "attempt", attempt, "err", err)
		},
	})
	_, err := r.Execute(ctx, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, fn(ctx)
	})
	return err
}

// retryableRegistryError decides what is worth trying again.
//
// A missing binary and an authentication failure will fail identically every
// time, and retrying them just makes the operator wait longer for the same
// message. Everything else — a 5xx, a reset connection, a timeout — gets
// another go.
func retryableRegistryError(err error) bool {
	if err == nil {
		return false
	}
	var notFound *execx.NotFoundError
	if errors.As(err, &notFound) {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, permanent := range []string{
		"unauthorized", "authentication required", "denied", "forbidden",
		"no such file or directory", "manifest unknown", "invalid reference",
	} {
		if strings.Contains(msg, permanent) {
			return false
		}
	}
	return true
}

// Dry plans without touching a registry. It backs KILN_DRY=1 and `kiln
// doctor`, and it deliberately reports Signed=false and a placeholder digest
// so nothing downstream can mistake a rehearsal for a real artifact.
type Dry struct{ Log ports.Logger }

// NewDry builds a dry publisher.
func NewDry(log ports.Logger) *Dry {
	if log == nil {
		log = obs.Discard()
	}
	return &Dry{Log: log}
}

// DryDigest is the placeholder a dry run reports instead of a real digest.
const DryDigest = "sha256:" + "0000000000000000000000000000000000000000000000000000000000000000"

func (d *Dry) Publish(_ context.Context, req ports.PublishRequest) (ports.PublishResult, error) {
	if req.Artifact.Kind == config.KindBinaries {
		d.Log.Info("dry run: would release binaries",
			"from", req.Artifact.From, "config", req.Artifact.Config, "ref", req.Ref)
		return ports.PublishResult{
			Kind:      config.KindBinaries,
			Digest:    DryDigest,
			Reference: req.Ref,
			Signed:    false,
		}, nil
	}

	plan, err := BuildPlan(req.Artifact, req.SHA, req.Ref)
	if err != nil {
		return ports.PublishResult{}, err
	}
	d.Log.Info("dry run: would publish",
		"image", plan.Image, "tags", plan.Refs(), "sha", req.SHA)
	return ports.PublishResult{
		Kind:      config.KindImage,
		Digest:    DryDigest,
		Reference: plan.Image + "@" + DryDigest,
		Tags:      plan.Refs(),
		Signed:    false,
	}, nil
}
