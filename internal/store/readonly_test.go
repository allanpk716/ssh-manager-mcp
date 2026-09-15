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
	// Plan 20 B1 transactional API is guarded like its tx-less ancestors.
	if _, err := s.AddServerWithCredentials(&models.Server{Name: "n", Host: "h", Port: 22, User: "u"},
		&models.Credential{Type: models.CredPassword, Secret: []byte("x")}, nil); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("AddServerWithCredentials: err=%v want ErrReadOnly", err)
	}
	if err := s.UpdateServerWithCredentials(&models.Server{ID: "x", Name: "n"}, nil, nil); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("UpdateServerWithCredentials: err=%v want ErrReadOnly", err)
	}
	if err := s.DeleteServerCascading("x"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("DeleteServerCascading: err=%v want ErrReadOnly", err)
	}
	// Plan 21 A2 clear-credential is a mutation too — the read-only cache must
	// refuse it like every other write.
	if err := s.ClearServerCredential("x"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("ClearServerCredential: err=%v want ErrReadOnly", err)
	}
	if _, err := s.DeleteOrphanCredentials(); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("DeleteOrphanCredentials: err=%v want ErrReadOnly", err)
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

	// Plan 12 T1 cache_tokens mutations are also guarded. Plan 39: the read-only
	// gate fires BEFORE the profile-existence check (any profileID arg proves it).
	if _, _, err := s.AddCacheToken("dev", "any-profile"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("AddCacheToken: err=%v want ErrReadOnly", err)
	}
	if err := s.BindCacheToken("dev", "any-profile"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("BindCacheToken: err=%v want ErrReadOnly", err)
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

// --- Plan 48: 只读态的 host_keys 唯一窄缝(spec §2.3、§8 T9) ------------------

// TestReadOnly_ApplyForwardedHostKeyNarrowGap pins the narrow gap: the
// forwarded-pin receipt is the ONLY host_keys write a read-only cache accepts,
// stamped with the broker-identical metadata (blob/forward/<device>), with no
// local audit row (the authoritative one lives broker-side). Everything else —
// SaveHostKey, InsertForwardedPin — stays refused with the shared sentinel,
// whose text is pinned verbatim (spec §4: the sentinel text must not change).
func TestReadOnly_ApplyForwardedHostKeyNarrowGap(t *testing.T) {
	keyA, _ := pinHostKey(t)
	keyB, _ := pinHostKey(t)

	s := newTestStore(t)
	s.SetForwardDevice("dev-laptop")
	s.SetReadOnly(nil)

	if ErrReadOnly.Error() != "store is read-only (offline cache); connect to the server to mutate" {
		t.Fatalf("shared ErrReadOnly sentinel text drifted: %q", ErrReadOnly.Error())
	}

	// unknown host: the forwarded receipt inserts through the narrow gap
	if err := s.ApplyForwardedHostKey("10.0.0.9", 22, keyA); err != nil {
		t.Fatalf("ApplyForwardedHostKey on read-only store: %v", err)
	}
	format, source, device, blob := pinMetaRow(t, s, "10.0.0.9:22")
	if format != "blob" || source != "forward" || device != "dev-laptop" || !bytes.Equal(blob, keyA) {
		t.Fatalf("forwarded row = %q/%q/%q (%v), want blob/forward/dev-laptop with the presented bytes", format, source, device, blob)
	}
	if n := countAudit(t, s); n != 0 {
		t.Fatalf("forwarded receipt must not write local audit, got %d rows", n)
	}

	// the gap is NARROW: every other host_keys write stays refused — verbatim
	if err := s.SaveHostKey("10.0.0.9", 22, keyB); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("SaveHostKey readonly: err=%v want ErrReadOnly", err)
	}
	if _, err := s.InsertForwardedPin("10.0.0.9", 22, keyB, "dev", AuditRow{}); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("InsertForwardedPin readonly: err=%v want ErrReadOnly", err)
	}
}

// TestReadOnly_ApplyForwardedHostKeyDualModeGate: with an anchor already
// present (cached from the broker), the receipt passes ONLY on dual-mode
// equality — including a fingerprint anchor matched against the presented
// key's fingerprint — and never disturbs the existing row.
func TestReadOnly_ApplyForwardedHostKeyDualModeGate(t *testing.T) {
	keyA, fpA := pinHostKey(t)
	keyB, _ := pinHostKey(t)

	s := newTestStore(t)
	s.SetForwardDevice("dev-laptop")
	if err := s.SaveHostKey("10.0.0.8", 22, keyA); err != nil {
		t.Fatal(err)
	}
	seedFingerprintPin(t, s, "10.0.0.7", 22, fpA)
	s.SetReadOnly(nil)

	// equal blob anchor → race-tolerant pass, row untouched
	if err := s.ApplyForwardedHostKey("10.0.0.8", 22, keyA); err != nil {
		t.Fatalf("equal blob anchor must pass: %v", err)
	}
	if got := mustLoadPin(t, s, "10.0.0.8", 22); got == nil || !bytes.Equal(got.Blob, keyA) || got.Format != "blob" {
		t.Fatalf("equal pass disturbed the anchor: %+v", got)
	}
	// different key → refused
	if err := s.ApplyForwardedHostKey("10.0.0.8", 22, keyB); err == nil {
		t.Fatal("different key against an existing anchor must be refused")
	}
	// fingerprint anchor vs presented key (指纹锚对呈现指纹)
	if err := s.ApplyForwardedHostKey("10.0.0.7", 22, keyA); err != nil {
		t.Fatalf("presented key matching the fingerprint anchor must pass: %v", err)
	}
	if err := s.ApplyForwardedHostKey("10.0.0.7", 22, keyB); err == nil {
		t.Fatal("presented key NOT matching the fingerprint anchor must be refused")
	}
	// still exactly the two original rows, byte-identical
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM host_keys`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("gate must never add or replace rows, count = %d", n)
	}
}
