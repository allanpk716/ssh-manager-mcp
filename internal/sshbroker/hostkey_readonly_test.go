package sshbroker

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/store"
)

// testPublicKey generates a fresh ed25519 ssh.PublicKey for host-key callback tests.
// (No existing signer/public-key helper lives in this package — hostkey_test.go uses
// a real testsshd, client_test.go rolls its own RSA path — so this 3-liner is the
// minimal fixture for the read-only TOFU tests below.)
func testPublicKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	// ed25519.GenerateKey returns (PublicKey, PrivateKey, error) — pub first.
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return pk
}

// TestHostKeyTOFU_ReadOnlyRefusesUnknown asserts that in read-only (cache) mode, an UNKNOWN
// host key is rejected (not TOFU-pinned) — there is no pin path offline, so MITM-then-pin is
// impossible. SaveHostKey returns ErrReadOnly; HostKeyTOFU surfaces it as a hard refusal
// wrapped as "save host key: <ErrReadOnly>" so errors.Is(err, store.ErrReadOnly) resolves.
func TestHostKeyTOFU_ReadOnlyRefusesUnknown(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "hk.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetReadOnly(nil) // read-only, no audit sidecar

	cb, err := HostKeyTOFU(s, "10.0.0.99", 22)
	if err != nil {
		t.Fatal(err)
	}
	remote := testPublicKey(t)
	if err := cb("10.0.0.99", nil, remote); err == nil {
		t.Fatal("unknown host key on a read-only store must be REFUSED, not pinned")
	} else if !errors.Is(err, store.ErrReadOnly) {
		t.Fatalf("refusal must wrap store.ErrReadOnly, got: %v", err)
	}
}

// TestHostKeyTOFU_ReadOnlyAllowsKnown asserts a KNOWN (cached) host key still matches in
// read-only mode (reads are unaffected) — legitimate offline SSH to a previously-pinned host works.
func TestHostKeyTOFU_ReadOnlyAllowsKnown(t *testing.T) {
	dir := t.TempDir()
	mk := make([]byte, 32)
	s, err := store.Open(filepath.Join(dir, "hk.db"), mk)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	remote := testPublicKey(t)
	marshaled := remote.Marshal()
	// pin the key while WRITABLE, then go read-only
	if err := s.SaveHostKey("10.0.0.99", 22, marshaled); err != nil {
		t.Fatal(err)
	}
	s.SetReadOnly(nil)

	cb, err := HostKeyTOFU(s, "10.0.0.99", 22)
	if err != nil {
		t.Fatal(err)
	}
	if err := cb("10.0.0.99", nil, remote); err != nil {
		t.Fatalf("known host key must match in read-only mode: %v", err)
	}
}
