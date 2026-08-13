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
