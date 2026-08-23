// Package attest builds the provenance statement Kiln publishes alongside an
// artifact.
//
// Until now Kiln's half of the provenance chain lived in a local JSON file.
// Warden's claim — "the configured checks ran and passed on this commit" — is
// a signed note anyone can verify. Kiln's claim — "this artifact was built
// from that commit" — was verifiable by nobody, because the only record was a
// ledger on the build box. An auditor asking "where did this image come from?"
// had to trust the machine that answered.
//
// This package closes that. It emits an in-toto Statement carrying a SLSA v1
// provenance predicate, which cosign attaches to the artifact's digest. The
// claim then travels with the artifact and can be checked by anyone holding
// the public key — including RollOps at deploy time.
package attest

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// The in-toto and SLSA type URIs this package emits. They are protocol
// constants, not settings: a verifier matches on them exactly.
const (
	StatementType = "https://in-toto.io/Statement/v1"
	PredicateType = "https://slsa.dev/provenance/v1"

	// BuildType names how Kiln builds, so a verifier reading the predicate
	// knows which externalParameters to expect.
	BuildType = "https://klarlabs.de/kiln/buildtype/v1"

	// BuilderIDPrefix identifies Kiln as the build platform. The running
	// version is appended, because "which builder produced this" is a
	// different question from "which builder do I trust".
	BuilderIDPrefix = "https://github.com/klarlabs-studio/kiln"
)

// CosignType is the --type value for `cosign attest`. Passing the predicate
// type URI rather than a shorthand keeps the attachment self-describing.
const CosignType = PredicateType

// Statement is an in-toto v1 statement.
type Statement struct {
	Type          string     `json:"_type"`
	Subject       []Subject  `json:"subject"`
	PredicateType string     `json:"predicateType"`
	Predicate     Provenance `json:"predicate"`
}

// Subject is what the statement is about.
type Subject struct {
	Name   string            `json:"name"`
	Digest map[string]string `json:"digest"`
}

// Provenance is the SLSA v1 predicate.
type Provenance struct {
	BuildDefinition BuildDefinition `json:"buildDefinition"`
	RunDetails      RunDetails      `json:"runDetails"`
}

// BuildDefinition says what was built and from what.
type BuildDefinition struct {
	BuildType string `json:"buildType"`
	// ExternalParameters are the inputs a caller controlled. Everything here
	// is attacker-visible by design — it is what somebody reproducing the
	// build would need.
	ExternalParameters ExternalParameters `json:"externalParameters"`
	// InternalParameters are the build platform's own choices.
	InternalParameters InternalParameters `json:"internalParameters"`
	// ResolvedDependencies pins the source. The git commit is the link back to
	// Warden's note, which is bound to that same commit.
	ResolvedDependencies []ResourceDescriptor `json:"resolvedDependencies,omitempty"`
}

// ExternalParameters records the request that produced this artifact.
type ExternalParameters struct {
	// Repository is the source, as a SPDX-style git URI where one is known.
	Repository string `json:"repository,omitempty"`
	// Ref is the ref the commit was discovered on.
	Ref string `json:"ref,omitempty"`
	// Event is pull_request, push or tag.
	Event string `json:"event"`
	// Artifact is the pipeline entry: image or binaries.
	Artifact string `json:"artifact"`
	// Config names the file that described the build.
	Config string `json:"config,omitempty"`
	// BuildArgs are the docker build arguments, when the artifact used any.
	//
	// They belong in externalParameters because they are caller-controlled
	// inputs that change what the image contains: senat-api and senat-runtime
	// are one commit and one Dockerfile differing only by BIN=, and without
	// this their attestations would be identical while their contents are not.
	// Anyone reproducing the build needs them.
	BuildArgs map[string]string `json:"buildArgs,omitempty"`
}

// InternalParameters records what the platform decided, including the two
// facts an auditor most wants and a signature alone cannot express.
type InternalParameters struct {
	// Isolated reports that the build ran without the operator's credentials.
	Isolated bool `json:"isolated"`
	// SourceGate describes how the commit was gated — see SourceGate. This is
	// the field that distinguishes a build whose checks ran from one that
	// inherited a note, and it is the whole reason this predicate is worth
	// signing.
	SourceGate SourceGate `json:"sourceGate"`
}

// SourceGate is Kiln's record of Warden's verdict.
type SourceGate struct {
	// Tool is the gate that ran, normally "warden".
	Tool string `json:"tool"`
	// Verified is true when the commit passed a gate — either because this
	// build ran it, or because a trusted note already carried it.
	Verified bool `json:"verified"`
	// Reproved is true when this build executed the checks itself. False means
	// the verdict was inherited from a signed note, which is legitimate and
	// must still be visible: a reader deciding how much to trust this artifact
	// needs to know which happened.
	Reproved bool `json:"reproved"`
	// Reason is the human-readable justification, carried verbatim from the
	// provenance decision.
	Reason string `json:"reason,omitempty"`
}

// RunDetails says who built it and when.
type RunDetails struct {
	Builder  Builder  `json:"builder"`
	Metadata Metadata `json:"metadata"`
}

// Builder identifies the build platform.
type Builder struct {
	ID string `json:"id"`
	// Version pins the components whose behaviour affected the result.
	Version map[string]string `json:"version,omitempty"`
}

// Metadata is the invocation's identity and timing.
type Metadata struct {
	InvocationID string `json:"invocationId"`
	StartedOn    string `json:"startedOn"`
	FinishedOn   string `json:"finishedOn,omitempty"`
}

// ResourceDescriptor points at an input.
type ResourceDescriptor struct {
	URI    string            `json:"uri,omitempty"`
	Digest map[string]string `json:"digest,omitempty"`
	Name   string            `json:"name,omitempty"`
}

// Input is everything needed to describe one build. It is a plain struct
// rather than a reference to the engine's types so this package stays
// independent of them — a predicate format should not move when an internal
// field is renamed.
type Input struct {
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

// Build assembles the statement.
//
// It never fails on missing optional context: a build with no known repository
// still produces a valid statement, because a predicate that refuses to exist
// teaches a verifier nothing. It does insist on a subject, since a statement
// about nothing is not a statement.
func Build(in Input) (Statement, error) {
	alg, hex, ok := splitDigest(in.SubjectDigest)
	if !ok {
		return Statement{}, fmt.Errorf("attest: %q is not a digest of the form sha256:<hex>", in.SubjectDigest)
	}
	if strings.TrimSpace(in.SubjectName) == "" {
		return Statement{}, fmt.Errorf("attest: no subject name")
	}

	gateTool := in.GateTool
	if gateTool == "" {
		gateTool = "warden"
	}

	stmt := Statement{
		Type:          StatementType,
		PredicateType: PredicateType,
		Subject: []Subject{{
			Name:   in.SubjectName,
			Digest: map[string]string{alg: hex},
		}},
		Predicate: Provenance{
			BuildDefinition: BuildDefinition{
				BuildType: BuildType,
				ExternalParameters: ExternalParameters{
					Repository: repoURI(in.Repo),
					Ref:        in.Ref,
					Event:      in.Event,
					Artifact:   in.ArtifactKind,
					Config:     in.Config,
					BuildArgs:  in.BuildArgs,
				},
				InternalParameters: InternalParameters{
					Isolated: in.Isolated,
					SourceGate: SourceGate{
						Tool: gateTool,
						// Kiln only reaches a publish after the gate is
						// satisfied, so a statement exists at all only when
						// the commit was gated one way or the other.
						Verified: true,
						Reproved: in.GateReproved,
						Reason:   in.GateReason,
					},
				},
				ResolvedDependencies: sourceDependency(in),
			},
			RunDetails: RunDetails{
				Builder: Builder{
					ID:      BuilderIDPrefix + "@" + orUnknown(in.KilnVersion),
					Version: in.ToolVersions,
				},
				Metadata: Metadata{
					InvocationID: in.InvocationID,
					StartedOn:    in.StartedOn.UTC().Format(time.RFC3339),
					FinishedOn:   optionalTime(in.FinishedOn),
				},
			},
		},
	}
	return stmt, nil
}

// sourceDependency pins the commit the artifact came from. gitCommit is the
// digest algorithm SLSA reserves for exactly this, and it is what lets a
// verifier walk from the artifact back to Warden's note.
func sourceDependency(in Input) []ResourceDescriptor {
	if in.SHA == "" {
		return nil
	}
	dep := ResourceDescriptor{
		Name:   "source",
		Digest: map[string]string{"gitCommit": in.SHA},
	}
	if uri := repoURI(in.Repo); uri != "" {
		dep.URI = uri
		if in.Ref != "" {
			dep.URI += "@" + in.Ref
		}
	}
	return []ResourceDescriptor{dep}
}

// repoURI renders owner/name as the git URI SLSA expects. An unidentifiable
// repository yields an empty string rather than a fabricated URL.
func repoURI(repo string) string {
	if strings.TrimSpace(repo) == "" {
		return ""
	}
	if strings.Contains(repo, "://") {
		return repo
	}
	return "git+https://github.com/" + repo
}

// splitDigest parses "sha256:<hex>".
func splitDigest(d string) (alg, hex string, ok bool) {
	alg, hex, found := strings.Cut(strings.TrimSpace(d), ":")
	if !found || alg == "" || hex == "" {
		return "", "", false
	}
	return alg, hex, true
}

func optionalTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// JSON renders the whole statement — the shape a *verifier* reads back out of
// an attestation.
//
// This is not what goes into cosign's --predicate file. See PredicateJSON.
func (s Statement) JSON() ([]byte, error) {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("attest: encode statement: %w", err)
	}
	return append(data, '\n'), nil
}

// PredicateJSON renders just the predicate body, which is what cosign's
// --predicate flag wants.
//
// The distinction is easy to get wrong and silent when you do. `cosign attest`
// builds the in-toto statement itself: it sets _type, sets predicateType from
// --type, derives the subject from the artifact being attested, and drops the
// file's entire contents in as the predicate. Hand it a complete statement and
// you get a statement nested inside a statement — predicate.predicate.
// buildDefinition — which still signs and still verifies, so nothing complains,
// while every field a consumer reads by path is one level from where they look.
//
// Letting cosign derive the subject is the better arrangement anyway: the
// subject then comes from the artifact cosign is actually attesting rather than
// from a builder asserting what it thinks it built.
func (s Statement) PredicateJSON() ([]byte, error) {
	data, err := json.MarshalIndent(s.Predicate, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("attest: encode predicate: %w", err)
	}
	return append(data, '\n'), nil
}

// Parse reads a statement back, for verification.
func Parse(data []byte) (Statement, error) {
	var s Statement
	if err := json.Unmarshal(data, &s); err != nil {
		return Statement{}, fmt.Errorf("attest: parse statement: %w", err)
	}
	if s.PredicateType != PredicateType {
		return Statement{}, fmt.Errorf("attest: predicate type %q, want %q", s.PredicateType, PredicateType)
	}
	return s, nil
}

// SourceCommit returns the commit the artifact was built from, which is the
// hinge of the whole chain: it is what Warden's note is bound to.
func (s Statement) SourceCommit() string {
	for _, dep := range s.Predicate.BuildDefinition.ResolvedDependencies {
		if c := dep.Digest["gitCommit"]; c != "" {
			return c
		}
	}
	return ""
}

// BuiltByKiln reports whether this statement claims a Kiln build. A verifier
// should check it before reading Kiln-specific fields out of the predicate.
func (s Statement) BuiltByKiln() bool {
	return s.Predicate.BuildDefinition.BuildType == BuildType &&
		strings.HasPrefix(s.Predicate.RunDetails.Builder.ID, BuilderIDPrefix)
}
