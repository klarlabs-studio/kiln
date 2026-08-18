package envconfig

import (
	"reflect"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	for _, k := range []string{
		"KILN_DB", "KILN_DRY", "KILN_WARDEN", "KILN_NOX", "KILN_TRUSTED_KEYS",
		"GITHUB_TOKEN", "GH_TOKEN", "GITHUB_REPOSITORY", "KILN_MCP_ALLOW_RUN",
		"KILN_ADDR", "KILN_TOKEN", "KILN_WEBHOOK_SECRET", "KILN_DIR", "KILN_LOG_LEVEL",
	} {
		t.Setenv(k, "")
	}

	env := Load()

	if env.DB != DefaultDB {
		t.Errorf("DB = %q, want %q", env.DB, DefaultDB)
	}
	if env.Warden != DefaultWarden || env.Nox != DefaultNox {
		t.Errorf("binaries = %q/%q, want %q/%q", env.Warden, env.Nox, DefaultWarden, DefaultNox)
	}
	if env.Addr != DefaultAddr {
		t.Errorf("Addr = %q, want %q", env.Addr, DefaultAddr)
	}
	if env.Dry || env.MCPAllowRun {
		t.Error("opt-in modes must default to off")
	}
	if env.TrustedKeys != nil {
		t.Errorf("TrustedKeys = %v, want nil", env.TrustedKeys)
	}
}

func TestTokenFallsBackToGHToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "gh-value")

	if got := Load().Token; got != "gh-value" {
		t.Errorf("Token = %q, want gh-value", got)
	}
}

func TestGitHubTokenWinsOverGHToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "primary")
	t.Setenv("GH_TOKEN", "secondary")

	if got := Load().Token; got != "primary" {
		t.Errorf("Token = %q, want primary", got)
	}
}

func TestTrustedKeysSplit(t *testing.T) {
	t.Setenv("KILN_TRUSTED_KEYS", " aaa , bbb ,, ")

	want := []string{"aaa", "bbb"}
	if got := Load().TrustedKeys; !reflect.DeepEqual(got, want) {
		t.Errorf("TrustedKeys = %v, want %v", got, want)
	}
}

func TestTruthySpellings(t *testing.T) {
	on := []string{"1", "true", "TRUE", "yes", "on"}
	off := []string{"", "0", "false", "no", "off", "maybe"}

	for _, v := range on {
		if !truthy(v) {
			t.Errorf("truthy(%q) = false, want true", v)
		}
	}
	for _, v := range off {
		if truthy(v) {
			t.Errorf("truthy(%q) = true, want false", v)
		}
	}
}

func TestPhaseTimeoutDefaults(t *testing.T) {
	t.Setenv("KILN_PHASE_TIMEOUT", "")

	if got := Load().PhaseTimeout; got != DefaultPhaseTimeout {
		t.Errorf("PhaseTimeout = %v, want %v", got, DefaultPhaseTimeout)
	}
}

func TestPhaseTimeoutIsParsed(t *testing.T) {
	t.Setenv("KILN_PHASE_TIMEOUT", "90m")

	if got := Load().PhaseTimeout; got != 90*time.Minute {
		t.Errorf("PhaseTimeout = %v", got)
	}
}

func TestZeroPhaseTimeoutIsHonoured(t *testing.T) {
	t.Setenv("KILN_PHASE_TIMEOUT", "0")

	// An operator who typed 0 meant "no bound", not "give me the default".
	if got := Load().PhaseTimeout; got != 0 {
		t.Errorf("PhaseTimeout = %v, want 0", got)
	}
}

func TestAnUnparsablePhaseTimeoutKeepsTheSafetyNet(t *testing.T) {
	for _, bad := range []string{"soon", "45", "-5m"} {
		t.Setenv("KILN_PHASE_TIMEOUT", bad)

		// A typo must not silently remove the bound; that is how a watcher
		// ends up wedged with nobody knowing why.
		if got := Load().PhaseTimeout; got != DefaultPhaseTimeout {
			t.Errorf("KILN_PHASE_TIMEOUT=%q gave %v, want the default", bad, got)
		}
	}
}
