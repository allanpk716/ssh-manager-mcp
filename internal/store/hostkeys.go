package store

import (
	"database/sql"
	"fmt"
)

// hostKeyID is the storage key for a host's pinned public key. Always host:port
// (unconditional, even for :22) so same-host-different-port servers never collide.
// OpenSSH known_hosts uses bare "host" for :22 and "[host]:port" otherwise; that
// format-specific rendering lives in the known_hosts serializer, not here.
func hostKeyID(host string, port int) string {
	return fmt.Sprintf("%s:%d", host, port)
}

// GetHostKey returns the stored marshaled host key for host:port, or (nil, nil) if absent.
func (s *Store) GetHostKey(host string, port int) ([]byte, error) {
	var blob []byte
	err := s.db.QueryRow(`SELECT key_blob FROM host_keys WHERE host_port=?`, hostKeyID(host, port)).Scan(&blob)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return blob, nil
}

// SaveHostKey records (trusts on first use) a marshaled host key for host:port.
func (s *Store) SaveHostKey(host string, port int, marshaledKey []byte) error {
	if s.readOnly {
		return ErrReadOnly
	}
	_, err := s.db.Exec(
		`INSERT INTO host_keys (host_port, key_blob, created_at) VALUES (?,?,?)
		 ON CONFLICT(host_port) DO UPDATE SET key_blob=excluded.key_blob`,
		hostKeyID(host, port), marshaledKey, now(),
	)
	return err
}
