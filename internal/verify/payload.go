package verify

import (
	"encoding/base64"
	"fmt"
)

// decodePayload decodes a DSSE payload.
//
// Both alphabets are accepted: the DSSE spec says standard base64, but tools
// in this space have shipped the URL-safe variant, and a verifier that refuses
// a valid attestation over an encoding detail is a verifier people route
// around.
func decodePayload(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if data, err := enc.DecodeString(s); err == nil {
			return data, nil
		}
	}
	return nil, fmt.Errorf("attestation payload is not base64")
}
