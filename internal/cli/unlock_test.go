package cli

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/store"
)

func TestUnlockPassphraseFallbackDerivesKey(t *testing.T) {
	dir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_STORE": filepath.Join(dir, "test.db")})

	// force keychain unavailable -> passphrase fallback
	prevKc := keychain
	keychain = &unavailableKeychain{}
	defer func() { keychain = prevKc }()

	// inject a fixed passphrase
	prevPrompt := passphrasePrompt
	passphrasePrompt = func() ([]byte, error) { return []byte("my-passphrase"), nil }
	defer func() { passphrasePrompt = prevPrompt }()

	root := NewRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetArgs([]string{"unlock"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unlock: %v", err)
	}

	hexStr := strings.TrimSpace(strings.TrimPrefix(strings.TrimSuffix(out.String(), "\n"), "export SSHMGR_MASTERKEY_HEX="))
	if _, err := hex.DecodeString(hexStr); err != nil {
		t.Fatalf("output not hex: %q", out.String())
	}
	meta, _ := store.LoadMeta(filepath.Join(dir, "test.db.meta.json"))
	if meta == nil {
		t.Fatal("meta.json not created")
	}
	want := store.DeriveFromPassphrase([]byte("my-passphrase"), meta.PassphraseSalt)
	if hex.EncodeToString(want) != hexStr {
		t.Fatal("derived key does not match passphrase+salt")
	}
}

type unavailableKeychain struct{}

func (unavailableKeychain) Get() ([]byte, error) { return nil, os.ErrNotExist }
func (unavailableKeychain) Set([]byte) error     { return nil }
