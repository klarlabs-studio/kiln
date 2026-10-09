package config

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Build-argument placeholders.
//
// `args` values are literal by design: a build whose output depends on the
// box's environment is not reproducible from the commit. A placeholder is the
// opposite of an environment lookup — it names a value DERIVED FROM THE COMMIT
// OR TAG KILN IS BUILDING, so the same commit always resolves the same way and
// the reproducibility claim holds.
//
// Only the `${kiln.<name>}` form is recognised. Everything else — `${HOME}`,
// `$PATH` — is a literal, passed to docker untouched. There is no escape
// syntax: no literal needs one, since a value that is not `${kiln.…}` is never
// rewritten.
const (
	placeholderSHA      = "kiln.sha"
	placeholderShortSHA = "kiln.short_sha"
	placeholderVersion  = "kiln.version"
	placeholderRef      = "kiln.ref"
)

// placeholderPattern matches any `${kiln.<anything>}` so an unknown name is
// reported at load time rather than shipped to docker as literal text.
var placeholderPattern = regexp.MustCompile(`\$\{(kiln\.[^}]*)\}`)

// ArgPlaceholders returns the known placeholder names, sorted.
func ArgPlaceholders() []string {
	names := []string{placeholderSHA, placeholderShortSHA, placeholderVersion, placeholderRef}
	sort.Strings(names)

	return names
}

func knownPlaceholder(name string) bool {
	for _, n := range ArgPlaceholders() {
		if n == name {
			return true
		}
	}

	return false
}

// validateArgPlaceholders rejects an unknown `${kiln.*}` name.
func validateArgPlaceholders(where string, args map[string]string) error {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		for _, m := range placeholderPattern.FindAllStringSubmatch(args[k], -1) {
			if !knownPlaceholder(m[1]) {
				return fmt.Errorf("%s.args.%s: unknown placeholder ${%s} (known: %s)",
					where, k, m[1], "${"+strings.Join(ArgPlaceholders(), "}, ${")+"}")
			}
		}
	}

	return nil
}

// ArgContext is what a placeholder resolves against: the commit and ref being
// built. Version is empty when the ref is not a semver tag.
type ArgContext struct {
	SHA      string
	ShortSHA string
	Version  string
	Ref      string
}

// ResolveArgs returns args with every `${kiln.*}` placeholder substituted. A
// map with no placeholders comes back equal. `${kiln.version}` with no version
// is an error: an image labelled with an empty or invented version is worse
// than a build that stops.
func ResolveArgs(args map[string]string, ctx ArgContext) (map[string]string, error) {
	if args == nil {
		return nil, nil
	}

	out := make(map[string]string, len(args))

	for k, v := range args {
		var err error

		out[k] = placeholderPattern.ReplaceAllStringFunc(v, func(m string) string {
			name := placeholderPattern.FindStringSubmatch(m)[1]

			switch name {
			case placeholderSHA:
				return ctx.SHA
			case placeholderShortSHA:
				return ctx.ShortSHA
			case placeholderRef:
				return ctx.Ref
			case placeholderVersion:
				if ctx.Version == "" && err == nil {
					err = fmt.Errorf("args.%s uses ${%s}, but ref %q is not a semver tag; "+
						"publish this artifact only on tag events (on: [tag])", k, name, ctx.Ref)
				}

				return ctx.Version
			}

			return m
		})
		if err != nil {
			return nil, err
		}
	}

	return out, nil
}
