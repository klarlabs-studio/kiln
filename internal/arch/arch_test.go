// Package arch holds the architecture test. It has no production code: its
// only job is to fail the build when a layer reaches somewhere it must not.
//
// Without this the layering is a convention, and a convention that nothing
// checks is a comment. Every import added from here on is checked against the
// dependency rule below, so the architecture cannot rot quietly between
// reviews.
package arch

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// modulePath is the import prefix every internal package shares.
const modulePath = "go.klarlabs.de/kiln/internal/"

// layers are matched longest-prefix-first against a package's path under
// internal/, so "domain/run" resolves to the domain layer.
//
// The rule is the classic one: a layer may import inward, never outward.
// domain is the centre and depends on nothing but the standard library.
// The pseudo-layer for packages the migration has not placed yet. A layer that
// lists it is still being moved; a layer that does not is finished, and the
// list is therefore a visible record of what is left rather than a comment
// that goes stale.
const unassigned = "unassigned"

var layers = []struct {
	prefix string
	name   string
	// mayImport lists the layers this one is allowed to reach. A layer may
	// always import itself.
	mayImport []string
}{
	// The centre. Nothing internal, not even a logger: a rule with one
	// exception is a rule nobody can apply without asking.
	{"domain/", "domain", nil},
	// Adapters. They speak the domain's language to the outside world, so
	// they may know the domain and never the other way round.
	{"infrastructure/", "infrastructure", []string{"domain", unassigned}},
	// Delivery: the CLI, the MCP server, the daemon. Outermost, so it may
	// reach anything — its job is to assemble, not to decide.
	{"interfaces/", "interfaces", []string{"domain", "application", "infrastructure", unassigned}},
}

func layerOf(pkg string) (string, bool) {
	best, name := -1, ""
	for _, l := range layers {
		if strings.HasPrefix(pkg, l.prefix) && len(l.prefix) > best {
			best, name = len(l.prefix), l.name
		}
	}
	return name, best >= 0
}

func mayImport(from, to string) bool {
	if from == to {
		return true
	}
	for _, l := range layers {
		if l.name != from {
			continue
		}
		for _, allowed := range l.mayImport {
			if allowed == to {
				return true
			}
		}
	}
	return false
}

// TestTheDependencyRuleHolds walks every .go file under internal/ and checks
// each internal import against the rule.
func TestTheDependencyRuleHolds(t *testing.T) {
	root := ".."

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}

		pkg := filepath.ToSlash(filepath.Dir(strings.TrimPrefix(path, root+string(filepath.Separator))))
		from, known := layerOf(pkg + "/")
		if !known {
			// The importing package has no layer yet, so there is no rule to
			// check it against. The migration lands one layer at a time, and a
			// rule that failed on all thirty packages at once could never be
			// introduced.
			return nil
		}

		file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if parseErr != nil {
			return parseErr
		}
		for _, spec := range file.Imports {
			imported, unquoteErr := strconv.Unquote(spec.Path.Value)
			if unquoteErr != nil {
				return unquoteErr
			}
			if !strings.HasPrefix(imported, modulePath) {
				continue // standard library or a third-party module
			}
			target := strings.TrimPrefix(imported, modulePath) + "/"
			to, targetKnown := layerOf(target)
			if !targetKnown {
				to = unassigned
			}
			if !mayImport(from, to) {
				t.Errorf("%s (%s) imports %s (%s):\n"+
					"\tthe dependency rule points inward; %s may only import %v",
					path, from, imported, to, from, allowedFor(from))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func allowedFor(name string) []string {
	for _, l := range layers {
		if l.name == name {
			if len(l.mayImport) == 0 {
				return []string{"the standard library"}
			}
			return l.mayImport
		}
	}
	return nil
}
