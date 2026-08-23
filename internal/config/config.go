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
	"time"

	"gopkg.in/yaml.v3"

	"go.klarlabs.de/kiln/internal/prune"
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
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	On         On     `yaml:"on"`
	Prove      Prove  `yaml:"prove"`
	// Publish is a list because "signed artifact" is not a synonym for
	// "container image". One proven commit routinely yields several: an image
	// RollOps deploys, and the release binaries a human downloads. A third kind
	// later is one more list entry rather than one more top-level key.
	Publish []Artifact `yaml:"publish"`
	// Tasks are the automation that is not a check and not an artifact —
	// uploading a scan result, opening a remediation pull request, refreshing
	// a docs site. Everything a pipeline does that is neither "decide whether
	// this commit is good" nor "produce something signed".
	//
	// Deliberately a separate language from `.warden.yaml`, and deliberately
	// weaker: a task cannot mint provenance. Whatever it does, the signed
	// artifacts of a run are exactly what `publish:` produced, so growing this
	// surface can never dilute the claim kiln exists to make.
	Tasks map[string]Task `yaml:"tasks"`
	// Services are containers started beside the gate — a database a test
	// suite talks to, a fake API. They run for the whole of prove and tasks
	// and are torn down afterwards whatever happened.
	Services map[string]Service `yaml:"services,omitempty"`
	Watch    Watch              `yaml:"watch"`
}

// Service is a container the gate needs beside it.
type Service struct {
	Image string            `yaml:"image"`
	Env   map[string]string `yaml:"env,omitempty"`
	// Command overrides the image's entrypoint arguments.
	Command []string `yaml:"command,omitempty"`
	// Port is the port *inside* the container. The host port is allocated by
	// docker and exported as KILN_SERVICE_<NAME>_PORT — never fixed, because a
	// box runs many repositories and two pipelines both wanting 5432 would
	// collide in a way that looks like a flaky test.
	Port int `yaml:"port,omitempty"`
	// Ready is a command run inside the container until it succeeds, e.g.
	// `pg_isready -U postgres`.
	Ready        string   `yaml:"ready,omitempty"`
	ReadyTimeout Duration `yaml:"ready_timeout,omitempty"`
}

// Task is one named automation.
type Task struct {
	// On routes the task to events. `schedule` is kiln's own: it fires from a
	// watch tick rather than from a commit.
	On []string `yaml:"on"`
	// Every is the interval for a scheduled task.
	Every Duration `yaml:"every,omitempty"`
	// Run is the command, executed by `sh -c` in the checked-out worktree.
	// Multi-line is one script, not a list of steps: a task that half-ran is
	// the failure mode of every step runner, and a shell already has `set -e`.
	Run string `yaml:"run"`
	// Workdir is relative to the worktree root.
	Workdir string `yaml:"workdir,omitempty"`
	// AllowFailure records the task's result without failing the run. For
	// something advisory — a nightly report — a red run trains people to
	// ignore red runs.
	AllowFailure bool `yaml:"allow_failure,omitempty"`
	// Keep are globs, relative to the worktree, whose matches are copied out
	// before the tree is destroyed — a coverage report, a scan, the log that
	// explains the failure. The local equivalent of upload-artifact.
	Keep []string `yaml:"keep,omitempty"`
	// PullRequest opens or updates a pull request from whatever the command
	// changed in the worktree. Nil leaves the changes where they are, which
	// for most tasks is nothing at all.
	PullRequest *PullRequest `yaml:"pull_request,omitempty"`
}

// PullRequest describes the pull request a task's changes should land in.
type PullRequest struct {
	// Branch is the head branch. Reused across runs on purpose: a daily
	// remediation task updates its own pull request rather than opening
	// thirty of them.
	Branch string   `yaml:"branch"`
	Title  string   `yaml:"title"`
	Body   string   `yaml:"body,omitempty"`
	Labels []string `yaml:"labels,omitempty"`
	// Base is the target branch. Empty means the repository default.
	Base string `yaml:"base,omitempty"`
}

// ScheduleEvent is the pseudo-event a scheduled task routes to.
const ScheduleEvent = "schedule"

// TasksFor returns the tasks routed to an event, in a stable order.
//
// Sorted by name because a map has none, and a run whose task order changed
// between ticks would make its own log unreadable.
func (p Pipeline) TasksFor(event string) []NamedTask {
	out := make([]NamedTask, 0, len(p.Tasks))
	for name, t := range p.Tasks {
		if slices.Contains(t.On, event) {
			out = append(out, NamedTask{Name: name, Task: t})
		}
	}
	slices.SortFunc(out, func(a, b NamedTask) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// NamedTask pairs a task with the name it was written under.
type NamedTask struct {
	Name string
	Task Task
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

// ArtifactKind discriminates the entries in the publish list.
type ArtifactKind string

const (
	// KindImage is an OCI image: docker build, push, cosign the digest.
	KindImage ArtifactKind = "image"
	// KindBinaries is a binary release: cross-compiled archives, checksums and
	// a GitHub Release, produced by goreleaser.
	KindBinaries ArtifactKind = "binaries"
)

// Artifact is one thing to publish.
//
// The fields of both kinds live on one struct rather than in a union. That
// keeps yaml's KnownFields check working — it operates on the struct, and it
// is what turns a typo into an error — at the cost of needing validate to
// reject a field belonging to the other kind. The error that produces is far
// better than the one a custom unmarshaller would give.
type Artifact struct {
	Kind ArtifactKind `yaml:"kind"`
	// On restricts this artifact to certain events (pull_request, push, tag).
	// Empty means every event that routes to publish at all.
	On []string `yaml:"on"`

	// Image fields.
	Image      string   `yaml:"image"`
	Tags       []Tag    `yaml:"tags"`
	Platforms  []string `yaml:"platforms"`
	Dockerfile string   `yaml:"dockerfile"`
	Context    string   `yaml:"context"`
	// Args are docker build arguments, as explicit key=value pairs.
	//
	// A map rather than a list, because a build argument given twice is a
	// mistake YAML can catch for free. There is deliberately no passthrough
	// form (`--build-arg FOO` taking FOO from the environment): a build whose
	// output depends on the box's environment is not reproducible from the
	// commit, and reproducibility from the commit is the whole claim kiln
	// makes about an artifact.
	//
	// They are recorded in the provenance, because two images built from one
	// commit and one Dockerfile — senat-api and senat-runtime differ only by
	// BIN= — are otherwise indistinguishable in their attestations.
	Args map[string]string `yaml:"args"`

	// Binaries fields. From names the tool, for the same reason prove.from
	// does: the coupling is explicit in the file, and there is one value.
	From   string `yaml:"from"`
	Config string `yaml:"config"`

	// Sign applies to both kinds. Kiln signs the image digest, or the release
	// checksum manifest.
	Sign string `yaml:"sign"`

	// Keep is how many of this image's sha-tagged builds stay on the build
	// box. Nil takes the default; 0 disables local pruning for this image.
	//
	// A pointer because "unset" and "never prune" are different answers and an
	// operator who wrote 0 meant the second one.
	Keep *int `yaml:"keep"`
}

// Watch configures unattended discovery. PullRequests and Tags are pointers so
// "absent" is distinguishable from "explicitly false"; both default to true.
type Watch struct {
	Remote       string `yaml:"remote"`
	Ref          string `yaml:"ref"`
	PullRequests *bool  `yaml:"pull_requests"`
	Tags         *bool  `yaml:"tags"`
}

// Duration is a YAML-friendly time.Duration.
//
// A bare number is refused rather than guessed at. `every: 30` could mean
// seconds, minutes or hours depending on what the author had in mind, and a
// scheduler that picks one of those silently will pick the wrong one on
// somebody's production box.
type Duration time.Duration

// UnmarshalYAML parses "24h", "15m", "90s".
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var raw string
	if err := node.Decode(&raw); err != nil {
		return fmt.Errorf("every: want a duration string like \"24h\", got %q", node.Value)
	}
	parsed, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("every: %q is not a duration (want something like \"24h\" or \"15m\")", raw)
	}
	if parsed <= 0 {
		return fmt.Errorf("every: %q is not a positive interval", raw)
	}
	*d = Duration(parsed)
	return nil
}

// Std returns the standard-library duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

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
	if err := scanForLegacyPublish(raw); err != nil {
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

// scanForLegacyPublish recognises the single-artifact `publish:` mapping that
// preceded the list, and answers with the migration rather than yaml's
// "cannot unmarshal !!map into []config.Artifact".
//
// The shape changed because "signed artifact" was never a synonym for
// "container image": one commit yields an image and a set of release
// binaries, and a mapping can only describe one of them.
func scanForLegacyPublish(raw []byte) error {
	var probe struct {
		Publish yaml.Node `yaml:"publish"`
	}
	if err := yaml.Unmarshal(raw, &probe); err != nil {
		return nil //nolint:nilerr // deliberate: defer the parse error to Decode
	}
	if probe.Publish.Kind != yaml.MappingNode {
		return nil
	}
	return errors.New(`publish: is a list of artifacts, not a single image.

Wrap the existing block in a list entry and give it a kind:

  publish:
    - kind: image
      image: ghcr.io/owner/name
      tags: [sha, latest]

A binary release is a second entry — see docs/configuration.md`)
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
	for i := range p.Publish {
		p.Publish[i].applyDefaults()
	}
	p.Watch.applyDefaults()
}

func (a *Artifact) applyDefaults() {
	if a.Kind == "" {
		// An entry that names no kind is an image. That is the common case and
		// the one the schema started with, so the shorthand costs nothing.
		a.Kind = KindImage
	}
	if a.Sign == "" {
		a.Sign = "cosign"
	}

	switch a.Kind {
	case KindImage:
		if a.Keep == nil {
			a.Keep = intPtr(prune.DefaultKeep)
		}
		if len(a.Tags) == 0 {
			a.Tags = []Tag{TagSHA, TagLatest}
		}
		if a.Dockerfile == "" {
			a.Dockerfile = "Dockerfile"
		}
		if a.Context == "" {
			a.Context = "."
		}
		if len(a.Platforms) == 0 {
			a.Platforms = []string{"linux/amd64"}
		}
	case KindBinaries:
		if a.From == "" {
			a.From = "goreleaser"
		}
		if a.Config == "" {
			a.Config = ".goreleaser.yaml"
		}
		if len(a.On) == 0 {
			// Tags only, by default. goreleaser derives the version from the
			// tag, so a binary release on a branch push would either fail or
			// publish something with a version nobody can ask for.
			a.On = []string{"tag"}
		}
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
	if p.WantsPublish() && len(p.Publish) == 0 {
		return errors.New("an event routes to publish but the publish: list is empty")
	}
	if err := p.validateTasks(); err != nil {
		return err
	}
	if err := p.validateServices(); err != nil {
		return err
	}
	for i, a := range p.Publish {
		if err := a.validate(i); err != nil {
			return err
		}
	}
	return p.validateNoDuplicateImages()
}

// validateNoDuplicateImages catches two entries publishing the same image.
// They would race each other onto the same moving tag, and which one won would
// depend on ordering nobody wrote down.
// validateTasks refuses a task that would silently never run.
//
// Every rule here exists because the alternative is a task that looks
// configured and does nothing — the worst outcome for automation, since the
// absence of a result is indistinguishable from a result of "nothing to do".
func (p Pipeline) validateTasks() error {
	for name, t := range p.Tasks {
		where := fmt.Sprintf("tasks.%s", name)
		if strings.TrimSpace(name) == "" {
			return errors.New("tasks: a task needs a name")
		}
		if strings.ContainsAny(name, " \t/") {
			return fmt.Errorf("%s: a task name is used as a check name and a log field; "+
				"keep it to letters, digits, dashes and underscores", where)
		}
		if strings.TrimSpace(t.Run) == "" {
			return fmt.Errorf("%s.run is required: a task with no command is not a task", where)
		}
		if len(t.On) == 0 {
			return fmt.Errorf("%s.on is required (pull_request, push, tag or schedule): "+
				"a task routed to nothing would never run and nothing would say so", where)
		}

		scheduled := false
		for _, event := range t.On {
			switch event {
			case "pull_request", "push", "tag":
			case ScheduleEvent:
				scheduled = true
			default:
				return fmt.Errorf("%s.on: unknown event %q (want pull_request, push, tag or schedule)",
					where, event)
			}
		}

		switch {
		case scheduled && t.Every.Std() <= 0:
			return fmt.Errorf(`%s is scheduled but has no interval: add every: "24h"`, where)
		case !scheduled && t.Every.Std() > 0:
			return fmt.Errorf("%s has every: but is not routed to schedule — the interval would be ignored", where)
		}
		if pr := t.PullRequest; pr != nil {
			switch {
			case strings.TrimSpace(pr.Branch) == "":
				return fmt.Errorf("%s.pull_request.branch is required: it is the identity that makes "+
					"a repeating task update its pull request instead of opening another one", where)
			case strings.TrimSpace(pr.Title) == "":
				return fmt.Errorf("%s.pull_request.title is required", where)
			case pr.Branch == pr.Base:
				return fmt.Errorf("%s.pull_request: branch and base are both %q", where, pr.Branch)
			case strings.HasPrefix(pr.Branch, "refs/"):
				return fmt.Errorf("%s.pull_request.branch is a branch name, not a ref: %q", where, pr.Branch)
			}
			for _, event := range t.On {
				if event == "pull_request" {
					// A task on a pull request opening pull requests is a loop
					// with a write credential in it.
					return fmt.Errorf("%s: a task routed to pull_request cannot open pull requests", where)
				}
			}
		}
		for _, pattern := range t.Keep {
			// The pattern comes from the repository, so this is reachable from
			// a pull request. The runtime check is the real one; this refuses
			// the obvious form at load time, where the message can explain
			// itself.
			if filepath.IsAbs(pattern) || strings.Contains(pattern, "..") {
				return fmt.Errorf("%s.keep: %q must stay inside the worktree", where, pattern)
			}
		}
		if filepath.IsAbs(t.Workdir) || strings.Contains(t.Workdir, "..") {
			// The worktree is the boundary. A task reaching outside it is
			// reaching into whatever else the box builds.
			return fmt.Errorf("%s.workdir must stay inside the worktree, got %q", where, t.Workdir)
		}
	}
	return nil
}

// validateServices refuses a service that cannot work.
func (p Pipeline) validateServices() error {
	for name, svc := range p.Services {
		where := fmt.Sprintf("services.%s", name)
		switch {
		case strings.TrimSpace(name) == "":
			return errors.New("services: a service needs a name")
		case strings.ContainsAny(name, " \t/"):
			return fmt.Errorf("%s: a service name becomes a container name and an environment "+
				"variable; keep it to letters, digits, dashes and underscores", where)
		case strings.TrimSpace(svc.Image) == "":
			return fmt.Errorf("%s.image is required", where)
		case svc.Port < 0 || svc.Port > 65535:
			return fmt.Errorf("%s.port %d is not a port", where, svc.Port)
		case svc.Ready != "" && svc.Port == 0:
			// Not fatal in principle, but it is always a mistake: a readiness
			// probe with nothing listening means the author expected an
			// address to be exported and will not get one.
			return fmt.Errorf("%s has ready: but no port: nothing would be exported to connect to", where)
		}
	}
	return nil
}

func (p Pipeline) validateNoDuplicateImages() error {
	seen := make(map[string]int, len(p.Publish))
	for i, a := range p.Publish {
		if a.Kind != KindImage {
			continue
		}
		if first, dup := seen[a.Image]; dup {
			return fmt.Errorf("publish[%d]: image %q is already published by publish[%d]", i, a.Image, first)
		}
		seen[a.Image] = i
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

func (a Artifact) validate(idx int) error {
	where := fmt.Sprintf("publish[%d]", idx)

	if a.Sign != "cosign" {
		return fmt.Errorf("%s.sign must be \"cosign\", got %q", where, a.Sign)
	}
	for _, event := range a.On {
		switch event {
		case "pull_request", "push", "tag":
		default:
			return fmt.Errorf("%s.on: unknown event %q (want pull_request, push or tag)", where, event)
		}
	}

	switch a.Kind {
	case KindImage:
		return a.validateImage(where)
	case KindBinaries:
		return a.validateBinaries(where)
	default:
		return fmt.Errorf("%s.kind: unknown artifact kind %q (want image or binaries)", where, a.Kind)
	}
}

// misplaced reports a field that belongs to the other kind. Silently ignoring
// it would let an operator write a setting that never takes effect — the same
// failure KnownFields exists to prevent, one level down.
func misplaced(where, field string, kind ArtifactKind) error {
	return fmt.Errorf("%s.%s does not apply to kind %q and would be ignored", where, field, kind)
}

func (a Artifact) validateImage(where string) error {
	if a.From != "" {
		return misplaced(where, "from", KindImage)
	}
	if a.Config != "" {
		return misplaced(where, "config", KindImage)
	}
	if strings.TrimSpace(a.Image) == "" {
		return fmt.Errorf("%s.image is required", where)
	}

	seen := make(map[Tag]bool, len(a.Tags))
	for _, t := range a.Tags {
		switch t {
		case TagSHA, TagLatest, TagSemver:
		default:
			return fmt.Errorf("%s.tags: unknown tag %q (want sha, latest or semver)", where, t)
		}
		if seen[t] {
			return fmt.Errorf("%s.tags: %q listed twice", where, t)
		}
		seen[t] = true
	}
	if !seen[TagSHA] {
		return fmt.Errorf(`%s.tags must include "sha": the immutable tag is what RollOps pins to`, where)
	}
	// A SHA-only tag set is rejected, not defaulted. RollOps' imagePolicy
	// watches a moving tag to discover new digests; with nothing but sha-<x>
	// tags there is no tag to watch, and the pipeline would build artifacts no
	// deploy could ever find.
	if !seen[TagLatest] && !seen[TagSemver] {
		return fmt.Errorf(
			`%s.tags is sha-only: add "latest" or "semver" — RollOps' imagePolicy cannot follow a moving target that never moves`, where)
	}
	if strings.TrimSpace(a.Dockerfile) == "" {
		return fmt.Errorf("%s.dockerfile must not be empty", where)
	}
	if strings.TrimSpace(a.Context) == "" {
		return fmt.Errorf("%s.context must not be empty", where)
	}
	return nil
}

func (a Artifact) validateBinaries(where string) error {
	for field, set := range map[string]bool{
		"keep":       a.Keep != nil,
		"image":      a.Image != "",
		"tags":       len(a.Tags) > 0,
		"platforms":  len(a.Platforms) > 0,
		"dockerfile": a.Dockerfile != "",
		"context":    a.Context != "",
		"args":       len(a.Args) > 0,
	} {
		if set {
			return misplaced(where, field, KindBinaries)
		}
	}
	// goreleaser owns cross-compilation, archives, checksums and the GitHub
	// Release. Kiln does not invent a second release language any more than it
	// invents a second check language — .goreleaser.yaml is where that lives.
	if a.From != "goreleaser" {
		return fmt.Errorf(
			"%s.from must be \"goreleaser\", got %q: .goreleaser.yaml is the only release language kiln speaks", where, a.From)
	}
	if strings.TrimSpace(a.Config) == "" {
		return fmt.Errorf("%s.config must not be empty", where)
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

// ArtifactsFor returns the artifacts to produce for an event.
//
// Two gates, both of which must pass: the event must route to publish at all
// (on.<event> lists publish), and the artifact must not exclude it. The second
// is what keeps a binary release on tags while an image builds on every push.
func (p Pipeline) ArtifactsFor(event string) []Artifact {
	if !p.Wants(event, StepPublish) {
		return nil
	}
	out := make([]Artifact, 0, len(p.Publish))
	for _, a := range p.Publish {
		if len(a.On) == 0 || slices.Contains(a.On, event) {
			out = append(out, a)
		}
	}
	return out
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
// PublishesOn reports whether this artifact is published for the given event.
// An empty On list means every publishing event, which is what the parser
// leaves for an image that did not name one.
func (a Artifact) PublishesOn(event string) bool {
	if len(a.On) == 0 {
		return true
	}
	for _, e := range a.On {
		if e == event {
			return true
		}
	}
	return false
}

// TagOnly reports an artifact that is only ever published from a tag. Planning
// one against a branch ref describes a build that cannot happen.
func (a Artifact) TagOnly() bool {
	return a.PublishesOn("tag") && !a.PublishesOn("push") && !a.PublishesOn("pull_request")
}

func (a Artifact) TagKinds() []Tag {
	out := slices.Clone(a.Tags)
	slices.Sort(out)
	return out
}

// PrunableImages lists the image repositories this pipeline publishes, with
// how many builds of each to retain locally.
func (p Pipeline) PrunableImages() map[string]int {
	out := map[string]int{}
	for _, a := range p.Publish {
		if a.Kind != KindImage || a.Image == "" {
			continue
		}
		keep := prune.DefaultKeep
		if a.Keep != nil {
			keep = *a.Keep
		}
		out[a.Image] = keep
	}
	return out
}

func boolPtr(b bool) *bool { return &b }

func intPtr(i int) *int { return &i }
