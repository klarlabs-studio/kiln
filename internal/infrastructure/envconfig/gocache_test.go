package envconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.klarlabs.de/kiln/internal/infrastructure/gocache"
)

func TestGoCacheDefaultsToAKilnDirectory(t *testing.T) {
	t.Setenv("KILN_GOCACHE", "")
	t.Setenv("GOCACHE", "")

	env := Load()

	base, err := os.UserCacheDir()
	if err != nil {
		t.Skip("no user cache directory on this machine")
	}
	if want := filepath.Join(base, "kiln", "go-build"); env.GoCache != want {
		t.Errorf("GoCache = %q, want %q", env.GoCache, want)
	}
	if env.GoCacheMaxSize != gocache.DefaultMaxSize {
		t.Errorf("GoCacheMaxSize = %d, want the default", env.GoCacheMaxSize)
	}
}

func TestAnOperatorGOCACHEDisablesKilnsCache(t *testing.T) {
	t.Setenv("KILN_GOCACHE", "")
	t.Setenv("GOCACHE", "/srv/go-cache")
	if got := Load().GoCache; got != "" {
		t.Errorf("GoCache = %q, want empty", got)
	}
}

func TestKilnGoCacheOffDisablesIt(t *testing.T) {
	t.Setenv("KILN_GOCACHE", "off")
	t.Setenv("GOCACHE", "")
	if got := Load().GoCache; got != "" {
		t.Errorf("GoCache = %q, want empty", got)
	}
}

func TestARelativeKilnGoCacheIsRefusedAtBoot(t *testing.T) {
	t.Setenv("KILN_GOCACHE", "relative/go-build")
	t.Setenv("GOCACHE", "")
	err := Load().Validate()
	if err == nil || !strings.Contains(err.Error(), "KILN_GOCACHE") {
		t.Errorf("Validate = %v, want KILN_GOCACHE named", err)
	}
}

func TestGoCacheMaxSizeIsParsed(t *testing.T) {
	for in, want := range map[string]int64{
		"20GiB": 20 << 30, "512MiB": 512 << 20, "2G": 2 << 30, "1048576": 1 << 20,
		"0": 0, "3 GiB": 3 << 30, "64KiB": 64 << 10, "1TiB": 1 << 40,
	} {
		t.Setenv("KILN_GOCACHE_MAX_SIZE", in)
		if got := Load().GoCacheMaxSize; got != want {
			t.Errorf("KILN_GOCACHE_MAX_SIZE=%q → %d, want %d", in, got, want)
		}
	}
}

// A typo must not silently remove the cap — the same rule as the timeouts.
func TestAnUnparsableGoCacheMaxSizeKeepsTheDefault(t *testing.T) {
	for _, bad := range []string{"lots", "-5GiB", "10XB", "GiB"} {
		t.Setenv("KILN_GOCACHE_MAX_SIZE", bad)
		if got := Load().GoCacheMaxSize; got != gocache.DefaultMaxSize {
			t.Errorf("KILN_GOCACHE_MAX_SIZE=%q → %d, want the default", bad, got)
		}
	}
}
