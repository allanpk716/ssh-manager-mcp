package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// TestServeCertInfo_PrintsFingerprint asserts `serve cert-info` prints the
// serve TLS cert's SPKI fingerprint (auto-generating the cert on first run).
func TestServeCertInfo_PrintsFingerprint(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SSHMGR_SERVE_CERT", filepath.Join(dir, "serve-cert.pem"))
	t.Setenv("SSHMGR_SERVE_KEY", filepath.Join(dir, "serve-key.pem"))

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
