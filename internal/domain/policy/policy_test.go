package policy_test

import (
	"strings"
	"testing"

	"go.klarlabs.de/kiln/internal/domain/policy"
)

const minimal = `
apiVersion: kiln.klarlabs.de/v1
kind: VerificationPolicy
signature:
  key: cosign.pub
`

func parse(t *testing.T, body string) (policy.Policy, error) {
	t.Helper()
	return policy.Parse(strings.NewReader(body))
}

func TestAKeyedPolicyLoads(t *testing.T) {
	p, err := parse(t, minimal)
	if err != nil {
		t.Fatal(err)
	}
	if p.Signature.Key != "cosign.pub" {
		t.Errorf("key = %q", p.Signature.Key)
	}
}

func TestKeylessNeedsBothHalves(t *testing.T) {
	// An identity from any issuer is worthless: anyone who runs an OIDC
	// provider can mint a certificate saying whatever they like.
	_, err := parse(t, `
apiVersion: kiln.klarlabs.de/v1
kind: VerificationPolicy
signature:
  identity: https://github.com/o/r/.github/workflows/release.yml@refs/tags/v1
`)
	if err == nil || !strings.Contains(err.Error(), "issuer") {
		t.Errorf("err = %v, want a complaint about the missing issuer", err)
	}
}

func TestAPolicyMustSayWhoseSignatureCounts(t *testing.T) {
	// The failure mode this prevents: a policy that verifies a signature by
	// anybody and reports a pass.
	_, err := parse(t, `
apiVersion: kiln.klarlabs.de/v1
kind: VerificationPolicy
provenance:
  builders: ["https://github.com/klarlabs-studio/kiln@"]
`)
	if err == nil || !strings.Contains(err.Error(), "signature") {
		t.Errorf("err = %v, want a refusal to verify nothing", err)
	}
}

func TestKeyAndIdentityAreAlternatives(t *testing.T) {
	_, err := parse(t, `
apiVersion: kiln.klarlabs.de/v1
kind: VerificationPolicy
signature:
  key: cosign.pub
  identity: someone
  issuer: https://token.actions.githubusercontent.com
`)
	if err == nil {
		t.Error("a policy naming both a key and an identity was accepted; it does not say which must hold")
	}
}

func TestRequiringASourceVerdictNeedsAKeyToCheckItWith(t *testing.T) {
	_, err := parse(t, `
apiVersion: kiln.klarlabs.de/v1
kind: VerificationPolicy
signature:
  key: cosign.pub
source:
  required: true
`)
	if err == nil || !strings.Contains(err.Error(), "source.keys") {
		t.Errorf("err = %v, want it to point at the missing keys", err)
	}
}

func TestAMisspelledFieldIsAnError(t *testing.T) {
	// The worst outcome for security configuration is a rule that silently
	// does not apply: the run reports success having checked less than the
	// author believed it did.
	_, err := parse(t, `
apiVersion: kiln.klarlabs.de/v1
kind: VerificationPolicy
signature:
  key: cosign.pub
provenance:
  builder: ["https://github.com/klarlabs-studio/kiln@"]
`)
	if err == nil {
		t.Error("a misspelled `builder:` was accepted and would have checked nothing")
	}
}

func TestTheApiVersionIsSpelledExactly(t *testing.T) {
	for _, body := range []string{
		strings.Replace(minimal, "kiln.klarlabs.de/v1", "kiln.klarlabs.de/v2", 1),
		strings.Replace(minimal, "VerificationPolicy", "Pipeline", 1),
	} {
		if _, err := parse(t, body); err == nil {
			t.Errorf("accepted:\n%s", body)
		}
	}
}

func TestAnEmptyFileIsRefusedClearly(t *testing.T) {
	_, err := parse(t, "")
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("err = %v, want it to say the file is empty", err)
	}
}

func TestChecksStateWhatWillBeProved(t *testing.T) {
	p, err := parse(t, `
apiVersion: kiln.klarlabs.de/v1
kind: VerificationPolicy
signature:
  identity: https://github.com/o/r/.github/workflows/release.yml@refs/tags/v1
  issuer: https://token.actions.githubusercontent.com
provenance:
  builders: ["https://github.com/klarlabs-studio/kiln@"]
source:
  keys: ["Zm9v"]
  gates: ["https://warden.klarlabs.de"]
  levels: [WARDEN_SOURCE_SIGNED]
  required: true
`)
	if err != nil {
		t.Fatal(err)
	}

	// The point of printing these is that a reader of CI output can see what
	// was asked for, not only whether it passed.
	got := strings.Join(p.Checks(), "\n")
	for _, want := range []string{"token.actions.githubusercontent.com", "kiln@", "gate key", "warden.klarlabs.de", "WARDEN_SOURCE_SIGNED"} {
		if !strings.Contains(got, want) {
			t.Errorf("Checks() = %q, want it to mention %q", got, want)
		}
	}
	if strings.Contains(got, "advisory") {
		t.Errorf("a required source verdict was described as advisory: %q", got)
	}
}

func TestAnAdvisorySourceVerdictSaysSo(t *testing.T) {
	p, err := parse(t, `
apiVersion: kiln.klarlabs.de/v1
kind: VerificationPolicy
signature:
  key: cosign.pub
source:
  keys: ["Zm9v"]
`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(p.Checks(), "\n"), "advisory") {
		t.Error("an optional source verdict must not read as a requirement")
	}
}
