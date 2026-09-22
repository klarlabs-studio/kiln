// Package authority turns a surface's request into an established trust
// context, then runs the engine under the repository lock.
//
// Surfaces ask. This package decides. Adding HTTP, MCP or a future interface
// must not require reimplementing fork defaults, SHA membership or locking —
// those checks live here so a new door is boring.
package authority

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"go.klarlabs.de/kiln/internal/application/engine"
	"go.klarlabs.de/kiln/internal/application/ports"
	"go.klarlabs.de/kiln/internal/domain/config"
	"go.klarlabs.de/kiln/internal/domain/isolation"
	"go.klarlabs.de/kiln/internal/domain/run"
	"go.klarlabs.de/kiln/internal/domain/trust"
)

// ErrUnestablished reports that a caller asked for publishable authority
// the evidence does not support.
var ErrUnestablished = errors.New("authority: sha is not on a trusted ref")

// Git is the membership questions authority asks of a repository.
type Git interface {
	Contains(ctx context.Context, dir, sha, tip string) (bool, error)
	HeadSHA(ctx context.Context, dir, remote, branch string) (string, error)
	Tags(ctx context.Context, dir string) ([]ports.Ref, error)
	Resolve(ctx context.Context, dir, ref string) (string, error)
	// Show reads one file out of a commit. Used only when the operator
	// opted into commit-controlled policy.
	Show(ctx context.Context, dir, sha, path string) ([]byte, error)
}

// ForkLookup answers whether a pull request is from a fork. Missing or
// failed lookups are the caller's problem to treat as untrusted; this
// package treats a nil lookup as "I cannot tell".
type ForkLookup func(ctx context.Context, number int) (fork bool, ok bool)

// Resolver derives a trust.Context from a claim and the repository.
type Resolver struct {
	Git     Git
	Dir     string
	Remote  string
	Watched string
	// ForkOf reports (fork, known). known is false when the question
	// cannot be asked.
	ForkOf ForkLookup
}

// Resolve classifies a claimed SHA.
//
// A claim with Established set is already evidence (watch, webhook) and is
// honoured after applying the fork floor. Any other claim that asks for
// push or tag must show the SHA belongs to a matching trusted ref.
func (r *Resolver) Resolve(ctx context.Context, claim trust.Claim) (trust.Context, error) {
	if strings.TrimSpace(claim.SHA) == "" {
		return trust.Context{}, errors.New("authority: no commit")
	}
	if !claim.Event.Valid() {
		return trust.Context{}, fmt.Errorf("authority: unknown event %q", claim.Event)
	}

	out := trust.Context{
		Event:       claim.Event,
		Fork:        claim.Fork,
		Ref:         claim.Ref,
		SHA:         claim.SHA,
		Established: claim.Established,
	}

	switch claim.Event {
	case isolation.EventPullRequest:
		out.Fork = r.pullFork(ctx, claim)
		if out.Ref == "" && claim.PR > 0 {
			out.Ref = fmt.Sprintf("refs/pull/%d/head", claim.PR)
		}
		out.Established = true
		return out, nil

	case isolation.EventPush, isolation.EventTag:
		if claim.Established {
			return out, nil
		}
		if err := r.requireMembership(ctx, claim); err != nil {
			return trust.Context{}, err
		}
		out.Established = true
		if out.Ref == "" {
			out.Ref = r.defaultRef(claim.Event)
		}
		return out, nil
	}
	return trust.Context{}, fmt.Errorf("authority: unknown event %q", claim.Event)
}

func (r *Resolver) pullFork(ctx context.Context, claim trust.Claim) bool {
	if claim.Fork {
		return true
	}
	if claim.PR <= 0 {
		return true
	}
	if r.ForkOf == nil {
		return true
	}
	fork, ok := r.ForkOf(ctx, claim.PR)
	if !ok {
		return true
	}
	return fork
}

func (r *Resolver) requireMembership(ctx context.Context, claim trust.Claim) error {
	if r.Git == nil {
		return fmt.Errorf("%w: no repository to ask", ErrUnestablished)
	}

	switch claim.Event {
	case isolation.EventTag:
		if claim.Ref != "" {
			return r.mustContain(ctx, claim.SHA, claim.Ref)
		}
		ok, err := r.onAnyTag(ctx, claim.SHA)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: %s is not a tag this repository knows", ErrUnestablished, short(claim.SHA))
		}
		return nil
	default:
		tips := r.pushTips(claim.Ref)
		var last error
		for _, tip := range tips {
			err := r.mustContain(ctx, claim.SHA, tip)
			if err == nil {
				return nil
			}
			if !errors.Is(err, ErrUnestablished) {
				last = err
				continue
			}
			last = err
		}
		if last == nil {
			return fmt.Errorf("%w: %s is not on %s", ErrUnestablished, short(claim.SHA), r.watchedRef())
		}
		return last
	}
}

func (r *Resolver) pushTips(ref string) []string {
	var tips []string
	if ref != "" {
		tips = append(tips, ref)
	}
	branch := r.Watched
	if branch == "" {
		branch = "main"
	}
	remote := r.Remote
	if remote == "" {
		remote = "origin"
	}
	tips = append(tips,
		"refs/heads/"+branch,
		"refs/remotes/"+remote+"/"+branch,
	)
	return unique(tips)
}

func (r *Resolver) watchedRef() string {
	if r.Watched != "" {
		return "refs/heads/" + r.Watched
	}
	return "refs/heads/main"
}

func (r *Resolver) defaultRef(event isolation.Event) string {
	if event == isolation.EventTag {
		return ""
	}
	return r.watchedRef()
}

func (r *Resolver) mustContain(ctx context.Context, sha, tip string) error {
	ok, err := r.Git.Contains(ctx, r.Dir, sha, tip)
	if err != nil {
		return fmt.Errorf("authority: ask whether %s is on %s: %w", short(sha), tip, err)
	}
	if !ok {
		// Some tips are not refs git can resolve (no origin yet). Try
		// resolving first so a local checkout still works.
		if resolved, rerr := r.Git.Resolve(ctx, r.Dir, tip); rerr == nil {
			ok, err = r.Git.Contains(ctx, r.Dir, sha, resolved)
			if err != nil {
				return err
			}
			if ok {
				return nil
			}
		}
		return fmt.Errorf("%w: %s is not on %s", ErrUnestablished, short(sha), tip)
	}
	return nil
}

func (r *Resolver) onAnyTag(ctx context.Context, sha string) (bool, error) {
	tags, err := r.Git.Tags(ctx, r.Dir)
	if err != nil {
		return false, fmt.Errorf("authority: list tags: %w", err)
	}
	for _, tag := range tags {
		if tag.SHA == sha {
			return true, nil
		}
		ok, err := r.Git.Contains(ctx, r.Dir, sha, tag.SHA)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

func unique(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func short(sha string) string { return run.ShortSHA(sha) }

// bindPolicy selects the pipeline this run is governed by.
//
// Today's default is the operator checkout. `policy.from: commit` is an
// explicit trust-boundary change: the SHA supplies `.kiln.yaml`, discovery
// (Watch) stays operator-owned, and a fork cannot start the commit's
// services. The engine's recorded identity is swapped for the duration of
// the run and restored afterwards so a long-lived process does not keep
// the last SHA's digest.
func (r *Runner) bindPolicy(ctx context.Context, established trust.Context) (config.Pipeline, func(), error) {
	noop := func() {}
	if r == nil {
		return config.Pipeline{}, noop, nil
	}
	if !r.Pipeline.CommitControlled() {
		return r.Pipeline, noop, nil
	}

	pipe, id, err := r.loadCommitPolicy(ctx, established)
	if err != nil {
		return config.Pipeline{}, noop, err
	}
	if r.Engine == nil {
		return pipe, noop, nil
	}
	saved := r.Engine.Policy
	r.Engine.Policy = id
	return pipe, func() { r.Engine.Policy = saved }, nil
}

func (r *Runner) loadCommitPolicy(ctx context.Context, established trust.Context) (config.Pipeline, trust.PolicyIdentity, error) {
	if r.Resolver == nil || r.Resolver.Git == nil {
		return config.Pipeline{}, trust.PolicyIdentity{}, fmt.Errorf(
			"authority: policy.from is commit but there is no repository to read %s from", config.FileName)
	}

	raw, err := r.Resolver.Git.Show(ctx, r.Dir, established.SHA, config.FileName)
	if err != nil {
		return config.Pipeline{}, trust.PolicyIdentity{}, fmt.Errorf(
			"authority: policy.from is commit but %s has no %s: %w",
			short(established.SHA), config.FileName, err)
	}

	pipe, err := config.Parse(bytes.NewReader(raw))
	if err != nil {
		return config.Pipeline{}, trust.PolicyIdentity{}, fmt.Errorf(
			"authority: %s:%s: %w", short(established.SHA), config.FileName, err)
	}

	// Discovery is the operator's. A commit must not retarget the box.
	pipe.Watch = r.Pipeline.Watch
	if established.Fork {
		// A fork that authors services is "run this image as docker"
		// before the gate. Isolation already strips secrets and publish;
		// this is the remaining privilege the commit would otherwise get.
		pipe.Services = nil
	}

	sum := sha256.Sum256(raw)
	id := trust.PolicyIdentity{
		Source: trust.PolicyCommit,
		Path:   config.FileName,
		Digest: "sha256:" + hex.EncodeToString(sum[:]),
		Commit: established.SHA,
	}
	return pipe, id, nil
}

// Runner is the common execute path every mutating surface should call.
type Runner struct {
	Engine   *engine.Engine
	Resolver *Resolver
	Locks    ports.Locks
	Repo     string
	Dir      string
	Pipeline config.Pipeline
	Log      ports.Logger
}

// Request is a resolved claim plus the output stream for this invocation.
type Request struct {
	Claim  trust.Claim
	Output io.Writer
}

// Execute resolves the claim, takes the repository lock, and runs the engine.
func (r *Runner) Execute(ctx context.Context, in Request, holder string) (*run.Run, error) {
	return r.execute(ctx, in, holder, true)
}

// ExecuteLocked resolves the claim and runs the engine. The caller already
// holds the repository lock — watch, or kilnd after it has waited.
func (r *Runner) ExecuteLocked(ctx context.Context, in Request) (*run.Run, error) {
	return r.execute(ctx, in, "", false)
}

func (r *Runner) execute(ctx context.Context, in Request, holder string, acquire bool) (*run.Run, error) {
	established, err := r.Resolver.Resolve(ctx, in.Claim)
	if err != nil {
		return nil, err
	}

	pipeline, restore, err := r.bindPolicy(ctx, established)
	if err != nil {
		return nil, err
	}
	defer restore()

	runFn := func() (*run.Run, error) {
		return r.Engine.Execute(ctx, engine.Request{
			Trust:    established,
			SHA:      established.SHA,
			Event:    established.Event,
			Fork:     established.Fork,
			Ref:      established.Ref,
			Repo:     r.Repo,
			Dir:      r.Dir,
			Pipeline: pipeline,
			Output:   in.Output,
		})
	}

	if !acquire || r.Locks == nil {
		return runFn()
	}

	l, err := r.Locks.TryAcquire(r.Dir, holder)
	if err != nil {
		return nil, err
	}
	defer func() { _ = l.Release() }()
	return runFn()
}
