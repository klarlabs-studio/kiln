// Package envconfig reads the KILN_* operator environment.
//
// The split between this and internal/config is deliberate and load-bearing.
// `.kiln.yaml` lives in the repository and is therefore attacker-controlled on
// a fork pull request; the environment lives on the operator's machine and is
// not. Anything that grants trust — trusted signing keys, tokens, the MCP
// run opt-in — must be read here and never from the pipeline file.
package envconfig

import (
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"go.klarlabs.de/kiln/internal/infrastructure/gocache"
	"go.klarlabs.de/kiln/internal/infrastructure/prune"
)

// Defaults for the values an operator usually leaves alone.
const (
	DefaultDB         = ".kiln/state.json"
	DefaultAddr       = "127.0.0.1:8088"
	DefaultWarden     = "warden"
	DefaultNox        = "nox"
	DefaultGoreleaser = "goreleaser"
	DefaultLogLevel   = "info"

	// DefaultPhaseTimeout bounds one phase — the gate, or one artifact's
	// publish. Generous, because a cold cross-compile of several targets is
	// legitimately slow; finite, because a hung docker pull would otherwise
	// pin a watcher until somebody noticed.
	DefaultPhaseTimeout = 45 * time.Minute

	// DefaultMaxParallel is how many kiln builds one machine runs at once.
	// One, because the box that prompted the limit — eight agents ticking
	// together on a Mac — could not survive two `go test -race` runs beside
	// docker builds without swapping. An operator with a bigger machine raises
	// it; nobody should have to discover the limit by running out of disk.
	DefaultMaxParallel = 1

	// DefaultMinFreeGB is the free space a watch tick insists on. Enough to
	// link a race-instrumented test binary and write its build cache, which
	// is what failed first on a real box that ran out.
	DefaultMinFreeGB = 5
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
	// CosignKey is the signing key for publish (KILN_COSIGN_KEY). Empty selects
	// keyless signing, which mints an ephemeral key against an OIDC identity.
	//
	// Keyless is the better default where an OIDC identity exists to prove —
	// a CI runner with a workload identity. It is unusable on a self-hosted
	// box: with no identity to present, cosign falls back to the device flow
	// and blocks on a browser nobody is in front of, which is a hang rather
	// than a failure and eventually expires mid-publish, leaving a tag with
	// some images signed and some not.
	//
	// The value is passed to cosign's --key, so it takes any form cosign does:
	// a file path, env://VAR, k8s://namespace/secret, or a KMS URI
	// (awskms://, gcpkms://, azurekms://, hashivault://). A KMS URI is the way
	// to keep the private key off the build box entirely.
	//
	// An encrypted key file additionally needs COSIGN_PASSWORD in the
	// environment; cosign reads that itself, and prompts without it — the same
	// hang in a different place.
	//
	// k8s:// is the exception: cosign reads the passphrase from the secret's
	// own `cosign.password` field, which is what
	// `cosign generate-key-pair k8s://…` writes, and does NOT consult
	// COSIGN_PASSWORD there. A secret carrying the passphrase under any other
	// name fails with a bare "decryption failed", which reads like a wrong key
	// rather than a misnamed field.
	//
	// The value must REFERENCE a key, never contain one. A PEM body here is
	// refused at startup by ValidateCosignKey: cosign would treat it as a
	// filename, fail, and the failing argument would reach the logs and the
	// git-tracked run ledger.
	CosignKey string
	// Token authorizes GitHub Checks and the PR fork lookup. Without it, Kiln
	// posts no Checks and treats every pull request as a fork.
	Token string
	// GitHubAppID, GitHubAppInstallationID and GitHubAppKeyFile configure a
	// GitHub App, the recommended credential for a box: kiln mints
	// short-lived installation tokens from the app's private key, and they
	// take precedence over Token. All three or none.
	GitHubAppID             string
	GitHubAppInstallationID string
	GitHubAppKeyFile        string
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
	// PhaseTimeout bounds each phase (KILN_PHASE_TIMEOUT). Zero disables the
	// bound, which an operator with a genuinely enormous build may want and
	// should have to ask for.
	PhaseTimeout time.Duration
	// BuildCacheMaxAge prunes docker build cache older than this
	// (KILN_BUILD_CACHE_MAX_AGE). Zero leaves the cache alone.
	//
	// A machine-level setting rather than a pipeline one: the daemon's cache
	// is shared by every repository on the box, so it cannot sensibly be
	// configured per repository.
	BuildCacheMaxAge time.Duration
	// GoCache is the kiln-owned Go build cache every gate, task and release
	// build uses (KILN_GOCACHE), or empty when kiln manages none: the
	// operator set GOCACHE themselves, or KILN_GOCACHE=off. See package
	// gocache for why box builds stopped sharing the operator's cache.
	GoCache string
	// GoCacheMaxSize caps GoCache after each tick's idle sweep
	// (KILN_GOCACHE_MAX_SIZE, e.g. 10GiB). Zero leaves only the idle sweep.
	GoCacheMaxSize int64
	// MaxParallel bounds concurrent builds across every kiln process of this
	// user on the machine (KILN_MAX_PARALLEL). Zero means no limit.
	//
	// Machine-level for the same reason as BuildCacheMaxAge: the CPU, memory
	// and disk being protected are shared by every repository on the box.
	MaxParallel int
	// SlotDir is where the build slots live (KILN_SLOT_DIR). Empty selects the
	// user's cache directory. Every process meant to share a limit must see
	// the same directory, so this is for the operator who needs to move it —
	// two accounts sharing one machine, or a test that must not queue behind
	// the real box it runs on.
	SlotDir string
	// MinFree is the free disk space below which a watch tick builds nothing
	// (KILN_MIN_FREE_GB, in GiB). Zero disables the check.
	//
	// Machine-level for the same reason as the cache bound: every repository
	// on the box shares the disk.
	MinFree uint64
}

// Load reads the environment. It never fails: a missing variable is a default
// or an empty value, and the surfaces that actually require one (kilnd's
// bearer token) say so at boot with a message naming the variable.
func Load() Env {
	return Env{
		DB:          firstNonEmpty(os.Getenv("KILN_DB"), DefaultDB),
		Dry:         truthy(os.Getenv("KILN_DRY")),
		Warden:      firstNonEmpty(os.Getenv("KILN_WARDEN"), DefaultWarden),
		Nox:         firstNonEmpty(os.Getenv("KILN_NOX"), DefaultNox),
		Goreleaser:  firstNonEmpty(os.Getenv("KILN_GORELEASER"), DefaultGoreleaser),
		TrustedKeys: splitList(os.Getenv("KILN_TRUSTED_KEYS")),
		CosignKey:   strings.TrimSpace(os.Getenv("KILN_COSIGN_KEY")),
		Token:       firstNonEmpty(os.Getenv("GITHUB_TOKEN"), os.Getenv("GH_TOKEN")),
		Repository:  os.Getenv("GITHUB_REPOSITORY"),

		GitHubAppID:             strings.TrimSpace(os.Getenv("KILN_GITHUB_APP_ID")),
		GitHubAppInstallationID: strings.TrimSpace(os.Getenv("KILN_GITHUB_APP_INSTALLATION_ID")),
		GitHubAppKeyFile:        strings.TrimSpace(os.Getenv("KILN_GITHUB_APP_KEY_FILE")),

		MCPAllowRun:   truthy(os.Getenv("KILN_MCP_ALLOW_RUN")),
		Addr:          firstNonEmpty(os.Getenv("KILN_ADDR"), DefaultAddr),
		DaemonToken:   os.Getenv("KILN_TOKEN"),
		WebhookSecret: os.Getenv("KILN_WEBHOOK_SECRET"),
		Dir:           os.Getenv("KILN_DIR"),
		LogLevel:      firstNonEmpty(os.Getenv("KILN_LOG_LEVEL"), DefaultLogLevel),
		PhaseTimeout:  duration(os.Getenv("KILN_PHASE_TIMEOUT"), DefaultPhaseTimeout),
		BuildCacheMaxAge: duration(
			os.Getenv("KILN_BUILD_CACHE_MAX_AGE"), prune.DefaultBuildCacheMaxAge),
		GoCache:        goCache(),
		GoCacheMaxSize: size(os.Getenv("KILN_GOCACHE_MAX_SIZE"), gocache.DefaultMaxSize),
		MaxParallel:    count(os.Getenv("KILN_MAX_PARALLEL"), DefaultMaxParallel),
		SlotDir:        strings.TrimSpace(os.Getenv("KILN_SLOT_DIR")),
		MinFree:        gibibytes(os.Getenv("KILN_MIN_FREE_GB"), DefaultMinFreeGB),
	}
}

func goCache() string {
	operator, set := os.LookupEnv("GOCACHE")
	return gocache.Resolve(os.Getenv("KILN_GOCACHE"), operator, set, os.UserCacheDir)
}

// size parses a byte count with an optional binary unit — 10GiB, 512MiB, 2G,
// or bare bytes. Units are powers of 1024 whether or not the "i" is written:
// a cache cap is compared against du, which counts that way.
//
// "0" is honoured as "no cap"; anything unparsable falls back, for the same
// reason duration does.
func size(v string, fallback int64) int64 {
	v = strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(v), " ", ""))
	if v == "" {
		return fallback
	}
	shift := 0
	for _, u := range []struct {
		suffix string
		shift  int
	}{{"TIB", 40}, {"GIB", 30}, {"MIB", 20}, {"KIB", 10},
		{"TB", 40}, {"GB", 30}, {"MB", 20}, {"KB", 10},
		{"T", 40}, {"G", 30}, {"M", 20}, {"K", 10}, {"B", 0}} {
		if strings.HasSuffix(v, u.suffix) {
			v, shift = strings.TrimSuffix(v, u.suffix), u.shift
			break
		}
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 || n > math.MaxInt64>>shift {
		return fallback
	}
	return n << shift
}

// count parses a non-negative integer, falling back to the default.
//
// The same contract as duration: "0" is honoured, and anything unparsable or
// negative falls back, because a typo must not silently lift a limit.
func count(v string, fallback int) int {
	v = strings.TrimSpace(v)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return fallback
	}
	return n
}

// duration parses a timeout, falling back to the default.
//
// "0" is honoured as "no bound" rather than treated as unset: an operator who
// typed it meant it. Anything unparsable falls back, because a typo must not
// silently remove a safety net.
func duration(v string, fallback time.Duration) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return fallback
	}
	return d
}

// gibibytes parses a whole number of GiB into bytes, with the same rules as
// duration: "0" disables, anything unparsable falls back.
func gibibytes(v string, fallback uint64) uint64 {
	n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 32)
	if err != nil {
		n = fallback
	}
	return n << 30
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
