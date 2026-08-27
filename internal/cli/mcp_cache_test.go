package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/clientops"
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
	prev := clientops.DekProvider
	clientops.DekProvider = func(string) store.KeyProvider { return mem }
	t.Cleanup(func() { clientops.DekProvider = prev })

	// --- exercise the hydration path directly (the guts of RunStdioCache, without srv.Run) ---
	loaded, err := clientops.LoadCacheSnapshot()
	if err != nil {
		t.Fatalf("LoadCacheSnapshot: %v", err)
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
	if err := hyd.WriteAudit(store.AuditRow{Action: "exec", ProjectID: proj.ID, Status: "ok"}); err != nil {
		t.Fatalf("WriteAudit to sidecar: %v", err)
	}
	rows, _ := hyd.AuditRows(1)
	if len(rows) != 0 {
		t.Fatal("offline audit must NOT write to the cache db")
	}
	ab, _ := os.ReadFile(auditPath)
	if len(ab) == 0 {
		t.Fatal("offline audit must append to the sidecar")
	}
}

// TestScopedPull_HydratesAndIronRuleHolds (Plan 39 e2e): pull /snapshot over
// REAL HTTP from a serve whose device code is BOUND to a profile, hydrate the
// cache exactly as `mcp --cache` would, and prove the offline surface equals
// the bound profile end to end: the same project token validates, list_servers
// returns ONLY the granted server, the unauthorized server and its credential
// never left the server, and the iron rule rejects the out-of-profile server
// id with ErrNotInProfile (denied BEFORE any dial attempt).
func TestScopedPull_HydratesAndIronRuleHolds(t *testing.T) {
	// --- seed the serve side: 2 servers, 1 granted; project token on that profile ---
	dir := t.TempDir()
	mk, _ := store.GenerateMasterKey()
	src, err := store.Open(filepath.Join(dir, "serve.db"), mk)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	cid, _ := src.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	cid2, _ := src.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("topsecret")})
	gpuID, err := src.AddServer(&models.Server{Name: "gpu", Host: "192.0.2.10", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	if err != nil {
		t.Fatal(err)
	}
	secretID, err := src.AddServer(&models.Server{Name: "secret", Host: "192.0.2.99", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid2})
	if err != nil {
		t.Fatal(err)
	}
	profID, _ := src.AddProfile("team-a")
	if err := src.GrantServers(profID, []string{gpuID}); err != nil {
		t.Fatal(err)
	}
	_, projToken, err := src.AddProject("my-agent", profID)
	if err != nil {
		t.Fatal(err)
	}
	r, err := mcpserver.NewServeRunner(src)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	srv := httptest.NewServer(r.HTTPHandler())
	t.Cleanup(srv.Close)
	_, code, err := src.AddCacheToken("laptop", profID)
	if err != nil {
		t.Fatal(err)
	}

	// --- cache-side seams (DEK + dir), then a REAL plaintext pull (test server has no TLS) ---
	binDir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": binDir})
	dek, _ := store.GenerateMasterKey()
	mem := &store.MemKeyProvider{}
	_ = mem.Set(dek)
	prev := clientops.DekProvider
	clientops.DekProvider = func(string) store.KeyProvider { return mem }
	t.Cleanup(func() { clientops.DekProvider = prev })

	if _, err := clientops.DoPull(srv.URL, code, "", clientops.PullOpts{AllowPlain: true}); err != nil {
		t.Fatalf("scoped pull: %v", err)
	}

	// --- the pulled snapshot is the bound profile's set (cache-layer defense) ---
	loaded, err := clientops.LoadCacheSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Servers) != 1 || loaded.Servers[0].Name != "gpu" {
		t.Fatalf("cache must hold exactly the granted server, got %+v", loaded.Servers)
	}
	if len(loaded.Credentials) != 1 || string(loaded.Credentials[0].Secret) != "pw" {
		t.Fatalf("cache must hold exactly gpu's credential, got %+v", loaded.Credentials)
	}
	if len(loaded.Profiles) != 1 || len(loaded.Projects) != 1 || len(loaded.Audit) != 0 {
		t.Fatalf("scoped snapshot shape mismatch: profiles=%d projects=%d audit=%d",
			len(loaded.Profiles), len(loaded.Projects), len(loaded.Audit))
	}

	// --- hydrate exactly as RunStdioCache does ---
	tmp, _ := os.CreateTemp("", "scoped-hyd-*.db")
	tmpPath := tmp.Name()
	tmp.Close()
	t.Cleanup(func() { os.Remove(tmpPath) })
	hyd, err := store.Open(tmpPath, mk)
	if err != nil {
		t.Fatal(err)
	}
	defer hyd.Close()
	if err := hyd.ImportSnapshot(loaded); err != nil {
		t.Fatalf("ImportSnapshot(scoped): %v", err)
	}
	af, err := os.OpenFile(filepath.Join(binDir, "cache-audit.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer af.Close()
	hyd.SetReadOnly(af)

	// (a) the machine's own project token validates offline (hash verbatim in the scoped snapshot)
	proj, err := hyd.VerifyToken(projToken)
	if err != nil || proj == nil {
		t.Fatalf("project token must validate against the scoped cache: proj=%v err=%v", proj, err)
	}
	// (b) list_servers = the granted set
	servers, err := mcpserver.ListServersForProfile(hyd, proj.ProfileID)
	if err != nil || len(servers) != 1 || servers[0].Name != "gpu" {
		t.Fatalf("offline list_servers must be exactly [gpu]: %+v err=%v", servers, err)
	}
	// (c) the iron rule still rejects the out-of-profile server id — denied at the
	// authorization check, before any dial (the server isn't even IN the store).
	_, err = mcpserver.ExecCommandForProfile(context.Background(), hyd, proj.ID, proj.ProfileID, secretID, "echo hi", false, 0)
	if !errors.Is(err, mcpserver.ErrNotInProfile) {
		t.Fatalf("out-of-profile exec must be denied with ErrNotInProfile, got %v", err)
	}
}
