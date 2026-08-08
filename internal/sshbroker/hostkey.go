package sshbroker

import (
	"bytes"
	"errors"
	"fmt"
	"net"

	"golang.org/x/crypto/ssh"
)

// ErrHostKeyMismatch is returned by the TOFU callback when a server's host key
// differs from the previously-recorded one (possible MITM). Callers (e.g. the MCP
// server) can errors.Is this to surface a clear warning to the client.
var ErrHostKeyMismatch = errors.New("host key mismatch: possible MITM, connection rejected")

// HostKeyStore is the subset of *store.Store that HostKeyTOFU needs (also faked in tests).
type HostKeyStore interface {
	GetHostKey(host string) ([]byte, error)
	SaveHostKey(host string, marshaledKey []byte) error
}

// HostKeyTOFU returns a trust-on-first-use host-key callback bound to st.
// First connection to host: records its key. Subsequent: must match, else rejected.
func HostKeyTOFU(st HostKeyStore, host string) (ssh.HostKeyCallback, error) {
	return func(_ string, _ net.Addr, remote ssh.PublicKey) error {
		marshaled := remote.Marshal()
		stored, err := st.GetHostKey(host)
		if err != nil {
			return err
		}
		if stored == nil {
			if err := st.SaveHostKey(host, marshaled); err != nil {
				return fmt.Errorf("save host key: %w", err)
			}
			return nil // trust on first use
		}
		if !bytes.Equal(marshaled, stored) {
			return ErrHostKeyMismatch
		}
		return nil
	}, nil
}
