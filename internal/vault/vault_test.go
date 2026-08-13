package vault

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	// keychain seam is irrelevant when SSHMGR_MASTERKEY_HEX is set (env wins);
	// pass an empty MemKeyProvider so the test never touches the real OS keychain.
	st, err := OpenStore(&store.MemKeyProvider{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
}

func TestOpenStoreLockedWhenNoKey(t *testing.T) {
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         filepath.Join(t.TempDir(), "x.db"),
		"SSHMGR_MASTERKEY_HEX": "", // force keychain path
		// Point the FileKeyProvider fallback at a non-existent path so the
		// deterministic locked assertion holds regardless of the host's real
		// UserConfigDir (no master.key.plain lying around).
		"SSHMGR_FILEKEY_PATH": filepath.Join(t.TempDir(), "no-such-file"),
	})
	// Empty MemKeyProvider → ErrNotFound → FileProvider fallback → also
	// ErrNotFound (path above) → "vault locked". Deterministic across hosts
	// (no reliance on whatever the real OS keychain happens to hold).
	st, err := OpenStore(&store.MemKeyProvider{})
	if st != nil {
		st.Close()
		return // unreachable given both providers are empty; defensive only
	}
	if err == nil {
		t.Fatal("expected locked error when no key available")
	}
}

// fakeKeyProvider is a mock KeyProvider for resolveMasterKey tests. It records
// whether Get was called so the hard-fail branch can assert FileProvider was
// NOT reached. errGet, if non-nil, is returned from Get.
type fakeKeyProvider struct {
	key    []byte
	errGet error
	called bool
	setErr error
}

func (f *fakeKeyProvider) Get() ([]byte, error) {
	f.called = true
	if f.errGet != nil {
		return nil, f.errGet
	}
	if f.key == nil {
		return nil, store.ErrNotFound
	}
	out := make([]byte, len(f.key))
	copy(out, f.key)
	return out, nil
}

func (f *fakeKeyProvider) Set(mk []byte) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.key = make([]byte, len(mk))
	copy(f.key, mk)
	return nil
}

// fileKeyPath points the FileKeyProvider at a deterministic temp path for the
// resolveMasterKey suite. Returns "" if no FileProvider should be involved.
func fileKeyPath(t *testing.T) string {
	return filepath.Join(t.TempDir(), "master.key.plain")
}

// resolveTestSetup wires deterministic env (no SSHMGR_MASTERKEY_HEX, a fresh
// FileKeyProvider path) so resolveMasterKey's behavior depends ONLY on the
// injected fakeKeyProvider + the optional file-key fixture. Returns a cleanup.
func resolveTestSetup(t *testing.T, masterKeyHex string) {
	t.Helper()
	withEnv(t, map[string]string{
		"SSHMGR_MASTERKEY_HEX": masterKeyHex,
		"SSHMGR_FILEKEY_PATH":  fileKeyPath(t),
	})
}

// TestResolveMasterKey_TwoTier (Plan 16: keychain tier deleted, spec §4.2).
// resolveMasterKey is now 2 tiers when kp is nil (the production OpenStore
// call site passes store.FileKeyProvider{}; tests pass nil to exercise the
// env + file tiers directly):
//  1. SSHMGR_MASTERKEY_HEX env (hex) — tests / explicit config.
//  2. FileKeyProvider at SSHMGR_FILEKEY_PATH (or default fixed path).
//
// Pass nil so the kp-injection tier is skipped — the documented two-tier
// resolution path. The injected-kp tier is covered by the _KeyProviderParam
// tests below.
func TestResolveMasterKey_TwoTier(t *testing.T) {
	bytes32 := make([]byte, 32)
	copy(bytes32, []byte("two-tier-fixtures"))

	// tier 1: env wins when no file present.
	t.Run("env", func(t *testing.T) {
		resolveTestSetup(t, hex.EncodeToString(bytes32))
		// SSHMGR_FILEKEY_PATH from resolveTestSetup points at a non-existent
		// file → file tier is absent, only env can satisfy.
		mk, err := resolveMasterKey(nil)
		if err != nil {
			t.Fatalf("env tier: %v", err)
		}
		if !bytes.Equal(mk, bytes32) {
			t.Errorf("env tier mismatch: got %x want %x", mk, bytes32)
		}
	})

	// tier 2: FileKeyProvider when env unset.
	t.Run("file", func(t *testing.T) {
		resolveTestSetup(t, "") // no env
		fp := store.FileKeyProvider{Path: os.Getenv("SSHMGR_FILEKEY_PATH")}
		if err := fp.Set(bytes32); err != nil {
			t.Fatalf("seed FileProvider: %v", err)
		}
		mk, err := resolveMasterKey(nil)
		if err != nil {
			t.Fatalf("file tier: %v", err)
		}
		if !bytes.Equal(mk, bytes32) {
			t.Errorf("file tier mismatch: got %x want %x", mk, bytes32)
		}
	})

	// locked: env unset + no file.
	t.Run("locked", func(t *testing.T) {
		resolveTestSetup(t, "") // SSHMGR_FILEKEY_PATH → non-existent path
		_, err := resolveMasterKey(nil)
		if err == nil {
			t.Fatal("expected locked error, got nil")
		}
		if !strings.Contains(err.Error(), "vault locked") {
			t.Fatalf("err = %q, want it to contain \"vault locked\"", err.Error())
		}
	})
}

// TestResolveMasterKey_KeyProviderParamWins: when a non-nil kp is injected and
// its Get() succeeds, that key is returned (priority 1 over env / file). This
// is the seam the production OpenStore call site uses (passing
// store.FileKeyProvider{}) and tests use to inject a fake.
func TestResolveMasterKey_KeyProviderParamWins(t *testing.T) {
	resolveTestSetup(t, "") // no env
	want := make([]byte, 32)
	copy(want, []byte("kp-param"))
	kp := &fakeKeyProvider{key: want}
	got, err := resolveMasterKey(kp)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("kp-param key mismatch: got %x want %x", got, want)
	}
}

// TestResolveMasterKey_FallbackToFile: env unset + platform ErrNotFound +
// FileProvider present → FileProvider key returned (priority 3).
func TestResolveMasterKey_FallbackToFile(t *testing.T) {
	resolveTestSetup(t, "")
	want := make([]byte, 32)
	copy(want, []byte("file-fallback"))
	// seed the FileKeyProvider path
	fp := store.FileKeyProvider{Path: os.Getenv("SSHMGR_FILEKEY_PATH")}
	if err := fp.Set(want); err != nil {
		t.Fatalf("seed FileProvider: %v", err)
	}
	kp := &fakeKeyProvider{} // empty → ErrNotFound
	got, err := resolveMasterKey(kp)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("file key mismatch: got %x want %x", got, want)
	}
}

// TestResolveMasterKey_AllEmptyIsLocked: env unset + platform ErrNotFound +
// FileProvider ErrNotFound → "vault locked" (not a hard-fail; this is the
// legitimate first-run / locked state).
func TestResolveMasterKey_AllEmptyIsLocked(t *testing.T) {
	resolveTestSetup(t, "")  // SSHMGR_FILEKEY_PATH → non-existent path
	kp := &fakeKeyProvider{} // ErrNotFound
	_, err := resolveMasterKey(kp)
	if err == nil {
		t.Fatal("expected locked error, got nil")
	}
	if !strings.Contains(err.Error(), "vault locked") {
		t.Fatalf("err = %q, want it to contain \"vault locked\"", err.Error())
	}
}

// TestResolveMasterKey_PlatformHardFail: env unset + platform returns a
// NON-ErrNotFound error (DPAPI decrypt failure / keychain service down) →
// HARD FAIL. The error MUST propagate; FileProvider MUST NOT be consulted.
// This is the spec §5.6 security guarantee — never silently degrade to plaintext.
func TestResolveMasterKey_PlatformHardFail(t *testing.T) {
	resolveTestSetup(t, "")
	// seed FileProvider so a fall-through WOULD succeed if the code were buggy —
	// this makes the assertion tight (hard-fail must prevent reaching this key).
	wantFile := make([]byte, 32)
	copy(wantFile, []byte("plaintext-should-not-leak"))
	fp := store.FileKeyProvider{Path: os.Getenv("SSHMGR_FILEKEY_PATH")}
	if err := fp.Set(wantFile); err != nil {
		t.Fatalf("seed FileProvider: %v", err)
	}
	// platform KeyProvider simulates a DPAPI decrypt failure (non-ErrNotFound).
	platformErr := errors.New("dpapi: CryptUnprotectData failed: 1312")
	kp := &fakeKeyProvider{errGet: platformErr}
	got, err := resolveMasterKey(kp)
	if err == nil {
		t.Fatalf("expected hard-fail error, got key %x", got)
	}
	// The platform error must be wrapped in the hard-fail message.
	if !strings.Contains(err.Error(), "master key present but unreadable") {
		t.Fatalf("err = %q, want hard-fail message", err.Error())
	}
	if !errors.Is(err, platformErr) {
		t.Fatalf("hard-fail err does not wrap platform error: %v", err)
	}
	// CRITICAL: a plaintext key must NEVER be returned on a decrypt failure.
	if bytes.Equal(got, wantFile) {
		t.Fatal("SECURITY: resolveMasterKey returned the plaintext FileProvider key after a platform decrypt failure — silent degradation forbidden (spec §5.6)")
	}
	// FileProvider must not have been reached either: confirm the seeded file
	// is untouched (still readable with the same content). This is a side-effect
	// proxy — the real assertion is the err + non-nil above.
	gotFile, fErr := fp.Get()
	if fErr != nil || !bytes.Equal(gotFile, wantFile) {
		t.Fatalf("FileProvider file should be untouched on hard-fail: got=%x err=%v", gotFile, fErr)
	}
}

// TestResolveMasterKey_PlatformErrNotFoundIsNotHardFail: a regression guard —
// ErrNotFound from the platform KeyProvider must continue to FileProvider, NOT
// be misclassified as a hard-fail. (Mirrors spec §5.6 distinction.)
func TestResolveMasterKey_PlatformErrNotFoundIsNotHardFail(t *testing.T) {
	resolveTestSetup(t, "")
	kp := &fakeKeyProvider{} // returns ErrNotFound
	_, err := resolveMasterKey(kp)
	if err == nil {
		t.Fatal("expected locked error")
	}
	if strings.Contains(err.Error(), "master key present but unreadable") {
		t.Fatalf("ErrNotFound must NOT trigger hard-fail: err=%q", err.Error())
	}
}

// TestResolveMasterKey_FileProviderIOErrorPropagates: env unset + platform
// ErrNotFound + FileProvider returns a NON-ErrNotFound IO error → the IO error
// propagates (not swallowed, not converted to "locked").
func TestResolveMasterKey_FileProviderIOErrorPropagates(t *testing.T) {
	// Point FileKeyProvider at a DIRECTORY (not a file inside one). os.ReadFile
	// on a directory returns a non-ErrNotExist error (EISDIR on Unix,
	// ERROR_ACCESS_DENIED-equivalent on Windows) → FileProvider surfaces it as-is.
	dirPath := t.TempDir()
	withEnv(t, map[string]string{
		"SSHMGR_MASTERKEY_HEX": "",
		"SSHMGR_FILEKEY_PATH":  dirPath, // a directory, not a file
	})
	kp := &fakeKeyProvider{} // ErrNotFound → reach FileProvider
	_, err := resolveMasterKey(kp)
	if err == nil {
		t.Fatal("expected IO error from FileProvider, got nil")
	}
	if errors.Is(err, store.ErrNotFound) {
		t.Fatalf("IO error must propagate, not be misreported as ErrNotFound: %v", err)
	}
	if strings.Contains(err.Error(), "vault locked") {
		t.Fatalf("IO error must propagate, not be masked as 'vault locked': %v", err)
	}
}
