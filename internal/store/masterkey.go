package store

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/crypto/argon2"
)

// ErrNotFound is returned when a master key is not present in a provider.
var ErrNotFound = errors.New("master key not found")

// KeyProvider abstracts master-key custody so tests inject a fake (real keychain is flaky in CI).
type KeyProvider interface {
	Get() ([]byte, error) // returns ErrNotFound if absent
	Set(key []byte) error
}

// MemKeyProvider is an in-memory provider for tests.
type MemKeyProvider struct {
	key []byte
}

func (m *MemKeyProvider) Get() ([]byte, error) {
	if m.key == nil {
		return nil, ErrNotFound
	}
	out := make([]byte, len(m.key))
	copy(out, m.key)
	return out, nil
}

func (m *MemKeyProvider) Set(key []byte) error {
	m.key = make([]byte, len(key))
	copy(m.key, key)
	return nil
}

// Delete clears the in-memory key, satisfying the optional Delete capability
// QuarantineCache consumes via its interface{ Delete() error } assertion (same
// shape as FileKeyProvider/DpapiKeyProvider). Get after Delete returns
// ErrNotFound; deleting an already-empty provider is a nil-error no-op.
func (m *MemKeyProvider) Delete() error {
	m.key = nil
	return nil
}

// GenerateMasterKey returns 32 random bytes.
func GenerateMasterKey() ([]byte, error) {
	k := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, k); err != nil {
		return nil, err
	}
	return k, nil
}

// ValidMasterKeyLen reports whether b has the length GenerateMasterKey
// produces (== keyLen, 32 bytes). It does NOT verify entropy or that b is the
// key the vault was created with — it is a structural sanity check for probes
// that want to reject obviously-truncated / wrong-length / garbage master-key
// files WITHOUT opening the store (store.Open has side effects: it creates
// store.db + runs migration). The full decrypt-validity check happens lazily
// inside GetCredential; this length check is the lightest faithful proxy that
// still catches the common on-disk failure modes (truncated write, wrong file,
// zero-byte file).
//
// The canonical length lives in crypto.go (keyLen, used by seal/open); this
// helper is the only exported name for it so callers don't hardcode 32.
func ValidMasterKeyLen(b []byte) bool { return len(b) == keyLen }

// DeriveFromPassphrase derives a 32-byte master key from a passphrase (Argon2id).
func DeriveFromPassphrase(passphrase, salt []byte) []byte {
	return argon2.IDKey(passphrase, salt, 1, 64*1024, 4, 32)
}

// NewSalt16 returns 16 random bytes for passphrase derivation.
func NewSalt16() []byte { return newSalt() }

// Meta holds vault metadata persisted next to the store (used for passphrase fallback).
type Meta struct {
	PassphraseSalt []byte `json:"passphrase_salt"`
}

func LoadMeta(path string) (*Meta, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Meta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func SaveMeta(path string, m *Meta) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}
