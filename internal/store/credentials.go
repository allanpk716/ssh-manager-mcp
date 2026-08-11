package store

import (
	"database/sql"

	"ssh-manager-mcp/internal/models"
)

// SetCredential encrypts and stores a credential, returning its id.
func (s *Store) SetCredential(c *models.Credential) (string, error) {
	if s.readOnly {
		return "", ErrReadOnly
	}
	secretBlob, err := seal(s.masterKey, c.Secret)
	if err != nil {
		return "", err
	}
	var passBlob []byte
	if len(c.Passphrase) > 0 {
		passBlob, err = seal(s.masterKey, c.Passphrase)
		if err != nil {
			return "", err
		}
	}
	id := newID()
	ts := now()
	_, err = s.db.Exec(
		`INSERT INTO credentials (id, type, secret_blob, passphrase_blob, created_at, updated_at) VALUES (?,?,?,?,?,?)`,
		id, string(c.Type), secretBlob, passBlob, ts, ts,
	)
	if err != nil {
		return "", err
	}
	return id, nil
}

// GetCredential decrypts and returns a credential by id.
func (s *Store) GetCredential(id string) (*models.Credential, error) {
	var (
		typ       string
		secretRaw []byte
		passRaw   []byte
	)
	err := s.db.QueryRow(
		`SELECT type, secret_blob, passphrase_blob FROM credentials WHERE id = ?`, id,
	).Scan(&typ, &secretRaw, &passRaw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	secret, err := open(s.masterKey, secretRaw)
	if err != nil {
		return nil, err
	}
	c := &models.Credential{ID: id, Type: models.CredentialType(typ), Secret: secret}
	if passRaw != nil {
		pass, err := open(s.masterKey, passRaw)
		if err != nil {
			return nil, err
		}
		c.Passphrase = pass
	}
	return c, nil
}
