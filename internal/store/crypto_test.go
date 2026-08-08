package store

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	pt := []byte("hunter2")
	blob, err := seal(key, pt)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	got, err := open(key, blob)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !bytes.Equal(got, pt) {
		t.Fatalf("got %q want %q", got, pt)
	}
}

func TestOpenWrongKeyFails(t *testing.T) {
	key1 := make([]byte, 32)
	key2 := make([]byte, 32)
	rand.Read(key1)
	rand.Read(key2)
	blob, err := seal(key1, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := open(key2, blob); err == nil {
		t.Fatal("open with wrong key should fail")
	}
}

func TestSealIsRandom(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)
	a, _ := seal(key, []byte("same"))
	b, _ := seal(key, []byte("same"))
	if bytes.Equal(a, b) {
		t.Fatal("two seals of same plaintext must differ (random salt+nonce)")
	}
}
