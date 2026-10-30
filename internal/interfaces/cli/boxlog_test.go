package cli

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.klarlabs.de/kiln/internal/boot"
	"go.klarlabs.de/kiln/internal/infrastructure/logrotate"
	"go.klarlabs.de/kiln/internal/infrastructure/obs"
	"go.klarlabs.de/kiln/internal/infrastructure/store"
)

// The watcher derives the log it rotates from the repository rather than
// being told it by the unit, so the two must agree on the path — or the
// rotation trims a file nobody writes while box.log keeps growing. Deriving
// it is also what lets boxes installed before rotation existed benefit
// without reinstalling.
func TestTheWatcherRotatesTheFileTheInstalledUnitWritesTo(t *testing.T) {
	repo := t.TempDir()
	deps := &boot.Deps{
		Dir:   repo,
		Store: store.NewFile(filepath.Join(repo, ".kiln", "runs.json")),
		Log:   obs.Discard(),
	}

	rot, ok := newWatcher(deps, false).Logs.(*logrotate.Rotator)
	if !ok {
		t.Fatalf("watcher Logs = %T, want a *logrotate.Rotator", newWatcher(deps, false).Logs)
	}

	for _, goos := range []string{"darwin", "linux"} {
		a := &agent{repo: repo, every: 30 * time.Minute, exe: "/bin/kiln", goos: goos}
		unit := a.plist()
		if goos == "linux" {
			unit = a.service()
		}
		if !strings.Contains(unit, rot.Path) {
			t.Errorf("%s unit does not write to %s, the file the watcher rotates:\n%s", goos, rot.Path, unit)
		}
	}
}
