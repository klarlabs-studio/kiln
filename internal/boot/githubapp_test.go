package boot

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.klarlabs.de/kiln/internal/infrastructure/envconfig"
	"go.klarlabs.de/kiln/internal/infrastructure/github"
	"go.klarlabs.de/kiln/internal/infrastructure/obs"
)

func writeAppKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "app.pem")
	data := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestGitHubApp_BuildsATokenSourceFromTheEnvironment(t *testing.T) {
	env := envconfig.Env{GitHubAppID: "1234", GitHubAppInstallationID: "42", GitHubAppKeyFile: writeAppKey(t)}

	app, err := githubApp(env)
	if err != nil {
		t.Fatalf("githubApp: %v", err)
	}
	if app == nil || app.AppID != "1234" || app.InstallationID != "42" || app.Key == nil {
		t.Fatalf("app = %+v", app)
	}

	c := buildClient(env, github.Repo{Owner: "o", Name: "r"}, app, obs.Discard())
	if !c.Enabled() || c.Tokens == nil {
		t.Error("an app-configured box has no usable GitHub client")
	}
}

func TestGitHubApp_NoneConfiguredIsNil(t *testing.T) {
	app, err := githubApp(envconfig.Env{})
	if err != nil || app != nil {
		t.Errorf("app = %v, err = %v", app, err)
	}
	if buildClient(envconfig.Env{}, github.Repo{Owner: "o", Name: "r"}, nil, obs.Discard()) != nil {
		t.Error("a client was built with no credential at all")
	}
}

// A key file kiln cannot read must stop the boot, naming the variable: a
// box that fell back to "no token" would look configured and post nothing.
func TestGitHubApp_AnUnreadableKeyStopsTheBoot(t *testing.T) {
	env := envconfig.Env{GitHubAppID: "1234", GitHubAppInstallationID: "42",
		GitHubAppKeyFile: filepath.Join(t.TempDir(), "missing.pem")}

	if _, err := githubApp(env); err == nil || !strings.Contains(err.Error(), "KILN_GITHUB_APP_KEY_FILE") {
		t.Errorf("err = %v, want one naming KILN_GITHUB_APP_KEY_FILE", err)
	}
}
