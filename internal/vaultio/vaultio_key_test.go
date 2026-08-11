package vaultio

import (
	"bytes"
	"errors"
	"testing"
)

func TestEncryptWithKey_RoundTrip(t *testing.T) {
	key := make([]byte, 32) // 32 zero bytes is a valid (if weak) key for the envelope test
	pt := []byte(`{"version":1,"servers":[]}`)
	out, err := EncryptWithKey(key, pt)
	if err != nil {
		t.Fatalf("EncryptWithKey: %v", err)
	}
	got, err := DecryptWithKey(key, out)
	if err != nil {
		t.Fatalf("DecryptWithKey: %v", err)
	}
	if !bytes.Equal(got, pt) {
		t.Fatalf("round-trip mismatch: got %q want %q", got, pt)
	}
}

func TestEncryptWithKey_WrongKeyFails(t *testing.T) {
	out, _ := EncryptWithKey(make([]byte, 32), []byte("secret"))
	if _, err := DecryptWithKey(bytes.Repeat([]byte{1}, 32), out); err == nil {
		t.Fatal("DecryptWithKey with wrong key must fail (GCM auth)")
	}
}

func TestEncryptWithKey_TamperedFails(t *testing.T) {
	out, _ := EncryptWithKey(make([]byte, 32), []byte("secret"))
	out[len(out)-1] ^= 0xFF
	if _, err := DecryptWithKey(make([]byte, 32), out); err == nil {
		t.Fatal("DecryptWithKey of tampered ciphertext must fail")
	}
}

func TestDecryptWithKey_TruncatedFails(t *testing.T) {
	out, _ := EncryptWithKey(make([]byte, 32), []byte("x"))
	short := out[:len(magic)+4]
	if _, err := DecryptWithKey(make([]byte, 32), short); !errors.Is(err, ErrTruncated) {
		t.Fatalf("truncated: err=%v want ErrTruncated", err)
	}
}

func TestDecryptWithKey_BadMagicFails(t *testing.T) {
	bad := append([]byte("XXXXXXXX"), make([]byte, 40)...)
	if _, err := DecryptWithKey(make([]byte, 32), bad); !errors.Is(err, ErrBadMagic) {
		t.Fatalf("bad magic: err=%v want ErrBadMagic", err)
	}
}

func TestEncryptWithKey_DifferentNonces(t *testing.T) {
	a, _ := EncryptWithKey(make([]byte, 32), []byte("x"))
	b, _ := EncryptWithKey(make([]byte, 32), []byte("x"))
	if bytes.Equal(a, b) {
		t.Fatal("two EncryptWithKey calls produced identical output (nonce not random)")
	}
}

func TestEncryptWithKey_WrongKeyLength(t *testing.T) {
	if _, err := EncryptWithKey(make([]byte, 16), []byte("x")); err == nil {
		t.Fatal("EncryptWithKey must reject a non-32-byte key")
	}
}

func TestDecryptWithKey_WrongKeyLength(t *testing.T) {
	// DecryptWithKey validates len(key)==32 BEFORE parsing the blob (same guard as
	// EncryptWithKey) — so even a valid-shaped blob is rejected with a key-length error,
	// not handed to aes.NewCipher (which would silently accept 16/24/32).
	out, _ := EncryptWithKey(make([]byte, 32), []byte("x"))
	if _, err := DecryptWithKey(make([]byte, 16), out); err == nil {
		t.Fatal("DecryptWithKey must reject a non-32-byte key")
	}
}
