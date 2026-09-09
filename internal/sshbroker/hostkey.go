package sshbroker

import (
	"errors"
	"fmt"
	"net"

	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/store"
)

// ErrHostKeyMismatch is returned by the TOFU callback when a server's host key
// differs from the previously-recorded one (possible MITM). Callers (e.g. the MCP
// server) can errors.Is this to surface a clear warning to the client. The
// wrapped text (Plan 48 rider 2) additionally names BOTH fingerprints —
// presented vs pinned — and nothing else about the keys.
var ErrHostKeyMismatch = errors.New("host key mismatch: possible MITM, connection rejected")

// PresentedHostKeyError marks a SaveHostKey-path error whose Error() text is
// ALREADY the final user-facing presentation — the Plan 48 §2.1/§4 pin-forward
// branch texts, asserted byte-for-byte by the clientops and cache-wiring tests.
// HostKeyTOFU must not prepend its "save host key: " wrapper to these: any
// added prefix breaks the verbatim contract. The forwarding HostKeyStore
// wrapper (mcpserver) raises its failures through this type; errors.Is / As
// keep working via Unwrap (e.g. ErrPinForwardEqual, store.ErrReadOnly).
type PresentedHostKeyError struct {
	Err error
}

func (e *PresentedHostKeyError) Error() string { return e.Err.Error() }
func (e *PresentedHostKeyError) Unwrap() error { return e.Err }

// HostKeyStore is the subset of *store.Store that HostKeyTOFU needs (also faked in tests).
type HostKeyStore interface {
	GetHostKey(host string, port int) (*store.Pin, error)
	SaveHostKey(host string, port int, marshaledKey []byte) error
}

// HostKeyTOFU returns a trust-on-first-use host-key callback bound to st and host:port.
// First connection: records the key. Subsequent: must satisfy the stored anchor
// under the system-wide dual-mode semantics (store.Pin.Matches — Plan 48 §5:
// blob anchors compare bytewise, fingerprint anchors compare the presented
// key's SHA256 fingerprint), else rejected with both fingerprints in the text.
func HostKeyTOFU(st HostKeyStore, host string, port int) (ssh.HostKeyCallback, error) {
	return func(_ string, _ net.Addr, remote ssh.PublicKey) error {
		marshaled := remote.Marshal()
		pin, err := st.GetHostKey(host, port)
		if err != nil {
			return err
		}
		if pin == nil {
			if err := st.SaveHostKey(host, port, marshaled); err != nil {
				// Already-presented branch text (Plan 48 §2.1/§4) — return it
				// verbatim; the "save host key: " wrapper is for raw store
				// failures only (read-only ErrReadOnly keeps its asserted form).
				var pe *PresentedHostKeyError
				if errors.As(err, &pe) {
					return err
				}
				return fmt.Errorf("save host key: %w", err)
			}
			return nil // trust on first use
		}
		if !pin.Matches(marshaled) {
			return fmt.Errorf("%w: presented %s != pinned %s",
				ErrHostKeyMismatch, ssh.FingerprintSHA256(remote), pinnedFingerprint(pin))
		}
		return nil
	}, nil
}

// pinnedFingerprint renders the anchor's fingerprint for the mismatch text —
// the ONLY content rider 2 lets the error add beyond the sentinel. A
// fingerprint anchor IS the string; a blob anchor is parsed to compute its
// SHA256 fingerprint. A blob that no longer parses (hand-edited DB — no
// writer produces one) degrades to a non-material placeholder.
func pinnedFingerprint(p *store.Pin) string {
	if p.Format == store.PinFormatFingerprint {
		return string(p.Blob)
	}
	if pub, err := ssh.ParsePublicKey(p.Blob); err == nil {
		return ssh.FingerprintSHA256(pub)
	}
	return "unparseable"
}
