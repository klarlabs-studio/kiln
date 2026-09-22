//go:build linux

package execx

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const landlockCreateRulesetVersion = 1 << 0

const abi1FS = unix.LANDLOCK_ACCESS_FS_EXECUTE |
	unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
	unix.LANDLOCK_ACCESS_FS_READ_FILE |
	unix.LANDLOCK_ACCESS_FS_READ_DIR |
	unix.LANDLOCK_ACCESS_FS_REMOVE_DIR |
	unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
	unix.LANDLOCK_ACCESS_FS_MAKE_CHAR |
	unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
	unix.LANDLOCK_ACCESS_FS_MAKE_REG |
	unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
	unix.LANDLOCK_ACCESS_FS_MAKE_FIFO |
	unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
	unix.LANDLOCK_ACCESS_FS_MAKE_SYM

type landlockRulesetAttr struct {
	AccessFS uint64
}

type landlockPathBeneath struct {
	AllowedAccess uint64
	ParentFD      int32
}

// LandlockAvailable reports that this kernel can create a Landlock ruleset.
func LandlockAvailable() bool {
	_, err := landlockABI()
	return err == nil
}

// LandlockABI is the kernel's Landlock ABI, or 0 if none.
func LandlockABI() int {
	n, err := landlockABI()
	if err != nil {
		return 0
	}
	return n
}

func landlockABI() (int, error) {
	r1, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, landlockCreateRulesetVersion)
	if errno != 0 {
		return 0, errno
	}
	return int(r1), nil
}

func applyLandlock(root string) error {
	if strings.TrimSpace(root) == "" {
		return fmt.Errorf("no confine root")
	}
	abi, err := landlockABI()
	if err != nil {
		return fmt.Errorf("landlock is not available: %w", err)
	}
	handled := uint64(abi1FS)
	if abi >= 2 {
		handled |= uint64(unix.LANDLOCK_ACCESS_FS_REFER)
	}
	if abi >= 3 {
		handled |= uint64(unix.LANDLOCK_ACCESS_FS_TRUNCATE)
	}
	if abi >= 4 {
		handled |= uint64(unix.LANDLOCK_ACCESS_FS_IOCTL_DEV)
	}

	attr := landlockRulesetAttr{AccessFS: handled}
	fd, _, errno := unix.Syscall(
		unix.SYS_LANDLOCK_CREATE_RULESET,
		uintptr(unsafe.Pointer(&attr)),
		unsafe.Sizeof(attr),
		0,
	)
	if errno != 0 {
		return fmt.Errorf("landlock_create_ruleset: %w", errno)
	}
	ruleset := int(fd)
	defer func() { _ = unix.Close(ruleset) }()

	rw := handled
	ro := handled & uint64(unix.LANDLOCK_ACCESS_FS_EXECUTE|
		unix.LANDLOCK_ACCESS_FS_READ_FILE|
		unix.LANDLOCK_ACCESS_FS_READ_DIR|
		unix.LANDLOCK_ACCESS_FS_REFER|
		unix.LANDLOCK_ACCESS_FS_IOCTL_DEV)

	if err := addPath(ruleset, root, rw); err != nil {
		return fmt.Errorf("grant worktree %s: %w", root, err)
	}
	for _, p := range toolchainPaths() {
		_ = addPath(ruleset, p, ro)
	}
	if cmd := os.Getenv(confineCmdEnv); cmd != "" {
		_ = addPath(ruleset, filepath.Dir(cmd), ro)
	}

	return restrictSelf(ruleset)
}

// restrictSelf installs no_new_privs and the Landlock domain on every
// Go OS thread. restrict_self is per-thread; applying it to one thread
// and then execve from another would start an unconfined child.
func restrictSelf(ruleset int) error {
	if err := allThreadsOrThis(unix.SYS_PRCTL, unix.PR_SET_NO_NEW_PRIVS, 1, 0, "PR_SET_NO_NEW_PRIVS"); err != nil {
		return err
	}
	return allThreadsOrThis(unix.SYS_LANDLOCK_RESTRICT_SELF, uintptr(ruleset), 0, 0, "landlock_restrict_self")
}

func allThreadsOrThis(trap, a1, a2, a3 uintptr, name string) error {
	if _, _, errno := syscall.AllThreadsSyscall(trap, a1, a2, a3); errno == 0 {
		return nil
	} else if errno != syscall.ENOTSUP {
		return fmt.Errorf("%s: %w", name, errno)
	}
	if _, _, errno := unix.RawSyscall(trap, a1, a2, a3); errno != 0 {
		return fmt.Errorf("%s: %w", name, errno)
	}
	return nil
}

func addPath(ruleset int, path string, access uint64) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		path = filepath.Dir(path)
	}
	fd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()

	rule := landlockPathBeneath{AllowedAccess: access, ParentFD: int32(fd)}
	_, _, errno := unix.Syscall6(
		unix.SYS_LANDLOCK_ADD_RULE,
		uintptr(ruleset),
		unix.LANDLOCK_RULE_PATH_BENEATH,
		uintptr(unsafe.Pointer(&rule)),
		0, 0, 0,
	)
	if errno != 0 {
		return errno
	}
	return nil
}

func toolchainPaths() []string {
	out := []string{"/usr", "/lib", "/lib64", "/bin", "/sbin", "/etc", "/opt", "/dev", "/proc", "/sys", "/usr/local"}
	for _, key := range []string{"GOROOT", "GOMODCACHE", "GOCACHE", "GOPATH"} {
		if v := os.Getenv(key); v != "" {
			out = append(out, v)
		}
	}
	return out
}
