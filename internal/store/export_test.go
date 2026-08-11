package store

import (
	"bytes"
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
