// Package gittest builds throwaway git repositories for tests.
//
// Several packages need a real repository rather than a fake: worktree checks
// out commits, watch fetches refs, and the resolver peels annotated tags.
// Faking git well enough to test those would mean reimplementing git, so the
// tests use the real thing against a temp directory instead.
package gittest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Repo is a temporary git repository.
type Repo struct {
	// Dir is the working tree root.
	Dir string
	t   *testing.T
}

// New creates an initialised repository with a `main` branch and no commits.
// It is configured with a local identity and with signing off, so it works on
// a developer machine whose global config would otherwise interfere.
func New(t *testing.T) *Repo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	dir := t.TempDir()
	r := &Repo{Dir: dir, t: t}
	r.Git("init", "-q", "-b", "main")
	r.Git("config", "user.email", "kiln@test.local")
	r.Git("config", "user.name", "Kiln Test")
	r.Git("config", "commit.gpgsign", "false")
	r.Git("config", "tag.gpgsign", "false")
	return r
}

// Git runs a git command in the repository and fails the test on error.
func (r *Repo) Git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.Dir
	// A hermetic environment: the developer's global config, credential helper
	// and hooks must not leak into a test repository.
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// Write creates or replaces a file, creating parent directories.
func (r *Repo) Write(rel, content string) {
	r.t.Helper()
	path := filepath.Join(r.Dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

// Commit writes a file, stages everything and commits, returning the new SHA.
func (r *Repo) Commit(message, file, content string) string {
	r.t.Helper()
	r.Write(file, content)
	r.Git("add", "-A")
	r.Git("commit", "-q", "-m", message)
	return r.Head()
}

// Head is the current commit id.
func (r *Repo) Head() string { return r.Git("rev-parse", "HEAD") }

// Tag creates an annotated tag at HEAD. Annotated rather than lightweight on
// purpose: an annotated tag has its own object id, which is the case that
// catches a resolver that forgets to peel.
func (r *Repo) Tag(name string) { r.Git("tag", "-a", name, "-m", name) }

// Clone makes a second repository with this one as `origin`, for exercising
// fetch-based discovery.
func (r *Repo) Clone(t *testing.T) *Repo {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("git", "clone", "-q", r.Dir, dir)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v\n%s", err, out)
	}
	c := &Repo{Dir: dir, t: t}
	c.Git("config", "user.email", "kiln@test.local")
	c.Git("config", "user.name", "Kiln Test")
	return c
}
