package publish

import (
	"strings"
	"testing"

	"go.klarlabs.de/kiln/internal/application/ports"
	"go.klarlabs.de/kiln/internal/domain/config"
	"go.klarlabs.de/kiln/internal/gittest"
)

// pet-medical's web image cannot be built without this: its `npm ci` pulls a
// private package from GitHub Packages behind a BuildKit secret. Which is how
// the gap was found.
func TestBuildPlan_CarriesSecretsAsEnvNames(t *testing.T) {
	plan, err := BuildPlan(config.Artifact{
		Image:   "ghcr.io/acme/web",
		Tags:    []config.Tag{config.TagSHA, config.TagLatest},
		Secrets: map[string]string{"node_auth": "env://NODE_AUTH_TOKEN"},
	}, "c3f7aca11112222333344445555666677778888", "refs/heads/main")
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	// The env:// scheme is resolved once, here — the plan holds the variable
	// NAME, and nothing anywhere holds the value.
	if got := plan.Secrets["node_auth"]; got != "NODE_AUTH_TOKEN" {
		t.Errorf("Secrets[node_auth] = %q, want NODE_AUTH_TOKEN", got)
	}
}

func TestPlan_SecretFlagsAreSortedAndWellFormed(t *testing.T) {
	p := Plan{Secrets: map[string]string{"zed": "ZED_TOKEN", "alpha": "ALPHA_TOKEN"}}

	got := strings.Join(p.SecretFlags(), " ")
	want := "--secret id=alpha,env=ALPHA_TOKEN --secret id=zed,env=ZED_TOKEN"

	if got != want {
		t.Errorf("SecretFlags() = %q\nwant %q", got, want)
	}
}

func TestPlan_NoSecretsMeansNoFlags(t *testing.T) {
	if got := (Plan{}).SecretFlags(); len(got) != 0 {
		t.Errorf("SecretFlags() = %v, want none", got)
	}
}

// The plan is printed to operators and folded into provenance, so its secret
// line is pinned exactly: ids, space-separated, sorted.
//
// An earlier version of this test set the real token in the environment and
// asserted the output did not contain it. That could never fail — the Plan
// holds variable NAMES, never values, so there was nothing to leak. Asserting
// the rendered line is what actually catches a change here.
func TestPlan_StringShowsSecretIDsOnly(t *testing.T) {
	p := Plan{
		Image: "ghcr.io/acme/web", SHATag: "ghcr.io/acme/web:sha-c3f7aca",
		Dockerfile: "web/Dockerfile", Context: "./web", Platforms: []string{"linux/amd64"},
		Secrets: map[string]string{"zed": "ZED_TOKEN", "node_auth": "NODE_AUTH_TOKEN"},
	}

	out := p.String()
	if !strings.Contains(out, "secrets node_auth zed\n") {
		t.Errorf("plan does not render its secret ids in sorted, id-only form:\n%s", out)
	}
}

// An unset variable reaches BuildKit as an empty secret. What happens then is
// the Dockerfile's business — `npm ci` 401s minutes later with a message about
// the registry, and a more forgiving Dockerfile would publish a signed image
// built without the credential. Naming the variable up front cannot be misread.
func TestBuildEnv_RefusesWhenTheVariableIsUnset(t *testing.T) {
	t.Setenv("KILN_TEST_PRESENT", "value")

	_, err := buildEnv(Plan{Secrets: map[string]string{
		"present": "KILN_TEST_PRESENT",
		"missing": "KILN_TEST_DEFINITELY_UNSET",
	}})
	if err == nil {
		t.Fatal("a build with an unset secret variable was allowed to start")
	}

	if !strings.Contains(err.Error(), "KILN_TEST_DEFINITELY_UNSET") {
		t.Errorf("the error does not name the missing variable: %v", err)
	}

	if strings.Contains(err.Error(), "KILN_TEST_PRESENT") {
		t.Errorf("the error names a variable that was present: %v", err)
	}
}

func TestBuildEnv_EnablesBuildKitWhenSecretsAreUsed(t *testing.T) {
	t.Setenv("KILN_TEST_TOKEN", "value")

	env, err := buildEnv(Plan{Secrets: map[string]string{"tok": "KILN_TEST_TOKEN"}})
	if err != nil {
		t.Fatalf("buildEnv: %v", err)
	}

	// --secret is a BuildKit flag. An older daemon, or DOCKER_BUILDKIT=0 in
	// the operator's shell, would otherwise fail on an unknown flag.
	var found bool

	for _, kv := range env {
		if kv == "DOCKER_BUILDKIT=1" {
			found = true
		}
	}

	if !found {
		t.Error("DOCKER_BUILDKIT=1 is not set for a build that uses --secret")
	}
}

// Every pipeline written before this existed has no secrets, and must keep
// inheriting the parent environment exactly as it did.
func TestBuildEnv_NoSecretsInheritsTheParentEnvironment(t *testing.T) {
	env, err := buildEnv(Plan{})
	if err != nil {
		t.Fatalf("buildEnv: %v", err)
	}

	if env != nil {
		t.Errorf("buildEnv() = %v, want nil (inherit) when there are no secrets", env)
	}
}

// THE FLAG MUST REACH docker build.
//
// Every other test here checks the renderer — that SecretFlags() produces the
// right strings. None of them notices if the call to it is deleted from the
// build path, which is the whole feature. Mutation testing found exactly that
// gap: removing `args = append(args, plan.SecretFlags()...)` left the suite
// green, and the web image would have built without its credential.
func TestPublish_BuildCarriesTheSecretFlag(t *testing.T) {
	t.Setenv("KILN_TEST_NODE_AUTH", "value")

	repo := gittest.New(t)
	head := repo.Commit("first", "Dockerfile", "FROM scratch\n")
	fake := dockerFake(t)

	art := cfg(config.TagSHA, config.TagLatest)
	art.Secrets = map[string]string{"node_auth": "env://KILN_TEST_NODE_AUTH"}

	if _, err := newPublisher(fake).Publish(t.Context(), ports.PublishRequest{
		RepoDir: repo.Dir, SHA: head, Ref: "refs/heads/main", Artifact: art,
	}); err != nil {
		t.Fatalf("Publish: %v\n%s", err, fake.Transcript())
	}

	build := fake.Find("docker build")
	if build == nil {
		t.Fatalf("no build ran: %s", fake.Transcript())
	}

	if !strings.Contains(build.String(), "--secret id=node_auth,env=KILN_TEST_NODE_AUTH") {
		t.Errorf("docker build did not carry the secret:\n%s", build.String())
	}
}

// And a build that declares a secret it cannot supply must not start at all,
// rather than producing a signed image built without the credential.
func TestPublish_RefusesWhenASecretVariableIsUnset(t *testing.T) {
	repo := gittest.New(t)
	head := repo.Commit("first", "Dockerfile", "FROM scratch\n")
	fake := dockerFake(t)

	art := cfg(config.TagSHA, config.TagLatest)
	art.Secrets = map[string]string{"node_auth": "env://KILN_TEST_UNSET_ENTIRELY"}

	_, err := newPublisher(fake).Publish(t.Context(), ports.PublishRequest{
		RepoDir: repo.Dir, SHA: head, Ref: "refs/heads/main", Artifact: art,
	})
	if err == nil {
		t.Fatal("published with an unset secret variable")
	}

	if fake.Ran("docker push") {
		t.Errorf("an image was pushed despite the missing secret: %s", fake.Transcript())
	}
}
