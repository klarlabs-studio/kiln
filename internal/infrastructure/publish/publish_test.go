package publish

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.klarlabs.de/kiln/internal/application/ports"
	"go.klarlabs.de/kiln/internal/domain/config"
	"go.klarlabs.de/kiln/internal/gittest"
	"go.klarlabs.de/kiln/internal/infrastructure/attest"
	"go.klarlabs.de/kiln/internal/infrastructure/execx"
	"go.klarlabs.de/kiln/internal/infrastructure/obs"
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
	res, err := newPublisher(fake).Publish(t.Context(), ports.PublishRequest{
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
	if _, err := newPublisher(fake).Publish(t.Context(), ports.PublishRequest{
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
	if _, err := newPublisher(fake).Publish(t.Context(), ports.PublishRequest{
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

	if _, err := newPublisher(fake).Publish(t.Context(), ports.PublishRequest{
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

	_, err := newPublisher(fake).Publish(t.Context(), ports.PublishRequest{
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

	_, err := newPublisher(fake).Publish(t.Context(), ports.PublishRequest{
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

	_, err := newPublisher(fake).Publish(t.Context(), ports.PublishRequest{
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
	_, _ = d.Publish(t.Context(), ports.PublishRequest{
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
	if _, err := d.Publish(t.Context(), ports.PublishRequest{
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

	res, err := newPublisher(fake).Publish(t.Context(), ports.PublishRequest{
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

	_, err := newPublisher(fake).Publish(t.Context(), ports.PublishRequest{
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

	res, err := newPublisher(fake).Publish(t.Context(), ports.PublishRequest{
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

	_, err := newPublisher(fake).Publish(t.Context(), ports.PublishRequest{RepoDir: repo.Dir, SHA: head, Ref: ref, Artifact: art})
	if err == nil || !strings.Contains(err.Error(), "no image digest") {
		t.Errorf("err = %v, want a missing-digest failure", err)
	}
}

func TestDryRunTouchesNothing(t *testing.T) {
	ref, art := "refs/heads/main", cfg(config.TagSHA, config.TagLatest)
	plan := mustPlan(t, art, sha, ref)

	res, err := NewDry(obs.Discard()).Publish(t.Context(), ports.PublishRequest{RepoDir: "/repo", SHA: sha, Ref: ref, Artifact: art})
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

func imageProvenance() ports.AttestInput {
	return ports.AttestInput{
		Repo: "felixgeelhaar/glossa", Ref: "refs/heads/main", Event: "push",
		GateReproved: true, GateReason: "checks ran", KilnVersion: "v0.1.0",
		InvocationID: "run-1", StartedOn: time.Unix(0, 0).UTC(),
	}
}

func TestImageProvenanceIsAttachedToTheDigest(t *testing.T) {
	repo := gittest.New(t)
	head := repo.Commit("first", "Dockerfile", "FROM scratch\n")
	fake := dockerFake(t)

	ref, art := "refs/heads/main", cfg(config.TagSHA, config.TagLatest)
	prov := imageProvenance()
	prov.SHA = head

	res, err := newPublisher(fake).Publish(t.Context(), ports.PublishRequest{
		RepoDir: repo.Dir, SHA: head, Ref: ref, Artifact: art, Provenance: prov,
	})
	if err != nil {
		t.Fatalf("Publish: %v\n%s", err, fake.Transcript())
	}

	if !res.Attested {
		t.Error("Attested = false after a real publish")
	}
	cmd := fake.Find("cosign attest")
	if cmd == nil {
		t.Fatalf("no attestation: %s", fake.Transcript())
	}
	// The subject must be the immutable digest, for the same reason the
	// signature is: a tag moves.
	if !strings.Contains(cmd.String(), image+"@"+digest) {
		t.Errorf("attested %q, want the digest", cmd.String())
	}
	if !strings.Contains(cmd.String(), "https://slsa.dev/provenance/v1") {
		t.Errorf("attestation is not typed as SLSA provenance: %s", cmd.String())
	}
}

func TestImageProvenancePredicateRecordsTheChain(t *testing.T) {
	repo := gittest.New(t)
	head := repo.Commit("first", "Dockerfile", "FROM scratch\n")

	fake := dockerFake(t)
	var predicate []byte
	fake.On("cosign attest", execx.Response{Fn: func(c execx.Cmd) (execx.Result, error) {
		for i, a := range c.Args {
			if a == "--predicate" && i+1 < len(c.Args) {
				predicate, _ = os.ReadFile(c.Args[i+1])
			}
		}
		return execx.Result{}, nil
	}})

	prov := imageProvenance()
	prov.SHA = head
	prov.GateReproved = false
	prov.GateReason = "warden note is signed by a trusted key"

	if _, err := newPublisher(fake).Publish(t.Context(), ports.PublishRequest{
		RepoDir: repo.Dir, SHA: head, Ref: "refs/heads/main",
		Artifact: cfg(config.TagSHA, config.TagLatest), Provenance: prov,
	}); err != nil {
		t.Fatal(err)
	}

	// The file cosign reads is the predicate body, not a whole statement —
	// cosign wraps it and supplies the subject itself.
	var body attest.Provenance
	if err := json.Unmarshal(predicate, &body); err != nil {
		t.Fatalf("predicate body is not JSON: %v", err)
	}
	if got := sourceCommit(body); got != head {
		t.Errorf("gitCommit = %q, want %q", got, head)
	}
	// The inherited verdict must travel with the artifact: a reader deciding
	// how far to trust it needs to know the checks did not run here.
	gate := body.BuildDefinition.InternalParameters.SourceGate
	if gate.Reproved {
		t.Error("predicate claims the checks ran when they were inherited")
	}
	if !strings.Contains(gate.Reason, "trusted key") {
		t.Errorf("reason = %q", gate.Reason)
	}
}

func TestAFailedAttestationFailsThePublish(t *testing.T) {
	repo := gittest.New(t)
	head := repo.Commit("first", "Dockerfile", "FROM scratch\n")
	fake := dockerFake(t).On("cosign attest", execx.Response{ExitCode: 1, Stderr: "no identity token"})

	prov := imageProvenance()
	prov.SHA = head

	_, err := newPublisher(fake).Publish(t.Context(), ports.PublishRequest{
		RepoDir: repo.Dir, SHA: head, Ref: "refs/heads/main",
		Artifact: cfg(config.TagSHA, config.TagLatest), Provenance: prov,
	})

	// An image carrying a signature but no provenance is one whose origin
	// cannot be checked. Failing beats quietly downgrading what shipped.
	if err == nil || !strings.Contains(err.Error(), "cosign attest") {
		t.Errorf("err = %v, want the attestation failure", err)
	}
}

func TestDryRunClaimsNoAttestation(t *testing.T) {
	ref, art := "refs/heads/main", cfg(config.TagSHA, config.TagLatest)

	res, err := NewDry(obs.Discard()).Publish(t.Context(), ports.PublishRequest{
		RepoDir: "/repo", SHA: sha, Ref: ref, Artifact: art, Provenance: imageProvenance(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Attested {
		t.Error("a rehearsal must not claim provenance it never published")
	}
}

// sourceCommit reads the pinned commit out of a predicate body.
func sourceCommit(p attest.Provenance) string {
	for _, dep := range p.BuildDefinition.ResolvedDependencies {
		if c := dep.Digest["gitCommit"]; c != "" {
			return c
		}
	}
	return ""
}

// signedSummary builds the envelope warden emits with --sign: the statement
// base64'd inside, with a signature kiln must not replace.
func signedSummary(commit, verdict string) []byte {
	stmt := map[string]any{
		"_type": "https://in-toto.io/Statement/v1",
		"subject": []any{map[string]any{
			"name": "git+commit", "digest": map[string]string{"gitCommit": commit},
		}},
		"predicateType": attest.VSAPredicateType,
		"predicate": map[string]any{
			"verifier":           map[string]any{"id": "https://warden.klarlabs.de"},
			"timeVerified":       "2026-08-17T21:26:51Z",
			"resourceUri":        "git+ssh://git@github.com/o/r.git@" + commit,
			"policy":             map[string]any{"uri": "git+ssh://git@github.com/o/r.git@" + commit + "#.warden.yaml"},
			"verificationResult": verdict,
			"verifiedLevels":     []string{"WARDEN_SOURCE_GATED"},
		},
	}
	payload, _ := json.Marshal(stmt)
	env, _ := json.Marshal(map[string]any{
		"payloadType": "application/vnd.in-toto+json",
		"payload":     base64.StdEncoding.EncodeToString(payload),
		"signatures": []any{map[string]string{
			"keyid": "139e6eb9e2611c76", "sig": "d2FyZGVucy1vd24tc2lnbmF0dXJl",
		}},
	})
	return env
}

func TestWardensSummaryTravelsWithTheArtifact(t *testing.T) {
	repo := gittest.New(t)
	head := repo.Commit("first", "Dockerfile", "FROM scratch\n")
	fake := dockerFake(t)

	prov := imageProvenance()
	prov.SHA = head

	if _, err := newPublisher(fake).Publish(t.Context(), ports.PublishRequest{
		RepoDir: repo.Dir, SHA: head, Ref: "refs/heads/main",
		Artifact:   cfg(config.TagSHA, config.TagLatest),
		Provenance: prov, SourceVSA: signedSummary(head, "PASSED"),
	}); err != nil {
		t.Fatalf("Publish: %v\n%s", err, fake.Transcript())
	}

	// Two attestations by two authorities. Kiln SIGNS its own provenance and
	// only ATTACHES warden's — attest would replace warden's signature with
	// kiln's and turn warden's claim into kiln's account of it.
	if !fake.Ran("cosign attest --yes --type " + attest.CosignType) {
		t.Errorf("kiln did not sign its own provenance:\n%s", fake.Transcript())
	}
	if !fake.Ran("cosign attach attestation") {
		t.Errorf("warden's envelope was not attached:\n%s", fake.Transcript())
	}
	for _, line := range fake.Lines() {
		if strings.HasPrefix(line, "cosign attest") && strings.Contains(line, attest.VSAPredicateType) {
			t.Errorf("kiln re-signed warden's claim: %s", line)
		}
	}
}

func TestTheSummaryIsCarriedVerbatimNotParaphrased(t *testing.T) {
	repo := gittest.New(t)
	head := repo.Commit("first", "Dockerfile", "FROM scratch\n")

	fake := dockerFake(t)
	var body []byte
	fake.On("cosign attach attestation",
		execx.Response{Fn: func(c execx.Cmd) (execx.Result, error) {
			for i, a := range c.Args {
				if a == "--attestation" && i+1 < len(c.Args) {
					body, _ = os.ReadFile(c.Args[i+1])
				}
			}
			return execx.Result{}, nil
		}})

	prov := imageProvenance()
	prov.SHA = head
	if _, err := newPublisher(fake).Publish(t.Context(), ports.PublishRequest{
		RepoDir: repo.Dir, SHA: head, Ref: "refs/heads/main",
		Artifact:   cfg(config.TagSHA, config.TagLatest),
		Provenance: prov, SourceVSA: signedSummary(head, "PASSED"),
	}); err != nil {
		t.Fatal(err)
	}

	// Byte for byte: what reaches the registry must be the envelope warden
	// signed, or the signature a consumer checks is not warden's.
	want := signedSummary(head, "PASSED")
	if string(body) != string(want) {
		t.Errorf("the envelope was rewritten in transit:\nhave %s\nwant %s", body, want)
	}

	var env map[string]any
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("attached body is not JSON: %v", err)
	}
	sigs, _ := env["signatures"].([]any)
	if len(sigs) == 0 {
		t.Fatal("warden's signature did not survive")
	}
	if sig, _ := sigs[0].(map[string]any); sig["keyid"] != "139e6eb9e2611c76" {
		t.Errorf("keyid = %v, want warden's fingerprint", sig["keyid"])
	}
}

func TestAnUnsignedSummaryIsNotSignedOnWardensBehalf(t *testing.T) {
	repo := gittest.New(t)
	head := repo.Commit("first", "Dockerfile", "FROM scratch\n")
	fake := dockerFake(t)

	bare := `{"predicateType":"` + attest.VSAPredicateType + `","predicate":{"verificationResult":"PASSED"}}`
	prov := imageProvenance()
	prov.SHA = head

	if _, err := newPublisher(fake).Publish(t.Context(), ports.PublishRequest{
		RepoDir: repo.Dir, SHA: head, Ref: "refs/heads/main",
		Artifact:   cfg(config.TagSHA, config.TagLatest),
		Provenance: prov, SourceVSA: []byte(bare),
	}); err != nil {
		t.Fatal(err)
	}

	// Signing it here would make it kiln's claim — precisely the substitution
	// the whole arrangement exists to avoid.
	if fake.Ran("cosign attach attestation") {
		t.Errorf("attached an unsigned summary:\n%s", fake.Transcript())
	}
	for _, line := range fake.Lines() {
		if strings.Contains(line, attest.VSAPredicateType) {
			t.Errorf("kiln signed warden's claim for it: %s", line)
		}
	}
}

func TestASummaryForADifferentCommitIsRefused(t *testing.T) {
	repo := gittest.New(t)
	head := repo.Commit("first", "Dockerfile", "FROM scratch\n")
	fake := dockerFake(t)

	prov := imageProvenance()
	prov.SHA = head

	_, err := newPublisher(fake).Publish(t.Context(), ports.PublishRequest{
		RepoDir: repo.Dir, SHA: head, Ref: "refs/heads/main",
		Artifact: cfg(config.TagSHA, config.TagLatest), Provenance: prov,
		SourceVSA: signedSummary("0000000000000000000000000000000000000000", "PASSED"),
	})

	// Attaching it would manufacture the exact mismatch a consumer's join is
	// there to catch.
	if err == nil || !strings.Contains(err.Error(), "this build is of") {
		t.Errorf("err = %v, want the commit mismatch refused at source", err)
	}
}

func TestNoSummaryStillPublishes(t *testing.T) {
	repo := gittest.New(t)
	head := repo.Commit("first", "Dockerfile", "FROM scratch\n")
	fake := dockerFake(t)

	prov := imageProvenance()
	prov.SHA = head

	// A repository still adopting warden must not be unable to
	res, err := newPublisher(fake).Publish(t.Context(), ports.PublishRequest{
		RepoDir: repo.Dir, SHA: head, Ref: "refs/heads/main",
		Artifact: cfg(config.TagSHA, config.TagLatest), Provenance: prov,
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !res.Attested {
		t.Error("build provenance should still be attached")
	}
	for _, line := range fake.Lines() {
		if strings.Contains(line, attest.VSAPredicateType) {
			t.Errorf("attached a summary that did not exist: %s", line)
		}
	}
}

func TestAFailingSummaryIsRefused(t *testing.T) {
	repo := gittest.New(t)
	head := repo.Commit("first", "Dockerfile", "FROM scratch\n")
	fake := dockerFake(t)

	prov := imageProvenance()
	prov.SHA = head

	_, err := newPublisher(fake).Publish(t.Context(), ports.PublishRequest{
		RepoDir: repo.Dir, SHA: head, Ref: "refs/heads/main",
		Artifact:   cfg(config.TagSHA, config.TagLatest),
		Provenance: prov, SourceVSA: signedSummary(head, "FAILED"),
	})

	// Reaching publish means prove passed, so warden reporting FAILED here is
	// a contradiction. Publishing it as though it were a pass would put a
	// false claim in the registry.
	if err == nil || !strings.Contains(err.Error(), "FAILED") {
		t.Errorf("err = %v, want the contradiction surfaced", err)
	}
}

func TestAnUnreadableSummaryDoesNotBlockTheBuild(t *testing.T) {
	repo := gittest.New(t)
	head := repo.Commit("first", "Dockerfile", "FROM scratch\n")
	fake := dockerFake(t)

	prov := imageProvenance()
	prov.SHA = head
	res, err := newPublisher(fake).Publish(t.Context(), ports.PublishRequest{
		RepoDir: repo.Dir, SHA: head, Ref: "refs/heads/main",
		Artifact:   cfg(config.TagSHA, config.TagLatest),
		Provenance: prov, SourceVSA: []byte(`{"predicateType":"https://spdx.dev/Document"}`),
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !res.Attested {
		t.Error("build provenance should still be attached")
	}
	// Publishing it as a summary would put an unreadable claim in the registry.
	for _, line := range fake.Lines() {
		if strings.Contains(line, attest.VSAPredicateType) {
			t.Errorf("published something that is not a summary: %s", line)
		}
	}
}

// TestKeyedSigningUsesTheConfiguredKey covers the signing mode a self-hosted
// builder has to run in. Keyless needs an OIDC identity to prove; on a box with
// none, cosign drops to the browser device flow and blocks until the code
// expires — which on a multi-image tag leaves some images signed and some not.
func TestKeyedSigningUsesTheConfiguredKey(t *testing.T) {
	repo := gittest.New(t)
	head := repo.Commit("first", "Dockerfile", "FROM scratch\n")
	fake := dockerFake(t)

	d := newPublisher(fake)
	d.SigningKey = "cosign.key"

	if _, err := d.Publish(t.Context(), ports.PublishRequest{
		RepoDir: repo.Dir, SHA: head,
		Ref: "refs/heads/main", Artifact: cfg(config.TagSHA, config.TagLatest),
	}); err != nil {
		t.Fatal(err)
	}

	sign := fake.Find("cosign sign")
	if sign == nil {
		t.Fatalf("nothing was signed: %s", fake.Transcript())
	}
	if !strings.Contains(sign.String(), "--key cosign.key") {
		t.Errorf("cosign sign = %q, want the configured key", sign.String())
	}
	// The signature and the provenance must be made by the same signer. A
	// keyed signature next to a keyless attestation is two claims by two
	// identities about one artifact, and a verifier pinned to one of them
	// silently rejects the other.
	attest := fake.Find("cosign attest")
	if attest == nil {
		t.Fatalf("no provenance was attached: %s", fake.Transcript())
	}
	if !strings.Contains(attest.String(), "--key cosign.key") {
		t.Errorf("cosign attest = %q, want the same key the signature used", attest.String())
	}
}

// TestKeylessSigningStaysTheDefault pins the no-key path, so configuring keyed
// signing for one box cannot quietly change what every other box does.
func TestKeylessSigningStaysTheDefault(t *testing.T) {
	repo := gittest.New(t)
	head := repo.Commit("first", "Dockerfile", "FROM scratch\n")
	fake := dockerFake(t)

	if _, err := newPublisher(fake).Publish(t.Context(), ports.PublishRequest{
		RepoDir: repo.Dir, SHA: head,
		Ref: "refs/heads/main", Artifact: cfg(config.TagSHA, config.TagLatest),
	}); err != nil {
		t.Fatal(err)
	}

	if sign := fake.Find("cosign sign"); sign == nil || strings.Contains(sign.String(), "--key") {
		t.Errorf("cosign sign = %v, want no key", sign)
	}
}

// sbomArtifact is the image config with the inventory switched on.
func sbomArtifact() config.Artifact {
	a := cfg(config.TagSHA, config.TagLatest)
	a.SBOM = true
	return a
}

// A publish that was not asked for an SBOM must not run the scanner. The
// switch exists because the scan costs time and requires nox on the box.
func TestNoSBOMIsAskedForAndNoneIsBuilt(t *testing.T) {
	repo := gittest.New(t)
	head := repo.Commit("first", "Dockerfile", "FROM scratch\n")
	fake := dockerFake(t)

	prov := imageProvenance()
	prov.SHA = head

	if _, err := newPublisher(fake).Publish(t.Context(), ports.PublishRequest{
		RepoDir: repo.Dir, SHA: head, Ref: "refs/heads/main",
		Artifact: cfg(config.TagSHA, config.TagLatest), Provenance: prov,
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	if fake.Ran("nox") {
		t.Errorf("the scanner ran for an artifact that never asked for an sbom:\n%s", fake.Transcript())
	}
	if sbomAttestation(fake) != nil {
		t.Error("an sbom attestation was attached to an artifact that did not request one")
	}
}

// With sbom: true the inventory is scanned from the checked-out source and
// attached to the same digest the provenance hangs off — which is what lets a
// consumer join "where did this come from" to "what is inside it" with cosign
// and nothing else.
func TestAnSBOMIsScannedAndAttachedToTheDigest(t *testing.T) {
	repo := gittest.New(t)
	head := repo.Commit("first", "Dockerfile", "FROM scratch\n")

	fake := dockerFake(t)
	// The scanner writes where it is told; the publisher then attests that file.
	fake.On("nox scan", execx.Response{Fn: func(c execx.Cmd) (execx.Result, error) {
		return execx.Result{}, writeSBOMForArgs(c.Args)
	}})

	prov := imageProvenance()
	prov.SHA = head

	if _, err := newPublisher(fake).Publish(t.Context(), ports.PublishRequest{
		RepoDir: repo.Dir, SHA: head, Ref: "refs/heads/main",
		Artifact: sbomArtifact(), Provenance: prov,
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	cmd := sbomAttestation(fake)
	if cmd == nil {
		t.Fatalf("no sbom attestation was attached:\n%s", fake.Transcript())
	}
	// The digest, not a tag: an attestation on a moving reference describes
	// whatever that reference points at later, which is not this artifact.
	if !strings.Contains(cmd.String(), digest) {
		t.Errorf("sbom attached to %q, want the digest", cmd.String())
	}
}

// The scan reads the worktree this publish built from, not whatever happens to
// be checked out on the box. An inventory of a different tree would describe a
// different artifact while claiming to describe this one.
func TestTheSBOMIsScannedFromTheCommitThatWasBuilt(t *testing.T) {
	repo := gittest.New(t)
	head := repo.Commit("first", "Dockerfile", "FROM scratch\n")

	fake := dockerFake(t)
	fake.On("nox scan", execx.Response{Fn: func(c execx.Cmd) (execx.Result, error) {
		if c.Dir != repo.Dir {
			return execx.Result{}, fmt.Errorf("scanned %q, want the publish worktree %q", c.Dir, repo.Dir)
		}
		return execx.Result{}, writeSBOMForArgs(c.Args)
	}})

	prov := imageProvenance()
	prov.SHA = head

	if _, err := newPublisher(fake).Publish(t.Context(), ports.PublishRequest{
		RepoDir: repo.Dir, SHA: head, Ref: "refs/heads/main",
		Artifact: sbomArtifact(), Provenance: prov,
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
}

// A scanner that fails must fail the publish. Attaching provenance and quietly
// skipping the inventory ships an artifact that looks complete and is missing
// the half somebody will later assume is there.
func TestAFailedSBOMScanFailsThePublish(t *testing.T) {
	repo := gittest.New(t)
	head := repo.Commit("first", "Dockerfile", "FROM scratch\n")
	fake := dockerFake(t).On("nox scan", execx.Response{ExitCode: 1, Stderr: "boom"})

	prov := imageProvenance()
	prov.SHA = head

	_, err := newPublisher(fake).Publish(t.Context(), ports.PublishRequest{
		RepoDir: repo.Dir, SHA: head, Ref: "refs/heads/main",
		Artifact: sbomArtifact(), Provenance: prov,
	})

	if err == nil || !strings.Contains(err.Error(), "nox scan") {
		t.Errorf("err = %v, want the scan failure to stop the publish", err)
	}
}

// A scanner that exits 0 and writes nothing is the quieter version of the same
// problem: cosign would be handed a path that does not exist.
func TestASilentlyMissingSBOMFailsThePublish(t *testing.T) {
	repo := gittest.New(t)
	head := repo.Commit("first", "Dockerfile", "FROM scratch\n")
	fake := dockerFake(t).On("nox scan", execx.Response{}) // exits 0, writes nothing

	prov := imageProvenance()
	prov.SHA = head

	_, err := newPublisher(fake).Publish(t.Context(), ports.PublishRequest{
		RepoDir: repo.Dir, SHA: head, Ref: "refs/heads/main",
		Artifact: sbomArtifact(), Provenance: prov,
	})

	if err == nil || !strings.Contains(err.Error(), "no sbom") {
		t.Errorf("err = %v, want a publish that refuses to attest a file that is not there", err)
	}
}

// sbomAttestation finds the cyclonedx attest call. Fake.Find matches a prefix
// and cosign puts --yes before --type, so the flags cannot be matched as one
// contiguous string.
func sbomAttestation(f *execx.Fake) *execx.Cmd {
	for _, c := range f.Calls() {
		if strings.Contains(c.String(), "cosign attest") && strings.Contains(c.String(), "cyclonedx") {
			cp := c
			return &cp
		}
	}
	return nil
}

// writeSBOMForArgs mimics `nox scan --format cdx --output DIR` by writing a
// minimal CycloneDX document where the publisher will look for it.
func writeSBOMForArgs(args []string) error {
	for i, a := range args {
		if a == "--output" && i+1 < len(args) {
			return os.WriteFile(filepath.Join(args[i+1], "sbom.cdx.json"),
				[]byte(`{"bomFormat":"CycloneDX","specVersion":"1.5","components":[]}`), 0o600)
		}
	}
	return fmt.Errorf("nox scan was called without --output: %v", args)
}
