package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"testing"
)

func TestPKCEKnownVector(t *testing.T) {
	// RFC 7636 Appendix B test vector.
	const wantVerifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	const wantChallenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"

	gotRaw, err := base64.RawURLEncoding.DecodeString(wantVerifier)
	if err != nil {
		t.Fatalf("decode RFC vector: %v", err)
	}

	pair := newPKCEPair(newFixedReader(gotRaw))
	if pair.Verifier != wantVerifier {
		t.Errorf("verifier = %q; want %q", pair.Verifier, wantVerifier)
	}
	if pair.Challenge != wantChallenge {
		t.Errorf("challenge = %q; want %q", pair.Challenge, wantChallenge)
	}

	// Independently verify challenge = SHA-256(verifier).
	hash := sha256.Sum256([]byte(wantVerifier))
	got := base64.RawURLEncoding.EncodeToString(hash[:])
	if got != wantChallenge {
		t.Errorf("independent SHA-256 verification: challenge = %q; want %q", got, wantChallenge)
	}
}

func TestPKCEFormat(t *testing.T) {
	pair := newPKCEPair(rand.Reader)
	if len(pair.Verifier) != 43 {
		t.Errorf("verifier length = %d; want 43", len(pair.Verifier))
	}
	if len(pair.Challenge) != 43 {
		t.Errorf("challenge length = %d; want 43", len(pair.Challenge))
	}
	for i, c := range pair.Verifier {
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			t.Errorf("verifier[%d] = %q; not base64url allowed", i, string(c))
			break
		}
	}
}

func TestRandomState(t *testing.T) {
	s := randomState()
	if len(s) != 22 { // 16 bytes → ceil(16*4/3) = 22 chars
		t.Errorf("state length = %d; want 22", len(s))
	}
}

// ---------------------------------------------------------------------
// Helpers

type fixedReader struct {
	data []byte
	off  int
}

func newFixedReader(data []byte) *fixedReader {
	return &fixedReader{data: data}
}

func (r *fixedReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, nil // EOF without error matches io.LimitedReader behavior
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	return n, nil
}
