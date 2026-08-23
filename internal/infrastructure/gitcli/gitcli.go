// Package gitcli answers the application's questions about a repository by
// running git.
//
// Every format string, tab-separated field and peeled annotated tag lives
// here. That is the point of the port it implements: the application asks what
// it wants to know, and none of the shape of git's output leaks past this
// file.
package gitcli

import (
	"context"
	"fmt"
	"strings"

	"go.klarlabs.de/kiln/internal/application/ports"
	"go.klarlabs.de/kiln/internal/infrastructure/execx"
)

// PullRefNamespace is where pull request heads are parked locally. A private
// namespace rather than refs/pull/*, so a fetch cannot collide with anything
// the operator keeps.
const PullRefNamespace = "refs/kiln/pr/"

// Git implements ports.Git over the git command line.
type Git struct {
	Runner execx.Runner
}

// New returns a Git backed by the given runner.
func New(r execx.Runner) *Git { return &Git{Runner: r} }

func (g *Git) Fetch(ctx context.Context, dir, remote, refspec string) error {
	_, err := g.Runner.Run(ctx, execx.Cmd{
		Name: "git",
		Args: []string{"fetch", "--prune", "--quiet", remote, refspec},
		Dir:  dir,
	})
	return err
}

func (g *Git) HeadSHA(ctx context.Context, dir, remote, branch string) (string, error) {
	res, err := g.Runner.Run(ctx, execx.Cmd{
		Name: "git",
		Args: []string{"rev-parse", "--verify", fmt.Sprintf("refs/remotes/%s/%s", remote, branch)},
		Dir:  dir,
	})
	if err != nil {
		return "", fmt.Errorf("gitcli: resolve %s/%s: %w", remote, branch, err)
	}
	return strings.TrimSpace(res.Output()), nil
}

func (g *Git) Tags(ctx context.Context, dir string) ([]ports.Ref, error) {
	res, err := g.Runner.Run(ctx, execx.Cmd{
		Name: "git",
		Args: []string{"for-each-ref", "--format=%(refname)\t%(objecttype)\t%(objectname)\t%(*objectname)", "refs/tags/"},
		Dir:  dir,
	})
	if err != nil {
		return nil, fmt.Errorf("gitcli: list tags: %w", err)
	}

	var out []ports.Ref
	for line := range strings.SplitSeq(res.Output(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 3 {
			continue
		}
		ref, objType, objName := fields[0], fields[1], fields[2]
		sha := objName
		if objType == "tag" && len(fields) > 3 && fields[3] != "" {
			// %(*objectname) is the peeled commit. An annotated tag's own
			// object id is not something a worktree can check out.
			sha = fields[3]
		}
		out = append(out, ports.Ref{Name: ref, SHA: sha})
	}
	return out, nil
}

func (g *Git) PullRefs(ctx context.Context, dir string) ([]ports.Ref, error) {
	res, err := g.Runner.Run(ctx, execx.Cmd{
		Name: "git",
		Args: []string{"for-each-ref", "--format=%(refname)\t%(objectname)", PullRefNamespace},
		Dir:  dir,
	})
	if err != nil {
		return nil, fmt.Errorf("gitcli: list pull request refs: %w", err)
	}

	var out []ports.Ref
	for line := range strings.SplitSeq(res.Output(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		ref, sha, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		var number int
		if _, err := fmt.Sscanf(strings.TrimPrefix(ref, PullRefNamespace), "%d", &number); err != nil {
			continue
		}
		// Keyed by the ref a job carries, not by the local parking namespace,
		// so the application and its records name the same thing.
		out = append(out, ports.Ref{Name: fmt.Sprintf("refs/pull/%d/head", number), SHA: sha})
	}
	return out, nil
}

func (g *Git) Contains(ctx context.Context, dir, sha, tip string) (bool, error) {
	if sha == "" || tip == "" {
		return false, nil
	}
	_, err := g.Runner.Run(ctx, execx.Cmd{
		Name: "git",
		Args: []string{"merge-base", "--is-ancestor", sha, tip},
		Dir:  dir,
	})
	// A non-zero exit is the answer "no", not a failure to answer.
	return err == nil, nil
}
