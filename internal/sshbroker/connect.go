package sshbroker

import (
	"fmt"
	"os"
	"strings"
)

// allowedHostKeyAlgos is the explicit allowlist for the
// SSHMGR_SSH_HOST_KEY_ALGORITHMS knob (fail-closed on typos: an unknown value
// aborts the connection before any dial rather than silently falling back).
var allowedHostKeyAlgos = map[string]bool{
	"ssh-ed25519": true, "ecdsa-sha2-nistp256": true, "ecdsa-sha2-nistp384": true,
	"ecdsa-sha2-nistp521": true, "rsa-sha2-256": true, "rsa-sha2-512": true, "ssh-rsa": true,
}

// hostKeyAlgos reads SSHMGR_SSH_HOST_KEY_ALGORITHMS (comma-separated host key
// algorithm preference, e.g. "ssh-rsa,rsa-sha2-512" for old servers that need
// SHA-1 RSA). Empty/whitespace → nil (use the library default set).
func hostKeyAlgos() []string {
	v := strings.TrimSpace(os.Getenv("SSHMGR_SSH_HOST_KEY_ALGORITHMS"))
	if v == "" {
		return nil
	}
	return strings.Split(v, ",")
}

// hostKeyAlgosChecked validates the knob against allowedHostKeyAlgos and
// returns the trimmed, validated list. Any unknown element fails with an error
// naming the env var and the allowed list — callers abort BEFORE dialing
// (fail-closed), so a typo can never silently downgrade or hang a connection.
func hostKeyAlgosChecked() ([]string, error) {
	raw := hostKeyAlgos()
	if raw == nil {
		return nil, nil
	}
	l := make([]string, len(raw))
	for i, a := range raw {
		l[i] = strings.TrimSpace(a)
		if !allowedHostKeyAlgos[l[i]] {
			return nil, fmt.Errorf("SSHMGR_SSH_HOST_KEY_ALGORITHMS: unknown algorithm %q (allowed: ed25519, ecdsa-sha2-nistp256/384/521, rsa-sha2-256/512, ssh-rsa)", a)
		}
	}
	return l, nil
}
