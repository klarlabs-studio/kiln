package service

import (
	"context"

	"go.klarlabs.de/kiln/internal/application/ports"
	"go.klarlabs.de/kiln/internal/domain/config"
)

// Start satisfies ports.Services.
//
// It returns the interface rather than *Set so a nil set arrives as a nil
// interface: the application defers Stop before it knows whether anything
// started, and a typed nil would satisfy the interface and then be called.
func (r *Runner) Start(ctx context.Context, services map[string]config.Service, runID string) (ports.ServiceSet, error) {
	set, err := r.startAll(ctx, services, runID)
	if set == nil {
		return nil, err
	}
	return set, err
}
