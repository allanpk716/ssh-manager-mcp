package cli

import (
	"bytes"
	"encoding/hex"
	"path/filepath"
	"runtime"
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

	outLine := strings.TrimSpace(out.String())
	var hexStr string
	if runtime.GOOS == "windows" {
		// Plan 50: on Windows the line is PowerShell syntax — a POSIX `export`
		// was inert text there (the NUC10 2026-10 session that motivated this).
		const prefix = "$env:SSHMGR_MASTERKEY_HEX = '"
		if !strings.HasPrefix(outLine, prefix) || !strings.HasSuffix(outLine, "'") {
			t.Fatalf("output not the PowerShell env line: %q", outLine)
		}
		hexStr = outLine[len(prefix) : len(outLine)-1]
	} else {
		hexStr = strings.TrimPrefix(outLine, "export SSHMGR_MASTERKEY_HEX=")
	}
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

// TestMasterKeyEnvLineMatchesPlatform pins the Plan 50 shell-syntax fix: the
// emitted line must be sourceable by the platform's default shell — PowerShell
// assignment on Windows, POSIX export elsewhere. The assertion derives the
// expectation from the same runtime.GOOS the helper uses, so it stays green on
// both CI platforms while pinning that the two can never be swapped.
func TestMasterKeyEnvLineMatchesPlatform(t *testing.T) {
	mk := []byte{0xde, 0xad, 0xbe, 0xef}
	got := masterKeyEnvLine(mk)
	var want string
	if runtime.GOOS == "windows" {
		want = "$env:SSHMGR_MASTERKEY_HEX = 'deadbeef'"
		if masterKeyUnsetLine() != "Remove-Item Env:SSHMGR_MASTERKEY_HEX" {
			t.Fatalf("unset line wrong on windows: %q", masterKeyUnsetLine())
		}
	} else {
		want = "export SSHMGR_MASTERKEY_HEX=deadbeef"
		if masterKeyUnsetLine() != "unset SSHMGR_MASTERKEY_HEX" {
			t.Fatalf("unset line wrong on unix: %q", masterKeyUnsetLine())
		}
	}
	if got != want {
		t.Fatalf("env line = %q, want %q", got, want)
	}
}
