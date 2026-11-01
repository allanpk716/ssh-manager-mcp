package sshbroker

import (
	"errors"
	"fmt"
	"net"

	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/models"
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

// HostKeyStore is the subset of *store.Store that HostKeyTOFU needs (also faked
// in tests). 按服务器条目读写（票 #58）：锚跟随条目而不是地址。
type HostKeyStore interface {
	GetEntryPin(srv *models.Server) (*store.Pin, error)
	SaveEntryPin(srv *models.Server, marshaledKey []byte) error
}

// HostKeyTOFU returns a trust-on-first-use host-key callback bound to st and one
// server entry. First connection: records the key on the entry. Subsequent:
// must satisfy the stored anchor under the system-wide dual-mode semantics
// (store.Pin.Matches — Plan 48 §5: blob anchors compare bytewise, fingerprint
// anchors compare the presented key's SHA256 fingerprint), else rejected with
// both fingerprints in the text. 条目改地址时锚不动，新地址上是另一台机器就
// 会在这里报密钥不一致。
func HostKeyTOFU(st HostKeyStore, srv *models.Server) (ssh.HostKeyCallback, error) {
	return func(_ string, _ net.Addr, remote ssh.PublicKey) error {
		marshaled := remote.Marshal()
		pin, err := st.GetEntryPin(srv)
		if err != nil {
			return err
		}
		if pin == nil {
			// 虚拟局域网首次锚定规则（票 #58 只留接口，地址数据源恒为空，
			// 由网络拓扑票接入后生效）：条目有虚拟局域网地址时，首次信任
			// 只接受虚拟局域网握手地址。
			if vlan := store.EntryVirtualLanAddresses(srv); len(vlan) > 0 && !containsString(vlan, store.EntryFallbackAddr(srv)) {
				return fmt.Errorf("first trust for server %q requires a virtual LAN address, got %s", srv.Name, store.EntryFallbackAddr(srv))
			}
			if err := st.SaveEntryPin(srv, marshaled); err != nil {
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

// containsString 判断列表是否包含指定字符串。
func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
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
