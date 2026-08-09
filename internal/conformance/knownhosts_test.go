package conformance

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// TestKnownHostsRoundtrip proves we parse and re-render an OpenSSH known_hosts
// line without loss, and that the real ssh-keygen can FIND an entry we wrote
// (true format compatibility, §13.3). The broker never touches ~/.ssh at
// runtime — this is a parse/serialize compat proof using a throwaway file.
func TestKnownHostsRoundtrip(t *testing.T) {
	requireConformance(t)

	// Generate a host key via ssh-keygen; reuse the T2 helper (same shape —
	// "host" vs "client" key is just usage, the format is identical).
	_, pub := generateKey(t, "ed25519", "")
	patterns := "[example.com]:2222"
	_, key := parsePubLine(t, pub)

	// Roundtrip: format → parse must preserve patterns, type, and key bytes.
	formatted := FormatKnownHostsLine(patterns, key)
	gotPatterns, gotType, gotKey, err := ParseKnownHostsLine(formatted)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if gotPatterns != patterns || gotType != key.Type() {
		t.Fatalf("roundtrip lost data: patterns=%q type=%q", gotPatterns, gotType)
	}
	if string(gotKey.Marshal()) != string(key.Marshal()) {
		t.Fatal("roundtrip lost key bytes")
	}

	// Cross-check: write our line into a throwaway known_hosts and have the
	// real ssh-keygen -F find it. Proves the rendered line is genuinely
	// OpenSSH-readable, not just self-consistent.
	dir := t.TempDir()
	kh := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(kh, []byte(formatted+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	found := sshKeygenFind(t, kh, "example.com", 2222)
	// "found:" is the marker ssh-keygen emits on a real match
	// ("# Host [example.com]:2222 found: line 1"); requiring it prevents a
	// false pass from the host name merely being echoed back.
	if !strings.Contains(found, "found:") || !strings.Contains(found, "example.com") {
		t.Fatalf("ssh-keygen -F did not find our entry:\n%s", found)
	}
}

// parsePubLine parses an authorized_keys/pub line "type b64 [comment]" into a
// public key. Uses ssh.ParseAuthorizedKey — the text parser for this format.
// (ssh.ParsePublicKey expects length-prefixed wire format and short-reads on
// the authorized-keys text form — established in T2.)
func parsePubLine(t *testing.T, line string) (string, ssh.PublicKey) {
	t.Helper()
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		t.Fatalf("parse pub: %v", err)
	}
	return key.Type(), key
}

// sshKeygenFind runs `ssh-keygen -F <search> -f file` and returns its stdout.
// The search token for a non-default port is "[host]:port" — this build's
// ssh-keygen has no -p option in the find form (it errors with "Too many
// arguments"), and the bracketed token is the OpenSSH convention for non-22
// ports. ssh-keygen -F exits nonzero with empty stdout when the host is absent;
// the caller decides success from the output text, not the exit code.
func sshKeygenFind(t *testing.T, file, host string, port int) string {
	t.Helper()
	search := host
	if port != 0 && port != 22 {
		search = "[" + host + "]:" + strconv.Itoa(port)
	}
	out, _ := exec.Command("ssh-keygen", "-F", search, "-f", file).Output()
	return string(out)
}
