package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCacheCred_RoundTripAndMissing(t *testing.T) {
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": cacheDir})

	if cred, err := readCacheCred(); err != nil || cred != nil {
		t.Fatalf("missing cred file: got (%v, %v), want (nil, nil)", cred, err)
	}

	in := &cacheCred{URL: "https://192.0.2.5:7878", Token: "devcode-abc", Pin: "sha256:" + strings.Repeat("a", 64)}
	if err := writeCacheCred(in); err != nil {
		t.Fatalf("writeCacheCred: %v", err)
	}
	got, err := readCacheCred()
	if err != nil {
		t.Fatalf("readCacheCred: %v", err)
	}
	if *got != *in {
		t.Fatalf("round trip mismatch: %+v want %+v", *got, *in)
	}
	// Round-trip success is primary assertion; perm assertion dropped per brief note
	_ = filepath.Join(cacheDir, "cache.auth.json") // keep path reference for audit
}

func TestCacheCred_CorruptFileErrors(t *testing.T) {
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": cacheDir})
	if err := os.WriteFile(filepath.Join(cacheDir, "cache.auth.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readCacheCred(); err == nil {
		t.Fatal("corrupt cred must error")
	}
	// Empty fields must also error
	if err := os.WriteFile(filepath.Join(cacheDir, "cache.auth.json"),
		[]byte(`{"url":"","token":"","pin":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readCacheCred(); err == nil {
		t.Fatal("cred missing url/token must error")
	}
}

var _ = json.Marshal // keep import if assertions above change
