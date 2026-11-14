package store

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
)

// --- Plan 48: 锚定转发存储原语(spec §5、§8 T9/T10) ---------------------------
//
// 三列迁移(pin_format/pin_source/pin_device)、双模等值(insert-only 原语与
// ApplyForwardedHostKey 共同的唯一等值定义)、InsertForwardedPin 单事务、
// 快照链在 export_test.go、只读窄缝在 readonly_test.go。

// pinHostKey mints a fresh ed25519 host key as (marshaled wire bytes, SHA256
// fingerprint) — hermetic key material; pin tests need no listener.
func pinHostKey(t *testing.T) (marshaled []byte, fp string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return pk.Marshal(), ssh.FingerprintSHA256(pk)
}

// oldShapeHostKeys is the v0.14 host_keys shape: no pin metadata columns.
const oldShapeHostKeys = `
CREATE TABLE host_keys (
  host_port TEXT PRIMARY KEY,
  key_blob BLOB NOT NULL,
  created_at INTEGER NOT NULL
);
`

// TestMigrateHostKeysPinColumnsOldShape: 旧库升级态 — a v0.14-shape DB gains
// the pin columns via Open's guarded ADD COLUMN, and the pre-existing anchor
// lands on the matching entry as a blob/tofu pin with no device（票 #58 的
// 升级迁移把旧表迁空，host_keys 不再是事实源）。
func TestMigrateHostKeysPinColumnsOldShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(oldShapeHostKeys); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO host_keys (host_port, key_blob, created_at) VALUES ('10.0.0.5:22', x'0102', 1700000000)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE servers (id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, host TEXT NOT NULL,
		port INTEGER NOT NULL, user TEXT NOT NULL, auth_method TEXT NOT NULL, credential_id TEXT, sudo_credential_id TEXT,
		shell_type TEXT NOT NULL DEFAULT 'posix', tags TEXT, description TEXT DEFAULT '', location TEXT DEFAULT '',
		hardware TEXT DEFAULT '', services TEXT DEFAULT '', role TEXT DEFAULT '', caveats TEXT DEFAULT '',
		expose_host INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
		revision INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO servers (id,name,host,port,user,auth_method,created_at,updated_at) VALUES ('srv-old','old','10.0.0.5',22,'u','password',1700000000,1700000000)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	mk := make([]byte, 32)
	randRead(t, mk)
	s, err := Open(path, mk) // migrate: guarded ADD COLUMN x3
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	for _, col := range []string{"pin_format", "pin_source", "pin_device"} {
		if !hasColumn(t, s.db, "host_keys", col) {
			t.Fatalf("migrated host_keys missing %s", col)
		}
	}
	if !hasColumn(t, s.db, "entry_host_keys", "key_algorithm") {
		t.Fatal("migrated entry_host_keys missing key_algorithm")
	}
	pins, err := s.ListEntryPins()
	if err != nil || len(pins) != 1 {
		t.Fatalf("迁移后按条目的锚 = %+v, %v", pins, err)
	}
	got := pins[0]
	if got.ServerID != "srv-old" || got.Source != "tofu" || got.Device != "" || got.HandshakeAddr != "10.0.0.5:22" {
		t.Fatalf("迁移后的锚来源信息 = %+v", got)
	}
	var legacy int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM host_keys`).Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	if legacy != 0 {
		t.Fatalf("迁移后旧锚表应清空，仍有 %d 行", legacy)
	}
}

// TestPinMatchesDualMode pins the system's single equality definition (§5) in
// all four quadrants: blob↔blob equal/unequal, blob pin against a mismatching
// presented key, fingerprint anchor against the presented key's fingerprint
// (equal/unequal). Exercises Pins read back FROM THE DB, not just hand-built.
func TestPinMatchesDualMode(t *testing.T) {
	s := newTestStore(t)
	keyA, fpA := pinHostKey(t)
	keyB, _ := pinHostKey(t)

	blobID := addEntryForTest(t, s, "blobpin", "203.0.113.1", 22)
	fpID := addEntryForTest(t, s, "fppin", "203.0.113.2", 22)
	if err := s.SaveEntryPin(serverOf(t, s, blobID), keyA); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertManualEntryPin(serverOf(t, s, fpID), "", []byte(fpA), PinFormatFingerprint, false, func(old *PinMeta) AuditRow {
		return AuditRow{Action: "pin-manual"}
	}); err != nil {
		t.Fatal(err)
	}

	blobPin := pinEntryForTest(t, s, blobID)
	if blobPin.Format != PinFormatBlob || !bytes.Equal(blobPin.Blob, keyA) {
		t.Fatalf("blob anchor readback: %+v", blobPin)
	}
	fpPin := pinEntryForTest(t, s, fpID)
	if fpPin.Format != PinFormatFingerprint || string(fpPin.Blob) != fpA {
		t.Fatalf("fingerprint anchor readback: %+v", fpPin)
	}

	// blob ↔ blob: byte equality both ways
	if !blobPin.Matches(keyA) {
		t.Fatal("blob pin must match its own key bytes")
	}
	if blobPin.Matches(keyB) {
		t.Fatal("blob pin must reject a different key")
	}
	// fingerprint anchor vs presented key (指纹锚对呈现指纹)
	if !fpPin.Matches(keyA) {
		t.Fatal("fingerprint pin must match the key it fingerprints")
	}
	if fpPin.Matches(keyB) {
		t.Fatal("fingerprint pin must reject a different key")
	}
	// a garbage fingerprint string matches nothing (fail closed)
	if (&Pin{Blob: []byte("SHA256:not-a-real-fingerprint"), Format: PinFormatFingerprint}).Matches(keyA) {
		t.Fatal("garbage fingerprint pin must match nothing")
	}
	// unparseable presented bytes match nothing (fail closed, both modes)
	if blobPin.Matches([]byte("garbage")) || fpPin.Matches([]byte("garbage")) {
		t.Fatal("unparseable presented key must match nothing")
	}
	// nil anchor matches nothing (caller treats nil as unpinned)
	var nilPin *Pin
	if nilPin.Matches(keyA) {
		t.Fatal("nil pin must match nothing")
	}
}
