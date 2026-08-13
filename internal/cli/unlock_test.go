package cli

import (
	"bytes"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/store"
)

// TestUnlockPassphraseFallbackDerivesKey: when masterKeyProvider().Get()
// returns a NON-ErrNotFound error, `unlock` falls back to the passphrase path:
// prompts, derives via Argon2id + salt, prints the export line, persists salt
// to meta.json.
//
// We trigger the non-ErrNotFound branch by pointing SSHMGR_FILEKEY_PATH at a
// DIRECTORY (os.ReadFile on a directory returns a non-fs.ErrNotExist error —
// ERROR_ACCESS_DENIED-equivalent on Windows, EISDIR on Unix). This is distinct
// from "file absent" → ErrNotFound → unlock would first-run GENERATE instead
// (the happy-path first-run flow, exercised by the unlock first-run coverage
// the Plan 16 T8/T9 work owns).
//
// Plan 16 T3: previously this test injected a fake via the deleted `keychain`
// seam (unavailableKeychain). It now drives FileKeyProvider via SSHMGR_FILEKEY_PATH
// (the same env unlock reads in masterKeyProvider()). No package-level seam —
// the "fake" is a real directory path that Get() cannot read.
func TestUnlockPassphraseFallbackDerivesKey(t *testing.T) {
	dir := t.TempDir()
	withEnv(t, map[string]string{
		"SSHMGR_STORE": filepath.Join(dir, "test.db"),
		// Point FILEKEY_PATH at the temp DIR itself (not a file inside it).
		// ReadFile on a directory returns a non-ErrNotExist IO error → unlock's
		// non-ErrNotFound branch → passphrase fallback.
		"SSHMGR_FILEKEY_PATH": dir,
	})

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
