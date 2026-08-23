// Package policyfile reads a verification policy off disk.
//
// The domain parses a policy from an io.Reader and validates it; finding the
// file is this package's job, so the model itself never touches a filesystem.
package policyfile

import (
	"fmt"
	"os"
	"path/filepath"

	"go.klarlabs.de/kiln/internal/domain/policy"
)

// Load reads a policy from disk.
func Load(path string) (policy.Policy, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return policy.Policy{}, fmt.Errorf("policy: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	p, err := policy.Parse(f)
	if err != nil {
		return policy.Policy{}, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}
