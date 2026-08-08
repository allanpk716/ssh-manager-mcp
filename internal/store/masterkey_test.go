package store

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
)

func TestMemKeyProviderRoundTrip(t *testing.T) {
	kp := &MemKeyProvider{}
	if _, err := kp.Get(); err != ErrNotFound {
		t.Fatalf("empty mem: want ErrNotFound, got %v", err)
	}
	key := make([]byte, 32)
	rand.Read(key)
	if err := kp.Set(key); err != nil {
		t.Fatal(err)
	}
	got, err := kp.Get()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, key) {
		t.Fatal("mismatch after set/get")
	}
}

func TestDeriveFromPassphraseDeterministic(t *testing.T) {
	salt := []byte("0123456789abcdef")
	a := DeriveFromPassphrase([]byte("correct horse"), salt)
	b := DeriveFromPassphrase([]byte("correct horse"), salt)
	if !bytes.Equal(a, b) {
		t.Fatal("same passphrase+salt must derive same key")
	}
	if len(a) != 32 {
		t.Fatalf("key len = %d, want 32", len(a))
	}
	c := DeriveFromPassphrase([]byte("different"), salt)
	if bytes.Equal(a, c) {
		t.Fatal("different passphrase must derive different key")
	}
}

func TestMetaSaveLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "meta.json")
	salt := []byte("abcdef0123456789")
	if err := SaveMeta(p, &Meta{PassphraseSalt: salt}); err != nil {
		t.Fatal(err)
	}
	m, err := LoadMeta(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(m.PassphraseSalt, salt) {
		t.Fatal("salt mismatch")
	}
	if _, err := LoadMeta(filepath.Join(dir, "missing")); !os.IsNotExist(err) && err != nil {
		t.Fatalf("missing meta: want nil or IsNotExist, got %v", err)
	}
}
