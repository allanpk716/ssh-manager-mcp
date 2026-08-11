package store

import (
	"database/sql"
	"fmt"
	"time"

	"ssh-manager-mcp/internal/models"
)

// AddCacheToken creates a device-authorization code for offline-cache pulls, returning its id
// and the ONE-TIME plaintext token. Mirrors AddProject's token model but is owner-level (no
// profile binding) and never carried in a Snapshot. The plaintext is shown only here — store
// only the hash, exactly like project tokens.
func (s *Store) AddCacheToken(name string) (string, string, error) {
	token, err := GenerateToken()
	if err != nil {
		return "", "", err
	}
	salt := newSalt()
	hash := HashToken([]byte(token), salt)
	id := newID()
	ts := now()
	_, err = s.db.Exec(
		`INSERT INTO cache_tokens (id,name,token_hash,token_salt,token_prefix,status,created_at,updated_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		id, name, hash, salt, tokenPrefix(token), string(models.CacheTokenActive), ts, ts,
	)
	if err != nil {
		return "", "", err
	}
	return id, token, nil
}

// VerifyCacheToken returns the active cache token matching the plaintext, or (nil, nil) if none.
// Only status='active' admits — a revoked device code is rejected even with the correct secret
// (Lazy: takes effect on the next /snapshot fetch). Prefiltered by token_prefix so Argon2id
// (64 MiB) only runs on true candidates, mirroring VerifyToken.
func (s *Store) VerifyCacheToken(token string) (*models.CacheToken, error) {
	prefix := tokenPrefix(token)
	rows, err := s.db.Query(
		`SELECT id,name,token_hash,token_salt,token_prefix,status,last_pull_at FROM cache_tokens WHERE token_prefix=? AND status='active'`,
		prefix,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			ct       models.CacheToken
			hash     []byte
			salt     []byte
			status   string
			lastPull sql.NullInt64
		)
		if err := rows.Scan(&ct.ID, &ct.Name, &hash, &salt, &ct.TokenPrefix, &status, &lastPull); err != nil {
			return nil, err
		}
		if verifyTokenHash([]byte(token), salt, hash) {
			ct.Status = models.CacheTokenStatus(status)
			if lastPull.Valid {
				ct.LastPullAt = time.Unix(lastPull.Int64, 0)
			}
			return &ct, nil
		}
	}
	return nil, rows.Err()
}

// ListCacheTokens returns every device code (owner-facing fields only — never the hash).
func (s *Store) ListCacheTokens() ([]*models.CacheToken, error) {
	rows, err := s.db.Query(`SELECT id,name,token_prefix,status,last_pull_at,created_at,updated_at FROM cache_tokens ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*models.CacheToken
	for rows.Next() {
		var (
			ct       models.CacheToken
			status   string
			lastPull sql.NullInt64
			created  int64
			updated  int64
		)
		if err := rows.Scan(&ct.ID, &ct.Name, &ct.TokenPrefix, &status, &lastPull, &created, &updated); err != nil {
			return nil, err
		}
		ct.Status = models.CacheTokenStatus(status)
		if lastPull.Valid {
			ct.LastPullAt = time.Unix(lastPull.Int64, 0)
		}
		ct.CreatedAt = time.Unix(created, 0)
		ct.UpdatedAt = time.Unix(updated, 0)
		out = append(out, &ct)
	}
	return out, rows.Err()
}

// RevokeCacheToken permanently revokes a device code by name (Lazy: next /snapshot fetch rejected).
// Errors if the name is absent.
func (s *Store) RevokeCacheToken(name string) error {
	res, err := s.db.Exec(`UPDATE cache_tokens SET status=?, updated_at=? WHERE name=?`, string(models.CacheTokenRevoked), now(), name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("cache token %q not found", name)
	}
	return nil
}

// TouchCacheToken bumps last_pull_at for id (called by /snapshot on a successful fetch).
// Errors if the id is absent.
func (s *Store) TouchCacheToken(id string) error {
	res, err := s.db.Exec(`UPDATE cache_tokens SET last_pull_at=? WHERE id=?`, now(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("cache token id %q not found", id)
	}
	return nil
}
