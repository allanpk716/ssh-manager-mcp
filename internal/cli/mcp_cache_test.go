package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vaultio"
)

// TestHydrateReadOnlyStore_TokenValidatesAndReadsWork is the load-bearing hydration test:
// build a cache.bin from a seeded snapshot, then run the hydration path (the part of
// RunStdioCache up to NewServer) and assert (a) the project token validates against the cache,
// (b) the in-profile server is readable through the UNCHANGED NewServer read path, (c) a
// mutation is refused with ErrReadOnly, and (d) offline audit lands in the sidecar (not the
// cache db). Proves the agent surface is unchanged offline — same token, same iron rule.
func TestHydrateReadOnlyStore_TokenValidatesAndReadsWork(t *testing.T) {
	// --- seed a server-side store: server + profile + project (capture the token) ---
	dir := t.TempDir()
	mk, _ := store.GenerateMasterKey()
	src, err := store.Open(filepath.Join(dir, "src.db"), mk)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	cid, _ := src.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	srvID, _ := src.AddServer(&models.Server{Name: "gpu", Host: "192.0.2.10", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	profID, _ := src.AddProfile("team-a")
	_ = src.GrantServers(profID, []string{srvID})
	_, projToken, _ := src.AddProject("my-agent", profID)

	snap, err := src.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}

	// --- write a cache.bin exactly as `cache pull` would (DEK + EncryptWithKey) ---
	dek, _ := store.GenerateMasterKey()
	plaintext, _ := json.Marshal(snap)
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "cache.bin")
	blob, _ := vaultio.EncryptWithKey(dek, plaintext)
	if err := os.WriteFile(binPath, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": binDir})

	// --- inject the DEK into the keychain seam so hydration finds it ---
	mem := &store.MemKeyProvider{}
	_ = mem.Set(dek)
	prev := dekProvider
	dekProvider = func() store.KeyProvider { return mem }
	t.Cleanup(func() { dekProvider = prev })

	// --- exercise the hydration path directly (the guts of RunStdioCache, without srv.Run) ---
	loaded, err := loadCacheSnapshot()
	if err != nil {
		t.Fatalf("loadCacheSnapshot: %v", err)
	}
	tmp, _ := os.CreateTemp("", "hyd-*.db")
	tmpPath := tmp.Name()
	tmp.Close()
	t.Cleanup(func() { os.Remove(tmpPath) })
	hyd, err := store.Open(tmpPath, mk)
	if err != nil {
		t.Fatal(err)
	}
	defer hyd.Close()
	if err := hyd.ImportSnapshot(loaded); err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	auditPath := filepath.Join(binDir, "cache-audit.log")
	af, err := os.OpenFile(auditPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer af.Close()
	hyd.SetReadOnly(af)

	// (a) the SAME project token validates against the cached projects (hash preserved verbatim)
	proj, err := hyd.VerifyToken(projToken)
	if err != nil || proj == nil {
		t.Fatalf("project token does not validate against the cache: proj=%v err=%v", proj, err)
	}
	// (b) the in-profile server is readable through the (unchanged) NewServer read path
	servers, err := mcpserver.ListServersForProfile(hyd, proj.ProfileID)
	if err != nil || len(servers) != 1 || servers[0].Name != "gpu" {
		t.Fatalf("ListServersForProfile against cache: %+v err=%v", servers, err)
	}
	// (c) a mutation is refused
	if _, err := hyd.AddServer(&models.Server{Name: "x", Host: "h", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid}); !errors.Is(err, store.ErrReadOnly) {
		t.Fatalf("mutation against cache must return ErrReadOnly, got %v", err)
	}
	// (d) offline audit lands in the sidecar, not the cache db. AuditRows reads the db; after
	// the sidecar write it must still return 0 rows (we never inserted into audit_log). The
	// store.db field is unexported and cli is a different package, so AuditRows is the seam.
	_ = hyd.WriteAudit(store.AuditRow{Action: "exec", ProjectID: proj.ID, Status: "ok"})
	rows, _ := hyd.AuditRows(1)
	if len(rows) != 0 {
		t.Fatal("offline audit must NOT write to the cache db")
	}
	ab, _ := os.ReadFile(auditPath)
	if len(ab) == 0 {
		t.Fatal("offline audit must append to the sidecar")
	}
}
