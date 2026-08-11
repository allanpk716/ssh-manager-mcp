package store

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// TestReadOnly_MutationsRefused drives every mutation method against a read-only store
// and asserts each returns ErrReadOnly (and performs no write).
func TestReadOnly_MutationsRefused(t *testing.T) {
	s := newTestStore(t)
	s.SetReadOnly(nil)

	// (string, error) shape
	if _, err := s.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("x")}); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("SetCredential: err=%v want ErrReadOnly", err)
	}
	if _, err := s.AddProfile("p"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("AddProfile: err=%v want ErrReadOnly", err)
	}
	if _, err := s.AddServer(&models.Server{Name: "n", Host: "h", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: "c"}); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("AddServer: err=%v want ErrReadOnly", err)
	}
	// error shape
	if err := s.UpdateServer(&models.Server{ID: "x", Name: "n"}); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("UpdateServer: err=%v want ErrReadOnly", err)
	}
	if err := s.DeleteServer("x"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("DeleteServer: err=%v want ErrReadOnly", err)
	}
	if err := s.GrantServers("p", []string{"s"}); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("GrantServers: err=%v want ErrReadOnly", err)
	}
	if _, _, err := s.AddProject("p", "prof"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("AddProject: err=%v want ErrReadOnly", err)
	}
	if _, err := s.RotateProject("x"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("RotateProject: err=%v want ErrReadOnly", err)
	}
	if err := s.SetProjectStatus("x", models.ProjectDisabled); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("SetProjectStatus: err=%v want ErrReadOnly", err)
	}
	if err := s.SaveHostKey("h", 22, []byte("k")); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("SaveHostKey: err=%v want ErrReadOnly", err)
	}
	if err := s.ImportSnapshot(&Snapshot{Version: 1}); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("ImportSnapshot: err=%v want ErrReadOnly", err)
	}

	// Plan 12 T1 cache_tokens mutations are also guarded.
	if _, _, err := s.AddCacheToken("dev"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("AddCacheToken: err=%v want ErrReadOnly", err)
	}
	if err := s.RevokeCacheToken("dev"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("RevokeCacheToken: err=%v want ErrReadOnly", err)
	}
	if err := s.TouchCacheToken("x"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("TouchCacheToken: err=%v want ErrReadOnly", err)
	}
}

// TestReadOnly_ReadsStillWork asserts the read path is unaffected (the broker reads the cache).
func TestReadOnly_ReadsStillWork(t *testing.T) {
	s := newTestStore(t)
	// seed a server BEFORE going read-only
	cid, _ := s.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	sid, _ := s.AddServer(&models.Server{Name: "gpu", Host: "1.1.1.1", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	s.SetReadOnly(nil)

	srv, err := s.GetServer(sid)
	if err != nil || srv == nil || srv.Name != "gpu" {
		t.Fatalf("GetServer after SetReadOnly: srv=%+v err=%v", srv, err)
	}
	cred, err := s.GetCredential(cid)
	if err != nil || cred == nil || string(cred.Secret) != "pw" {
		t.Fatalf("GetCredential after SetReadOnly: cred=%+v err=%v", cred, err)
	}
}

// TestReadOnly_AuditSidecar asserts WriteAudit appends JSONL to the sidecar and does NOT
// insert into audit_log (the table row count must be unchanged).
func TestReadOnly_AuditSidecar(t *testing.T) {
	s := newTestStore(t)
	path := filepath.Join(t.TempDir(), "audit.log")
	af, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { af.Close() })
	s.SetReadOnly(af)

	before := countAudit(t, s)
	if err := s.WriteAudit(AuditRow{Action: "exec", ServerID: "s1", Status: "ok"}); err != nil {
		t.Fatalf("WriteAudit sidecar: %v", err)
	}
	after := countAudit(t, s)
	if after != before {
		t.Fatalf("audit_log row count changed (%d -> %d): sidecar must not touch the db", before, after)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte(`"action":"exec"`)) || !bytes.HasSuffix(got, []byte("\n")) {
		t.Fatalf("sidecar JSONL malformed: %s", got)
	}
}

// TestReadOnly_WriteAudit_NoSidecar asserts that with no sidecar set, WriteAudit returns ErrReadOnly.
func TestReadOnly_WriteAudit_NoSidecar(t *testing.T) {
	s := newTestStore(t)
	s.SetReadOnly(nil)
	if err := s.WriteAudit(AuditRow{Action: "exec"}); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("WriteAudit w/o sidecar: err=%v want ErrReadOnly", err)
	}
}

func countAudit(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
