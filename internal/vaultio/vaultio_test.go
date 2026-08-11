package vaultio

import (
	"bytes"
	"errors"
	"testing"
)

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	pt := []byte(`{"version":1,"servers":[]}`)
	out, err := Encrypt([]byte("correct horse battery staple"), pt)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	got, err := Decrypt([]byte("correct horse battery staple"), out)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, pt) {
		t.Fatalf("round-trip mismatch: got %q want %q", got, pt)
	}
}

func TestDecrypt_WrongPassphraseFails(t *testing.T) {
	out, _ := Encrypt([]byte("right"), []byte("secret"))
	if _, err := Decrypt([]byte("wrong"), out); err == nil {
		t.Fatal("Decrypt with wrong passphrase must fail (GCM auth)")
	}
}

func TestDecrypt_TamperedFails(t *testing.T) {
	out, _ := Encrypt([]byte("pw"), []byte("secret"))
	out[len(out)-1] ^= 0xFF // flip a ciphertext byte
	if _, err := Decrypt([]byte("pw"), out); err == nil {
		t.Fatal("Decrypt of tampered ciphertext must fail")
	}
}

func TestDecrypt_TruncatedFails(t *testing.T) {
	out, _ := Encrypt([]byte("pw"), []byte("x"))
	if _, err := Decrypt([]byte("pw"), out[:len(magic)+4]); !errors.Is(err, ErrTruncated) {
		t.Fatalf("truncated blob: err=%v, want ErrTruncated", err)
	}
}

func TestDecrypt_BadMagicFails(t *testing.T) {
	bad := append([]byte("XXXXXXXX"), make([]byte, 40)...)
	if _, err := Decrypt([]byte("pw"), bad); !errors.Is(err, ErrBadMagic) {
		t.Fatalf("bad magic: err=%v, want ErrBadMagic", err)
	}
}

func TestEncrypt_DifferentSaltsProduceDifferentCiphertext(t *testing.T) {
	// randomness sanity: two encrypts of the same plaintext differ (random salt+nonce)
	a, _ := Encrypt([]byte("pw"), []byte("x"))
	b, _ := Encrypt([]byte("pw"), []byte("x"))
	if bytes.Equal(a, b) {
		t.Fatal("two Encrypt calls produced identical output (salt/nonce not random)")
	}
}
