package cli

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/buildinfo"
	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/updater"
)

// TestServeCertInfo_PrintsFingerprint asserts `serve cert-info` prints the
// serve TLS cert's SPKI fingerprint (auto-generating the cert on first run).
func TestServeCertInfo_PrintsFingerprint(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SSHMGR_SERVE_CERT", filepath.Join(dir, "serve-cert.pem"))
	t.Setenv("SSHMGR_SERVE_KEY", filepath.Join(dir, "serve-key.pem"))
	seamServeCertInfoNotInstalled(t)

	root := NewRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetArgs([]string{"serve", "cert-info"})
	if err := root.Execute(); err != nil {
		t.Fatalf("serve cert-info: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "sha256:") {
		t.Fatalf("cert-info must print fingerprint: %s", s)
	}
}

func seamServeCertInfoNotInstalled(t *testing.T) {
	t.Helper()
	origProbe, origTLS := serveServiceProbe, registeredServeTLSFiles
	t.Cleanup(func() {
		serveServiceProbe = origProbe
		registeredServeTLSFiles = origTLS
	})
	serveServiceProbe = func(string) updater.ProbeResult {
		return updater.ProbeResult{State: updater.ProbeNotInstalled, Desc: "not installed"}
	}
	registeredServeTLSFiles = func(string) (string, string, error) {
		return "", "", errors.New("must not read registration when service is not installed")
	}
}

func writeTestCertificatePair(t *testing.T, certPath, keyPath string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "sshmgr cert-info test"},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	fp, err := mcpserver.ServeCertificateFingerprint(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	return fp
}

func TestServeCertInfoUsesRegisteredCustomCertificate(t *testing.T) {
	dir := t.TempDir()
	defaultCert := filepath.Join(dir, "serve-cert.pem")
	defaultKey := filepath.Join(dir, "serve-key.pem")
	customCert := filepath.Join(dir, "custom-cert.pem")
	customKey := filepath.Join(dir, "custom-key.pem")
	t.Setenv("SSHMGR_SERVE_CERT", defaultCert)
	t.Setenv("SSHMGR_SERVE_KEY", defaultKey)
	want := writeTestCertificatePair(t, customCert, customKey)

	origProbe, origTLS := serveServiceProbe, registeredServeTLSFiles
	t.Cleanup(func() {
		serveServiceProbe = origProbe
		registeredServeTLSFiles = origTLS
	})
	serveServiceProbe = func(name string) updater.ProbeResult {
		if name != buildinfo.ServeServiceName {
			t.Fatalf("probe service name = %q, want %q", name, buildinfo.ServeServiceName)
		}
		return updater.ProbeResult{State: updater.ProbeInstalled, Desc: "installed (running)"}
	}
	registeredServeTLSFiles = func(name string) (string, string, error) {
		if name != buildinfo.ServeServiceName {
			t.Fatalf("registration service name = %q, want %q", name, buildinfo.ServeServiceName)
		}
		return customCert, customKey, nil
	}

	root := NewRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetArgs([]string{"serve", "cert-info"})
	if err := root.Execute(); err != nil {
		t.Fatalf("serve cert-info: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, customCert) || !strings.Contains(got, customKey) || !strings.Contains(got, want) {
		t.Fatalf("custom certificate output missing paths or fingerprint:\n%s", got)
	}
	if !strings.Contains(got, fmt.Sprintf("服务使用自定义证书：%s", customCert)) {
		t.Fatalf("custom certificate explanation missing:\n%s", got)
	}
	if strings.Contains(got, defaultCert) {
		t.Fatalf("registered custom certificate must take precedence over default:\n%s", got)
	}
}

func TestServeCertInfoRegistrationReadFailureWarnsAndFallsBack(t *testing.T) {
	dir := t.TempDir()
	defaultCert := filepath.Join(dir, "serve-cert.pem")
	defaultKey := filepath.Join(dir, "serve-key.pem")
	t.Setenv("SSHMGR_SERVE_CERT", defaultCert)
	t.Setenv("SSHMGR_SERVE_KEY", defaultKey)

	origProbe, origTLS := serveServiceProbe, registeredServeTLSFiles
	t.Cleanup(func() {
		serveServiceProbe = origProbe
		registeredServeTLSFiles = origTLS
	})
	serveServiceProbe = func(string) updater.ProbeResult {
		return updater.ProbeResult{State: updater.ProbeInstalled, Desc: "installed (running)"}
	}
	registeredServeTLSFiles = func(string) (string, string, error) {
		return "", "", errors.New("registration record is unreadable")
	}

	out := &bytes.Buffer{}
	warnings := &bytes.Buffer{}
	if err := runServeCertInfo(out, warnings); err != nil {
		t.Fatalf("serve cert-info fallback: %v", err)
	}
	if !strings.Contains(out.String(), defaultCert) || !strings.Contains(out.String(), "sha256:") {
		t.Fatalf("fallback output missing default certificate:\n%s", out.String())
	}
	if !strings.Contains(warnings.String(), "改用默认证书路径") {
		t.Fatalf("registration read failure must warn before fallback:\n%s", warnings.String())
	}
}

func TestServeCertInfoInstalledWithoutCustomCertificateUsesDefault(t *testing.T) {
	dir := t.TempDir()
	defaultCert := filepath.Join(dir, "serve-cert.pem")
	defaultKey := filepath.Join(dir, "serve-key.pem")
	t.Setenv("SSHMGR_SERVE_CERT", defaultCert)
	t.Setenv("SSHMGR_SERVE_KEY", defaultKey)

	origProbe, origTLS := serveServiceProbe, registeredServeTLSFiles
	t.Cleanup(func() {
		serveServiceProbe = origProbe
		registeredServeTLSFiles = origTLS
	})
	serveServiceProbe = func(string) updater.ProbeResult {
		return updater.ProbeResult{State: updater.ProbeInstalled, Desc: "installed (running)"}
	}
	registeredServeTLSFiles = func(string) (string, string, error) {
		return "", "", nil
	}

	out := &bytes.Buffer{}
	warnings := &bytes.Buffer{}
	if err := runServeCertInfo(out, warnings); err != nil {
		t.Fatalf("serve cert-info default fallback: %v", err)
	}
	if !strings.Contains(out.String(), defaultCert) || !strings.Contains(out.String(), "sha256:") {
		t.Fatalf("default fallback output missing certificate:\n%s", out.String())
	}
	if strings.Contains(out.String(), "服务使用自定义证书") || warnings.Len() != 0 {
		t.Fatalf("registered service without custom certificate needs no warning or custom line:\n%s\n%s", out.String(), warnings.String())
	}
}
