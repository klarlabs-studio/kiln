package envconfig

import (
	"strings"
	"testing"
)

func TestLoad_ReadsGitHubAppCredentials(t *testing.T) {
	t.Setenv("KILN_GITHUB_APP_ID", "1234")
	t.Setenv("KILN_GITHUB_APP_INSTALLATION_ID", "42")
	t.Setenv("KILN_GITHUB_APP_KEY_FILE", "/etc/kiln/app.pem")

	env := Load()
	if env.GitHubAppID != "1234" || env.GitHubAppInstallationID != "42" || env.GitHubAppKeyFile != "/etc/kiln/app.pem" {
		t.Errorf("env = %+v", env)
	}
	if !env.GitHubApp() {
		t.Error("a complete app configuration was not recognised")
	}
}

// Two of the three set is a typo or a half-finished setup. Running on
// would silently fall back to a personal token, or to none, and the box
// would look configured while treating every pull request as a fork.
func TestValidate_RefusesAHalfConfiguredGitHubApp(t *testing.T) {
	for missing, env := range map[string]Env{
		"KILN_GITHUB_APP_KEY_FILE": {GitHubAppID: "1234", GitHubAppInstallationID: "42"},
		"KILN_GITHUB_APP_ID":       {GitHubAppKeyFile: "/etc/kiln/app.pem"},
	} {
		err := env.Validate()
		if err == nil {
			t.Fatalf("%+v passed validation", env)
		}
		if !strings.Contains(err.Error(), missing) {
			t.Errorf("message does not name %s: %v", missing, err)
		}
	}
	if (Env{}).Validate() != nil {
		t.Error("no app configuration at all must stay valid")
	}
}

// The installation is optional: an app installed on two organisations has
// two, and a box watching both cannot name one for every repository.
func TestAnAppWithoutAnInstallationIDIsConfigured(t *testing.T) {
	env := Env{GitHubAppID: "1234", GitHubAppKeyFile: "/etc/kiln/app.pem"}
	if err := env.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
	if !env.GitHubApp() {
		t.Error("an app with no installation id was not recognised")
	}
}
