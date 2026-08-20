package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"go.klarlabs.de/kiln/internal/execx"
)

// runBox installs, inspects and removes the schedule that ticks a repository.
//
// The reason this is a command and not a paragraph in the documentation: the
// step between "kiln works here" and "kiln is my CI" was previously a cron
// line with a token in it, or a Kubernetes manifest and a toolchain image to
// maintain. Both are places people stop. A box is just a machine that already
// has your tools on it — usually the one you are typing on — and the schedule
// is four lines of plist that nobody should have to write.
func runBox(ctx context.Context, args []string, io IO) error {
	fs := newFlagSet("box", io)
	every := fs.Duration("every", 5*time.Minute, "how often to tick")
	dir := fs.String("dir", "", "repository to watch (default: the working directory)")
	branchesOnly := fs.Bool("branches-only", false,
		"watch the tracked branch only, ignoring pull requests and tags")
	// The action is pulled out before parsing, so `box install --every 10m`
	// works. Go's flag package stops at the first non-flag argument, which
	// meant every flag written after the verb — the natural way to write it —
	// was silently ignored, defaults applied, and nothing said so.
	action, rest := splitAction(args)
	if err := fs.Parse(rest); err != nil {
		return wrapExit(ExitUsage, err)
	}
	repo, err := boxDir(*dir)
	if err != nil {
		return wrapExit(ExitConfig, err)
	}

	agent, err := newAgent(repo, *every, *branchesOnly)
	if err != nil {
		return wrapExit(ExitConfig, err)
	}

	switch action {
	case "install":
		return agent.install(ctx, io)
	case "status":
		return agent.status(io)
	case "uninstall":
		return agent.uninstall(ctx, io)
	default:
		return failWith(ExitUsage, "usage: kiln box install|status|uninstall [--every 5m]")
	}
}

// splitAction separates the verb from the flags, wherever the verb appears.
func splitAction(args []string) (string, []string) {
	for i, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		return a, append(append([]string{}, args[:i]...), args[i+1:]...)
	}
	return "", args
}

func boxDir(dir string) (string, error) {
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		dir = wd
	}
	return filepath.Abs(dir)
}

// agent is one scheduled tick, described in whatever the platform's init
// system wants.
type agent struct {
	repo  string
	every time.Duration
	exe   string
	label string
	unit  string
	goos  string
	// branchesOnly runs `poll` rather than `watch`: the tracked branch, no
	// pull requests, no tags, and no GitHub token needed for any of it.
	branchesOnly bool
}

// command is what the schedule runs.
//
// A first box usually wants the branch only. Pointing a fresh box at a
// repository with a dozen open pull requests means gating all of them before
// it gets to the branch you actually care about, and on a laptop that is an
// afternoon of fans.
func (a *agent) command() string {
	if a.branchesOnly {
		return "poll"
	}
	return "watch"
}

func newAgent(repo string, every time.Duration, branchesOnly bool) (*agent, error) {
	if every < time.Minute {
		// Below a minute the ticks overlap their own fetch and the repository
		// lock spends the day refusing them.
		return nil, fmt.Errorf("--every %s is too frequent; a minute is the floor", every)
	}

	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("cannot find the kiln binary: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return nil, err
	}

	a := &agent{repo: repo, every: every, exe: exe, goos: runtime.GOOS, branchesOnly: branchesOnly}
	a.label = "de.klarlabs.kiln." + sanitise(filepath.Base(repo))

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	switch a.goos {
	case "darwin":
		a.unit = filepath.Join(home, "Library", "LaunchAgents", a.label+".plist")
	case "linux":
		a.unit = filepath.Join(home, ".config", "systemd", "user", a.label+".timer")
	default:
		return nil, fmt.Errorf("kiln box supports macOS and Linux; on %s, run `kiln watch --every %s` "+
			"under whatever supervises processes there", a.goos, every)
	}
	return a, nil
}

func (a *agent) install(ctx context.Context, io IO) error {
	if err := os.MkdirAll(filepath.Dir(a.unit), 0o750); err != nil {
		return wrapExit(ExitError, err)
	}

	var body string
	switch a.goos {
	case "darwin":
		body = a.plist()
	case "linux":
		if err := os.WriteFile(a.servicePath(), []byte(a.service()), 0o600); err != nil {
			return wrapExit(ExitError, err)
		}
		body = a.timer()
	}
	if err := os.WriteFile(a.unit, []byte(body), 0o600); err != nil {
		return wrapExit(ExitError, err)
	}

	if err := a.load(ctx); err != nil {
		return wrapExit(ExitError, err)
	}

	scope := "every ref"
	if a.branchesOnly {
		scope = "the tracked branch only"
	}
	io.print(fmt.Sprintf("watching %s (%s) every %s\n", a.repo, scope, a.every))
	io.print("  unit  " + a.unit + "\n")
	io.print("  logs  " + a.logPath() + "\n\n")
	io.print("It ticks as you, with your toolchain, and reads the token from `kiln login`.\n")
	io.print("Stop it with: kiln box uninstall\n")
	return nil
}

func (a *agent) status(io IO) error {
	if _, err := os.Stat(a.unit); err != nil {
		io.print("no schedule installed for " + a.repo + "\n\nRun: kiln box install\n")
		return nil
	}
	io.print("installed: " + a.unit + "\n")

	info, err := os.Stat(a.logPath())
	if err != nil {
		io.print("no log yet — the first tick has not run\n")
		return nil
	}
	io.print(fmt.Sprintf("last tick wrote %s at %s\n",
		a.logPath(), info.ModTime().Format(time.RFC3339)))
	return nil
}

func (a *agent) uninstall(ctx context.Context, io IO) error {
	_ = a.unload(ctx)
	removed := false
	for _, path := range []string{a.unit, a.servicePath()} {
		if path == "" {
			continue
		}
		if err := os.Remove(path); err == nil {
			removed = true
		}
	}
	if !removed {
		io.print("nothing installed for " + a.repo + "\n")
		return nil
	}
	io.print("schedule removed. The ledger and retained output under .kiln/ are untouched.\n")
	return nil
}

func (a *agent) load(ctx context.Context) error {
	run := execx.NewSystem()
	switch a.goos {
	case "darwin":
		// bootout first, so `install` twice is a reload rather than an error
		// about the label already being loaded.
		_, _ = run.Run(ctx, execx.Cmd{Name: "launchctl", Args: []string{"bootout", a.domain() + "/" + a.label}})
		_, err := run.Run(ctx, execx.Cmd{Name: "launchctl", Args: []string{"bootstrap", a.domain(), a.unit}})
		return err
	case "linux":
		if _, err := run.Run(ctx, execx.Cmd{Name: "systemctl", Args: []string{"--user", "daemon-reload"}}); err != nil {
			return err
		}
		_, err := run.Run(ctx, execx.Cmd{
			Name: "systemctl", Args: []string{"--user", "enable", "--now", filepath.Base(a.unit)},
		})
		return err
	}
	return nil
}

func (a *agent) unload(ctx context.Context) error {
	run := execx.NewSystem()
	switch a.goos {
	case "darwin":
		_, err := run.Run(ctx, execx.Cmd{Name: "launchctl", Args: []string{"bootout", a.domain() + "/" + a.label}})
		return err
	case "linux":
		_, err := run.Run(ctx, execx.Cmd{
			Name: "systemctl", Args: []string{"--user", "disable", "--now", filepath.Base(a.unit)},
		})
		return err
	}
	return nil
}

func (a *agent) domain() string { return fmt.Sprintf("gui/%d", os.Getuid()) }

func (a *agent) logPath() string {
	return filepath.Join(a.repo, ".kiln", "box.log")
}

func (a *agent) servicePath() string {
	if a.goos != "linux" {
		return ""
	}
	return strings.TrimSuffix(a.unit, ".timer") + ".service"
}

// path is the PATH the schedule runs with, captured at install time.
//
// Not decoration. A launchd agent inherits a minimal PATH —
// /usr/bin:/bin:/usr/sbin:/sbin — and everything kiln shells out to lives
// somewhere else: warden, golangci-lint, cosign, and Go itself are all in
// /opt/homebrew/bin or ~/go/bin. The first box installed without this
// discovered thirteen commits and failed all thirteen with "warden is the
// source gate and kiln cannot pass a commit without it".
//
// Baking in the installing shell's PATH is a real trade: a tool installed
// somewhere new later will not be found until `kiln box install` is run again.
// That is a comprehensible failure with an obvious fix, which the silent
// minimal-PATH version is not.
func (a *agent) path() string {
	path := os.Getenv("PATH")
	if path == "" {
		return "/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin"
	}
	return path
}

// plist is the launchd job.
//
// RunAtLoad so `install` proves itself immediately rather than leaving the
// operator to wonder for five minutes. No token in here: kiln reads the
// keychain, which is the entire reason `kiln login` exists.
func (a *agent) plist() string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>%s</string>
    <string>--once</string>
  </array>
  <key>WorkingDirectory</key><string>%s</string>
  <key>EnvironmentVariables</key>
  <dict><key>PATH</key><string>%s</string></dict>
  <key>StartInterval</key><integer>%d</integer>
  <key>RunAtLoad</key><true/>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
  <key>ProcessType</key><string>Background</string>
</dict>
</plist>
`, a.label, a.exe, a.command(), a.repo, a.path(), int(a.every.Seconds()), a.logPath(), a.logPath())
}

func (a *agent) service() string {
	return fmt.Sprintf(`[Unit]
Description=kiln tick for %s

[Service]
Type=oneshot
WorkingDirectory=%s
Environment=PATH=%s
ExecStart=%s %s --once
StandardOutput=append:%s
StandardError=append:%s
`, a.repo, a.repo, a.path(), a.exe, a.command(), a.logPath(), a.logPath())
}

func (a *agent) timer() string {
	return fmt.Sprintf(`[Unit]
Description=kiln tick for %s

[Timer]
OnBootSec=1min
OnUnitActiveSec=%ds
Persistent=true

[Install]
WantedBy=timers.target
`, a.repo, int(a.every.Seconds()))
}

// sanitise makes a label out of a directory name.
func sanitise(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "repo"
	}
	return out
}
