// Package config loads and validates `.kiln.yaml`.
//
// `.kiln.yaml` is deliberately small. It says what to publish and which events
// route to which phase — nothing else. The *checks* live in `.warden.yaml`,
// because Kiln does not invent a second check language, and *deployment* lives
// in RollOps, because Kiln never applies. Both boundaries are enforced here
// rather than documented and hoped for: a `deploy:` or `apply:` key is a load
// error, and `prove.from` accepts exactly one value.
//
// Every validation in this package fails closed. A pipeline that cannot be
// understood is never a pipeline that quietly does less.
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// FileName is the pipeline file Kiln looks for at the repository root.
const FileName = ".kiln.yaml"

// The single accepted API version and kind. Widening these is a schema change
// that needs a migration note, not a quiet acceptance of extra spellings.
const (
	APIVersion = "kiln.klarlabs.de/v1"
	Kind       = "Pipeline"
)

// Step is a phase an event can route to.
type Step string

const (
	StepProve   Step = "prove"
	StepPublish Step = "publish"
)

// Tag is a tag kind the publisher knows how to compute.
type Tag string

const (
	// TagSHA is the immutable `sha-<short>` tag. Always required.
	TagSHA Tag = "sha"
	// TagLatest is the moving tag RollOps follows in digest mode.
	TagLatest Tag = "latest"
	// TagSemver is the `vX.Y.Z` tag derived from an annotated tag ref.
	TagSemver Tag = "semver"
)

// ErrNotFound reports that no `.kiln.yaml` exists. It is not a failure: a
// library repository legitimately has nothing to publish, and Kiln still
// proves it and posts the Check. Callers use Default() in that case.
var ErrNotFound = errors.New("no .kiln.yaml")

// Pipeline is the whole of `.kiln.yaml`.
type Pipeline struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	On         On       `yaml:"on"`
	Prove      Prove    `yaml:"prove"`
	Publish    *Publish `yaml:"publish"`
	Watch      Watch    `yaml:"watch"`
}

// On routes events to phases. An absent `tag` list inherits `push`, because a
// tag is a push that happens to carry a name and an operator who wrote
// `push: [prove, publish]` plainly meant releases to publish too.
type On struct {
	PullRequest []Step `yaml:"pull_request"`
	Push        []Step `yaml:"push"`
	Tag         []Step `yaml:"tag"`
}

// Prove points at the source gate. `from` exists as a field only so the
// coupling to Warden is explicit in the file; `warden` is its only value.
type Prove struct {
	From string `yaml:"from"`
	Nox  bool   `yaml:"nox"`
}

// Publish describes the artifact. Tags must always include the immutable sha
// tag plus at least one moving tag — see validate for why.
type Publish struct {
	Image      string   `yaml:"image"`
	Tags       []Tag    `yaml:"tags"`
	Sign       string   `yaml:"sign"`
	Platforms  []string `yaml:"platforms"`
	Dockerfile string   `yaml:"dockerfile"`
	Context    string   `yaml:"context"`
}

// Watch configures unattended discovery. PullRequests and Tags are pointers so
// "absent" is distinguishable from "explicitly false"; both default to true.
type Watch struct {
	Remote       string `yaml:"remote"`
	Ref          string `yaml:"ref"`
	PullRequests *bool  `yaml:"pull_requests"`
	Tags         *bool  `yaml:"tags"`
}

// Default is the pipeline used when a repository has no `.kiln.yaml`: prove
// every event, publish nothing.
func Default() Pipeline {
	p := Pipeline{
		APIVersion: APIVersion,
		Kind:       Kind,
		On: On{
			PullRequest: []Step{StepProve},
			Push:        []Step{StepProve},
			Tag:         []Step{StepProve},
		},
		Prove: Prove{From: "warden"},
	}
	p.Watch.applyDefaults()
	return p
}

// LoadDir reads `.kiln.yaml` from a repository root. A missing file returns
// ErrNotFound alongside Default(), so a caller that does not care about the
// distinction can ignore the error and still get a usable pipeline.
func LoadDir(dir string) (Pipeline, error) {
	return LoadFile(filepath.Join(dir, FileName))
}

// LoadFile reads a pipeline from an explicit path.
func LoadFile(path string) (Pipeline, error) {
	f, err := os.Open(path) //nolint:gosec // operator-supplied pipeline path
	if err != nil {
		if os.IsNotExist(err) {
			return Default(), fmt.Errorf("%w at %s", ErrNotFound, path)
		}
		return Pipeline{}, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	p, err := Parse(f)
	if err != nil {
		return Pipeline{}, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// Parse decodes and validates a pipeline from any reader.
//
// KnownFields(true) is what makes the schema a contract: a typo in a key is an
// error rather than a setting that silently does nothing. It is also the
// mechanism that rejects a `deploy:` block — but only with yaml's generic
// "field not found" wording, so scanForCDKeys runs first to say plainly where
// deployment actually lives.
func Parse(r io.Reader) (Pipeline, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return Pipeline{}, fmt.Errorf("read: %w", err)
	}
	if err := scanForCDKeys(raw); err != nil {
		return Pipeline{}, err
	}

	var p Pipeline
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		if errors.Is(err, io.EOF) {
			return Pipeline{}, errors.New("empty pipeline: apiVersion and kind are required")
		}
		return Pipeline{}, fmt.Errorf("parse: %w", err)
	}

	p.applyDefaults()
	if err := p.validate(); err != nil {
		return Pipeline{}, err
	}
	return p, nil
}

// cdKeys are the top-level keys that mean somebody is trying to make Kiln a CD
// tool. They get a named error instead of a generic unknown-field one because
// the fix is architectural — go use RollOps — not a spelling correction.
var cdKeys = []string{"deploy", "apply", "rollout", "canary", "rollback"}

func scanForCDKeys(raw []byte) error {
	var probe map[string]yaml.Node
	// A malformed document is not this check's problem; the real decoder below
	// reports it with a proper position.
	if err := yaml.Unmarshal(raw, &probe); err != nil {
		return nil //nolint:nilerr // deliberate: defer the parse error to Decode
	}
	for _, k := range cdKeys {
		if _, ok := probe[k]; ok {
			return fmt.Errorf(
				"%q is not a kiln key: kiln builds and signs artifacts, it never deploys them — "+
					"deployment belongs in RollOps", k)
		}
	}
	return nil
}

func (p *Pipeline) applyDefaults() {
	if p.Prove.From == "" {
		p.Prove.From = "warden"
	}
	if p.On.PullRequest == nil && p.On.Push == nil && p.On.Tag == nil {
		p.On.PullRequest = []Step{StepProve}
		p.On.Push = []Step{StepProve, StepPublish}
	}
	if p.On.Tag == nil {
		p.On.Tag = p.On.Push
	}
	if p.Publish != nil {
		p.Publish.applyDefaults()
	}
	p.Watch.applyDefaults()
}

func (pb *Publish) applyDefaults() {
	if len(pb.Tags) == 0 {
		pb.Tags = []Tag{TagSHA, TagLatest}
	}
	if pb.Sign == "" {
		pb.Sign = "cosign"
	}
	if pb.Dockerfile == "" {
		pb.Dockerfile = "Dockerfile"
	}
	if pb.Context == "" {
		pb.Context = "."
	}
	if len(pb.Platforms) == 0 {
		pb.Platforms = []string{"linux/amd64"}
	}
}

func (w *Watch) applyDefaults() {
	if w.Remote == "" {
		w.Remote = "origin"
	}
	if w.Ref == "" {
		w.Ref = "main"
	}
	if w.PullRequests == nil {
		w.PullRequests = boolPtr(true)
	}
	if w.Tags == nil {
		w.Tags = boolPtr(true)
	}
}

func (p Pipeline) validate() error {
	if p.APIVersion != APIVersion {
		return fmt.Errorf("apiVersion must be %q, got %q", APIVersion, p.APIVersion)
	}
	if p.Kind != Kind {
		return fmt.Errorf("kind must be %q, got %q", Kind, p.Kind)
	}
	if p.Prove.From != "warden" {
		return fmt.Errorf(
			"prove.from must be \"warden\", got %q: .warden.yaml is the only check language kiln speaks",
			p.Prove.From)
	}
	for name, steps := range map[string][]Step{
		"on.pull_request": p.On.PullRequest,
		"on.push":         p.On.Push,
		"on.tag":          p.On.Tag,
	} {
		if err := validateSteps(name, steps); err != nil {
			return err
		}
	}
	if p.WantsPublish() && p.Publish == nil {
		return errors.New("an event routes to publish but no publish: block is configured")
	}
	if p.Publish != nil {
		if err := p.Publish.validate(); err != nil {
			return err
		}
	}
	return nil
}

func validateSteps(field string, steps []Step) error {
	seen := make(map[Step]bool, len(steps))
	for _, s := range steps {
		switch s {
		case StepProve, StepPublish:
		default:
			return fmt.Errorf("%s: unknown step %q (want prove or publish)", field, s)
		}
		if seen[s] {
			return fmt.Errorf("%s: step %q listed twice", field, s)
		}
		seen[s] = true
	}
	// Publishing something that was never proven is the one ordering mistake
	// worth catching statically: it would produce a signed artifact with no
	// gate behind it, which is the exact thing Kiln exists to prevent.
	if seen[StepPublish] && !seen[StepProve] {
		return fmt.Errorf("%s: publish without prove — kiln does not sign an ungated commit", field)
	}
	return nil
}

func (pb Publish) validate() error {
	if strings.TrimSpace(pb.Image) == "" {
		return errors.New("publish.image is required")
	}
	if pb.Sign != "cosign" {
		return fmt.Errorf("publish.sign must be \"cosign\", got %q", pb.Sign)
	}

	seen := make(map[Tag]bool, len(pb.Tags))
	for _, t := range pb.Tags {
		switch t {
		case TagSHA, TagLatest, TagSemver:
		default:
			return fmt.Errorf("publish.tags: unknown tag %q (want sha, latest or semver)", t)
		}
		if seen[t] {
			return fmt.Errorf("publish.tags: %q listed twice", t)
		}
		seen[t] = true
	}
	if !seen[TagSHA] {
		return errors.New(`publish.tags must include "sha": the immutable tag is what RollOps pins to`)
	}
	// A SHA-only tag set is rejected, not defaulted. RollOps' imagePolicy
	// watches a moving tag to discover new digests; with nothing but sha-<x>
	// tags there is no tag to watch, and the pipeline would build artifacts no
	// deploy could ever find.
	if !seen[TagLatest] && !seen[TagSemver] {
		return errors.New(
			`publish.tags is sha-only: add "latest" or "semver" — RollOps' imagePolicy cannot follow a moving target that never moves`)
	}
	if strings.TrimSpace(pb.Dockerfile) == "" {
		return errors.New("publish.dockerfile must not be empty")
	}
	if strings.TrimSpace(pb.Context) == "" {
		return errors.New("publish.context must not be empty")
	}
	return nil
}

// Steps returns the phases configured for an event name.
func (p Pipeline) Steps(event string) []Step {
	switch event {
	case "pull_request":
		return p.On.PullRequest
	case "push":
		return p.On.Push
	case "tag":
		return p.On.Tag
	default:
		return nil
	}
}

// Wants reports whether an event routes to a step.
func (p Pipeline) Wants(event string, step Step) bool {
	return slices.Contains(p.Steps(event), step)
}

// WantsPublish reports whether any event routes to publish.
func (p Pipeline) WantsPublish() bool {
	for _, e := range []string{"pull_request", "push", "tag"} {
		if p.Wants(e, StepPublish) {
			return true
		}
	}
	return false
}

// WatchPullRequests and WatchTags read the tri-state watch toggles.
func (p Pipeline) WatchPullRequests() bool {
	return p.Watch.PullRequests == nil || *p.Watch.PullRequests
}
func (p Pipeline) WatchTags() bool { return p.Watch.Tags == nil || *p.Watch.Tags }

// TagKinds returns the configured tag kinds in a stable order, so `kiln doctor`
// prints the same plan twice in a row.
func (pb Publish) TagKinds() []Tag {
	out := slices.Clone(pb.Tags)
	slices.Sort(out)
	return out
}

func boolPtr(b bool) *bool { return &b }
