package paths

import (
	"os"
	"path/filepath"
)

// MasterKeyFilename is the on-disk master key file (plaintext under L1+ threat model).
const MasterKeyFilename = "master.key.plain"

// CacheDekFilename is the offline-cache DEK file.
const CacheDekFilename = "cache-dek.key"

// StoreFilename is the encrypted vault database.
const StoreFilename = "store.db"

// ServeLogFilename is the serve process log.
const ServeLogFilename = "serve.log"

// ServeCertFilename / ServeKeyFilename are the self-signed serve TLS cert + key
// (auto-generated on first `serve` start when no --tls-cert is given).
const (
	ServeCertFilename = "serve-cert.pem"
	ServeKeyFilename  = "serve-key.pem"
)

// ServeCertMarkerFilename is the "cert already initialized once" sentinel that
// lives ALONGSIDE the serve cert (same dir). Its presence with an ABSENT cert
// file means the cert was deleted out-of-band; LoadOrCreateServeCert refuses to
// silently regenerate in that case (a regen would invalidate every client's
// pin and look like a MITM). (xcheck F10)
const ServeCertMarkerFilename = ".serve-cert-initialized"

// VaultDir returns the program-fixed vault directory (env override via
// SSHMGR_STORE / SSHMGR_FILEKEY_PATH is handled per-file, not here).
// See spec §3.1. Platform root from vaultRoot() (paths_windows.go / paths_unix.go).
func VaultDir() (string, error) {
	root, err := vaultRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "ssh-manager"), nil
}

// StorePath returns the store.db path. SSHMGR_STORE overrides (test/migrate).
func StorePath() (string, error) {
	if v := os.Getenv("SSHMGR_STORE"); v != "" {
		return v, nil
	}
	dir, err := VaultDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, StoreFilename), nil
}

// MasterKeyPath returns the master.key path. SSHMGR_FILEKEY_PATH overrides (test/migrate).
func MasterKeyPath() (string, error) {
	if v := os.Getenv("SSHMGR_FILEKEY_PATH"); v != "" {
		return v, nil
	}
	dir, err := VaultDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, MasterKeyFilename), nil
}

// CacheDekPath returns the offline-cache DEK path.
func CacheDekPath() (string, error) {
	dir, err := VaultDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, CacheDekFilename), nil
}

// ServeLogPath returns the serve log path.
func ServeLogPath() (string, error) {
	dir, err := VaultDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, ServeLogFilename), nil
}

// ServeCertPath returns the auto-generated serve TLS cert path. SSHMGR_SERVE_CERT overrides (test).
func ServeCertPath() (string, error) {
	if v := os.Getenv("SSHMGR_SERVE_CERT"); v != "" {
		return v, nil
	}
	dir, err := VaultDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, ServeCertFilename), nil
}

// ServeKeyPath returns the auto-generated serve TLS private key path. SSHMGR_SERVE_KEY overrides (test).
func ServeKeyPath() (string, error) {
	if v := os.Getenv("SSHMGR_SERVE_KEY"); v != "" {
		return v, nil
	}
	dir, err := VaultDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, ServeKeyFilename), nil
}

// ServeCertMarkerPath returns the "cert already initialized once" sentinel path.
// It lives ALONGSIDE the serve cert (same dir), so it automatically follows the
// cert wherever it lands — including temp dirs in tests when SSHMGR_SERVE_CERT
// is overridden. SSHMGR_SERVE_MARKER overrides the location entirely (test).
//
// Presence of this marker with an ABSENT cert file means the cert was deleted
// out-of-band; LoadOrCreateServeCert refuses to silently regenerate in that
// case, because a regen would mint a new key → new fingerprint → invalidate
// every client's pin (looks like a MITM). (xcheck F10)
func ServeCertMarkerPath() (string, error) {
	if v := os.Getenv("SSHMGR_SERVE_MARKER"); v != "" {
		return v, nil
	}
	certPath, err := ServeCertPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(certPath), ServeCertMarkerFilename), nil
}
