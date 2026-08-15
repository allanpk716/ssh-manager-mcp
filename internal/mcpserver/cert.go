package mcpserver

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"ssh-manager-mcp/internal/paths"
	"ssh-manager-mcp/internal/store"
)

const serveCertSubject = "ssh-manager serve"

// SPKIFingerprint returns the canonical pinned fingerprint of a server cert's
// public key: "sha256:" + hex(sha256(SubjectPublicKeyInfo DER)). Pinning the
// SPKI (not the whole DER cert) means re-signing the SAME key keeps the pin
// valid, while swapping the key (a MITM) changes the fingerprint. This is the
// HPKP / Tailscale / step convention.
func SPKIFingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ParsePin validates that s is a "sha256:<64-hex>" fingerprint. Hex may be
// upper or lower case; on success it returns the lowercased normalized form
// and ok=true. Any other shape returns ("", false).
func ParsePin(s string) (string, bool) {
	const prefix = "sha256:"
	const hexLen = 64
	if len(s) != len(prefix)+hexLen || s[:len(prefix)] != prefix {
		return "", false
	}
	hexPart := s[len(prefix):]
	for _, c := range []byte(hexPart) {
		ok := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !ok {
			return "", false
		}
	}
	return prefix + strings.ToLower(hexPart), true
}

// LoadOrCreateServeCert is a three-state idempotent initializer:
//  1. cert present + parses as a valid keypair → load (return fingerprint).
//  2. cert absent + init marker ABSENT → first-time init: generate a fresh
//     ed25519 self-signed cert+key, write with HardenACL, AND write the init
//     marker, then return the fingerprint.
//  3. cert absent + init marker PRESENT → the cert was generated once (so
//     clients are pinned to its fingerprint) and has since been deleted
//     out-of-band. Return an error; refuse to silently regenerate, because a
//     new key → new fingerprint → every client's pin mismatches = looks like a
//     MITM. The operator must deliberately delete BOTH cert+marker, then
//     re-enroll clients.
//
// If the files exist but are corrupt or mismatched (state 1's parse fails), an
// error is returned — the caller MUST refuse to start, never silently
// regenerate. (xcheck F10)
func LoadOrCreateServeCert() (certPath, keyPath, fingerprint string, err error) {
	certPath, err = paths.ServeCertPath()
	if err != nil {
		return "", "", "", err
	}
	keyPath, err = paths.ServeKeyPath()
	if err != nil {
		return "", "", "", err
	}

	// Cert present? Try to load.
	if _, statErr := os.Stat(certPath); statErr == nil {
		fp, loadErr := loadServeCertFingerprint(certPath, keyPath)
		if loadErr != nil {
			return "", "", "", fmt.Errorf("serve cert at %s is corrupt or mismatches its key: %w "+
				"(refusing to start; to regenerate, delete BOTH the cert and the init marker, then re-enroll clients)",
				certPath, loadErr)
		}
		return certPath, keyPath, fp, nil
	} else if !os.IsNotExist(statErr) {
		return "", "", "", statErr
	}

	// F10: cert absent. Before silently regenerating, check the init marker.
	// If the marker exists, the cert was generated once (so clients are pinned
	// to its fingerprint) and has since been deleted out-of-band. Regenerating
	// would mint a new key → new fingerprint → invalidate every client's pin,
	// which would look like a MITM attack to every cache. Refuse; the operator
	// must deliberately delete BOTH the marker and the cert to acknowledge the
	// re-enroll cost.
	markerPath, err := paths.ServeCertMarkerPath()
	if err != nil {
		return "", "", "", err
	}
	if _, statErr := os.Stat(markerPath); statErr == nil {
		return "", "", "", fmt.Errorf("serve cert %s is missing but the initialization marker %s exists "+
			"(cert appears deleted out-of-band; refusing to silently regenerate — that would invalidate all client pins). "+
			"To regenerate deliberately, delete BOTH the marker and the cert, then re-enroll all clients", certPath, markerPath)
	} else if !os.IsNotExist(statErr) {
		return "", "", "", statErr
	}

	// Absent + marker absent → first-time init: generate a fresh self-signed cert + key, harden, re-load for fingerprint.
	if err := generateServeCert(certPath, keyPath); err != nil {
		return "", "", "", err
	}
	// Write the init marker so a future out-of-band cert deletion is detectable.
	// Atomic + HardenACL, same discipline as the cert/key files.
	if err := atomicWriteFile(markerPath, []byte("initialized\n"), 0o600); err != nil {
		return "", "", "", fmt.Errorf("write cert-init marker: %w", err)
	}
	if err := store.HardenACL(markerPath); err != nil {
		return "", "", "", fmt.Errorf("harden cert-init marker ACL: %w", err)
	}
	fp, err := loadServeCertFingerprint(certPath, keyPath)
	if err != nil {
		return "", "", "", err
	}
	return certPath, keyPath, fp, nil
}

// loadServeCertFingerprint parses the cert+keypair from disk, validates the
// keypair matches (via tls.LoadX509KeyPair), and returns the SPKI fingerprint.
func loadServeCertFingerprint(certPath, keyPath string) (string, error) {
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
		return "", err
	}
	der, err := os.ReadFile(certPath)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(der)
	if block == nil {
		return "", fmt.Errorf("no PEM block in %s", certPath)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", err
	}
	return SPKIFingerprint(cert), nil
}

// generateServeCert creates a fresh ed25519 self-signed cert and key at the
// given paths via temp+rename atomic writes, then calls store.HardenACL on
// each (Windows ACL hardening; no-op on Unix where 0600 is enforced).
func generateServeCert(certPath, keyPath string) error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generate ed25519 key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("generate serial: %w", err)
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "localhost"
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: serveCertSubject},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(100 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature, // ed25519 is a pure-signature algorithm; KeyEncipherment is meaningless for it (xcheck F9)
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{host},
		IPAddresses:  LocalNonLoopbackIPs(),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return fmt.Errorf("create certificate: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return fmt.Errorf("marshal PKCS8 key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes})

	if err := atomicWriteFile(certPath, certPEM, 0o600); err != nil {
		return fmt.Errorf("write cert: %w", err)
	}
	if err := atomicWriteFile(keyPath, keyPEM, 0o600); err != nil {
		return fmt.Errorf("write key: %w", err)
	}
	// HardenACL = master.key.plain-level protection on Windows; no-op on Unix.
	if err := store.HardenACL(keyPath); err != nil {
		return fmt.Errorf("harden serve key ACL: %w", err)
	}
	if err := store.HardenACL(certPath); err != nil {
		return fmt.Errorf("harden serve cert ACL: %w", err)
	}
	return nil
}

// LocalNonLoopbackIPs returns this host's non-loopback unicast IPs for the
// cert SAN. Core trust is the SPKI pin (not the hostname), but listing IPs avoids
// spurious name-check failures when a client connects by IP. Best-effort.
// Exported since Plan 19 T4: the role wizard's LAN-address picker reuses the
// exact same enumeration (the picked IP should be a cert SAN — that is what
// makes the address clients will use actually covered by the self-signed cert).
func LocalNonLoopbackIPs() []net.IP {
	var out []net.IP
	ifaces, err := net.InterfaceAddrs()
	if err != nil {
		return out
	}
	for _, a := range ifaces {
		ipNet, ok := a.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() {
			continue
		}
		out = append(out, ipNet.IP)
	}
	return out
}

// atomicWriteFile writes data to path via temp + fsync + chmod + rename, so a
// crash never leaves a half-written cert or key. Local duplicate of
// internal/cli/backup.go's atomicWriteFile; not refactored across packages to
// avoid scope creep (see plan NOTE).
func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op after successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil { // fsync — durability before rename
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		return err
	}
	// fsync the parent dir on non-Windows so the rename itself survives a crash
	// (parity with internal/cli/backup.go). On Windows there is no fsync on a
	// directory handle; os.Rename is already crash-consistent there.
	if runtime.GOOS != "windows" {
		if d, err := os.Open(dir); err == nil {
			_ = d.Sync()
			_ = d.Close()
		}
	}
	return os.Rename(tmpPath, path)
}
