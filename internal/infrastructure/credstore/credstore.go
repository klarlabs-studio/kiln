// Package credstore keeps kiln's GitHub token where the operating system
// keeps secrets.
//
// The alternative is what every self-hosted CI guide tells you to do: put a
// token in a file, or worse, in the cron line itself. That token is long-lived
// and can write to your repositories, and it ends up in a backup, a shell
// history, or a screen share. A scheduled job needs the credential without a
// human present, which is exactly the situation the platform keychains exist
// for.
//
// The fallback is a 0600 file, because a box without a keychain is still a box
// and refusing to run there would push people back to the cron line. When that
// happens kiln says so rather than pretending the secret is protected.
package credstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"go.klarlabs.de/kiln/internal/infrastructure/execx"
)

// Service and account name the entry in whichever store is used. Stable
// strings: changing them orphans everybody's stored token.
const (
	Service = "de.klarlabs.kiln"
	Account = "github-token"
)

// ErrNotFound reports that no token has been stored.
var ErrNotFound = errors.New("no stored token")

// ErrKeychainBlocked reports that the keychain read timed out, which in
// practice means it is waiting on a permission dialog. Separate from
// ErrNotFound because the remedy is different — there IS a token, something is
// standing between the process and it.
var ErrKeychainBlocked = errors.New("keychain did not answer")

// Store reads and writes the token.
type Store struct {
	// Runner shells out to the platform keychain tool.
	Runner execx.Runner
	// Dir overrides where the file fallback lives, for tests.
	Dir string
	// GOOS overrides the platform, for tests.
	GOOS string
}

// New builds a store.
func New(r execx.Runner) *Store { return &Store{Runner: r} }

// Kind describes where a token is being kept, so `kiln login` and
// `kiln doctor` can tell the operator the truth about it.
type Kind string

const (
	// KindKeychain is the macOS login keychain.
	KindKeychain Kind = "macOS keychain"
	// KindSecretService is the freedesktop secret service, via secret-tool.
	KindSecretService Kind = "secret service"
	// KindFile is the 0600 fallback.
	KindFile Kind = "file"
)

// Set stores the token and reports where it went.
func (s *Store) Set(ctx context.Context, token string) (Kind, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", errors.New("credstore: refusing to store an empty token")
	}

	switch s.platform() {
	case "darwin":
		if err := s.keychainSet(ctx, token); err == nil {
			return KindKeychain, nil
		}
	case "linux":
		if err := s.secretToolSet(ctx, token); err == nil {
			return KindSecretService, nil
		}
	}
	if err := s.fileSet(token); err != nil {
		return "", err
	}
	return KindFile, nil
}

// Get returns the stored token and where it came from.
func (s *Store) Get(ctx context.Context) (string, Kind, error) {
	switch s.platform() {
	case "darwin":
		token, err := s.keychainGet(ctx)
		if err == nil && token != "" {
			return token, KindKeychain, nil
		}
		// A blocked keychain is reported, not swallowed: falling through to
		// "no token" would leave a box quietly treating every pull request as
		// a fork and posting no checks, with nothing said about why.
		if errors.Is(err, ErrKeychainBlocked) {
			return "", "", err
		}
	case "linux":
		if token, err := s.secretToolGet(ctx); err == nil && token != "" {
			return token, KindSecretService, nil
		}
	}

	token, err := s.fileGet()
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "", "", ErrNotFound
	case err != nil:
		return "", "", err
	case token == "":
		return "", "", ErrNotFound
	}
	return token, KindFile, nil
}

// Delete removes the stored token from wherever it is.
func (s *Store) Delete(ctx context.Context) error {
	switch s.platform() {
	case "darwin":
		_, _ = s.Runner.Run(ctx, execx.Cmd{
			Name: "security",
			Args: []string{"delete-generic-password", "-s", Service, "-a", Account},
		})
	case "linux":
		_, _ = s.Runner.Run(ctx, execx.Cmd{
			Name: "secret-tool",
			Args: []string{"clear", "service", Service, "account", Account},
		})
	}
	if err := os.Remove(s.filePath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("credstore: remove %s: %w", s.filePath(), err)
	}
	return nil
}

func (s *Store) platform() string {
	if s.GOOS != "" {
		return s.GOOS
	}
	return runtime.GOOS
}

func (s *Store) keychainSet(ctx context.Context, token string) error {
	// -U updates in place, so logging in twice replaces rather than erroring
	// or, worse, leaving two entries and reading the older one.
	//
	// -w takes the secret as an argument, which puts it in this process's
	// argv. That is visible to other processes on the machine for the
	// lifetime of the call; the alternative interactive mode cannot be
	// scripted. It is a local, momentary exposure against a token that is
	// otherwise about to be written to disk in the fallback path.
	_, err := s.Runner.Run(ctx, execx.Cmd{
		Name: "security",
		Args: append([]string{
			"add-generic-password", "-U",
			"-s", Service, "-a", Account,
			"-l", "kiln GitHub token",
		}, append(s.trustedApps(), "-w", token)...),
	})
	return err
}

// trustedApps puts the kiln binary on the keychain item's access list.
//
// Without it, the first read from a background schedule pops a "kiln wants to
// use your confidential information" dialog — on a machine nobody is watching,
// which means the tick hangs until somebody notices a prompt behind their
// windows. -T names the binary allowed to read the item without asking.
//
// It is bound to the binary's path, so a kiln installed somewhere new needs
// `kiln login` again. That is the same trade the schedule's PATH makes, and
// the same reason: a comprehensible failure beats a silent one.
func (s *Store) trustedApps() []string {
	exe, err := os.Executable()
	if err != nil {
		return nil
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return []string{"-T", exe}
}

// keychainReadTimeout bounds the read.
//
// A keychain lookup that is going to succeed returns in milliseconds. One that
// is going to prompt never returns at all — `security` sits waiting for a
// dialog, and on a schedule nobody is watching, that is a tick that hangs
// forever rather than a box that reports something.
//
// Observed exactly that: a box's first tick blocked indefinitely on
// `security find-generic-password`, having produced no output. The -T access
// list below is supposed to prevent the prompt and did not, but the deeper
// problem is that a credential lookup in an unattended process must not be
// able to block without limit for ANY reason — a locked keychain, an OS
// update that invalidates the ACL, a binary replaced by an upgrade.
const keychainReadTimeout = 5 * time.Second

func (s *Store) keychainGet(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, keychainReadTimeout)
	defer cancel()

	res, err := s.Runner.Run(ctx, execx.Cmd{
		Name: "security",
		Args: []string{"find-generic-password", "-s", Service, "-a", Account, "-w"},
	})
	if err != nil {
		if ctx.Err() != nil {
			// Distinguished from "no such item" because the operator's next
			// move is different: not `kiln login`, but a keychain that is
			// asking a question nobody can see.
			return "", fmt.Errorf("%w: the keychain did not answer in %s — it is most likely showing a permission dialog; "+
				"run `kiln login` again from this terminal, or set GITHUB_TOKEN for an unattended box", ErrKeychainBlocked, keychainReadTimeout)
		}
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}

func (s *Store) secretToolSet(ctx context.Context, token string) error {
	_, err := s.Runner.Run(ctx, execx.Cmd{
		Name:  "secret-tool",
		Args:  []string{"store", "--label=kiln GitHub token", "service", Service, "account", Account},
		Stdin: strings.NewReader(token),
	})
	return err
}

func (s *Store) secretToolGet(ctx context.Context) (string, error) {
	res, err := s.Runner.Run(ctx, execx.Cmd{
		Name: "secret-tool",
		Args: []string{"lookup", "service", Service, "account", Account},
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}

// filePath is the fallback location, under the user's config directory.
func (s *Store) filePath() string {
	dir := s.Dir
	if dir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			base = filepath.Join(os.Getenv("HOME"), ".config")
		}
		dir = filepath.Join(base, "kiln")
	}
	return filepath.Join(dir, "token")
}

func (s *Store) fileSet(token string) error {
	path := s.filePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("credstore: create %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return fmt.Errorf("credstore: write %s: %w", path, err)
	}
	return nil
}

func (s *Store) fileGet() (string, error) {
	data, err := os.ReadFile(filepath.Clean(s.filePath()))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// Describe says where a token would be kept on this machine, for the message
// `kiln login` prints before asking for one.
func (s *Store) Describe() Kind {
	switch s.platform() {
	case "darwin":
		if _, err := s.Runner.LookPath("security"); err == nil {
			return KindKeychain
		}
	case "linux":
		if _, err := s.Runner.LookPath("secret-tool"); err == nil {
			return KindSecretService
		}
	}
	return KindFile
}
