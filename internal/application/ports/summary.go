package ports

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.klarlabs.de/kiln/internal/domain/run"
)

// TaskName is the check name for one task.
//
// One check per task rather than one for all of them, because a check is the
// unit branch protection can require and the unit a reader scans. "Kiln /
// Tasks failed" tells somebody to go read a log; "Kiln / sarif" tells them
// which thing broke without leaving the pull request.
func TaskName(task string) string { return "Kiln / " + task }

// TaskSummary renders the body of a task's check.
func TaskSummary(err error, tolerated bool, output string) (Conclusion, string, string) {
	body := strings.TrimSpace(output)
	if body != "" {
		body = "```\n" + body + "\n```"
	}

	switch {
	case err == nil:
		return ConclusionSuccess, "task passed", body
	case tolerated:
		// ConclusionNeutral, not failure: the pipeline was told this one may fail. A red
		// check for something the author declared advisory is how a wall of
		// red gets ignored.
		return ConclusionNeutral, "task failed (tolerated)", body
	default:
		return ConclusionFailure, "task failed", body
	}
}

// ProveSummary renders the body of the prove check.
func ProveSummary(skipped bool, reason string, err error) (Conclusion, string, string) {
	switch {
	case errors.Is(err, ErrToolMissing), errors.Is(err, ErrGateUnavailable):
		// Still a failure conclusion, and deliberately: a commit whose gate
		// never ran has not been gated, and ConclusionNeutral posts as a
		// *success* commit status — which would show an unverified commit
		// green. What changes is the claim, not the colour. "gate failed"
		// tells an author their change is bad; nothing looked at their change.
		return ConclusionFailure, "gate could not run",
			"```\n" + strings.TrimSpace(err.Error()) + "\n```"
	case err != nil:
		return ConclusionFailure, "gate failed", "```\n" + strings.TrimSpace(err.Error()) + "\n```"
	case skipped:
		// A skipped gate concludes `success`, not `skipped`: the commit *is*
		// gated — by the note Warden signed — and a branch protection rule
		// waiting on this check must be satisfied. The summary says how.
		return ConclusionSuccess, "gate satisfied by warden provenance", reason
	default:
		return ConclusionSuccess, "gate passed", reason
	}
}

// PublishSummary renders the body of the publish check.
//
// It lists every artifact the run produced, because a release that shipped an
// image and a set of binaries is one event, and splitting it across two checks
// would make branch protection wait on a name that does not always exist.
func PublishSummary(artifacts []run.Artifact, err error) (Conclusion, string, string) {
	if err != nil {
		return ConclusionFailure, "publish failed", "```\n" + strings.TrimSpace(err.Error()) + "\n```"
	}
	if len(artifacts) == 0 {
		return ConclusionNeutral, "nothing published", "No artifact was routed to this event."
	}

	var b strings.Builder
	allSigned := true
	for _, a := range artifacts {
		if !a.Signed {
			allSigned = false
		}
		switch a.Kind {
		case "binaries":
			fmt.Fprintf(&b, "**release `%s`** — checksums `%s`\n\n", a.Reference, a.Digest)
		default:
			fmt.Fprintf(&b, "**image** `%s`\n\n", a.Reference)
		}
		for _, name := range a.Names {
			fmt.Fprintf(&b, "- `%s`\n", name)
		}
		b.WriteString("\n")
	}

	if !allSigned {
		// A rehearsal must never read as a real artifact on a pull request page.
		b.WriteString("Dry run: nothing was pushed or signed.\n")
		return ConclusionNeutral, "dry run", b.String()
	}
	b.WriteString("Signed with cosign. RollOps can pin the image digest.\n")
	return ConclusionSuccess, summaryTitle(artifacts), b.String()
}

// summaryTitle names what was produced, so the check line is readable without
// opening it.
func summaryTitle(artifacts []run.Artifact) string {
	kinds := make([]string, 0, 2)
	for _, a := range artifacts {
		label := "image"
		if a.Kind == "binaries" {
			label = "binaries"
		}
		if !slices.Contains(kinds, label) {
			kinds = append(kinds, label)
		}
	}
	return "published and signed: " + strings.Join(kinds, " + ")
}
