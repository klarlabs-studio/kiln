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
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.klarlabs.de/fortify/retry"

	"go.klarlabs.de/kiln/internal/execx"
	"go.klarlabs.de/kiln/internal/obs"
	"go.klarlabs.de/kiln/internal/worktree"
)

// Request is one publish invocation.
type Request struct {
	// RepoDir is the repository to check the commit out of.
	RepoDir string
	// SHA is the commit to build. Publish checks it out into its own
	// worktree — the same reason prove does, and it must not rely on prove
	// having left one behind.
	SHA string
	// Plan is the resolved tag plan.
	Plan Plan
	// Output, when set, receives docker's live output. It is an io.Writer
	// rather than an *os.File so a nil value stays a nil interface; a typed-nil
	// *os.File would satisfy io.Writer and then panic on first write.
	Output io.Writer
}

// Result is the hand-off to RollOps.
type Result struct {
	// Digest is the immutable `sha256:…` the registry assigned.
	Digest string
	// Reference is image@digest — what cosign signed and what RollOps pins.
	Reference string
	// Tags are the fully qualified tags now pointing at the digest.
	Tags []string
	// Signed reports whether cosign ran. False only on a dry run.
	Signed bool
}

// Publisher builds, pushes and signs.
type Publisher interface {
	Publish(ctx context.Context, req Request) (Result, error)
}

// ErrToolMissing reports that docker or cosign is not installed. This is a
// publish failure, never a silent skip: a pipeline that quietly stopped
// signing would hand RollOps an artifact it will refuse, and the operator
// would learn about it at deploy time instead of build time.
var ErrToolMissing = errors.New("required tool missing")

// Docker is the real publisher.
type Docker struct {
	Runner execx.Runner
	Log    obs.Logger
	// Docker and Cosign are the binary names, overridable for testing and for
	// podman-compatible shims.
	Docker string
	Cosign string
	// PushRetry and SignRetry bound the transient-failure retries. Zero uses
	// the defaults, which are tuned for a registry having a bad minute rather
	// than for a registry that is down.
	PushRetries int
}

// NewDocker builds a publisher.
func NewDocker(r execx.Runner, log obs.Logger) *Docker {
	if log == nil {
		log = obs.Discard()
	}
	return &Docker{Runner: r, Log: log, Docker: "docker", Cosign: "cosign", PushRetries: 3}
}

// Publish builds the image from a fresh checkout, pushes every tag, resolves
// the digest and signs it.
func (d *Docker) Publish(ctx context.Context, req Request) (Result, error) {
	if err := d.preflight(); err != nil {
		return Result{}, err
	}

	var result Result
	err := worktree.With(ctx, d.Runner, req.RepoDir, req.SHA, func(dir string) error {
		var inner error
		result, inner = d.build(ctx, req, dir)
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

func (d *Docker) build(ctx context.Context, req Request, dir string) (Result, error) {
	plan := req.Plan
	multiArch := len(plan.Platforms) > 1

	var digest string
	var err error
	if multiArch {
		digest, err = d.buildxPush(ctx, req, dir)
	} else {
		digest, err = d.classicBuildPush(ctx, req, dir)
	}
	if err != nil {
		return Result{}, err
	}

	reference := plan.Image + "@" + digest
	if err := d.sign(ctx, reference, req); err != nil {
		return Result{}, err
	}

	return Result{
		Digest:    digest,
		Reference: reference,
		Tags:      plan.Refs(),
		Signed:    true,
	}, nil
}

// classicBuildPush is the single-platform path: plain `docker build`, one push
// per tag, then read the digest the registry assigned.
func (d *Docker) classicBuildPush(ctx context.Context, req Request, dir string) (string, error) {
	plan := req.Plan

	args := []string{"build", "--platform", plan.Platforms[0], "-f", plan.Dockerfile}
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
func (d *Docker) buildxPush(ctx context.Context, req Request, dir string) (string, error) {
	plan := req.Plan

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
func (d *Docker) push(ctx context.Context, ref, dir string, req Request) error {
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
func (d *Docker) sign(ctx context.Context, reference string, req Request) error {
	err := d.withRetry(ctx, "cosign sign", func(ctx context.Context) error {
		_, err := d.Runner.Run(ctx, execx.Cmd{
			Name: d.Cosign,
			// --yes suppresses the keyless confirmation prompt; an unattended
			// watch tick has no terminal to answer it on.
			Args:   []string{"sign", "--yes", reference},
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
type Dry struct{ Log obs.Logger }

// NewDry builds a dry publisher.
func NewDry(log obs.Logger) *Dry {
	if log == nil {
		log = obs.Discard()
	}
	return &Dry{Log: log}
}

// DryDigest is the placeholder a dry run reports instead of a real digest.
const DryDigest = "sha256:" + "0000000000000000000000000000000000000000000000000000000000000000"

func (d *Dry) Publish(_ context.Context, req Request) (Result, error) {
	d.Log.Info("dry run: would publish",
		"image", req.Plan.Image, "tags", req.Plan.Refs(), "sha", req.SHA)
	return Result{
		Digest:    DryDigest,
		Reference: req.Plan.Image + "@" + DryDigest,
		Tags:      req.Plan.Refs(),
		Signed:    false,
	}, nil
}

// Func adapts a function to Publisher, for tests.
type Func func(ctx context.Context, req Request) (Result, error)

func (f Func) Publish(ctx context.Context, req Request) (Result, error) { return f(ctx, req) }
