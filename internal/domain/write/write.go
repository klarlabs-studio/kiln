// Package write is the repository-write capability kiln is willing to exercise.
//
// A task may propose a change. It may not rewrite source-of-truth. The
// destination of a force-push is therefore not an arbitrary string: it is a
// branch kiln owns, under the kiln/ namespace, and that fact is established
// here before any git command runs.
package write

import (
	"fmt"
	"path"
	"strings"
)

// Namespace is the only prefix a proposal branch may use.
const Namespace = "kiln/"

// Owned reports that branch is a kiln-controlled proposal destination.
//
// The check is structural, not conventional. Configuration that names main,
// the watched branch, or anything else outside this namespace is refused
// rather than trusted to "usually" be a side branch.
func Owned(branch string) error {
	cleaned := path.Clean(strings.TrimSpace(branch))
	if cleaned == "" || cleaned == "." {
		return fmt.Errorf("proposal branch is required")
	}
	if strings.HasPrefix(cleaned, "/") || strings.Contains(cleaned, ":") {
		return fmt.Errorf("proposal branch %q is not a branch name", branch)
	}
	if !strings.HasPrefix(cleaned, Namespace) {
		return fmt.Errorf("proposal branch %q must start with %s: kiln may replace its own branches, not source-of-truth refs",
			branch, Namespace)
	}
	rest := strings.TrimPrefix(cleaned, Namespace)
	if rest == "" || rest == "." {
		return fmt.Errorf("proposal branch %q needs a name after %s", branch, Namespace)
	}
	return nil
}
