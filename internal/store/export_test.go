package store

import (
	"bytes"
	"encoding/json"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// TestExportSnapshot_CapturesAllTables seeds one of each row kind, exports,
// and asserts the DTO carries every row with DECRYPTED credential plaintext.
func TestExportSnapshot_CapturesAllTables(t *testing.T) {
	s := newTestStore(t) // store_test.go:11 — fresh store w/ random 32-byte master key

	// seed: profile, server (+ its credential), grant, project (hash retained in DB), host key, audit row
	credID, err := s.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("s3cr3t")})
	if err != nil {
		t.Fatal(err)
	}
	// (AddServer via the existing method is fine for SEEDING — it generates ids, which is what we want here)
	srv := &models.Server{Name: "gpu", Host: "192.0.2.10", Port: 22, User: "deploy",
		AuthMethod: models.AuthPassword, CredentialID: credID, Tags: []string{"prod"}}
	srvID, err := s.AddServer(srv)
	if err != nil {
		t.Fatal(err)
	}
	profID, err := s.AddProfile("team-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.GrantServers(profID, []string{srvID}); err != nil {
		t.Fatal(err)
	}
	projID, _, err := s.AddProject("my-agent", profID) // plaintext token discarded here
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveHostKey("192.0.2.10", 22, []byte("fake-host-key-blob")); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteAudit(AuditRow{Action: "exec", ProjectID: projID, ServerID: srvID, Status: "ok"}); err != nil {
		t.Fatal(err)
	}

	snap, err := s.ExportSnapshot()
	if err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	if snap.Version != 1 {
		t.Errorf("Version = %d, want 1", snap.Version)
	}
	if len(snap.Credentials) != 1 || !bytes.Equal(snap.Credentials[0].Secret, []byte("s3cr3t")) {
		t.Errorf("credentials not captured/decrypted: %+v", snap.Credentials)
	}
	if len(snap.Servers) != 1 || snap.Servers[0].Name != "gpu" {
		t.Errorf("servers not captured: %+v", snap.Servers)
	}
	if len(snap.Profiles) != 1 || snap.Profiles[0].Name != "team-a" {
		t.Errorf("profiles not captured: %+v", snap.Profiles)
	}
	if len(snap.Grants) != 1 || snap.Grants[0].ProfileID != profID || snap.Grants[0].ServerID != srvID {
		t.Errorf("grants not captured: %+v", snap.Grants)
	}
	// CRITICAL: token_hash/salt ARE captured (the whole point — raw SQL, not ListProjects)
	if len(snap.Projects) != 1 || len(snap.Projects[0].TokenHash) == 0 || len(snap.Projects[0].TokenSalt) == 0 {
		t.Errorf("projects hash/salt not captured: %+v", snap.Projects)
	}
	if len(snap.HostKeys) != 1 || snap.HostKeys[0].HostPort != "192.0.2.10:22" {
		t.Errorf("host_keys not captured: %+v", snap.HostKeys)
	}
	if len(snap.Audit) != 1 || snap.Audit[0].Action != "exec" {
		t.Errorf("audit not captured: %+v", snap.Audit)
	}
}

// TestImportSnapshot_RoundTrip_CrossMasterKey exports from store A (mk1), imports
// into a SECOND store B with a DIFFERENT master key, and asserts every table
// matches AND the original project plaintext token still validates on B.
func TestImportSnapshot_RoundTrip_CrossMasterKey(t *testing.T) {
	a := newTestStore(t) // mk1

	// seed A: one credential, one server (sudo too, to exercise SudoCredentialID),
	// one profile + grant, one project (capture the plaintext token!), one host key, one audit row.
	credID, _ := a.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw-A")})
	sudoID, _ := a.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("sudo-A")})
	srvID, _ := a.AddServer(&models.Server{Name: "gpu", Host: "192.0.2.10", Port: 22, User: "deploy",
		AuthMethod: models.AuthPassword, CredentialID: credID, SudoCredentialID: sudoID, Tags: []string{"prod"}, Description: "box"})
	profID, _ := a.AddProfile("team-a")
	a.GrantServers(profID, []string{srvID})
	projID, token, err := a.AddProject("my-agent", profID) // keep `token` — the proof
	if err != nil {
		t.Fatal(err)
	}
	a.SaveHostKey("192.0.2.10", 22, []byte("hk-blob"))
	a.WriteAudit(AuditRow{Action: "exec", ProjectID: projID, ServerID: srvID, Status: "ok"})

	snap, err := a.ExportSnapshot()
	if err != nil {
		t.Fatalf("export A: %v", err)
	}

	// B: fresh EMPTY store with a DIFFERENT master key (newTestStore mints a new random key).
	b := newTestStore(t)

	if err := b.ImportSnapshot(snap); err != nil {
		t.Fatalf("import into B: %v", err)
	}

	// servers match (same id — proves id-preserving insert)
	got, err := b.GetServer(srvID)
	if err != nil || got == nil || got.Name != "gpu" || got.Host != "192.0.2.10" || got.SudoCredentialID != sudoID {
		t.Fatalf("server mismatch on B: got=%+v err=%v", got, err)
	}
	// credential re-sealed under B's key AND decrypts to the original plaintext
	gc, err := b.GetCredential(credID)
	if err != nil || gc == nil || string(gc.Secret) != "pw-A" {
		t.Fatalf("credential not re-sealed/decrypted under B's key: %+v err=%v", gc, err)
	}
	// grants + profiles
	if ids, _ := b.ServersForProfile(profID); len(ids) != 1 || ids[0] != srvID {
		t.Fatalf("grants not restored on B: %v", ids)
	}
	// host keys
	hk, _ := b.GetHostKey("192.0.2.10", 22)
	if !bytes.Equal(hk, []byte("hk-blob")) {
		t.Fatalf("host key not restored: %v", hk)
	}
	// THE PROOF — original plaintext token from A still validates on B (hash preserved verbatim)
	pj, err := b.VerifyToken(token)
	if err != nil || pj == nil || pj.ID != projID {
		t.Fatalf("ORIGINAL TOKEN DOES NOT VALIDATE ON B after import: pj=%+v err=%v", pj, err)
	}
}

// TestSnapshotRoundTripCredentialLess (Plan 20 C0): a credential-less server
// survives export → import losslessly — empty-string CredentialID/AuthMethod in
// the snapshot both ways, NULL (not ”) on disk in the target (the FK on
// credential_id would reject ”), while its credential-backed neighbor keeps
// its binding.
func TestSnapshotRoundTripCredentialLess(t *testing.T) {
	a := newTestStore(t)
	credID, err := a.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.AddServer(&models.Server{Name: "withcred", Host: "192.0.2.1", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: credID}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.AddServer(&models.Server{Name: "bare", Host: "192.0.2.2", Port: 22, User: "u", Tags: []string{"imported"}}); err != nil {
		t.Fatal(err)
	}

	snap, err := a.ExportSnapshot()
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	var sawBare bool
	for _, sv := range snap.Servers {
		if sv.Name == "bare" {
			sawBare = true
			if sv.CredentialID != "" || sv.AuthMethod != "" {
				t.Fatalf("bare server not credential-less in snapshot: %+v", sv)
			}
		}
	}
	if !sawBare {
		t.Fatalf("bare server not captured in snapshot: %+v", snap.Servers)
	}

	b := newTestStore(t)
	if err := b.ImportSnapshot(snap); err != nil {
		t.Fatalf("import: %v", err)
	}
	got, err := b.GetServerByName("bare")
	if err != nil || got == nil {
		t.Fatalf("GetServerByName(bare) on B: %v %v", got, err)
	}
	if got.CredentialID != "" || got.AuthMethod != "" {
		t.Fatalf("bare server lost credential-less form on B: %+v", got)
	}
	// SQL-layer proof: the imported row carries NULL — '' would violate the FK.
	var nullCred int
	if err := b.db.QueryRow(`SELECT COUNT(*) FROM servers WHERE name='bare' AND credential_id IS NULL`).Scan(&nullCred); err != nil {
		t.Fatal(err)
	}
	if nullCred != 1 {
		t.Fatal("imported credential-less server must store NULL credential_id")
	}
	wc, _ := b.GetServerByName("withcred")
	if wc == nil || wc.CredentialID != credID || wc.AuthMethod != models.AuthPassword {
		t.Fatalf("credential-backed server corrupted across roundtrip: %+v", wc)
	}
}

// TestImportSnapshot_RefusesNonEmpty guards against silent clobber. We seed a
// real credential-backed server on both A and B — the intent ("B has >=1
// server") is unchanged; the refusal gate counts servers regardless of
// credentials.
func TestImportSnapshot_RefusesNonEmpty(t *testing.T) {
	a := newTestStore(t)
	aCredID, _ := a.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("x")})
	a.AddServer(&models.Server{Name: "s", Host: "192.0.2.55", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: aCredID})
	snap, _ := a.ExportSnapshot()

	b := newTestStore(t)
	bCredID, _ := b.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("y")})
	b.AddServer(&models.Server{Name: "existing", Host: "192.0.2.56", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: bCredID})
	if err := b.ImportSnapshot(snap); err != ErrVaultNotEmpty {
		t.Fatalf("import into non-empty: err=%v, want ErrVaultNotEmpty", err)
	}
}

// TestExportSnapshot_Deterministic asserts the SAME vault exports byte-identical
// JSON across repeated calls (the foundation backup's skip-if-unchanged relies on).
// Guards against missing ORDER BY in any ExportSnapshot query.
func TestExportSnapshot_Deterministic(t *testing.T) {
	s := newTestStore(t)
	cid, _ := s.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	srv1, _ := s.AddServer(&models.Server{Name: "zeta", Host: "192.0.2.1", User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	srv2, _ := s.AddServer(&models.Server{Name: "alpha", Host: "192.0.2.2", User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	prof1, _ := s.AddProfile("z-team")
	prof2, _ := s.AddProfile("a-team")
	s.GrantServers(prof1, []string{srv1, srv2})
	s.GrantServers(prof2, []string{srv1})
	s.AddProject("p2", prof2)
	s.AddProject("p1", prof1)
	s.SaveHostKey("192.0.2.2", 22, []byte("hk"))
	s.WriteAudit(AuditRow{Action: "exec", ProjectID: "p1", ServerID: srv1, Status: "ok"})
	s.WriteAudit(AuditRow{Action: "exec", ProjectID: "p2", ServerID: srv2, Status: "ok"})

	first, err := s.ExportSnapshot()
	if err != nil {
		t.Fatalf("first export: %v", err)
	}
	b1, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		snap, err := s.ExportSnapshot()
		if err != nil {
			t.Fatalf("export %d: %v", i, err)
		}
		b, err := json.Marshal(snap)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(b1, b) {
			t.Fatalf("export not deterministic on run %d:\n%s\n%s", i, b1, b)
		}
	}
}

// TestExportSnapshot_Deterministic_SameName covers the ORDER BY id (not name) case:
// two servers and two profiles with IDENTICAL names must still produce stable order
// (by primary key), else skip breaks when name collisions exist.
func TestExportSnapshot_Deterministic_SameName(t *testing.T) {
	s := newTestStore(t)
	cid, _ := s.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	// two servers with SAME name, different ids
	s1, _ := s.AddServer(&models.Server{Name: "dup", Host: "10.0.0.1", User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	s2, _ := s.AddServer(&models.Server{Name: "dup", Host: "10.0.0.2", User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	// two profiles with SAME name
	p1, _ := s.AddProfile("dup")
	p2, _ := s.AddProfile("dup")
	s.GrantServers(p1, []string{s1})
	s.GrantServers(p2, []string{s2})

	first, _ := json.Marshal(mustExport(t, s))
	for i := 0; i < 5; i++ {
		b, _ := json.Marshal(mustExport(t, s))
		if !bytes.Equal(first, b) {
			t.Fatalf("non-deterministic with same-name rows on run %d", i)
		}
	}
}

func mustExport(t *testing.T, s *Store) *Snapshot {
	t.Helper()
	snap, err := s.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// TestSnapshotExposeHostRoundTrip: export→import preserves ExposeHost in both
// states. Guards the SQL column lists AND the JSON field against silent
// regression — a lost bit silently degrades an owner opt-in back to masked
// (fail-safe direction, but still an owner-preference loss; spec §2/§6).
func TestSnapshotExposeHostRoundTrip(t *testing.T) {
	st := openTestStore(t) // helper landed in Task 1 (servers_test.go)
	if _, err := st.AddServer(&models.Server{
		Name: "exposed", Host: "h1", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, ExposeHost: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddServer(&models.Server{
		Name: "masked", Host: "h2", Port: 22, User: "u",
		AuthMethod: models.AuthPassword,
	}); err != nil {
		t.Fatal(err)
	}
	snap, err := st.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	// Field must exist on the wire (missing json field would decode as false).
	var sawExposed, sawMasked bool
	for _, sv := range snap.Servers {
		if sv.Name == "exposed" && sv.ExposeHost {
			sawExposed = true
		}
		if sv.Name == "masked" && !sv.ExposeHost {
			sawMasked = true
		}
	}
	if !sawExposed || !sawMasked {
		t.Fatalf("snapshot ExposeHost states wrong: exposed=%v masked=%v", sawExposed, sawMasked)
	}

	// Import into a fresh store and verify both states survive.
	mk2, _ := GenerateMasterKey()
	st2, err := Open(t.TempDir()+"/t2.db", mk2)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	if err := st2.ImportSnapshot(snap); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"exposed": true, "masked": false} {
		got, err := st2.GetServerByName(name)
		if err != nil || got == nil {
			t.Fatalf("imported %s: %v", name, err)
		}
		if got.ExposeHost != want {
			t.Fatalf("imported %s ExposeHost = %v, want %v", name, got.ExposeHost, want)
		}
	}
}
