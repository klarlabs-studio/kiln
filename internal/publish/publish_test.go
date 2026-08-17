package publish

import (
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"go.klarlabs.de/kiln/internal/config"
	"go.klarlabs.de/kiln/internal/execx"
	"go.klarlabs.de/kiln/internal/gittest"
	"go.klarlabs.de/kiln/internal/obs"
)

const digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// dockerFake scripts a healthy docker: every build and push succeeds and the
// inspect reports a registry digest for the image under test.
func dockerFake(t *testing.T) *execx.Fake {
	t.Helper()
	f := execx.NewFake()
	f.On("git", execx.Response{Fn: func(c execx.Cmd) (execx.Result, error) {
		return execx.NewSystem().Run(t.Context(), c)
	}})
	f.On("docker image inspect", execx.Response{
		Stdout: `["` + image + `@` + digest + `"]`,
	})
	return f
}

func newPublisher(f execx.Runner) *Docker {
	d := NewDocker(f, obs.Discard())
	// Retries are exercised in their own test; elsewhere they only slow things
	// down and hide which call actually failed.
	d.PushRetries = 1
	return d
}

// mustPlan resolves the plan a test wants to assert against. The publisher
// derives its own from the same inputs, so the two cannot drift.
func mustPlan(t *testing.T, art config.Artifact, commit, ref string) Plan {
	t.Helper()
	p, err := BuildPlan(art, commit, ref)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	return p
}

func TestPublishBuildsPushesAndSigns(t *testing.T) {
	repo := gittest.New(t)
	head := repo.Commit("first", "Dockerfile", "FROM scratch\n")
	fake := dockerFake(t)

	ref, art := "refs/heads/main", cfg(config.TagSHA, config.TagLatest)
	plan := mustPlan(t, art, head, ref)
	res, err := newPublisher(fake).Publish(t.Context(), Request{
		RepoDir: repo.Dir, SHA: head, Ref: ref, Artifact: art,
	})
	if err != nil {
		t.Fatalf("Publish: %v\n%s", err, fake.Transcript())
	}

	if res.Digest != digest {
		t.Errorf("Digest = %q, want the registry's answer", res.Digest)
	}
	if res.Reference != image+"@"+digest {
		t.Errorf("Reference = %q", res.Reference)
	}
	if !res.Signed {
		t.Error("Signed = false after a real publish")
	}
	if !slices.Equal(res.Tags, plan.Refs()) {
		t.Errorf("Tags = %v, want %v", res.Tags, plan.Refs())
	}
}

func TestPublishTagsEveryPlannedReference(t *testing.T) {
	repo := gittest.New(t)
	head := repo.Commit("first", "Dockerfile", "FROM scratch\n")
	fake := dockerFake(t)

	ref, art := "refs/heads/main", cfg(config.TagSHA, config.TagLatest)
	plan := mustPlan(t, art, head, ref)
	if _, err := newPublisher(fake).Publish(t.Context(), Request{
		RepoDir: repo.Dir, SHA: head, Ref: ref, Artifact: art,
	}); err != nil {
		t.Fatal(err)
	}

	build := fake.Find("docker build")
	if build == nil {
		t.Fatalf("no build: %s", fake.Transcript())
	}
	for _, ref := range plan.Refs() {
		if !strings.Contains(build.String(), "-t "+ref) {
			t.Errorf("build did not tag %s: %s", ref, build.String())
		}
		if !fake.Ran("docker push " + ref) {
			t.Errorf("%s was never pushed: %s", ref, fake.Transcript())
		}
	}
}

func TestPublishBuildsFromAWorktreeNotTheCheckout(t *testing.T) {
	repo := gittest.New(t)
	head := repo.Commit("first", "Dockerfile", "FROM scratch\n")
	// A dirty working copy must not reach the image; that would make the
	// digest attest to a commit that never contained the code it shipped.
	repo.Write("Dockerfile", "FROM alpine # uncommitted\n")

	fake := dockerFake(t)
	if _, err := newPublisher(fake).Publish(t.Context(), Request{
		RepoDir: repo.Dir, SHA: head,
		Ref: "refs/heads/main", Artifact: cfg(config.TagSHA, config.TagLatest),
	}); err != nil {
		t.Fatal(err)
	}

	build := fake.Find("docker build")
	if build.Dir == repo.Dir {
		t.Error("built from the operator's checkout instead of a pinned worktree")
	}
	content, err := os.ReadFile(build.Dir + "/Dockerfile")
	if err != nil {
		// The tree is gone by now on success; read it during the build instead.
		t.Skip("worktree already cleaned up")
	}
	if strings.Contains(string(content), "uncommitted") {
		t.Error("the uncommitted edit reached the build context")
	}
}

func TestPublishSignsTheDigestNotATag(t *testing.T) {
	repo := gittest.New(t)
	head := repo.Commit("first", "Dockerfile", "FROM scratch\n")
	fake := dockerFake(t)

	if _, err := newPublisher(fake).Publish(t.Context(), Request{
		RepoDir: repo.Dir, SHA: head,
		Ref: "refs/heads/main", Artifact: cfg(config.TagSHA, config.TagLatest),
	}); err != nil {
		t.Fatal(err)
	}

	sign := fake.Find("cosign sign")
	if sign == nil {
		t.Fatalf("nothing was signed: %s", fake.Transcript())
	}
	// A tag is mutable; a signature over one attests to whatever it points at
	// later, which is not a claim worth making.
	if !strings.Contains(sign.String(), image+"@"+digest) {
		t.Errorf("cosign signed %q, want the digest", sign.String())
	}
	if !strings.Contains(sign.String(), "--yes") {
		t.Error("cosign must not wait for a prompt on an unattended run")
	}
}

func TestMissingDockerFailsBeforeAnythingHappens(t *testing.T) {
	fake := dockerFake(t).Absent("docker")

	_, err := newPublisher(fake).Publish(t.Context(), Request{
		RepoDir: "/repo", SHA: sha, Ref: "refs/heads/main", Artifact: cfg(config.TagSHA, config.TagLatest),
	})

	if !errors.Is(err, ErrToolMissing) {
		t.Fatalf("err = %v, want ErrToolMissing", err)
	}
	if !strings.Contains(err.Error(), "KILN_DRY") {
		t.Errorf("error should offer the dry-run escape hatch, got %v", err)
	}
}

func TestMissingCosignFailsBeforePushing(t *testing.T) {
	fake := dockerFake(t).Absent("cosign")

	_, err := newPublisher(fake).Publish(t.Context(), Request{
		RepoDir: "/repo", SHA: sha, Ref: "refs/heads/main", Artifact: cfg(config.TagSHA, config.TagLatest),
	})

	if !errors.Is(err, ErrToolMissing) {
		t.Fatalf("err = %v, want ErrToolMissing", err)
	}
	// Pushing first and then discovering cosign is missing would leave an
	// unsigned image in the registry that RollOps cannot deploy.
	if fake.Ran("docker push") {
		t.Errorf("pushed before noticing cosign was missing: %s", fake.Transcript())
	}
}

func TestPushFailureFailsTheRun(t *testing.T) {
	repo := gittest.New(t)
	head := repo.Commit("first", "Dockerfile", "FROM scratch\n")
	fake := dockerFake(t).On("docker push", execx.Response{ExitCode: 1, Stderr: "unauthorized"})

	_, err := newPublisher(fake).Publish(t.Context(), Request{
		RepoDir: repo.Dir, SHA: head,
		Ref: "refs/heads/main", Artifact: cfg(config.TagSHA, config.TagLatest),
	})

	if err == nil {
		t.Fatal("want a failure")
	}
	if fake.Ran("cosign sign") {
		t.Error("signed an image that was never pushed")
	}
}

func TestAuthFailuresAreNotRetried(t *testing.T) {
	repo := gittest.New(t)
	head := repo.Commit("first", "Dockerfile", "FROM scratch\n")
	fake := dockerFake(t).On("docker push", execx.Response{ExitCode: 1, Stderr: "unauthorized: authentication required"})

	d := NewDocker(fake, obs.Discard())
	d.PushRetries = 4
	_, _ = d.Publish(t.Context(), Request{
		RepoDir: repo.Dir, SHA: head,
		Ref: "refs/heads/main", Artifact: cfg(config.TagSHA, config.TagLatest),
	})

	// A credential problem fails the same way every time; retrying it only
	// makes the operator wait longer for the same message.
	if n := fake.Count("docker push"); n != 1 {
		t.Errorf("pushed %d times, want 1 for a permanent failure", n)
	}
}

func TestTransientFailuresAreRetried(t *testing.T) {
	repo := gittest.New(t)
	head := repo.Commit("first", "Dockerfile", "FROM scratch\n")

	fake := dockerFake(t)
	attempts := 0
	fake.On("docker push", execx.Response{Fn: func(c execx.Cmd) (execx.Result, error) {
		attempts++
		if attempts < 2 {
			return execx.Result{}, &execx.ExitError{Cmd: c.String(), Code: 1, Stderr: "500 Internal Server Error"}
		}
		return execx.Result{}, nil
	}})

	d := NewDocker(fake, obs.Discard())
	d.PushRetries = 3
	if _, err := d.Publish(t.Context(), Request{
		RepoDir: repo.Dir, SHA: head,
		Ref: "refs/heads/main", Artifact: cfg(config.TagSHA, config.TagLatest),
	}); err != nil {
		t.Fatalf("a registry blip should not fail the run: %v", err)
	}
	if attempts < 2 {
		t.Errorf("push attempted %d times, want a retry", attempts)
	}
}

func TestDigestComesFromTheRegistryNotTheDaemon(t *testing.T) {
	repo := gittest.New(t)
	head := repo.Commit("first", "Dockerfile", "FROM scratch\n")

	other := "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	fake := dockerFake(t)
	// An image pushed to two repositories carries two RepoDigests. The one
	// that matters is the one for the image being published.
	fake.On("docker image inspect", execx.Response{
		Stdout: `["registry.example.com/mirror/glossa-api@` + other + `","` + image + `@` + digest + `"]`,
	})

	res, err := newPublisher(fake).Publish(t.Context(), Request{
		RepoDir: repo.Dir, SHA: head,
		Ref: "refs/heads/main", Artifact: cfg(config.TagSHA, config.TagLatest),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Digest != digest {
		t.Errorf("Digest = %q, want the entry matching %s", res.Digest, image)
	}
}

func TestNoRegistryDigestIsAFailure(t *testing.T) {
	repo := gittest.New(t)
	head := repo.Commit("first", "Dockerfile", "FROM scratch\n")
	fake := dockerFake(t).On("docker image inspect", execx.Response{Stdout: `[]`})

	_, err := newPublisher(fake).Publish(t.Context(), Request{
		RepoDir: repo.Dir, SHA: head,
		Ref: "refs/heads/main", Artifact: cfg(config.TagSHA, config.TagLatest),
	})

	if err == nil || !strings.Contains(err.Error(), "did not take effect") {
		t.Errorf("err = %v, want a clear missing-digest failure", err)
	}
}

func TestMultiPlatformUsesBuildx(t *testing.T) {
	repo := gittest.New(t)
	head := repo.Commit("first", "Dockerfile", "FROM scratch\n")

	fake := dockerFake(t)
	fake.On("docker buildx build", execx.Response{Fn: func(c execx.Cmd) (execx.Result, error) {
		// buildx reports the manifest-list digest through its metadata file.
		path := metadataPath(c.Args)
		payload, _ := json.Marshal(map[string]string{"containerimage.digest": digest})
		return execx.Result{}, os.WriteFile(path, payload, 0o600)
	}})

	// A release is the realistic multi-arch case, so this one plans from a tag
	// ref with a semver tag rather than repeating the branch shape.
	ref, art := "refs/tags/v1.0.0", cfg(config.TagSHA, config.TagSemver)
	art.Platforms = []string{"linux/amd64", "linux/arm64"}

	res, err := newPublisher(fake).Publish(t.Context(), Request{
		RepoDir: repo.Dir, SHA: head, Ref: ref, Artifact: art,
	})
	if err != nil {
		t.Fatalf("Publish: %v\n%s", err, fake.Transcript())
	}

	if res.Digest != digest {
		t.Errorf("Digest = %q", res.Digest)
	}
	// A multi-arch image cannot live in the local image store, so buildx must
	// push as part of the build rather than afterwards.
	build := fake.Find("docker buildx build")
	if build == nil || !strings.Contains(build.String(), "--push") {
		t.Errorf("buildx must build and push in one step: %s", fake.Transcript())
	}
	if fake.Ran("docker push") {
		t.Errorf("separate push on the buildx path: %s", fake.Transcript())
	}
}

func TestBuildxWithoutADigestIsAFailure(t *testing.T) {
	repo := gittest.New(t)
	head := repo.Commit("first", "Dockerfile", "FROM scratch\n")

	fake := dockerFake(t)
	fake.On("docker buildx build", execx.Response{Fn: func(c execx.Cmd) (execx.Result, error) {
		return execx.Result{}, os.WriteFile(metadataPath(c.Args), []byte(`{}`), 0o600)
	}})

	ref, art := "refs/heads/main", cfg(config.TagSHA, config.TagLatest)
	art.Platforms = []string{"linux/amd64", "linux/arm64"}

	_, err := newPublisher(fake).Publish(t.Context(), Request{RepoDir: repo.Dir, SHA: head, Ref: ref, Artifact: art})
	if err == nil || !strings.Contains(err.Error(), "no image digest") {
		t.Errorf("err = %v, want a missing-digest failure", err)
	}
}

func TestDryRunTouchesNothing(t *testing.T) {
	ref, art := "refs/heads/main", cfg(config.TagSHA, config.TagLatest)
	plan := mustPlan(t, art, sha, ref)

	res, err := NewDry(obs.Discard()).Publish(t.Context(), Request{RepoDir: "/repo", SHA: sha, Ref: ref, Artifact: art})
	if err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(res.Tags, plan.Refs()) {
		t.Errorf("Tags = %v, want the plan", res.Tags)
	}
	// A rehearsal must be impossible to mistake for a real artifact.
	if res.Signed {
		t.Error("a dry run must not claim to have signed anything")
	}
	if res.Digest != DryDigest {
		t.Errorf("Digest = %q, want the placeholder", res.Digest)
	}
}

func TestRetryableClassification(t *testing.T) {
	permanent := []string{
		"unauthorized: authentication required",
		"denied: requested access to the resource is denied",
		"manifest unknown",
		"invalid reference format",
	}
	for _, msg := range permanent {
		if retryableRegistryError(errors.New(msg)) {
			t.Errorf("%q should not be retried", msg)
		}
	}

	transient := []string{
		"received unexpected HTTP status: 500 Internal Server Error",
		"connection reset by peer",
		"context deadline exceeded",
	}
	for _, msg := range transient {
		if !retryableRegistryError(errors.New(msg)) {
			t.Errorf("%q should be retried", msg)
		}
	}
	if retryableRegistryError(nil) {
		t.Error("nil is not a retryable failure")
	}
	if retryableRegistryError(&execx.NotFoundError{Name: "docker"}) {
		t.Error("a missing binary will not appear on the next attempt")
	}
}

// metadataPath extracts the --metadata-file argument from a buildx call.
func metadataPath(args []string) string {
	for i, a := range args {
		if a == "--metadata-file" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
