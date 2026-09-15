package sshbroker

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

// fakeHostKeyStore is the package's HostKeyStore test double. Anchors are held
// as real *store.Pin values so the dual-mode matrix drives the same struct the
// production store returns (Plan 48 §5).
type fakeHostKeyStore struct {
	pins  map[string]*store.Pin // keyed by host:port
	saves int
}

func (f *fakeHostKeyStore) GetHostKey(host string, port int) (*store.Pin, error) {
	return f.pins[fmt.Sprintf("%s:%d", host, port)], nil
}
func (f *fakeHostKeyStore) SaveHostKey(host string, port int, k []byte) error {
	if f.pins == nil {
		f.pins = map[string]*store.Pin{}
	}
	f.saves++
	f.pins[fmt.Sprintf("%s:%d", host, port)] = &store.Pin{Blob: k, Format: store.PinFormatBlob}
	return nil
}

func TestHostKeyTOFURecordsThenVerifies(t *testing.T) {
	st := &fakeHostKeyStore{}
	addr, _, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()

	cb, err := HostKeyTOFU(st, "h", portOf(addr))
	if err != nil {
		t.Fatal(err)
	}
	// first connect: records host key
	cli, err := Connect(context.Background(), hostOf(addr), portOf(addr), "u", PasswordAuth("pw"), cb)
	if err != nil {
		t.Fatalf("first connect (TOFU): %v", err)
	}
	defer cli.Close()
	if len(st.pins) != 1 {
		t.Fatalf("expected 1 recorded key, got %d", len(st.pins))
	}

	// second connect: verifies, succeeds
	cb2, _ := HostKeyTOFU(st, "h", portOf(addr))
	cli2, err := Connect(context.Background(), hostOf(addr), portOf(addr), "u", PasswordAuth("pw"), cb2)
	if err != nil {
		t.Fatalf("second connect (verify): %v", err)
	}
	defer cli2.Close()
}

func TestHostKeyMismatchRejected(t *testing.T) {
	// Pre-seed the store with a key that differs from the test server's real key,
	// so the callback must reject the connection as a MITM.
	addr, _, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	st := &fakeHostKeyStore{pins: map[string]*store.Pin{
		fmt.Sprintf("h:%d", portOf(addr)): {Blob: []byte("stale-different-key"), Format: store.PinFormatBlob},
	}}
	cb, _ := HostKeyTOFU(st, "h", portOf(addr))
	_, err := Connect(context.Background(), hostOf(addr), portOf(addr), "u", PasswordAuth("pw"), cb)
	if err == nil {
		t.Fatal("mismatched host key must be rejected")
	}
	if !errors.Is(err, ErrHostKeyMismatch) {
		t.Fatalf("error must wrap ErrHostKeyMismatch, got %v", err)
	}
}

// --- Plan 48: 双模 TOFU + 双指纹 mismatch(spec §2.2、§5、rider 2) ------------

// anchorWith is fakeHostKeyStore seeding sugar: one pin under "h:22".
func anchorWith(p *store.Pin) *fakeHostKeyStore {
	return &fakeHostKeyStore{pins: map[string]*store.Pin{"h:22": p}}
}

// TestHostKeyTOFU_BlobAnchorEqualPassesWithoutWrite: blob 锚旧语义零回归 —
// a presented key equal to the anchored bytes passes, writing NOTHING (the
// TOFU save path must not fire for an already-anchored host).
func TestHostKeyTOFU_BlobAnchorEqualPassesWithoutWrite(t *testing.T) {
	remote := testPublicKey(t)
	st := anchorWith(&store.Pin{Blob: remote.Marshal(), Format: store.PinFormatBlob})
	cb, err := HostKeyTOFU(st, "h", 22)
	if err != nil {
		t.Fatal(err)
	}
	if err := cb("h", nil, remote); err != nil {
		t.Fatalf("presented key equal to a blob anchor must pass: %v", err)
	}
	if st.saves != 0 {
		t.Fatalf("equal-presented path must write nothing, got %d saves", st.saves)
	}
}

// TestHostKeyTOFU_FingerprintAnchorEqualPassesWithoutWrite: 指纹锚 + 呈现密钥
// 指纹等值 → pass with zero writes (Plan 48 §5 dual-mode in the callback).
func TestHostKeyTOFU_FingerprintAnchorEqualPassesWithoutWrite(t *testing.T) {
	remote := testPublicKey(t)
	st := anchorWith(&store.Pin{Blob: []byte(ssh.FingerprintSHA256(remote)), Format: store.PinFormatFingerprint})
	cb, err := HostKeyTOFU(st, "h", 22)
	if err != nil {
		t.Fatal(err)
	}
	if err := cb("h", nil, remote); err != nil {
		t.Fatalf("presented key matching a fingerprint anchor must pass: %v", err)
	}
	if st.saves != 0 {
		t.Fatalf("equal-presented path must write nothing, got %d saves", st.saves)
	}
}

// TestHostKeyTOFU_MismatchCarriesBothFingerprints: rider 2 — a blob-anchor
// rejection wraps ErrHostKeyMismatch, names BOTH fingerprints (presented and
// pinned — the pinned one computed FROM the stored blob), keeps the frozen
// prefix verbatim, single line, and adds NO other key material.
func TestHostKeyTOFU_MismatchCarriesBothFingerprints(t *testing.T) {
	remote := testPublicKey(t)
	pinned := testPublicKey(t)
	st := anchorWith(&store.Pin{Blob: pinned.Marshal(), Format: store.PinFormatBlob})
	cb, err := HostKeyTOFU(st, "h", 22)
	if err != nil {
		t.Fatal(err)
	}
	err = cb("h", nil, remote)
	if err == nil {
		t.Fatal("mismatching host key must be rejected")
	}
	if !errors.Is(err, ErrHostKeyMismatch) {
		t.Fatalf("error must wrap ErrHostKeyMismatch, got %v", err)
	}
	const prefix = "host key mismatch: possible MITM, connection rejected"
	if !strings.HasPrefix(err.Error(), prefix) {
		t.Fatalf("error text must keep the frozen prefix verbatim, got: %v", err)
	}
	if !strings.Contains(err.Error(), ssh.FingerprintSHA256(remote)) {
		t.Fatalf("error text must carry the PRESENTED fingerprint: %v", err)
	}
	if !strings.Contains(err.Error(), ssh.FingerprintSHA256(pinned)) {
		t.Fatalf("error text must carry the PINNED fingerprint: %v", err)
	}
	if strings.ContainsAny(err.Error(), "\n\r") {
		t.Fatalf("error text must stay single-line: %q", err.Error())
	}
	// rider 2: 只增指纹值 — the presented key's own bytes must not leak.
	if strings.Contains(err.Error(), base64.StdEncoding.EncodeToString(remote.Marshal())) {
		t.Fatalf("error text must not carry key material: %v", err)
	}
}

// TestHostKeyTOFU_FingerprintAnchorMismatchCarriesStoredString: 指纹锚不等值 —
// the PINNED side of the text is the stored fingerprint STRING as-is (a
// fingerprint anchor holds no key bytes to fingerprint).
func TestHostKeyTOFU_FingerprintAnchorMismatchCarriesStoredString(t *testing.T) {
	remote := testPublicKey(t)
	other := testPublicKey(t) // must differ from remote
	storedFP := ssh.FingerprintSHA256(other)
	st := anchorWith(&store.Pin{Blob: []byte(storedFP), Format: store.PinFormatFingerprint})
	cb, err := HostKeyTOFU(st, "h", 22)
	if err != nil {
		t.Fatal(err)
	}
	err = cb("h", nil, remote)
	if err == nil {
		t.Fatal("mismatching host key must be rejected")
	}
	if !errors.Is(err, ErrHostKeyMismatch) {
		t.Fatalf("error must wrap ErrHostKeyMismatch, got %v", err)
	}
	if !strings.Contains(err.Error(), ssh.FingerprintSHA256(remote)) {
		t.Fatalf("error text must carry the PRESENTED fingerprint: %v", err)
	}
	if !strings.Contains(err.Error(), storedFP) {
		t.Fatalf("error text must carry the pinned fingerprint STRING as-is (%s): %v", storedFP, err)
	}
}
