package envconfig

import (
	"errors"
	"fmt"
	"strings"
)

// ErrKeyMaterial reports that KILN_COSIGN_KEY holds a key rather than a
// reference to one.
var ErrKeyMaterial = errors.New("envconfig: KILN_COSIGN_KEY holds key material")

// pemPrefixes are the openings of a PEM block. cosign's --key takes a
// reference, never a body, so any of these means the operator exported the key
// instead of pointing at it.
var pemPrefixes = []string{
	"-----BEGIN",
	"LS0tLS1CRUdJTi", // the same, base64: a key round-tripped through a secret store
}

// ValidateCosignKey refuses a KILN_COSIGN_KEY that contains a key rather than
// naming one.
//
// This is checked before anything runs because of what happens otherwise.
// cosign is handed the PEM body as a filename, fails with "file name too long",
// and kiln then writes the failing argument into the retry warnings, the
// publish error, stderr, and the `error` field of the run record in
// .kiln/state.json — which is git-tracked. One `git add -A` away from
// committing a private key.
//
// Redacting those paths is worth doing and is done separately. It is the
// weaker fix: it depends on every future error path remembering. Refusing the
// value outright means the material never enters the process's data at all,
// and the operator gets a sentence naming the four forms that do work instead
// of a disclosure they have to notice.
//
// The check is a prefix test rather than anything clever. A reference is a
// path or a URI; neither begins with a PEM header, so there is no valid input
// this rejects.
func ValidateCosignKey(v string) error {
	trimmed := strings.TrimSpace(v)
	for _, p := range pemPrefixes {
		if strings.HasPrefix(trimmed, p) {
			// Deliberately no %q of the value: the point is to keep it out of
			// the operator's terminal and scrollback, which is where this
			// started.
			return fmt.Errorf("%w, not a reference to one. cosign --key takes a file path, "+
				"env://VAR, k8s://namespace/secret, or a KMS URI (awskms://, gcpkms://, "+
				"azurekms://, hashivault://). Write the key to a file and point at that, "+
				"or put it in a secret store — the value you gave would have been logged "+
				"on failure", ErrKeyMaterial)
		}
	}
	return nil
}

// Validate reports configuration that cannot work, before anything runs.
func (e Env) Validate() error {
	return ValidateCosignKey(e.CosignKey)
}
