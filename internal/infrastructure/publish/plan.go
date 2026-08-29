package publish

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"go.klarlabs.de/kiln/internal/domain/config"
)

// Plan is the set of references a publish will produce, resolved before
// anything is built. `kiln doctor` prints one without running docker, and the
// publisher builds from one, so what an operator is shown and what actually
// happens cannot drift.
type Plan struct {
	// Image is the repository, e.g. ghcr.io/felixgeelhaar/glossa-api.
	Image string
	// SHATag is the immutable reference RollOps pins to.
	SHATag string
	// MovingTags are the references RollOps' imagePolicy follows.
	MovingTags []string
	// Platforms is the target list, defaulted to linux/amd64.
	Platforms []string
	// Dockerfile and Context are relative to the checked-out tree.
	Dockerfile string
	Context    string
	// Args are the build arguments, as explicit key=value pairs.
	Args map[string]string
	// Secrets are BuildKit build secrets, as id -> environment variable name.
	// The VALUES are never held here: the plan is printed to operators and
	// folded into provenance, and a credential must survive neither.
	Secrets map[string]string
	// Notes records decisions worth telling the operator about — a semver tag
	// that could not be derived, a tag character that had to be rewritten.
	Notes []string
}

// BuildArgFlags renders the build arguments as docker flags, sorted by name.
//
// Sorted because the plan is shown to an operator and folded into provenance:
// map iteration order would make two identical builds render differently and
// produce different-looking attestations for the same inputs.
func (p Plan) BuildArgFlags() []string {
	if len(p.Args) == 0 {
		return nil
	}
	keys := make([]string, 0, len(p.Args))
	for k := range p.Args {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]string, 0, 2*len(keys))
	for _, k := range keys {
		out = append(out, "--build-arg", k+"="+p.Args[k])
	}
	return out
}

// SecretFlags renders the build secrets as docker flags, sorted by id.
//
// Sorted for the same reason build args are: the plan is shown to an operator
// and folded into provenance, and map iteration order would make two identical
// builds render differently.
//
// `env=` rather than `src=`: the value is passed from kiln's own environment to
// BuildKit, never written to a file that another process on the box could read
// and no cleanup could be trusted to remove.
func (p Plan) SecretFlags() []string {
	if len(p.Secrets) == 0 {
		return nil
	}

	ids := make([]string, 0, len(p.Secrets))
	for id := range p.Secrets {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	out := make([]string, 0, 2*len(ids))
	for _, id := range ids {
		out = append(out, "--secret", "id="+id+",env="+p.Secrets[id])
	}

	return out
}

// SecretIDs returns the configured secret ids, sorted, for the operator-facing
// plan summary. Never the values: the plan is printed.
func (p Plan) SecretIDs() []string {
	ids := make([]string, 0, len(p.Secrets))
	for id := range p.Secrets {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	return ids
}

// Refs returns every fully qualified reference, sha tag first.
func (p Plan) Refs() []string {
	out := make([]string, 0, 1+len(p.MovingTags))
	out = append(out, p.SHATag)
	out = append(out, p.MovingTags...)
	return out
}

// String renders the plan for `kiln doctor` and dry runs.
func (p Plan) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "image %s\n", p.Image)
	for _, ref := range p.Refs() {
		fmt.Fprintf(&b, "  tag  %s\n", ref)
	}
	fmt.Fprintf(&b, "  from %s (context %s) for %s\n",
		p.Dockerfile, p.Context, strings.Join(p.Platforms, ","))
	if flags := p.BuildArgFlags(); len(flags) > 0 {
		pairs := make([]string, 0, len(flags)/2)
		for i := 1; i < len(flags); i += 2 {
			pairs = append(pairs, flags[i])
		}
		fmt.Fprintf(&b, "  args %s\n", strings.Join(pairs, " "))
	}
	if ids := p.SecretIDs(); len(ids) > 0 {
		// Ids only. The whole point of a secret is that its value does not
		// appear in the things a build leaves behind, and the plan is printed.
		fmt.Fprintf(&b, "  secrets %s\n", strings.Join(ids, " "))
	}
	for _, n := range p.Notes {
		fmt.Fprintf(&b, "  note %s\n", n)
	}
	return b.String()
}

// secretEnvs resolves each configured secret to the environment variable it
// reads from. The config loader has already rejected any other form.
func secretEnvs(cfg config.Artifact) map[string]string {
	if len(cfg.Secrets) == 0 {
		return nil
	}

	out := make(map[string]string, len(cfg.Secrets))

	for id := range cfg.Secrets {
		if name, ok := cfg.SecretEnv(id); ok {
			out[id] = name
		}
	}

	return out
}

// semverRef matches the tag names Kiln will turn into a version tag. Build
// metadata is accepted here and rewritten below, because `+` is legal in
// semver and illegal in a Docker tag.
var semverRef = regexp.MustCompile(`^v?\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)

// BuildPlan resolves the tag plan for one commit.
//
// The invariant it enforces is the publish contract with RollOps: every
// successful publish produces an immutable sha tag *and* at least one moving
// tag. The config loader already rejects a sha-only tag list, but a list of
// [sha, semver] on a branch push would produce the same dead end at runtime,
// so it is checked again here where the ref is actually known.
func BuildPlan(cfg config.Artifact, sha, ref string) (Plan, error) {
	if strings.TrimSpace(cfg.Image) == "" {
		return Plan{}, errors.New("publish: no image configured")
	}
	if strings.TrimSpace(sha) == "" {
		return Plan{}, errors.New("publish: no commit to tag")
	}

	plan := Plan{
		Image:      cfg.Image,
		Platforms:  cfg.Platforms,
		Dockerfile: cfg.Dockerfile,
		Context:    cfg.Context,
		Args:       cfg.Args,
		Secrets:    secretEnvs(cfg),
	}
	if len(plan.Platforms) == 0 {
		plan.Platforms = []string{"linux/amd64"}
	}
	if plan.Dockerfile == "" {
		plan.Dockerfile = "Dockerfile"
	}
	if plan.Context == "" {
		plan.Context = "."
	}

	for _, kind := range cfg.TagKinds() {
		switch kind {
		case config.TagSHA:
			plan.SHATag = cfg.Image + ":sha-" + shortSHA(sha)
		case config.TagLatest:
			plan.MovingTags = append(plan.MovingTags, cfg.Image+":latest")
		case config.TagSemver:
			name, note := semverTag(ref)
			if note != "" {
				plan.Notes = append(plan.Notes, note)
			}
			if name != "" {
				plan.MovingTags = append(plan.MovingTags, cfg.Image+":"+name)
			}
		}
	}

	if plan.SHATag == "" {
		return Plan{}, errors.New(`publish: tag plan has no "sha" tag; RollOps has nothing immutable to pin`)
	}
	if len(plan.MovingTags) == 0 {
		return Plan{}, fmt.Errorf(
			"publish: tag plan for ref %q produces no moving tag — RollOps' imagePolicy would never see this build; "+
				"add \"latest\" to publish.tags, or publish semver only on tag events", ref)
	}
	return plan, nil
}

// semverTag derives a version tag from a ref, returning the tag and an
// explanatory note. An empty tag with a note means "this ref has no version",
// which is the normal case for a branch push.
func semverTag(ref string) (tag, note string) {
	name := strings.TrimPrefix(ref, "refs/tags/")
	if name == ref || name == "" {
		// Not a tag ref at all.
		return "", fmt.Sprintf("no semver tag: ref %q is not a tag", ref)
	}
	if !semverRef.MatchString(name) {
		return "", fmt.Sprintf("no semver tag: %q is not a version", name)
	}
	// Docker tags forbid '+', which semver uses for build metadata. Rewriting
	// to '_' is the convention OCI tooling settled on; silently dropping the
	// metadata would make two different builds share a tag.
	if strings.Contains(name, "+") {
		rewritten := strings.ReplaceAll(name, "+", "_")
		return rewritten, fmt.Sprintf("semver build metadata rewritten for OCI: %s → %s", name, rewritten)
	}
	return name, ""
}

func shortSHA(sha string) string {
	if len(sha) <= 7 {
		return sha
	}
	return sha[:7]
}
