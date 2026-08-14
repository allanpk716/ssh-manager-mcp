package clientops

import (
	"crypto/subtle"
	"crypto/tls"
	"fmt"
	"net/http"
	"strings"

	"ssh-manager-mcp/internal/mcpserver"
)

// ResolvePin resolves the server SPKI fingerprint by priority:
// env (SSHMGR_SERVE_PIN) > --pin flag > token-embedded "<code>:<pin>".
// Returns plain=true when no pin is available anywhere. Per xcheck F4 the
// caller now hard-fails on plain=true by default (no silent plaintext); the
// caller gates the plaintext fallback behind --allow-plaintext. Malformed
// env/flag pins are rejected earlier by cachePullCmd (F7), so by the time
// ResolvePin runs an env/flag value is either empty or well-formed. A
// malformed token-embedded pin still falls through to plain here (fail-safe
// against a hand-typed token), which then hits the --allow-plaintext gate.
//
// The embedded-pin split uses the FIRST colon, not the last: the pin itself is
// "sha256:<hex>" and contains a colon, so LastIndex would split inside the pin
// and yield a bare "<hex>" that ParsePin rejects. The device code is specified
// to contain no colon, so first-colon split is unambiguous.
func ResolvePin(envVal, flagVal, token string) (fp string, plain bool) {
	if v, ok := mcpserver.ParsePin(strings.TrimSpace(envVal)); ok {
		return v, false
	}
	if v, ok := mcpserver.ParsePin(strings.TrimSpace(flagVal)); ok {
		return v, false
	}
	// token-embedded: "<code>:sha256:..."
	if i := strings.Index(token, ":"); i >= 0 {
		if v, ok := mcpserver.ParsePin(token[i+1:]); ok {
			return v, false
		}
	}
	return "", true
}

// pinningTransport builds an http.Transport whose TLS handshake is pinned to fp:
// the server leaf cert's SPKI fingerprint MUST equal fp or the handshake fails.
//
// Trust model: the serve cert is SELF-SIGNED (see generateServeCert) — there is
// no external CA to chain to, and on Windows the system verifier additionally
// chokes on ed25519 ("Invalid algorithm specified"). So we cannot rely on the
// default certificate verification (it would always fail before our pin check
// ran). Instead we set BOTH InsecureSkipVerify=true (skip CA/chain/name
// verification — which is impossible for a self-signed cert anyway) AND
// VerifyConnection (enforce the SPKI pin). Per Go's crypto/tls docs,
// InsecureSkipVerify skips the default verifier but does NOT disable
// VerifyConnection, which becomes the sole trust anchor. This is the standard
// HPKP / Tailscale pinning pattern: trust comes from the pin, not from a CA.
// The pin is compared in constant time to avoid an oracle.
func pinningTransport(fp string) (*http.Transport, error) {
	want, ok := mcpserver.ParsePin(fp)
	if !ok {
		return nil, fmt.Errorf("invalid server pin format %q (want sha256:<64hex>)", fp)
	}
	tlsCfg := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // skip CA verification (self-signed serve cert); pin below is the trust anchor
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return fmt.Errorf("server presented no certificate")
			}
			got := mcpserver.SPKIFingerprint(cs.PeerCertificates[0])
			if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
				return fmt.Errorf("server fingerprint mismatch (expected %s, got %s)", want, got)
			}
			return nil
		},
	}
	return &http.Transport{TLSClientConfig: tlsCfg}, nil
}

// stripEmbeddedPin splits "<code>:<pin>" into (code, pin, ok). When the token
// has no valid embedded pin, returns the token unchanged with ok=false so the
// full token goes to the Authorization header as the device code. Uses the
// FIRST colon for the split (the pin "sha256:<hex>" contains its own colon).
func stripEmbeddedPin(token string) (code string, pin string, ok bool) {
	if i := strings.Index(token, ":"); i >= 0 {
		if v, parsed := mcpserver.ParsePin(token[i+1:]); parsed {
			return token[:i], v, true
		}
	}
	return token, "", false
}
