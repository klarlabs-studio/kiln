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

	// ArtifactKind is image or binaries; Config names the file that drove it.
	ArtifactKind string
	Config       string
	// BuildArgs are the docker build arguments the image was built with, if
	// any. Recorded so two images from one commit and one Dockerfile are
	// distinguishable in their attestations.
	BuildArgs map[string]string

	// Isolated reports a credential-free build (a fork pull request).
	Isolated bool
	// GateTool, GateReproved and GateReason describe the source gate.
	GateTool     string
	GateReproved bool
	GateReason   string

	// KilnVersion and ToolVersions pin the builder.
	KilnVersion  string
	ToolVersions map[string]string

	InvocationID string
	StartedOn    time.Time
	FinishedOn   time.Time
}
