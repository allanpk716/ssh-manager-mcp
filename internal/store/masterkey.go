package store

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/zalando/go-keyring"
	"golang.org/x/crypto/argon2"
)

const (
	keyringService = "ssh-manager"
	keyringUser    = "master-key"
)

// ErrNotFound is returned when a master key is not present in a provider.
var ErrNotFound = errors.New("master key not found")

// KeyProvider abstracts master-key custody so tests inject a fake (real keychain is flaky in CI).
type KeyProvider interface {
	Get() ([]byte, error) // returns ErrNotFound if absent
	Set(key []byte) error
}

// KeyringKeyProvider stores the master key in the OS keychain.
//
// Service selects the keychain service name. An empty Service falls back to the
// production default (keyringService="ssh-manager"). The eval sets a distinct
// service ("ssh-manager-eval") so it never touches the user's real entry.
// User selects the keychain user slot (empty → default "master-key"). The offline cache
// (Plan 12) uses User:"cache-dek" so its DEK is disjoint from the vault master key.
type KeyringKeyProvider struct {
	Service string
	User    string
}

// service returns the effective keychain service name (configured or default).
func (k KeyringKeyProvider) service() string {
	if k.Service != "" {
		return k.Service
	}
	return keyringService
}

// user returns the effective keychain user slot (configured or default "master-key").
func (k KeyringKeyProvider) user() string {
	if k.User != "" {
		return k.User
	}
	return keyringUser
}

func (k KeyringKeyProvider) Get() ([]byte, error) {
	s, err := keyring.Get(k.service(), k.user())
	if err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return base64.StdEncoding.DecodeString(s)
}

func (k KeyringKeyProvider) Set(key []byte) error {
	return keyring.Set(k.service(), k.user(), base64.StdEncoding.EncodeToString(key))
}

// Delete removes the master key from the keychain. Returns keyring.ErrNotFound
// (wrapped as store.ErrNotFound) if the entry is absent — callers tolerating a
// missing entry should ignore that error.
func (k KeyringKeyProvider) Delete() error {
	err := keyring.Delete(k.service(), k.user())
	if err != nil && errors.Is(err, keyring.ErrNotFound) {
		return ErrNotFound
	}
	return err
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

// GenerateMasterKey returns 32 random bytes.
func GenerateMasterKey() ([]byte, error) {
	k := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, k); err != nil {
		return nil, err
	}
	return k, nil
}

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
