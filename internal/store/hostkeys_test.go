package store

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"net"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/testsshd"
)

func TestHostKeySaveGetRoundTrip(t *testing.T) {
	s := newTestStore(t)
	got, err := s.GetHostKey("gpu.example", 22)
	if err != nil || got != nil {
		t.Fatalf("absent: got %v, %v", got, err)
	}
	blob := []byte{1, 2, 3, 4}
	if err := s.SaveHostKey("gpu.example", 22, blob); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetHostKey("gpu.example", 22)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, blob) {
		t.Fatalf("got %v want %v", got, blob)
	}
	// upsert: saving again replaces
	if err := s.SaveHostKey("gpu.example", 22, []byte{9, 9}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetHostKey("gpu.example", 22)
	if !bytes.Equal(got, []byte{9, 9}) {
		t.Fatal("upsert did not replace")
	}
}

func TestHostKeysKeyedByHostPort(t *testing.T) {
	// Two testsshd instances on different ports get distinct host keys (testsshd
	// generates a fresh key per Start). Storing both against the SAME host must not
	// clobber — this proves host:port keying (legacy host-only keying would collide).
	addr1, hk1, cleanup1 := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup1()
	addr2, hk2, cleanup2 := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup2()

	h1, p1, err := net.SplitHostPort(addr1)
	if err != nil {
		t.Fatal(err)
	}
	h2, p2, err := net.SplitHostPort(addr2)
	if err != nil {
		t.Fatal(err)
	}
	if hk1.Marshal() == nil || bytes.Equal(hk1.Marshal(), hk2.Marshal()) {
		t.Fatal("test servers must have distinct host keys for this test")
	}

	s := newTestStore(t)
	port1, _ := strconv.Atoi(p1)
	port2, _ := strconv.Atoi(p2)
	if err := s.SaveHostKey(h1, port1, hk1.Marshal()); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveHostKey(h2, port2, hk2.Marshal()); err != nil {
		t.Fatal(err)
	}

	got1, err := s.GetHostKey(h1, port1)
	if err != nil {
		t.Fatal(err)
	}
	got2, err := s.GetHostKey(h2, port2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got1, hk1.Marshal()) || !bytes.Equal(got2, hk2.Marshal()) {
		t.Fatal("host keys clobbered across ports — keying is not host:port")
	}
}

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

// seedFingerprintPin inserts a fingerprint-format pin row directly. (No
// production writer of that format exists yet — owner `--fingerprint` lands in
// a later task — so tests seed the row the way that command will.)
func seedFingerprintPin(t *testing.T, s *Store, host string, port int, fp string) {
	t.Helper()
	_, err := s.db.Exec(
		`INSERT INTO host_keys (host_port, key_blob, created_at, pin_format, pin_source, pin_device) VALUES (?,?,?,?,?,?)`,
		hostKeyID(host, port), []byte(fp), now(), PinFormatFingerprint, PinSourceTofu, "")
	if err != nil {
		t.Fatal(err)
	}
}

// mustLoadPin is the test-side point read of the anchor struct (the same
// loader GetHostKey/InsertForwardedPin/ApplyForwardedHostKey share).
func mustLoadPin(t *testing.T, s *Store, host string, port int) *Pin {
	t.Helper()
	p, err := loadPin(s.db, hostKeyID(host, port))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestHostKeysPinColumnsFreshSchema: 新库建表态 — a brand-new store has the
// three pin columns, and a legacy TOFU write lands as blob/tofu/empty (the
// DEFAULTs are the old semantics, verbatim).
func TestHostKeysPinColumnsFreshSchema(t *testing.T) {
	s := newTestStore(t)
	for _, col := range []string{"pin_format", "pin_source", "pin_device"} {
		if !hasColumn(t, s.db, "host_keys", col) {
			t.Fatalf("fresh host_keys table missing %s column", col)
		}
	}
	if err := s.SaveHostKey("h", 22, []byte("k")); err != nil {
		t.Fatal(err)
	}
	var format, source, device string
	if err := s.db.QueryRow(`SELECT pin_format, pin_source, pin_device FROM host_keys WHERE host_port='h:22'`).Scan(&format, &source, &device); err != nil {
		t.Fatal(err)
	}
	if format != "blob" || source != "tofu" || device != "" {
		t.Fatalf("legacy TOFU row = %q/%q/%q, want blob/tofu/empty", format, source, device)
	}
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
// the three columns via Open's guarded ADD COLUMN, and the pre-existing anchor
// back-fills to exactly its old meaning (a blob/tofu pin with no device).
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
	var format, source, device string
	if err := s.db.QueryRow(`SELECT pin_format, pin_source, pin_device FROM host_keys WHERE host_port='10.0.0.5:22'`).Scan(&format, &source, &device); err != nil {
		t.Fatal(err)
	}
	if format != "blob" || source != "tofu" || device != "" {
		t.Fatalf("migrated anchor = %q/%q/%q, want blob/tofu/empty", format, source, device)
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

	if err := s.SaveHostKey("blobpin", 22, keyA); err != nil {
		t.Fatal(err)
	}
	seedFingerprintPin(t, s, "fppin", 22, fpA)

	blobPin := mustLoadPin(t, s, "blobpin", 22)
	if blobPin == nil || blobPin.Format != PinFormatBlob || !bytes.Equal(blobPin.Blob, keyA) {
		t.Fatalf("blob anchor readback: %+v", blobPin)
	}
	fpPin := mustLoadPin(t, s, "fppin", 22)
	if fpPin == nil || fpPin.Format != PinFormatFingerprint || string(fpPin.Blob) != fpA {
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

// TestInsertForwardedPin_InsertsAndAudits: success path — the anchor lands
// with the exact forwarded metadata (blob/forward/device) and the audit row
// commits in the SAME transaction.
func TestInsertForwardedPin_InsertsAndAudits(t *testing.T) {
	s := newTestStore(t)
	keyA, _ := pinHostKey(t)

	equal, err := s.InsertForwardedPin("10.0.0.1", 22, keyA, "dev-laptop",
		AuditRow{Action: "pin-forward", ServerID: "srv1", Command: "host=10.0.0.1:22", Status: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	if equal {
		t.Fatal("fresh insert must report equal=false (nothing pre-existed)")
	}
	var format, source, device string
	if err := s.db.QueryRow(`SELECT pin_format, pin_source, pin_device FROM host_keys WHERE host_port='10.0.0.1:22'`).Scan(&format, &source, &device); err != nil {
		t.Fatal(err)
	}
	if format != "blob" || source != "forward" || device != "dev-laptop" {
		t.Fatalf("forwarded anchor = %q/%q/%q, want blob/forward/dev-laptop", format, source, device)
	}
	rows, err := s.AuditRows(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Action != "pin-forward" || rows[0].ServerID != "srv1" || rows[0].Status != "ok" {
		t.Fatalf("audit row must commit with the anchor: %+v", rows)
	}
}

// TestInsertForwardedPin_ConflictEqualAndUnequal: 409 semantics — insert-only
// never overwrites; equal is judged dual-mode against the existing anchor and
// the conflict path writes NO audit row.
func TestInsertForwardedPin_ConflictEqualAndUnequal(t *testing.T) {
	s := newTestStore(t)
	keyA, fpA := pinHostKey(t)
	keyB, _ := pinHostKey(t)

	if err := s.SaveHostKey("h", 22, keyA); err != nil {
		t.Fatal(err)
	}

	// same key → equal=true, anchor untouched, zero audit rows
	equal, err := s.InsertForwardedPin("h", 22, keyA, "dev", AuditRow{Action: "pin-forward", Status: "ok"})
	if err != nil || !equal {
		t.Fatalf("duplicate forward of the anchored key: equal=%v err=%v, want true/nil", equal, err)
	}
	if got := mustLoadPin(t, s, "h", 22); got == nil || !bytes.Equal(got.Blob, keyA) || got.Format != PinFormatBlob {
		t.Fatalf("existing anchor disturbed by conflict path: %+v", got)
	}
	var src string
	if err := s.db.QueryRow(`SELECT pin_source FROM host_keys WHERE host_port='h:22'`).Scan(&src); err != nil {
		t.Fatal(err)
	}
	if src != "tofu" {
		t.Fatalf("existing anchor source changed by conflict path: %q", src)
	}
	// different key → equal=false, anchor untouched
	equal, err = s.InsertForwardedPin("h", 22, keyB, "dev", AuditRow{Action: "pin-forward", Status: "ok"})
	if err != nil || equal {
		t.Fatalf("conflicting forward: equal=%v err=%v, want false/nil", equal, err)
	}
	if got := mustLoadPin(t, s, "h", 22); got == nil || !bytes.Equal(got.Blob, keyA) {
		t.Fatalf("existing anchor overwritten by insert-only violation: %+v", got)
	}
	if rows, _ := s.AuditRows(10); len(rows) != 0 {
		t.Fatalf("conflict path must not write audit rows, got %d", len(rows))
	}

	// fingerprint anchor judged dual-mode: the PRESENTED key matches via its
	// fingerprint, not byte equality with the stored fingerprint string.
	seedFingerprintPin(t, s, "fp", 22, fpA)
	if equal, _ := s.InsertForwardedPin("fp", 22, keyA, "dev", AuditRow{}); !equal {
		t.Fatal("forwarded key matching a fingerprint anchor must report equal=true")
	}
	if equal, _ := s.InsertForwardedPin("fp", 22, keyB, "dev", AuditRow{}); equal {
		t.Fatal("forwarded key NOT matching the fingerprint anchor must report equal=false")
	}
	if got := mustLoadPin(t, s, "fp", 22); got == nil || got.Format != PinFormatFingerprint {
		t.Fatalf("fingerprint anchor disturbed: %+v", got)
	}
}

// TestInsertForwardedPin_AuditFailureRollsBackPin: 原子性 — if the same-tx
// audit write fails, the anchor must NOT land (injection: no audit_log table).
func TestInsertForwardedPin_AuditFailureRollsBackPin(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.db.Exec(`DROP TABLE audit_log`); err != nil {
		t.Fatal(err)
	}
	keyA, _ := pinHostKey(t)
	if _, err := s.InsertForwardedPin("10.0.0.2", 22, keyA, "dev", AuditRow{Action: "pin-forward", Status: "ok"}); err == nil {
		t.Fatal("audit write failure must fail the whole insert")
	}
	if p := mustLoadPin(t, s, "10.0.0.2", 22); p != nil {
		t.Fatalf("anchor landed despite audit failure — insert is not atomic: %+v", p)
	}
}

// TestInsertForwardedPin_ConcurrentInsertOnly: 并发 insert-only — two
// goroutines racing the same unpinned host:port with different keys produce
// EXACTLY one anchor and one audit row; the loser must get equal=false, nil.
func TestInsertForwardedPin_ConcurrentInsertOnly(t *testing.T) {
	s := newTestStore(t)
	keyA, _ := pinHostKey(t)
	keyB, _ := pinHostKey(t)

	type outcome struct {
		equal bool
		err   error
	}
	results := make(chan outcome, 2)
	var wg sync.WaitGroup
	for _, blob := range [][]byte{keyA, keyB} {
		wg.Add(1)
		go func(b []byte) {
			defer wg.Done()
			equal, err := s.InsertForwardedPin("race", 22, b, "dev", AuditRow{Action: "pin-forward", Status: "ok"})
			results <- outcome{equal, err}
		}(blob)
	}
	wg.Wait()
	close(results)
	for r := range results {
		if r.err != nil {
			t.Fatalf("concurrent insert must never error: %v", r.err)
		}
		if r.equal {
			t.Fatal("loser of a different-key race must report equal=false")
		}
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM host_keys WHERE host_port='race:22'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("insert-only violated: %d anchors survived the race, want exactly 1", n)
	}
	var blob []byte
	if err := s.db.QueryRow(`SELECT key_blob FROM host_keys WHERE host_port='race:22'`).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(blob, keyA) && !bytes.Equal(blob, keyB) {
		t.Fatal("surviving anchor is neither contender — the row was corrupted")
	}
	if rows, _ := s.AuditRows(10); len(rows) != 1 {
		t.Fatalf("exactly the winner's audit row must exist, got %d", len(rows))
	}
}
