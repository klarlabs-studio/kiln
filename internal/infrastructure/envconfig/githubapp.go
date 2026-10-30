package envconfig

import (
	"fmt"
)

// GitHubApp reports whether a GitHub App is configured. The installation id
// is optional: without it, kiln asks GitHub which installation covers the
// repository, which is what a box watching repositories in more than one
// organisation needs.
func (e Env) GitHubApp() bool {
	return e.GitHubAppID != "" && e.GitHubAppKeyFile != ""
}

// validateGitHubApp refuses a partial app configuration, naming what is
// missing. Running on would quietly fall back to another credential, or to
// none, and the box would look configured while posting no checks.
func (e Env) validateGitHubApp() error {
	switch {
	case e.GitHubAppID == "" && e.GitHubAppKeyFile == "" && e.GitHubAppInstallationID == "":
		return nil
	case e.GitHubAppID == "" && e.GitHubAppKeyFile == "":
		return fmt.Errorf("github app: KILN_GITHUB_APP_INSTALLATION_ID set but " +
			"KILN_GITHUB_APP_ID, KILN_GITHUB_APP_KEY_FILE missing; set both")
	case e.GitHubAppID == "":
		return fmt.Errorf("github app: KILN_GITHUB_APP_KEY_FILE set but KILN_GITHUB_APP_ID missing; set both")
	case e.GitHubAppKeyFile == "":
		return fmt.Errorf("github app: KILN_GITHUB_APP_ID set but KILN_GITHUB_APP_KEY_FILE missing; set both")
	}
	return nil
}
