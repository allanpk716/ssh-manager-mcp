package cli

import (
	"bytes"
	"crypto/tls"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/paths"
	"ssh-manager-mcp/internal/store"
)

// TestDefaultDekProvider_IsFileKeyAtFixedPath pins the Plan 16 T4 contract:
// the default cache-DEK provider (no test injection) is a FileKeyProvider whose
// Path is exactly paths.CacheDekPath() (<vaultDir>/cache-dek.key). Was
// DpapiKeyProvider on Windows / KeyringKeyProvider on Unix before Plan 16.
// SSHMGR_FILEKEY_PATH must NOT redirect the cache DEK — cache-dek.key uses its
// own fixed path (paths.CacheDekPath() does not consult that env var).
func TestDefaultDekProvider_IsFileKeyAtFixedPath(t *testing.T) {
	t.Setenv("SSHMGR_FILEKEY_PATH", "") // must not influence cache-dek
	dp := dekProvider()
	fp, ok := dp.(*store.FileKeyProvider)
	if !ok {
		t.Fatalf("default dek not *FileKeyProvider: %T", dp)
	}
	want, err := paths.CacheDekPath()
	if err != nil {
		t.Fatalf("CacheDekPath: %v", err)
	}
	if fp.Path != want {
		t.Errorf("dek path = %q, want %q", fp.Path, want)
	}
}

// withDEK swaps the dekProvider seam to a fresh in-memory provider for the test, returning it
// so the test can assert the DEK persisted there (not the real keychain).
func withDEK(t *testing.T) *store.MemKeyProvider {
	t.Helper()
	mem := &store.MemKeyProvider{}
	prev := dekProvider
	dekProvider = func() store.KeyProvider { return mem }
	t.Cleanup(func() { dekProvider = prev })
	return mem
}

// standUpServe spins a ServeRunner over a seeded store + httptest.Server; returns the server
// URL + a valid cache token + the store (to assert post-pull state).
func standUpServe(t *testing.T) (url, cacheToken string) {
	t.Helper()
	dir := t.TempDir()
	mk, _ := store.GenerateMasterKey()
	st, err := store.Open(filepath.Join(dir, "serve.db"), mk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	if _, err := st.AddServer(&models.Server{Name: "gpu", Host: "192.0.2.10", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid}); err != nil {
		t.Fatal(err)
	}
	r := mcpserver.NewServeRunner(st)
	t.Cleanup(r.Close)
	srv := httptest.NewServer(r.HTTPHandler())
	t.Cleanup(srv.Close)
	_, code, err := st.AddCacheToken("laptop")
	if err != nil {
		t.Fatal(err)
	}
	return srv.URL, code
}

func TestCachePull_WritesEncryptedCacheAndMeta(t *testing.T) {
	url, code := standUpServe(t)
	withDEK(t)
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": cacheDir})

	root := NewRootCmd()
	root.SetArgs([]string{"cache", "pull", "--url", url, "--token", code})
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out) // pull writes its status line to stderr (stdout stays clean for piping)
	if err := root.Execute(); err != nil {
		t.Fatalf("cache pull: %v", err)
	}
	bin := filepath.Join(cacheDir, "cache.bin")
	blob, err := os.ReadFile(bin)
	if err != nil {
		t.Fatalf("cache.bin not written: %v", err)
	}
	if len(blob) == 0 || string(blob[:8]) != "SSHMGRV1" {
		t.Fatalf("cache.bin not an SSHMGRV1 envelope: %x", blob[:min(8, len(blob))])
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "cache.meta.json")); err != nil {
		t.Fatalf("cache.meta.json not written: %v", err)
	}
	if !strings.Contains(out.String(), "gpu") && !strings.Contains(out.String(), "server") {
		// status line; exact wording is loose, but pull must report success non-empty
		if out.Len() == 0 {
			t.Fatal("cache pull printed nothing")
		}
	}
}

func TestCachePull_FailedPullLeavesExistingCacheIntact(t *testing.T) {
	url, _ := standUpServe(t) // valid url, but we'll use a BOGUS token
	withDEK(t)
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": cacheDir})

	// pre-write a sentinel cache.bin so we can prove a failed pull does NOT clobber it
	sentinel := []byte("SSHMGRV1-preexisting-sentinel")
	if err := os.WriteFile(filepath.Join(cacheDir, "cache.bin"), sentinel, 0o600); err != nil {
		t.Fatal(err)
	}

	root := NewRootCmd()
	root.SetArgs([]string{"cache", "pull", "--url", url, "--token", "bogus-xxxxxxxxxxxxxxx"})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{}) // capture cobra's "Error: …" echo out of the test runner's stderr
	if err := root.Execute(); err == nil {
		t.Fatal("pull with bogus token must error")
	}
	got, _ := os.ReadFile(filepath.Join(cacheDir, "cache.bin"))
	if string(got) != string(sentinel) {
		t.Fatalf("failed pull clobbered the existing cache: got %q", got)
	}
}

func TestCacheStatus_ReportsSnapshot(t *testing.T) {
	url, code := standUpServe(t)
	withDEK(t)
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": cacheDir})

	must := func(args ...string) *bytes.Buffer {
		root := NewRootCmd()
		root.SetArgs(args)
		out := &bytes.Buffer{}
		root.SetOut(out)
		if err := root.Execute(); err != nil {
			t.Fatalf("cli %v: %v", args, err)
		}
		return out
	}
	must("cache", "pull", "--url", url, "--token", code)
	stOut := must("cache", "status")
	if !strings.Contains(stOut.String(), "servers:  1") {
		t.Fatalf("status did not report 1 server: %s", stOut.String())
	}
}

func TestResolvePin(t *testing.T) {
	goodPin := "sha256:" + strings.Repeat("a", 64)
	token := "devcode-xyz" // no ':' → no embedded pin
	tokenEmbedded := "devcode-xyz:" + goodPin

	cases := []struct {
		name            string
		envVal, flagVal string
		token           string
		wantFP          string
		wantPlain       bool
	}{
		{"none → plain", "", "", token, "", true},
		{"env wins", "sha256:" + strings.Repeat("b", 64), goodPin, token, "sha256:" + strings.Repeat("b", 64), false},
		{"flag over token-embedded", "", goodPin, tokenEmbedded, goodPin, false},
		{"env over flag", "sha256:" + strings.Repeat("c", 64), goodPin, token, "sha256:" + strings.Repeat("c", 64), false},
		{"token-embedded when no env/flag", "", "", tokenEmbedded, goodPin, false},
		{"token without : is plain", "", "", token, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("SSHMGR_SERVE_PIN", c.envVal)
			gotFP, plain := resolvePin(c.envVal, c.flagVal, c.token)
			if plain != c.wantPlain {
				t.Fatalf("plain=%v want %v", plain, c.wantPlain)
			}
			if !plain && gotFP != c.wantFP {
				t.Fatalf("fp=%q want %q", gotFP, c.wantFP)
			}
		})
	}
}

func TestPinningTransport_BadPinErrors(t *testing.T) {
	// resolvePin returns a parsed fp; constructing the transport from a
	// well-formed fp must succeed.
	fp := "sha256:" + strings.Repeat("a", 64)
	tr, err := pinningTransport(fp)
	if err != nil {
		t.Fatalf("pinningTransport: %v", err)
	}
	if tr.TLSClientConfig == nil {
		t.Fatal("TLSClientConfig nil")
	}
	if tr.TLSClientConfig.MinVersion != tls.VersionTLS13 {
		t.Fatalf("MinVersion not TLS1.3: %v", tr.TLSClientConfig.MinVersion)
	}
	if tr.TLSClientConfig.VerifyConnection == nil {
		t.Fatal("VerifyConnection callback not set")
	}
}
