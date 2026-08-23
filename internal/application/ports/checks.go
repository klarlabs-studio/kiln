package ports

import (
	"context"
)

// Conclusion is a completed check's verdict, in GitHub's vocabulary.
type Conclusion string

// Reporter posts check runs.
//
// Every method is best-effort from the engine's point of view: a run that
// built and signed a correct artifact must not be recorded as failed because
// GitHub was unreachable while Kiln tried to say so. The engine logs reporting
// errors and carries on.
type Reporter interface {
	// Start opens an in-progress check run for a phase.
	Start(ctx context.Context, name, sha string) error
	// Complete closes it with a verdict.
	Complete(ctx context.Context, name, sha string, conclusion Conclusion, title, summary string) error
}

// NoopReporter reports nowhere.
//
// This is what a run without GITHUB_TOKEN gets. It is a deliberate, quiet
// degradation: `kiln run` on a laptop should gate a commit and print the
// result, not fail because it could not tell GitHub about it.
type NoopReporter struct{}

const (
	ConclusionSuccess Conclusion = "success"
	ConclusionFailure Conclusion = "failure"
	ConclusionNeutral Conclusion = "neutral"
	ConclusionSkipped Conclusion = "skipped"
)

const (
	NameProve   = "Kiln / Prove"
	NamePublish = "Kiln / Publish"
)

func (NoopReporter) Start(context.Context, string, string) error { return nil }

func (NoopReporter) Complete(context.Context, string, string, Conclusion, string, string) error {
	return nil
}
