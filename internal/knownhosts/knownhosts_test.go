package knownhosts

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"golang.org/x/crypto/ssh"
)

// TestRoundtripInProcess proves format → parse is lossless (patterns, key
// type, key bytes) with a key generated IN PROCESS — no ssh-keygen, no gate:
// this runs in the default fast lane. The real-OpenSSH cross-check
// (`ssh-keygen -F` finding our rendered line) stays gated in
// internal/conformance's TestKnownHostsRoundtrip, next to its docker and
// binary helpers.
func TestRoundtripInProcess(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	patterns := "[example.com]:2222"

	gotPatterns, gotType, gotKey, err := ParseKnownHostsLine(FormatKnownHostsLine(patterns, key))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if gotPatterns != patterns || gotType != key.Type() {
		t.Fatalf("roundtrip lost data: patterns=%q type=%q", gotPatterns, gotType)
	}
	if string(gotKey.Marshal()) != string(key.Marshal()) {
		t.Fatal("roundtrip lost key bytes")
	}

	if _, _, _, err := ParseKnownHostsLine("[example.com]:2222 ssh-ed25519 !!notbase64!!"); err == nil {
		t.Fatal("malformed base64 must be rejected")
	}
	if _, _, _, err := ParseKnownHostsLine("[example.com]:2222"); err == nil {
		t.Fatal("a two-field line must be rejected")
	}
}
