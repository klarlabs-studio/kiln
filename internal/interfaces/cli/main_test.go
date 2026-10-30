package cli

import (
	"os"
	"testing"
)

// TestMain isolates the package from the machine it runs on.
//
// Every command boots, and boot claims <user cache>/kiln/go-build. Tests that
// build their own repository rather than going through the hermetic helper
// would otherwise create and mark that directory on the developer's machine.
// The helper still points KILN_GOCACHE at a temporary directory, so the wiring
// is exercised; this is only the floor beneath it.
//
// Every command also takes a machine build slot. A private slot directory
// keeps the suite from sharing the real ones and, on a box that is building,
// queueing behind it for the full wait before failing.
func TestMain(m *testing.M) {
	if err := os.Setenv("KILN_GOCACHE", "off"); err != nil {
		panic(err)
	}
	dir, err := os.MkdirTemp("", "kiln-cli-slots-")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("KILN_SLOT_DIR", dir); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
