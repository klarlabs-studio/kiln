package envconfig

import (
	"errors"
	"strings"
	"testing"
)

const samplePEM = `-----BEGIN ENCRYPTED SIGSTORE PRIVATE KEY-----
eyJrZGYiOnsibmFtZSI6InNjcnlwdCIsInBhcmFtcyI6eyJOIjo2NTUzNiwiciI6
-----END ENCRYPTED SIGSTORE PRIVATE KEY-----`

// The value that started #56: an operator exported the key instead of
// pointing at it. cosign tried to open() the PEM as a filename, failed with
// "file name too long", and kiln wrote the failing argument into the retry
// warnings, the publish error, stderr, and the error field of the run record
// in .kiln/state.json — which is git-tracked.
func TestKeyMaterialIsRefusedBeforeAnythingRuns(t *testing.T) {
	err := ValidateCosignKey(samplePEM)
	if err == nil {
		t.Fatal("a PEM private key was accepted as a --key reference")
	}
	if !errors.Is(err, ErrKeyMaterial) {
		t.Errorf("err = %v, want it to wrap ErrKeyMaterial so callers can classify it", err)
	}
	// The message must not quote the value back. Printing it is the disclosure
	// this refuses to make.
	if strings.Contains(err.Error(), "eyJrZGYi") {
		t.Error("the error echoed the key material it was rejecting")
	}
	// It must say what to do instead, or the operator's next move is a guess.
	for _, want := range []string{"file path", "env://", "k8s://", "kms"} {
		if !strings.Contains(strings.ToLower(err.Error()), want) {
			t.Errorf("error does not name %q as an accepted form: %s", want, err)
		}
	}
}

// A key round-tripped through a secret store arrives base64-encoded and is
// just as disclosing.
func TestBase64KeyMaterialIsAlsoRefused(t *testing.T) {
	// nox:ignore SEC-163 -- not a secret: the base64 of "-----BEGIN ENCRYPTED",
	// which is the prefix this check exists to recognise.
	if err := ValidateCosignKey("LS0tLS1CRUdJTiBFTkNSWVBURUQ="); err == nil {
		t.Error("base64 PEM was accepted")
	}
}

// Every form cosign actually takes must pass, or this check breaks signing
// for everyone to protect against one mistake.
func TestEveryValidReferenceIsAccepted(t *testing.T) {
	for _, ref := range []string{
		"", // keyless
		"cosign.key",
		"/etc/kiln/cosign.key",
		"env://COSIGN_PRIVATE_KEY",
		"k8s://kiln-system/cosign",
		"awskms:///alias/kiln",
		// nox:ignore SEC-520 -- a placeholder KMS URI, not a real project. It is
		// here because every form cosign accepts must keep working; dropping it
		// to quiet a scanner would stop testing the case it names.
		"gcpkms://projects/p/locations/l/keyRings/r/cryptoKeys/k",
		"azurekms://vault.vault.azure.net/key",
		"hashivault://kiln",
	} {
		if err := ValidateCosignKey(ref); err != nil {
			t.Errorf("ValidateCosignKey(%q) = %v, want accepted", ref, err)
		}
	}
}

func TestValidateReachesTheCosignKey(t *testing.T) {
	if err := (Env{CosignKey: samplePEM}).Validate(); err == nil {
		t.Error("Env.Validate accepted key material")
	}
	if err := (Env{CosignKey: "cosign.key"}).Validate(); err != nil {
		t.Errorf("Env.Validate rejected a file path: %v", err)
	}
}
