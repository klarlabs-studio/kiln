package cli

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"go.klarlabs.de/kiln/internal/config"
	"go.klarlabs.de/kiln/internal/infrastructure/execx"
	"go.klarlabs.de/kiln/internal/infrastructure/worktree"
)

// runInit writes a `.kiln.yaml` for the repository under the cursor.
//
// The file is small enough to write by hand, which is exactly why nobody
// should have to: the parts that are easy to get wrong — a sha-only tag list,
// an image name that does not match the registry you are logged into, a
// pipeline that routes an event to nothing — are all things kiln can see for
// itself by looking at the repository.
//
// It writes the smallest correct file and says what it inferred, rather than
// generating every option commented out. A generated file full of commented
// alternatives is a file nobody reads and everybody keeps.
func runInit(ctx context.Context, args []string, io IO) error {
	fs := newFlagSet("init", io)
	dir := fs.String("dir", "", "repository (default: the working directory)")
	force := fs.Bool("force", false, "overwrite an existing .kiln.yaml")
	if err := fs.Parse(args); err != nil {
		return wrapExit(ExitUsage, err)
	}

	repo, err := boxDir(*dir)
	if err != nil {
		return wrapExit(ExitConfig, err)
	}

	runner := execx.NewSystem()
	if !worktree.IsRepo(ctx, runner, repo) {
		return failWith(ExitConfig, "%s is not a git repository — kiln builds commits, so it needs one", repo)
	}

	path := filepath.Join(repo, config.FileName)
	if _, err := os.Stat(path); err == nil && !*force {
		return failWith(ExitConfig, "%s already exists (use --force to replace it)", config.FileName)
	}

	found := survey(ctx, runner, repo)
	body := found.render()

	// Parsed before it is written. A generator that emits something its own
	// loader rejects is worse than no generator, and this is the cheapest
	// possible way to never ship that.
	if _, err := config.Parse(strings.NewReader(body)); err != nil {
		return wrapExit(ExitError, fmt.Errorf("generated a pipeline kiln cannot load: %w", err))
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return wrapExit(ExitError, err)
	}

	io.print("wrote " + config.FileName + "\n\n")
	for _, line := range found.notes() {
		io.print("  " + line + "\n")
	}
	io.print("\nNext:\n")
	if !found.gate {
		io.print("  1. write .warden.yaml — kiln runs no checks of its own, it runs that file\n")
		io.print("  2. kiln login\n  3. kiln box install\n")
		return nil
	}
	io.print("  1. kiln login\n  2. kiln box install\n")
	return nil
}

// findings is what init could tell about a repository.
type findings struct {
	branch     string
	remote     string
	gate       bool
	dockerfile bool
	goreleaser bool
	image      string
}

func survey(ctx context.Context, runner execx.Runner, repo string) findings {
	f := findings{branch: "main", remote: "origin"}

	if res, err := runner.Run(ctx, execx.Cmd{
		Name: "git", Args: []string{"symbolic-ref", "--short", "HEAD"}, Dir: repo,
	}); err == nil && res.Output() != "" {
		f.branch = res.Output()
	}

	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(repo, name))
		return err == nil
	}
	f.gate = exists(".warden.yaml")
	f.dockerfile = exists("Dockerfile")
	f.goreleaser = exists(".goreleaser.yaml") || exists(".goreleaser.yml")

	// The image name is guessed from the remote rather than invented, because
	// a wrong one fails at the push — after the gate has run and the image has
	// been built, which is the most expensive moment to learn it.
	if res, err := runner.Run(ctx, execx.Cmd{
		Name: "git", Args: []string{"remote", "get-url", f.remote}, Dir: repo,
	}); err == nil {
		if slug := repoSlug(res.Output()); slug != "" {
			f.image = "ghcr.io/" + slug
		}
	}
	return f
}

// repoSlug pulls owner/name out of a git remote.
//
// Both shapes, because both are normal: the scp-like `git@host:owner/name` and
// any `scheme://host/owner/name`. Parsing the second by hand was wrong the
// first time — trimming "https://" leaves `ssh://` URLs mangled — which is
// what the table test is for.
func repoSlug(remote string) string {
	remote = strings.TrimSuffix(strings.TrimSpace(remote), ".git")
	if remote == "" {
		return ""
	}

	if !strings.Contains(remote, "://") {
		// git@github.com:acme/widget
		_, path, ok := strings.Cut(remote, ":")
		if !ok {
			return ""
		}
		return strings.Trim(path, "/")
	}

	parsed, err := url.Parse(remote)
	if err != nil {
		return ""
	}
	return strings.Trim(parsed.Path, "/")
}

func (f findings) render() string {
	var b strings.Builder
	b.WriteString(`# Written by kiln init. The checks live in .warden.yaml — kiln does not
# invent a second check language — and deployment lives in RollOps.
apiVersion: ` + config.APIVersion + `
kind: ` + config.Kind + `

on:
  pull_request: [prove]
`)
	if f.dockerfile || f.goreleaser {
		b.WriteString("  push: [prove, publish]\n")
	} else {
		b.WriteString("  push: [prove]\n")
	}

	if f.dockerfile || f.goreleaser {
		b.WriteString("\npublish:\n")
	}
	if f.dockerfile {
		image := f.image
		if image == "" {
			image = "ghcr.io/owner/name   # kiln could not read the remote; set this"
		}
		fmt.Fprintf(&b, `  - kind: image
    image: %s
    dockerfile: Dockerfile
    context: .
    tags: [sha, latest]
`, image)
	}
	if f.goreleaser {
		b.WriteString(`  - kind: binaries
    from: goreleaser
`)
	}

	fmt.Fprintf(&b, `
watch:
  remote: %s
  ref: %s
`, f.remote, f.branch)
	return b.String()
}

func (f findings) notes() []string {
	var out []string
	out = append(out, fmt.Sprintf("watching %s/%s", f.remote, f.branch))

	switch {
	case f.dockerfile && f.image != "":
		out = append(out, "found a Dockerfile → publishing "+f.image)
	case f.dockerfile:
		out = append(out, "found a Dockerfile, but could not read the remote — set image: yourself")
	}
	if f.goreleaser {
		out = append(out, "found .goreleaser.yaml → publishing binaries through it")
	}
	if !f.dockerfile && !f.goreleaser {
		out = append(out, "nothing to publish, so kiln will prove every event and publish nothing")
	}
	if !f.gate {
		out = append(out, "no .warden.yaml: kiln has no checks to run until there is one")
	}
	return out
}
