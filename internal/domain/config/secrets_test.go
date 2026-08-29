package config

import (
	"strings"
	"testing"
)

const secretsBase = `apiVersion: kiln.klarlabs.de/v1
kind: Pipeline
on:
  push: [prove, publish]
publish:
`

// The case this feature exists for: a web image whose `npm ci` pulls a private
// package from GitHub Packages and needs a token to do it.
func TestSecrets_PrivateRegistryToken(t *testing.T) {
	p, err := Parse(strings.NewReader(secretsBase + `  - kind: image
    image: ghcr.io/acme/web
    dockerfile: web/Dockerfile
    context: ./web
    tags: [sha, latest]
    secrets:
      node_auth: env://NODE_AUTH_TOKEN
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	got := p.Publish[0]

	name, ok := got.SecretEnv("node_auth")
	if !ok {
		t.Fatal("node_auth is not configured after parsing")
	}

	if name != "NODE_AUTH_TOKEN" {
		t.Errorf("node_auth reads %q, want NODE_AUTH_TOKEN", name)
	}

	if ids := got.SecretIDs(); len(ids) != 1 || ids[0] != "node_auth" {
		t.Errorf("SecretIDs() = %v, want [node_auth]", ids)
	}
}

// A literal value is the mistake worth refusing loudly: it is a credential
// committed to the repository, and it would work, which is what makes it
// dangerous.
func TestSecrets_LiteralValueIsRejected(t *testing.T) {
	_, err := Parse(strings.NewReader(secretsBase + `  - kind: image
    image: ghcr.io/acme/web
    dockerfile: Dockerfile
    tags: [sha, latest]
    secrets:
      node_auth: ghp_realtokenvalue
`))
	if err == nil {
		t.Fatal("a literal secret value was accepted")
	}

	if !strings.Contains(err.Error(), "env://") {
		t.Errorf("the error does not say what the accepted form is: %v", err)
	}
}

func TestSecrets_EmptyVariableNameIsRejected(t *testing.T) {
	_, err := Parse(strings.NewReader(secretsBase + `  - kind: image
    image: ghcr.io/acme/web
    dockerfile: Dockerfile
    tags: [sha, latest]
    secrets:
      node_auth: "env://"
`))
	if err == nil {
		t.Fatal(`"env://" with no variable name was accepted`)
	}
}

// An id carrying a comma or an equals sign would change what
// `--secret id=<id>,env=<VAR>` means, so it is refused rather than escaped.
func TestSecrets_IDWithFlagSyntaxIsRejected(t *testing.T) {
	for _, id := range []string{"node,auth", "node=auth", "node auth"} {
		t.Run(id, func(t *testing.T) {
			_, err := Parse(strings.NewReader(secretsBase + `  - kind: image
    image: ghcr.io/acme/web
    dockerfile: Dockerfile
    tags: [sha, latest]
    secrets:
      "` + id + `": env://TOKEN
`))
			if err == nil {
				t.Errorf("id %q was accepted; it would corrupt the --secret flag", id)
			}
		})
	}
}

// Fields belonging to the other kind are rejected rather than ignored — a
// setting that silently does nothing looks like it worked.
func TestSecrets_RejectedOnBinaries(t *testing.T) {
	_, err := Parse(strings.NewReader(secretsBase + `  - kind: binaries
    from: goreleaser
    config: .goreleaser.yaml
    secrets:
      node_auth: env://NODE_AUTH_TOKEN
`))
	if err == nil {
		t.Fatal("secrets was accepted on kind: binaries, where it does nothing")
	}

	if !strings.Contains(err.Error(), "secrets") {
		t.Errorf("the error does not name the offending field: %v", err)
	}
}

// Absence must stay the ordinary case: every pipeline written before this
// existed has no secrets and must parse and behave exactly as before.
func TestSecrets_AbsentIsNotAnError(t *testing.T) {
	p, err := Parse(strings.NewReader(secretsBase + `  - kind: image
    image: ghcr.io/acme/api
    dockerfile: Dockerfile
    tags: [sha, latest]
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if ids := p.Publish[0].SecretIDs(); ids != nil {
		t.Errorf("SecretIDs() = %v, want nil for an artifact with no secrets", ids)
	}
}
