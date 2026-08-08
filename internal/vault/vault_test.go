package vault

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/store"
)

func withEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	old := map[string]string{}
	for k, v := range kv {
		old[k] = os.Getenv(k)
		os.Setenv(k, v)
	}
	t.Cleanup(func() {
		for k, v := range old {
			os.Setenv(k, v)
		}
	})
}

func TestOpenStoreViaEnv(t *testing.T) {
	dir := t.TempDir()
	mk, _ := store.GenerateMasterKey()
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         filepath.Join(dir, "test.db"),
		"SSHMGR_MASTERKEY_HEX": hex.EncodeToString(mk),
	})
	st, err := OpenStore()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
}

func TestOpenStoreLockedWhenNoKey(t *testing.T) {
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         filepath.Join(t.TempDir(), "x.db"),
		"SSHMGR_MASTERKEY_HEX": "", // force keychain path
	})
	// KeyringKeyProvider on this host may or may not have a key; if it does, treat as pass.
	// The deterministic assertion: when the keychain has NO entry, OpenStore returns the locked error.
	st, err := OpenStore()
	if st != nil {
		st.Close()
		return // keychain had a key; nothing to assert
	}
	if err == nil {
		t.Fatal("expected locked error when no key available")
	}
}
