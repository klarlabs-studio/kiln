// Package envconfig reads the KILN_* operator environment.
//
// The split between this and internal/config is deliberate and load-bearing.
// `.kiln.yaml` lives in the repository and is therefore attacker-controlled on
// a fork pull request; the environment lives on the operator's machine and is
// not. Anything that grants trust — trusted signing keys, tokens, the MCP
// run opt-in — must be read here and never from the pipeline file.
package envconfig

import (
	"os"
	"strings"
)

// Defaults for the values an operator usually leaves alone.
const (
	DefaultDB         = ".kiln/state.json"
	DefaultAddr       = "127.0.0.1:8088"
	DefaultWarden     = "warden"
	DefaultNox        = "nox"
	DefaultGoreleaser = "goreleaser"
	DefaultLogLevel   = "info"
)

// Env is the resolved operator environment for one process.
type Env struct {
	// DB is the run ledger path (KILN_DB).
	DB string
	// Dry plans tags and skips every docker/cosign call (KILN_DRY=1).
	Dry bool
	// Warden, Nox and Goreleaser are the binary names Kiln shells out to.
	Warden     string
	Nox        string
	Goreleaser string
	// TrustedKeys are the signer keys a warden note must match before Kiln will
	// skip a re-prove. Operator-pinned; never sourced from a PR head.
	TrustedKeys []string
	// Token authorizes GitHub Checks and the PR fork lookup. Without it, Kiln
	// posts no Checks and treats every pull request as a fork.
	Token string
	// Repository is owner/name, used when the git remote is absent.
	Repository string
	// MCPAllowRun opts the MCP surface into push/tag runs (KILN_MCP_ALLOW_RUN=1).
	MCPAllowRun bool
	// Addr, DaemonToken and WebhookSecret configure kilnd.
	Addr          string
	DaemonToken   string
	WebhookSecret string
	// Dir is the repository kilnd operates on (KILN_DIR).
	Dir string
	// LogLevel sets the bolt level (KILN_LOG_LEVEL).
	LogLevel string
}

// Load reads the environment. It never fails: a missing variable is a default
// or an empty value, and the surfaces that actually require one (kilnd's
// bearer token) say so at boot with a message naming the variable.
func Load() Env {
	return Env{
		DB:            firstNonEmpty(os.Getenv("KILN_DB"), DefaultDB),
		Dry:           truthy(os.Getenv("KILN_DRY")),
		Warden:        firstNonEmpty(os.Getenv("KILN_WARDEN"), DefaultWarden),
		Nox:           firstNonEmpty(os.Getenv("KILN_NOX"), DefaultNox),
		Goreleaser:    firstNonEmpty(os.Getenv("KILN_GORELEASER"), DefaultGoreleaser),
		TrustedKeys:   splitList(os.Getenv("KILN_TRUSTED_KEYS")),
		Token:         firstNonEmpty(os.Getenv("GITHUB_TOKEN"), os.Getenv("GH_TOKEN")),
		Repository:    os.Getenv("GITHUB_REPOSITORY"),
		MCPAllowRun:   truthy(os.Getenv("KILN_MCP_ALLOW_RUN")),
		Addr:          firstNonEmpty(os.Getenv("KILN_ADDR"), DefaultAddr),
		DaemonToken:   os.Getenv("KILN_TOKEN"),
		WebhookSecret: os.Getenv("KILN_WEBHOOK_SECRET"),
		Dir:           os.Getenv("KILN_DIR"),
		LogLevel:      firstNonEmpty(os.Getenv("KILN_LOG_LEVEL"), DefaultLogLevel),
	}
}

// truthy accepts the spellings an operator plausibly types. Anything else is
// false, so a typo leaves the safe default in place rather than silently
// enabling a mode.
func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// splitList parses a comma-separated value, dropping empties so a trailing
// comma does not produce a phantom "" key that would match nothing.
func splitList(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
