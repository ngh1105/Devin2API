package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"io"
)

// pkcePair holds a PKCE code verifier and its corresponding challenge.
type pkcePair struct {
	Verifier  string
	Challenge string
}

// newPKCEPair generates a PKCE pair from the provided random source.
// The verifier is 32 random bytes encoded as base64url without padding
// (43 characters). The challenge is SHA-256 of the verifier, also
// base64url-encoded.
func newPKCEPair(rng io.Reader) pkcePair {
	b := make([]byte, 32)
	if _, err := io.ReadFull(rng, b); err != nil {
		// A short read would yield a guessable verifier; never continue.
		panic("pkce: random source failed: " + err.Error())
	}
	raw := base64.RawURLEncoding.EncodeToString(b)
	hash := sha256.Sum256([]byte(raw))
	return pkcePair{
		Verifier:  raw,
		Challenge: base64.RawURLEncoding.EncodeToString(hash[:]),
	}
}
