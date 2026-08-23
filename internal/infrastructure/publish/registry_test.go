package publish_test

import (
	"os"
	"path/filepath"
	"testing"

	"go.klarlabs.de/kiln/internal/infrastructure/publish"
)

func TestRegistryOfFollowsDockersOwnRule(t *testing.T) {
	for image, want := range map[string]string{
		"ghcr.io/klarlabs-studio/kiln":  "ghcr.io",
		"docker.io/klarlabs/glossa-api": "docker.io",
		// No host at all: docker reads these as Docker Hub, so kiln must too,
		// or a pipeline pushing to `you/app` would be checked against a
		// registry that does not exist.
		"klarlabs/glossa-api": "docker.io",
		"nginx":               "docker.io",
		// A host is a host because it looks like one — a dot, a colon, or
		// exactly localhost. This is the rule that keeps `you/app` from being
		// read as a registry called "you".
		"localhost:5000/app":        "localhost:5000",
		"localhost/app":             "localhost",
		"registry.example.com/team": "registry.example.com",
		"":                          "",
	} {
		if got := publish.RegistryOf(image); got != want {
			t.Errorf("RegistryOf(%q) = %q, want %q", image, got, want)
		}
	}
}

// dockerConfigDir writes a config.json and points DOCKER_CONFIG at it.
func dockerConfigDir(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if body != "" {
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("DOCKER_CONFIG", dir)
}

func TestALoggedInRegistryIsFound(t *testing.T) {
	dockerConfigDir(t, `{"auths": {"ghcr.io": {}}}`)

	if got := publish.CheckRegistryCredentials("ghcr.io"); got != publish.CredentialsPresent {
		t.Errorf("state = %v, want present", got)
	}
}

func TestDockerHubIsFoundUnderTheNameDockerActuallyWrites(t *testing.T) {
	// docker records Docker Hub as a v1 URL and always has. A check looking up
	// "docker.io" finds nothing and reports a missing login that is right
	// there — which is worse than not checking, because the operator then goes
	// looking for a problem that does not exist.
	dockerConfigDir(t, `{"auths": {"https://index.docker.io/v1/": {"auth": "eHg6eXk="}}}`)

	if got := publish.CheckRegistryCredentials("docker.io"); got != publish.CredentialsPresent {
		t.Errorf("state = %v, want present", got)
	}
}

func TestARegistryTheConfigDoesNotListIsMissing(t *testing.T) {
	dockerConfigDir(t, `{"auths": {"ghcr.io": {}}, "credsStore": "desktop"}`)

	// The evidence is positive: there is a config, it names registries, and
	// this is not one of them.
	if got := publish.CheckRegistryCredentials("docker.io"); got != publish.CredentialsMissing {
		t.Errorf("state = %v, want missing", got)
	}
}

func TestACredentialHelperCountsAsPresent(t *testing.T) {
	// A helper's entire job is to hold the credential somewhere this file
	// cannot see, so absence from `auths` proves nothing about it.
	dockerConfigDir(t, `{"credHelpers": {"docker.io": "osxkeychain"}}`)

	if got := publish.CheckRegistryCredentials("docker.io"); got != publish.CredentialsPresent {
		t.Errorf("state = %v, want present", got)
	}
}

func TestAnEmptyConfigWithAStoreSaysNothing(t *testing.T) {
	// Everything lives in the keychain and nothing is listed. Reporting "not
	// logged in" here would be a confident guess, and a doctor that cries wolf
	// is one nobody reads.
	dockerConfigDir(t, `{"auths": {}, "credsStore": "desktop"}`)

	if got := publish.CheckRegistryCredentials("docker.io"); got != publish.CredentialsUnknown {
		t.Errorf("state = %v, want unknown", got)
	}
}

func TestNoConfigAtAllIsUnknownNotMissing(t *testing.T) {
	dockerConfigDir(t, "")

	// A CI runner may inject credentials in a way this cannot see.
	if got := publish.CheckRegistryCredentials("ghcr.io"); got != publish.CredentialsUnknown {
		t.Errorf("state = %v, want unknown", got)
	}
}

func TestAnUnreadableConfigIsUnknown(t *testing.T) {
	dockerConfigDir(t, `{"auths": `)

	// Guessing from a file that did not parse is how a check produces a
	// confident wrong answer.
	if got := publish.CheckRegistryCredentials("ghcr.io"); got != publish.CredentialsUnknown {
		t.Errorf("state = %v, want unknown", got)
	}
}
