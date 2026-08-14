package clientops

import (
	"crypto/tls"
	"strings"
	"testing"
)

// TestResolvePin (moved verbatim from cli/cache_test.go; resolvePin → ResolvePin).
func TestResolvePin(t *testing.T) {
	goodPin := "sha256:" + strings.Repeat("a", 64)
	token := "devcode-xyz" // no ':' → no embedded pin
	tokenEmbedded := "devcode-xyz:" + goodPin

	cases := []struct {
		name            string
		envVal, flagVal string
		token           string
		wantFP          string
		wantPlain       bool
	}{
		{"none → plain", "", "", token, "", true},
		{"env wins", "sha256:" + strings.Repeat("b", 64), goodPin, token, "sha256:" + strings.Repeat("b", 64), false},
		{"flag over token-embedded", "", goodPin, tokenEmbedded, goodPin, false},
		{"env over flag", "sha256:" + strings.Repeat("c", 64), goodPin, token, "sha256:" + strings.Repeat("c", 64), false},
		{"token-embedded when no env/flag", "", "", tokenEmbedded, goodPin, false},
		{"token without : is plain", "", "", token, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("SSHMGR_SERVE_PIN", c.envVal)
			gotFP, plain := ResolvePin(c.envVal, c.flagVal, c.token)
			if plain != c.wantPlain {
				t.Fatalf("plain=%v want %v", plain, c.wantPlain)
			}
			if !plain && gotFP != c.wantFP {
				t.Fatalf("fp=%q want %q", gotFP, c.wantFP)
			}
		})
	}
}

// TestPinningTransport_BadPinErrors (moved verbatim from cli/cache_test.go).
func TestPinningTransport_BadPinErrors(t *testing.T) {
	// ResolvePin returns a parsed fp; constructing the transport from a
	// well-formed fp must succeed.
	fp := "sha256:" + strings.Repeat("a", 64)
	tr, err := pinningTransport(fp)
	if err != nil {
		t.Fatalf("pinningTransport: %v", err)
	}
	if tr.TLSClientConfig == nil {
		t.Fatal("TLSClientConfig nil")
	}
	if tr.TLSClientConfig.MinVersion != tls.VersionTLS13 {
		t.Fatalf("MinVersion not TLS1.3: %v", tr.TLSClientConfig.MinVersion)
	}
	if tr.TLSClientConfig.VerifyConnection == nil {
		t.Fatal("VerifyConnection callback not set")
	}
}

// TestPinningTransport_NoPeerCert_HardFails (moved verbatim from
// cli/cache_test.go) verifies the F12 branch: when the server presents no peer
// certificates (anonymous TLS or no-cert handshake), the VerifyConnection
// callback must hard-fail with a "no certificate" error. This is a unit test
// that directly invokes the callback with an empty ConnectionState.
func TestPinningTransport_NoPeerCert_HardFails(t *testing.T) {
	fp := "sha256:" + strings.Repeat("a", 64)
	tr, err := pinningTransport(fp)
	if err != nil {
		t.Fatal(err)
	}
	cb := tr.TLSClientConfig.VerifyConnection
	if cb == nil {
		t.Fatal("no VerifyConnection callback")
	}
	// Empty peer certs (anonymous / no-cert server).
	err = cb(tls.ConnectionState{PeerCertificates: nil})
	if err == nil {
		t.Fatal("expected error when server presents no certificate")
	}
	if !strings.Contains(err.Error(), "no certificate") {
		t.Fatalf("error should mention no certificate, got: %v", err)
	}
}
