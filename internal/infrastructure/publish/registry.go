package publish

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// DefaultRegistry is where an image name with no host lives. `nginx` and
// `you/app` are Docker Hub references; only a name whose first element looks
// like a host means anything else.
const DefaultRegistry = "docker.io"

// dockerHubAuthKey is the entry docker writes for Docker Hub.
//
// It is a v1 URL for historical reasons and has never been renamed, so a
// config with credentials for docker.io does not contain the string
// "docker.io" anywhere. Anything checking for Docker Hub credentials by
// looking up the registry host finds nothing and reports a missing login that
// is actually there.
const dockerHubAuthKey = "https://index.docker.io/v1/"

// RegistryOf returns the registry an image reference pushes to.
//
// The rule is docker's own: the first path element is a host only if it looks
// like one — it contains a dot or a colon, or it is exactly "localhost".
// Otherwise the whole thing is a Docker Hub repository, which is why
// `you/app` and `docker.io/you/app` are the same image.
func RegistryOf(image string) string {
	image = strings.TrimSpace(image)
	if image == "" {
		return ""
	}
	head, _, found := strings.Cut(image, "/")
	if !found {
		return DefaultRegistry
	}
	if strings.ContainsAny(head, ".:") || head == "localhost" {
		return head
	}
	return DefaultRegistry
}

// CredentialState is what could be established about a registry login without
// contacting the registry.
type CredentialState int

const (
	// CredentialsUnknown: no docker config to read, so nothing is known. Not
	// the same as absent — a CI runner may be injecting credentials another
	// way, and reporting "you are not logged in" there would be a false alarm
	// that teaches people to ignore the output.
	CredentialsUnknown CredentialState = iota
	// CredentialsPresent: the config names this registry, directly or through
	// a credential helper.
	CredentialsPresent
	// CredentialsMissing: there is a config, it lists other registries, and
	// this one is not among them.
	CredentialsMissing
)

// dockerConfig is the subset of ~/.docker/config.json that says who is logged
// in where.
type dockerConfig struct {
	Auths       map[string]json.RawMessage `json:"auths"`
	CredHelpers map[string]string          `json:"credHelpers"`
	CredsStore  string                     `json:"credsStore"`
}

// CheckRegistryCredentials reports whether the local docker configuration has
// a login for a registry.
//
// This reads configuration; it never contacts a registry and never touches the
// secret itself. The question it answers is the cheap one worth asking before
// a build starts — "will the push at the end of this have somewhere to
// authenticate" — rather than "are these credentials valid", which only the
// registry can answer and only by being asked.
//
// A credential store or helper counts as present. Its whole purpose is to hold
// credentials outside this file, so absence from `auths` proves nothing.
func CheckRegistryCredentials(registry string) CredentialState {
	cfg, ok := loadDockerConfig()
	if !ok {
		return CredentialsUnknown
	}

	keys := []string{registry}
	if registry == DefaultRegistry {
		keys = append(keys, dockerHubAuthKey, "index.docker.io", "registry-1.docker.io")
	}

	for _, k := range keys {
		if _, found := cfg.Auths[k]; found {
			return CredentialsPresent
		}
		if _, found := cfg.CredHelpers[k]; found {
			return CredentialsPresent
		}
	}

	// A store holds every login outside the file, so an empty auths map with a
	// store configured says nothing about this registry either way.
	if cfg.CredsStore != "" && len(cfg.Auths) == 0 {
		return CredentialsUnknown
	}
	if len(cfg.Auths) == 0 && len(cfg.CredHelpers) == 0 && cfg.CredsStore == "" {
		return CredentialsUnknown
	}
	return CredentialsMissing
}

// loadDockerConfig reads docker's config from DOCKER_CONFIG or the home
// directory, the same two places docker itself looks.
func loadDockerConfig() (dockerConfig, bool) {
	dir := os.Getenv("DOCKER_CONFIG")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return dockerConfig{}, false
		}
		dir = filepath.Join(home, ".docker")
	}

	data, err := os.ReadFile(filepath.Join(filepath.Clean(dir), "config.json"))
	if err != nil {
		return dockerConfig{}, false
	}
	var cfg dockerConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		// A config that cannot be parsed is not evidence of anything, and
		// guessing from it would produce exactly the confident-and-wrong
		// report this whole check exists to avoid.
		return dockerConfig{}, false
	}
	return cfg, true
}
