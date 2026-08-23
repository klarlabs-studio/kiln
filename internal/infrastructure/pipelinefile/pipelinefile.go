// Package pipelinefile reads a pipeline off disk.
//
// It exists so the domain does not. config.Parse takes an io.Reader and knows
// nothing about files; opening one, and deciding what a missing one means, is
// an adapter's job.
package pipelinefile

import (
	"fmt"
	"os"
	"path/filepath"

	"go.klarlabs.de/kiln/internal/domain/config"
)

// LoadDir reads `.kiln.yaml` from a repository root. A missing file returns
// config.ErrNotFound alongside config.Default(), so a caller that does not care about the
// distinction can ignore the error and still get a usable pipeline.
func LoadDir(dir string) (config.Pipeline, error) {
	return LoadFile(filepath.Join(dir, config.FileName))
}

// LoadFile reads a pipeline from an explicit path.
func LoadFile(path string) (config.Pipeline, error) {
	f, err := os.Open(path) //nolint:gosec // operator-supplied pipeline path
	if err != nil {
		if os.IsNotExist(err) {
			return config.Default(), fmt.Errorf("%w at %s", config.ErrNotFound, path)
		}
		return config.Pipeline{}, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	p, err := config.Parse(f)
	if err != nil {
		return config.Pipeline{}, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}
