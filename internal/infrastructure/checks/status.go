package checks

import (
	"context"
	"fmt"

	"go.klarlabs.de/kiln/internal/application/ports"
	"go.klarlabs.de/kiln/internal/infrastructure/obs"
)

// StatusPoster writes a commit status. GitHub, Gitea and Forgejo all
// accept the same four states; only GitHub also has a Checks API.
type StatusPoster interface {
	Enabled() bool
	CreateStatus(ctx context.Context, sha, state, context, description string) error
}

// Statuses reports phases as commit statuses. This is the reporter a
// Gitea or Forgejo box gets, and the fallback a GitHub PAT already uses
// after the Checks API refuses it.
type Statuses struct {
	Poster StatusPoster
	Log    ports.Logger
}

// NewStatuses builds a statuses-only reporter.
func NewStatuses(p StatusPoster, log ports.Logger) *Statuses {
	if log == nil {
		log = obs.Discard()
	}
	return &Statuses{Poster: p, Log: log}
}

func (s *Statuses) Start(ctx context.Context, name, sha string) error {
	if s.Poster == nil || !s.Poster.Enabled() {
		return nil
	}
	if err := s.Poster.CreateStatus(ctx, sha, "pending", name, "running"); err != nil {
		return fmt.Errorf("checks: status start %q: %w", name, err)
	}
	return nil
}

func (s *Statuses) Complete(ctx context.Context, name, sha string, c ports.Conclusion, title, _ string) error {
	if s.Poster == nil || !s.Poster.Enabled() {
		return nil
	}
	if err := s.Poster.CreateStatus(ctx, sha, statusState(c), name, title); err != nil {
		return fmt.Errorf("checks: status complete %q: %w", name, err)
	}
	s.Log.Debug("status posted", "check", name, "sha", sha, "state", statusState(c))
	return nil
}
