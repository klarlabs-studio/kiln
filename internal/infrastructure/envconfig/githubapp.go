package envconfig

import (
	"fmt"
	"strings"
)

// GitHubApp reports whether a GitHub App is configured.
func (e Env) GitHubApp() bool {
	return e.GitHubAppID != "" && e.GitHubAppInstallationID != "" && e.GitHubAppKeyFile != ""
}

// validateGitHubApp refuses a partial app configuration, naming what is
// missing. Running on would quietly fall back to another credential, or to
// none, and the box would look configured while posting no checks.
func (e Env) validateGitHubApp() error {
	vars := []struct{ name, value string }{
		{"KILN_GITHUB_APP_ID", e.GitHubAppID},
		{"KILN_GITHUB_APP_INSTALLATION_ID", e.GitHubAppInstallationID},
		{"KILN_GITHUB_APP_KEY_FILE", e.GitHubAppKeyFile},
	}
	var set, missing []string
	for _, v := range vars {
		if v.value == "" {
			missing = append(missing, v.name)
		} else {
			set = append(set, v.name)
		}
	}
	if len(set) == 0 || len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("github app: %s set but %s missing; set all three or none",
		strings.Join(set, ", "), strings.Join(missing, ", "))
}
