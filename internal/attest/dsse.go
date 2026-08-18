package attest

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"strings"
)

// pae reconstructs DSSE's pre-authentication encoding.
//
// It must match the signer's byte for byte, because the framing is part of
// what was signed: without it the same payload could be replayed under a
// different media type and the signature would still check out.
//
// This is the third implementation of these few bytes across warden, kiln and
// RollOps, and deliberately so. A shared helper would make all three agree by
// construction rather than by verification, which is the opposite of what
// independent checking means.
func pae(payloadType string, payload []byte) []byte {
	return fmt.Appendf(nil, "DSSEv1 %d %s %d %s",
		len(payloadType), payloadType, len(payload), payload)
}

// VerifiedBy checks the envelope's signature against a roster of public keys
// and returns the fingerprint of whichever one holds.
//
// Every key is tried rather than the one the envelope names. A DSSE keyid is
// attacker-controlled metadata: useful for picking a key out of a roster,
// worthless as an authorisation. A malformed roster entry is skipped rather
// than fatal — a stale line in a config file must neither authorise a deploy
// nor deny one that a good key authorises.
//
// Keys are base64 ed25519, the form `warden key show` prints.
func (e Envelope) VerifiedBy(keys []string) (keyID string, ok bool) {
	payload, err := base64.StdEncoding.DecodeString(e.Payload)
	if err != nil {
		return "", false
	}
	message := pae(e.PayloadType, payload)

	for _, encoded := range keys {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
		if err != nil || len(raw) != ed25519.PublicKeySize {
			continue
		}
		for _, sig := range e.Signatures {
			decoded, err := base64.StdEncoding.DecodeString(sig.Sig)
			if err != nil {
				continue
			}
			if ed25519.Verify(ed25519.PublicKey(raw), message, decoded) {
				return sig.KeyID, true
			}
		}
	}
	return "", false
}
