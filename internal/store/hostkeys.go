package store

import "database/sql"

// GetHostKey returns the stored marshaled host key for host, or (nil, nil) if absent.
func (s *Store) GetHostKey(host string) ([]byte, error) {
	var blob []byte
	err := s.db.QueryRow(`SELECT key_blob FROM host_keys WHERE host=?`, host).Scan(&blob)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return blob, nil
}

// SaveHostKey records (trusts on first use) a marshaled host key for host.
func (s *Store) SaveHostKey(host string, marshaledKey []byte) error {
	_, err := s.db.Exec(
		`INSERT INTO host_keys (host, key_blob, created_at) VALUES (?,?,?)
		 ON CONFLICT(host) DO UPDATE SET key_blob=excluded.key_blob`,
		host, marshaledKey, now(),
	)
	return err
}
