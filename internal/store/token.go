package store

import (
	"crypto/rand"
	"encoding/base64"
	"io"

	"golang.org/x/crypto/argon2"
)

// GenerateToken returns a new random 32-byte token, base64url-encoded.
func GenerateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashToken returns the Argon2id hash of the token plaintext under salt.
func HashToken(token, salt []byte) []byte {
	return argon2.IDKey(token, salt, 1, 64*1024, 4, 32)
}

func verifyTokenHash(token, salt, want []byte) bool {
	got := HashToken(token, salt)
	return constantTimeEqual(got, want)
}

func constantTimeEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := range a {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

func newSalt() []byte {
	b := make([]byte, 16)
	io.ReadFull(rand.Reader, b)
	return b
}
