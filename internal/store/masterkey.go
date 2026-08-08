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
type KeyringKeyProvider struct{}

func (KeyringKeyProvider) Get() ([]byte, error) {
	s, err := keyring.Get(keyringService, keyringUser)
	if err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return base64.StdEncoding.DecodeString(s)
}

func (KeyringKeyProvider) Set(key []byte) error {
	return keyring.Set(keyringService, keyringUser, base64.StdEncoding.EncodeToString(key))
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
