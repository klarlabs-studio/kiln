package cli

import (
	"os"
	"testing"
)

// TestMain turns kiln's Go build cache off for the whole package.
//
// Every command boots, and boot claims <user cache>/kiln/go-build. Tests that
// build their own repository rather than going through the hermetic helper
// would otherwise create and mark that directory on the developer's machine.
// The helper still points KILN_GOCACHE at a temporary directory, so the wiring
// is exercised; this is only the floor beneath it.
func TestMain(m *testing.M) {
	if err := os.Setenv("KILN_GOCACHE", "off"); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}
