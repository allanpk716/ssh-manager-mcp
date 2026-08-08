package store

import (
	"testing"
)

func TestGenerateTokenIsBase64URL32Bytes(t *testing.T) {
	tok, err := GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) < 40 {
		t.Fatalf("token too short: %q", tok)
	}
	tok2, _ := GenerateToken()
	if tok == tok2 {
		t.Fatal("tokens must be unique")
	}
}

func TestHashTokenVerifies(t *testing.T) {
	salt := []byte("0123456789abcdef")
	tok, _ := GenerateToken()
	h := HashToken([]byte(tok), salt)
	if !verifyTokenHash([]byte(tok), salt, h) {
		t.Fatal("hash should verify")
	}
	if verifyTokenHash([]byte("wrong"), salt, h) {
		t.Fatal("wrong token must not verify")
	}
}
