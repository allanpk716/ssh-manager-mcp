package store

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/zalando/go-keyring"
)

// TestKeyringKeyProviderServiceOverride proves the Service field routes Set/Get
// to the configured keychain service and that an empty Service falls back to the
// production default. Uses go-keyring's in-memory mock so it never touches the
// real OS keychain (safe in CI). MockInit mutates a package-level provider, so
// this test is NOT run in parallel.
func TestKeyringKeyProviderServiceOverride(t *testing.T) {
	keyring.MockInit()
	key := []byte("0123456789abcdef0123456789abcdef")
	eval := KeyringKeyProvider{Service: "ssh-manager-eval"}
	prod := KeyringKeyProvider{} // empty Service → default "ssh-manager"

	// Eval-service Set must not leak into the production service.
	if err := eval.Set(key); err != nil {
		t.Fatalf("eval Set: %v", err)
	}
	if got, err := prod.Get(); err != ErrNotFound {
		t.Fatalf("prod Get after eval Set: want ErrNotFound, got %v (got=%v)", err, got)
	}

	// Eval-service Get round-trips the same key.
	gotEval, err := eval.Get()
	if err != nil {
		t.Fatalf("eval Get: %v", err)
	}
	if !bytes.Equal(gotEval, key) {
		t.Fatal("eval service key mismatch after Set/Get")
	}

	// Direct assertion on the service() helper: configured wins, empty defaults.
	if got := eval.service(); got != "ssh-manager-eval" {
		t.Fatalf("eval.service() = %q, want ssh-manager-eval", got)
	}
	if got := prod.service(); got != keyringService {
		t.Fatalf("prod.service() = %q, want %q", got, keyringService)
	}

	// Delete removes the entry; a second Delete reports ErrNotFound.
	if err := eval.Delete(); err != nil {
		t.Fatalf("eval Delete: %v", err)
	}
	if err := eval.Delete(); err != ErrNotFound {
		t.Fatalf("eval Delete after delete: want ErrNotFound, got %v", err)
	}
}

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
