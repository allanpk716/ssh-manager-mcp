package mcpserver

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/paths"
)

// pkixName is a tiny test helper: a pkix.Name with only CommonName set.
func pkixName(s string) pkix.Name { return pkix.Name{CommonName: s} }

// bigOne is a tiny test helper: serial number 1.
func bigOne() *big.Int { return big.NewInt(1) }

func TestSPKIFingerprint(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	spki, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(spki)
	want := "sha256:" + hex.EncodeToString(sum[:])

	// Self-signed: priv signs the cert whose SPKI is pub.
	certDER, err := x509.CreateCertificate(nil, &x509.Certificate{
		Subject:      pkixName("test"),
		SerialNumber: bigOne(),
	}, &x509.Certificate{}, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		t.Fatal(err)
	}
	got := SPKIFingerprint(cert)
	if got != want {
		t.Fatalf("SPKIFingerprint = %q, want %q", got, want)
	}

	// The prefix is mandatory; a bare hex hash must NOT be returned.
	if !strings.HasPrefix(got, "sha256:") {
		t.Fatalf("SPKIFingerprint missing sha256: prefix: %q", got)
	}
}

func TestParsePin(t *testing.T) {
	cases := []struct {
		in   string
		ok   bool
		want string // expected normalized return when ok; "" means don't check value
	}{
		{"sha256:" + strings.Repeat("a", 64), true, "sha256:" + strings.Repeat("a", 64)},
		{"sha256:ABCD" + strings.Repeat("0", 60), true, "sha256:abcd" + strings.Repeat("0", 60)}, // uppercase ok, normalized to lower
		{"sha256:tooshort", false, ""},
		{"sha256:" + strings.Repeat("a", 63), false, ""}, // 63 hex chars — too short
		{"sha256:" + strings.Repeat("a", 65), false, ""}, // 65 hex chars — too long
		{"md5:" + strings.Repeat("a", 32), false, ""},    // wrong algorithm
		{"", false, ""},
		{"garbage", false, ""},
		{"sha256:" + strings.Repeat("z", 64), false, ""}, // non-hex alphabet
	}
	for _, c := range cases {
		got, ok := ParsePin(c.in)
		if ok != c.ok {
			t.Errorf("ParsePin(%q) ok=%v, want %v", c.in, ok, c.ok)
			continue
		}
		if ok && c.want != "" && got != c.want {
			t.Errorf("ParsePin(%q) returned %q, want normalized %q", c.in, got, c.want)
		}
		if !ok && got != "" {
			t.Errorf("ParsePin(%q) ok=false but returned %q (want empty)", c.in, got)
		}
	}
}

func TestLoadOrCreateServeCert_GenerateThenLoad(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "serve-cert.pem")
	keyPath := filepath.Join(dir, "serve-key.pem")
	t.Setenv("SSHMGR_SERVE_CERT", certPath)
	t.Setenv("SSHMGR_SERVE_KEY", keyPath)

	// First call: files absent → generate.
	gotCert, gotKey, fp1, err := LoadOrCreateServeCert()
	if err != nil {
		t.Fatalf("first LoadOrCreateServeCert: %v", err)
	}
	if gotCert != certPath || gotKey != keyPath {
		t.Fatalf("paths = (%q,%q), want (%q,%q)", gotCert, gotKey, certPath, keyPath)
	}
	if _, ok := ParsePin(fp1); !ok {
		t.Fatalf("generated fingerprint not a valid pin: %q", fp1)
	}
	if fi, err := os.Stat(certPath); err != nil || fi.Size() == 0 {
		t.Fatalf("cert file not written / empty: %v %v", fi, err)
	}
	if fi, err := os.Stat(keyPath); err != nil || fi.Size() == 0 {
		t.Fatalf("key file not written / empty: %v %v", fi, err)
	}

	// Second call: files present + parseable → IDEMPOTENT load, same fingerprint.
	_, _, fp2, err := LoadOrCreateServeCert()
	if err != nil {
		t.Fatalf("second LoadOrCreateServeCert: %v", err)
	}
	if fp1 != fp2 {
		t.Fatalf("fingerprint changed across calls: %q vs %q (must be idempotent)", fp1, fp2)
	}

	// Independent fingerprint check: recompute SPKIFingerprint from the on-disk cert.
	der, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(der)
	if block == nil {
		t.Fatal("no PEM block in cert file")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	if SPKIFingerprint(cert) != fp1 {
		t.Fatalf("on-disk SPKIFingerprint %q != returned %q", SPKIFingerprint(cert), fp1)
	}
}

func TestLoadOrCreateServeCert_CorruptReturnsError(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "serve-cert.pem")
	keyPath := filepath.Join(dir, "serve-key.pem")
	t.Setenv("SSHMGR_SERVE_CERT", certPath)
	t.Setenv("SSHMGR_SERVE_KEY", keyPath)

	// Cert exists but is garbage. Must NOT silently regenerate.
	if err := os.WriteFile(certPath, []byte("not a pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, _, err := LoadOrCreateServeCert()
	if err == nil {
		t.Fatal("LoadOrCreateServeCert returned nil error on corrupt cert; must refuse to start")
	}
	if !strings.Contains(err.Error(), certPath) {
		t.Errorf("error should mention the corrupt path %q; got: %v", certPath, err)
	}
	// Files must NOT have been regenerated.
	got, _ := os.ReadFile(certPath)
	if string(got) != "not a pem" {
		t.Errorf("corrupt cert was overwritten; a corrupt state must not be silently regenerated")
	}
}

func TestLoadOrCreateServeCert_DeletedCertRefusesRegen(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "serve-cert.pem")
	keyPath := filepath.Join(dir, "serve-key.pem")
	markerPath := filepath.Join(dir, paths.ServeCertMarkerFilename)
	t.Setenv("SSHMGR_SERVE_CERT", certPath)
	t.Setenv("SSHMGR_SERVE_KEY", keyPath)
	t.Setenv("SSHMGR_SERVE_MARKER", markerPath)

	// First call: cert absent, marker absent → first-time init: generate + write marker.
	_, _, fp1, err := LoadOrCreateServeCert()
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, statErr := os.Stat(markerPath); statErr != nil {
		t.Fatalf("init marker not written after first generate: %v", statErr)
	}

	// Simulate accidental cert deletion (operator rm'd the cert, NOT the marker).
	if err := os.Remove(certPath); err != nil {
		t.Fatal(err)
	}

	// Second call: cert gone but marker present → MUST refuse to silently regen.
	_, _, fp2, err := LoadOrCreateServeCert()
	if err == nil {
		t.Fatalf("expected refusal when cert deleted but marker present; got new cert fp=%s (fp1=%s) — silent regen invalidates all client pins", fp2, fp1)
	}
	// Error must explain how to recover (delete both marker+cert, re-enroll).
	if !strings.Contains(err.Error(), markerPath) {
		t.Errorf("error should mention the marker path %q so the operator knows what to delete; got: %v", markerPath, err)
	}

	// The refusal must NOT have re-created the cert (that would be the silent-regen
	// we are refusing). Marker should still be present (untouched).
	if _, statErr := os.Stat(certPath); statErr == nil {
		t.Errorf("refusal path must not re-create the cert; cert exists at %s", certPath)
	}
	if _, statErr := os.Stat(markerPath); statErr != nil {
		t.Errorf("marker should still exist after refusal (only cert was deleted): %v", statErr)
	}

	// Recovery path: operator deliberately deletes BOTH marker + cert → next call
	// generates fresh again (and writes a new marker). This proves the refusal is
	// conditioned on the marker's presence, not a permanent brick.
	if err := os.Remove(markerPath); err != nil {
		t.Fatal(err)
	}
	_, _, fp3, err := LoadOrCreateServeCert()
	if err != nil {
		t.Fatalf("after deleting marker, generate should succeed: %v", err)
	}
	if fp3 == fp1 {
		t.Fatalf("fresh generate after marker delete produced the SAME fingerprint %q — ed25519 key generation must be non-deterministic", fp1)
	}
}

func TestGenerateServeCert_KeyUsage_NoKeyEncipherment(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "serve-cert.pem")
	keyPath := filepath.Join(dir, "serve-key.pem")
	if err := generateServeCert(certPath, keyPath); err != nil {
		t.Fatal(err)
	}
	der, _ := os.ReadFile(certPath)
	block, _ := pem.Decode(der)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if cert.KeyUsage&x509.KeyUsageKeyEncipherment != 0 {
		t.Errorf("ed25519 cert should NOT set KeyEncipherment (meaningless for pure-signature alg), got KeyUsage=%v", cert.KeyUsage)
	}
	if cert.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		t.Errorf("cert should set DigitalSignature, got KeyUsage=%v", cert.KeyUsage)
	}
}
