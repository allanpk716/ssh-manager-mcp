package cli

import (
	"bytes"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

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
	if !strings.Contains(stOut.String(), "1") { // at least "1 server" reported
		t.Fatalf("status did not report counts: %s", stOut.String())
	}
}
