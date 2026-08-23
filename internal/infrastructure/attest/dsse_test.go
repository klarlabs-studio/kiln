package attest_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"go.klarlabs.de/kiln/internal/infrastructure/attest"
)

const payloadType = "application/vnd.in-toto+json"

// signEnvelope produces what warden emits: a DSSE envelope over the statement,
// signed with the gate's key. The test builds the pre-authentication encoding
// by hand rather than calling kiln's, so a change to kiln's framing shows up as
// a failure instead of agreeing with itself.
func signEnvelope(t *testing.T, priv ed25519.PrivateKey, statement []byte, keyID string) []byte {
	t.Helper()
	pae := []byte("DSSEv1 " + itoa(len(payloadType)) + " " + payloadType + " " + itoa(len(statement)) + " ")
	pae = append(pae, statement...)

	env := map[string]any{
		"payloadType": payloadType,
		"payload":     base64.StdEncoding.EncodeToString(statement),
		"signatures": []any{map[string]string{
			"keyid": keyID,
			"sig":   base64.StdEncoding.EncodeToString(ed25519.Sign(priv, pae)),
		}},
	}
	body, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func summaryJSON(t *testing.T) []byte {
	t.Helper()
	const commit = aCommit
	body, err := json.Marshal(map[string]any{
		"_type":         "https://in-toto.io/Statement/v1",
		"predicateType": attest.VSAPredicateType,
		"subject": []any{map[string]any{
			"name": "git+commit", "digest": map[string]string{"gitCommit": commit},
		}},
		"predicate": map[string]any{
			"verifier":           map[string]any{"id": "https://warden.klarlabs.de"},
			"resourceUri":        "git+ssh://git@github.com/o/r.git@" + commit,
			"verificationResult": "PASSED",
			"verifiedLevels":     []string{"WARDEN_SOURCE_SIGNED"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func keyPair(t *testing.T) (ed25519.PrivateKey, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv, base64.StdEncoding.EncodeToString(pub)
}

const aCommit = "8115748887775797df0398ed27080998f4d0c8d7"

func TestTheGatesSignatureVerifies(t *testing.T) {
	priv, pub := keyPair(t)
	env, _, err := attest.ParseEnvelope(signEnvelope(t, priv, summaryJSON(t), "139e6eb9e2611c76"))
	if err != nil {
		t.Fatal(err)
	}

	keyID, ok := env.VerifiedBy([]string{pub})
	if !ok {
		t.Fatal("a genuine envelope did not verify")
	}
	if keyID != "139e6eb9e2611c76" {
		t.Errorf("keyID = %q, want the signing fingerprint reported back", keyID)
	}
}

func TestAnEnvelopeSignedByAnyoneElseIsRefused(t *testing.T) {
	imposter, _ := keyPair(t)
	_, pub := keyPair(t)

	env, _, err := attest.ParseEnvelope(signEnvelope(t, imposter, summaryJSON(t), "x"))
	if err != nil {
		t.Fatal(err)
	}

	// The whole arrangement exists for this: a build platform that re-signed
	// warden's verdict would only be attesting to itself.
	if _, ok := env.VerifiedBy([]string{pub}); ok {
		t.Error("accepted a summary the pinned gate did not sign")
	}
}

func TestARewrittenVerdictBreaksTheSignature(t *testing.T) {
	priv, pub := keyPair(t)
	raw := signEnvelope(t, priv, summaryJSON(t), "x")

	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	payload, err := base64.StdEncoding.DecodeString(env["payload"].(string))
	if err != nil {
		t.Fatal(err)
	}
	// Flip PASSED to FAILED inside the signed payload, leaving the signature
	// untouched — the shape a forgery actually takes.
	env["payload"] = base64.StdEncoding.EncodeToString(
		[]byte(strings.Replace(string(payload), `"PASSED"`, `"FAILED"`, 1)))
	tampered, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}

	parsed, _, err := attest.ParseEnvelope(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := parsed.VerifiedBy([]string{pub}); ok {
		t.Error("a rewritten verdict verified")
	}
}

func TestTheMediaTypeIsPartOfWhatWasSigned(t *testing.T) {
	priv, pub := keyPair(t)
	raw := signEnvelope(t, priv, summaryJSON(t), "x")

	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	env["payloadType"] = "application/json"
	swapped, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}

	parsed, _, err := attest.ParseEnvelope(swapped)
	if err != nil {
		t.Fatal(err)
	}
	// This is what the pre-authentication encoding buys: identical bytes
	// re-presented as a different kind of document do not verify.
	if _, ok := parsed.VerifiedBy([]string{pub}); ok {
		t.Error("the payload verified under a media type it was not signed with")
	}
}

func TestAJunkRosterEntryNeitherAuthorisesNorDenies(t *testing.T) {
	priv, pub := keyPair(t)
	env, _, err := attest.ParseEnvelope(signEnvelope(t, priv, summaryJSON(t), "x"))
	if err != nil {
		t.Fatal(err)
	}

	// Rosters accumulate: a retired key, a line that was never a key, a
	// copy-paste with whitespace. One bad entry must not deny what a good one
	// authorises.
	roster := []string{"not-base64-at-all", base64.StdEncoding.EncodeToString([]byte("short")), "\n" + pub + "\n"}
	if _, ok := env.VerifiedBy(roster); !ok {
		t.Error("a valid signature was rejected because of unrelated roster entries")
	}
	// And a roster of only junk must not accept anything.
	if _, ok := env.VerifiedBy([]string{"not-base64-at-all"}); ok {
		t.Error("a junk roster accepted a signature")
	}
}

func TestAnEmptyRosterAcceptsNothing(t *testing.T) {
	priv, _ := keyPair(t)
	env, _, err := attest.ParseEnvelope(signEnvelope(t, priv, summaryJSON(t), "x"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := env.VerifiedBy(nil); ok {
		t.Error("no configured keys must mean no verification, not a free pass")
	}
}
