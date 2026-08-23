// Package binpath answers "which path should outlive this build?".
//
// Two things kiln writes are expected to survive an upgrade: the program named
// in a box's schedule, and the binary on a keychain item's trusted-application
// list. Both were written by resolving os.Executable() through EvalSymlinks,
// which is exactly backwards — under Homebrew that resolves the stable
// /opt/homebrew/bin/kiln into Caskroom/kiln/<version>/kiln, and `brew upgrade`
// deletes the directory.
//
// The schedule went silently dead; the keychain entry stopped being readable
// by a background job. Same cause, two symptoms, so one answer lives here
// rather than being written twice and drifting.
package binpath

import (
	"os"
	"os/exec"
	"path/filepath"
)

// Stable is the path to name for something that must outlive this build.
//
// It is the name a package manager repoints on upgrade, borrowed only when it
// resolves to the binary actually running — otherwise a box installed from a
// local build would be quietly attached to whatever kiln is on PATH, which is
// a different bug with the same shape.
func Stable(exe string) string {
	onPath, err := exec.LookPath(filepath.Base(exe))
	if err != nil {
		return exe
	}
	if SameFile(onPath, exe) {
		return onPath
	}
	return exe
}

// SameFile reports that two paths reach the same binary, following symlinks.
func SameFile(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}
