// Package mcpsrv exposes Kiln to agents over MCP.
//
// Agents get the same engine as everyone else, through a deliberately narrower
// door. Two rules shape this surface:
//
//   - Kiln never applies. There is no deploy tool here, and adding one would
//     not be a feature — it would be a category error. RollOps owns that.
//   - A push or tag run is the one that publishes and signs, so an agent may
//     not start one unless the operator has explicitly opted in with
//     KILN_MCP_ALLOW_RUN=1. Proving a pull request is always allowed: it
//     produces no artifact and holds no credential.
//
// All logging stays on stderr. A single stray line on stdout would corrupt the
// JSON-RPC stream and take the whole session down.
package mcpsrv

import (
	"context"
	"errors"
	"fmt"

	"go.klarlabs.de/mcp"

	"go.klarlabs.de/kiln/internal/domain/isolation"
	"go.klarlabs.de/kiln/internal/domain/run"
)

// Facade is the subset of Kiln the MCP surface needs. The CLI wires the real
// implementation in; tests wire a fake. Depending on an interface rather than
// on boot.Deps keeps the tool handlers testable without a git repository.
type Facade interface {
	// Doctor validates configuration and toolchain without running anything.
	Doctor(ctx context.Context) (DoctorOutput, error)
	// Status reads the ledger. An empty id means the latest run.
	Status(ctx context.Context, id string) (RunOutput, error)
	// Run executes one build.
	Run(ctx context.Context, in RunRequest) (RunOutput, error)
	// AllowPrivilegedRun reports whether KILN_MCP_ALLOW_RUN is set.
	AllowPrivilegedRun() bool
}

// RunRequest is a delivery-neutral run request.
type RunRequest struct {
	SHA   string
	Event isolation.Event
	Fork  bool
	Ref   string
	PR    int
}

// DoctorOutput is what kiln_doctor reports.
type DoctorOutput struct {
	Repository string `json:"repository,omitempty"`
	Directory  string `json:"directory"`
	// PipelineFound is false for a repository with no .kiln.yaml, which still
	// proves and still reports a Check.
	PipelineFound bool `json:"pipeline_found"`
	// Routing maps each event to the phases it triggers.
	Routing map[string][]string `json:"routing"`
	// Artifacts describes every entry in the publish list, so an agent can see
	// that a commit yields both an image and a binary release without having
	// to read .kiln.yaml itself.
	Artifacts []ArtifactSummary `json:"artifacts,omitempty"`
	// Image and Tags describe the first image entry, kept for agents written
	// against the single-artifact shape.
	Image string   `json:"image,omitempty"`
	Tags  []string `json:"tags,omitempty"`
	// Toolchain reports which binaries were found.
	Toolchain map[string]bool `json:"toolchain"`
	// ChecksEnabled reports whether a token is present.
	ChecksEnabled bool `json:"checks_enabled"`
	// ProvenanceSkip reports whether trusted keys are pinned.
	ProvenanceSkip bool `json:"provenance_skip_possible"`
	// Dry reports KILN_DRY.
	Dry bool `json:"dry_run"`
	// Problems are the findings that would break a real run.
	Problems []string `json:"problems,omitempty"`
	// Warnings are deliberate degradations worth naming.
	Warnings []string `json:"warnings,omitempty"`
}

// ArtifactSummary is one configured artifact.
type ArtifactSummary struct {
	// Kind is image or binaries.
	Kind string `json:"kind"`
	// Target is the image reference, or the release tool and its config.
	Target string `json:"target"`
	// On lists the events this artifact is restricted to; empty means all of
	// them.
	On []string `json:"on,omitempty"`
}

// RunOutput is a delivery-neutral run record.
type RunOutput struct {
	ID    string `json:"id"`
	SHA   string `json:"sha"`
	Ref   string `json:"ref,omitempty"`
	Event string `json:"event"`
	Fork  bool   `json:"fork"`
	Phase string `json:"phase"`
	// Skipped records that a trusted warden note stood in for the re-prove.
	Skipped bool     `json:"prove_skipped"`
	Digest  string   `json:"digest,omitempty"`
	Tags    []string `json:"tags,omitempty"`
	Error   string   `json:"error,omitempty"`
	// Succeeded is the single field an agent should branch on. Phase carries
	// the same information, but a string comparison is easy to get wrong and a
	// wrong one here reads a failure as a success.
	Succeeded bool `json:"succeeded"`
}

// FromRun converts a run record.
func FromRun(r *run.Run) RunOutput {
	if r == nil {
		return RunOutput{}
	}
	return RunOutput{
		ID: r.ID, SHA: r.SHA, Ref: r.Ref, Event: r.Event, Fork: r.Fork,
		Phase: string(r.Phase), Skipped: r.Skipped, Digest: r.Digest,
		Tags: r.Tags, Error: r.Error, Succeeded: r.Phase == run.PhaseSucceeded,
	}
}

// Tool argument schemas.
type (
	// DoctorInput takes no arguments.
	DoctorInput struct{}

	// StatusInput selects a run.
	StatusInput struct {
		RunID string `json:"run_id,omitempty" jsonschema:"description=Run id to read; omit for the most recent run"`
	}

	// RunInput is the argument schema for kiln_run.
	RunInput struct {
		SHA   string `json:"sha" jsonschema:"required,description=Commit to build. HEAD and other commit-ish values are resolved"`
		Event string `json:"event" jsonschema:"required,description=pull_request, push or tag"`
		Ref   string `json:"ref,omitempty" jsonschema:"description=Ref the commit was found on, e.g. refs/heads/main or refs/tags/v1.2.0"`
		PR    int    `json:"pr,omitempty" jsonschema:"description=Pull request number; lets kiln resolve fork status from the GitHub API"`
		Fork  bool   `json:"fork,omitempty" jsonschema:"description=Force untrusted handling: no secrets, no publish, no provenance skip"`
	}
)

// ErrRunNotPermitted is returned when an agent asks for a push or tag run and
// the operator has not opted in.
var ErrRunNotPermitted = errors.New(
	"kiln_run refuses push and tag events on this surface: they publish and sign a real artifact. " +
		"Set KILN_MCP_ALLOW_RUN=1 to permit them, or use event=pull_request, which only proves")

const runDescription = `Build one commit: prove it through warden and, where policy allows, publish a signed image.

Isolation is enforced by kiln, not by this call. A pull request never publishes and never
sees registry credentials, whatever is asked for here; a fork pull request additionally
cannot skip the re-prove. Push and tag runs do publish and sign, so this surface refuses
them unless the operator set KILN_MCP_ALLOW_RUN=1.

Kiln does not deploy. Shipping the resulting digest is RollOps' job.`

// NewServer builds the MCP server.
func NewServer(f Facade, version string) *mcp.Server {
	srv := mcp.NewServer(mcp.ServerInfo{
		Name:    "kiln",
		Version: version,
		Title:   "Kiln",
		Description: "Signed-artifact factory: prove a commit through warden, build it, " +
			"sign the digest with cosign, and report to GitHub Checks. Never deploys.",
	})

	srv.Tool("kiln_doctor").
		Description("Validate .kiln.yaml, the tag plan, the toolchain (warden, docker, cosign) and " +
			"credentials. Runs no gate and builds nothing, so it is always safe to call.").
		ReadOnly().
		Handler(func(ctx context.Context, _ DoctorInput) (DoctorOutput, error) {
			return f.Doctor(ctx)
		})

	srv.Tool("kiln_status").
		Description("Read a run from the ledger: its phase, whether the gate was skipped for a " +
			"trusted warden note, and the digest and tags it produced. Omit run_id for the latest.").
		ReadOnly().
		Handler(func(ctx context.Context, in StatusInput) (RunOutput, error) {
			out, err := f.Status(ctx, in.RunID)
			if err != nil {
				return RunOutput{}, VisibleError(err)
			}
			return out, nil
		})

	srv.Tool("kiln_run").
		Description(runDescription).
		Handler(func(ctx context.Context, in RunInput) (RunOutput, error) {
			return handleRun(ctx, f, in)
		})

	return srv
}

// handleRun authorizes and then executes.
//
// The gate is checked before the input is otherwise used, so a refusal is
// deterministic and does not leak whether the rest of the request was valid.
func handleRun(ctx context.Context, f Facade, in RunInput) (RunOutput, error) {
	event, ok := isolation.ParseEvent(in.Event)
	if !ok {
		return RunOutput{}, VisibleError(
			fmt.Errorf("event must be pull_request, push or tag, got %q", in.Event))
	}
	if event != isolation.EventPullRequest && !f.AllowPrivilegedRun() {
		return RunOutput{}, VisibleError(ErrRunNotPermitted)
	}
	if in.SHA == "" {
		return RunOutput{}, VisibleError(errors.New("sha is required"))
	}

	out, err := f.Run(ctx, RunRequest{
		SHA: in.SHA, Event: event, Fork: in.Fork, Ref: in.Ref, PR: in.PR,
	})
	if err != nil {
		// A failed build is a *result*, not a protocol error: the agent needs
		// the run record to see which phase failed and why. Only report an
		// error when there is no record at all.
		if out.ID != "" {
			return out, nil
		}
		return RunOutput{}, VisibleError(err)
	}
	return out, nil
}

// Serve runs the server on stdio until the context is cancelled.
func Serve(ctx context.Context, f Facade, version string) error {
	return mcp.ServeStdio(ctx, NewServer(f, version))
}

// visible marks an error whose message the caller is meant to read and act on.
//
// The dispatcher sanitizes a raw handler error to "internal error" before it
// reaches the client — right for a failure that might leak a path, wrong for a
// refusal the caller is supposed to resolve. An agent told "internal error"
// cannot learn that the operator must set KILN_MCP_ALLOW_RUN, so it has no
// move except to give up or retry identically.
type visible struct{ cause error }

func (e *visible) Error() string { return e.cause.Error() }

func (e *visible) Unwrap() []error {
	return []error{e.cause, &mcp.ToolInputError{Message: e.cause.Error()}}
}

// VisibleError wraps err so its message reaches the client. Returns nil for
// nil, so it can wrap a call site unconditionally.
func VisibleError(err error) error {
	if err == nil {
		return nil
	}
	return &visible{cause: err}
}
