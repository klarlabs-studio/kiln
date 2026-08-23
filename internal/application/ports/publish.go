package ports

import (
	"context"
	"io"

	"go.klarlabs.de/kiln/internal/domain/config"
)

// PublishRequest is one publish invocation: one artifact from one commit.
type PublishRequest struct {
	// RepoDir is the repository to check the commit out of.
	RepoDir string
	// SHA is the commit to build. Publish checks it out into its own
	// worktree — the same reason prove does, and it must not rely on prove
	// having left one behind.
	SHA string
	// Ref is the ref the commit was found on. The image publisher reads it for
	// the semver tag; the binaries publisher requires it to be a tag at all.
	Ref string
	// Artifact is the pipeline entry being published. Each publisher plans
	// from it, so the plan cannot drift from the configuration that produced
	// it.
	Artifact config.Artifact
	// Provenance carries the run-level facts the attestation needs — which
	// commit, which gate verdict, which builder. The publisher fills in the
	// subject once the digest exists, because the digest is the one field
	// nobody knows until the artifact is built.
	Provenance AttestInput
	// SourceVSA is Warden's own verification summary for the commit, carried
	// verbatim. Empty when the commit has no note.
	//
	// Kiln republishes it rather than summarising it: kiln reporting "the gate
	// passed" asks a reader to trust kiln about warden, where the VSA names
	// the verifier, the policy and the levels reached in a shape a generic
	// SLSA consumer already reads.
	SourceVSA []byte
	// Output, when set, receives docker's live output. It is an io.Writer
	// rather than an *os.File so a nil value stays a nil interface; a typed-nil
	// *os.File would satisfy io.Writer and then panic on first write.
	Output io.Writer
}

// PublishResult is one published artifact — the hand-off to whoever consumes it:
// RollOps for an image, a human or an installer for a release.
type PublishResult struct {
	// Kind echoes the artifact kind, so a caller aggregating several results
	// can tell them apart without re-reading the pipeline.
	Kind config.ArtifactKind
	// Digest is the immutable `sha256:…` identity. For an image that is what
	// the registry assigned; for a release it is the digest of the checksum
	// manifest, which in turn covers every file in it.
	Digest string
	// Reference is what a consumer names to get exactly this artifact:
	// image@digest, or the release tag.
	Reference string
	// Tags are the fully qualified image tags now pointing at the digest, or
	// the file names in a release.
	Tags []string
	// Signed reports whether cosign ran. False only on a dry run.
	Signed bool
	// Attested reports whether SLSA provenance was attached to the artifact.
	// Separate from Signed because they answer different questions: a
	// signature says somebody vouched for these bytes, provenance says where
	// they came from.
	Attested bool
}

// Publisher builds, pushes and signs.
type Publisher interface {
	Publish(ctx context.Context, req PublishRequest) (PublishResult, error)
}

// PublishFunc adapts a function to Publisher, for tests.
type PublishFunc func(ctx context.Context, req PublishRequest) (PublishResult, error)

func (f PublishFunc) Publish(ctx context.Context, req PublishRequest) (PublishResult, error) {
	return f(ctx, req)
}
