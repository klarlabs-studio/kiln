package ports

import (
	"time"
)

// AttestInput is everything needed to describe one build. It is a plain struct
// rather than a reference to the engine's types so this package stays
// independent of them — a predicate format should not move when an internal
// field is renamed.
type AttestInput struct {
	// SubjectName and SubjectDigest identify the artifact. The digest is the
	// full "sha256:..." form.
	SubjectName   string
	SubjectDigest string

	Repo  string
	SHA   string
	Ref   string
	Event string

	// BuilderID overrides the build platform recorded in the provenance.
	//
	// Empty means kiln built it, and kiln says so. It is set when something
	// else did — a GitLab pipeline, a Jenkins job — because the builder id is
	// the field a verifier pins its trust on (RollOps calls it AllowedBuilders),
	// and a foreign CI claiming to be kiln would be claiming a gate it never
	// ran. Whoever signs decides what they claim; the verifier decides whose
	// claims it accepts.
	BuilderID string

	// ArtifactKind is image or binaries; Config names the file that drove it.
	ArtifactKind string
	Config       string
	// BuildArgs are the docker build arguments the image was built with, if
	// any. Recorded so two images from one commit and one Dockerfile are
	// distinguishable in their attestations.
	BuildArgs map[string]string

	// Isolated reports a credential-free build (a fork pull request).
	Isolated bool
	// GateTool, GateVerified, GateReproved and GateReason describe the source
	// gate.
	//
	// GateVerified used to be assumed: kiln only reaches a publish after the
	// gate is satisfied, so the predicate hardcoded verified: true. That holds
	// for kiln and breaks the moment anything else builds the predicate — a
	// pipeline that ran no gate would still emit a verdict saying one passed,
	// which is the single claim this field exists to make checkable.
	GateTool     string
	GateVerified bool
	GateReproved bool
	GateReason   string

	// KilnVersion and ToolVersions pin the builder.
	KilnVersion  string
	ToolVersions map[string]string

	InvocationID string
	StartedOn    time.Time
	FinishedOn   time.Time
}
