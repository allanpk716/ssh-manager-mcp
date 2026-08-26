package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"ssh-manager-mcp/internal/models"
)

// AddCacheToken creates a device-authorization code for offline-cache pulls, bound
// to profileID (Plan 39: /snapshot serves only that profile's authorized servers
// + their credentials), returning its id and the ONE-TIME plaintext token.
// Mirrors AddProject's token model but is disjoint from project tokens and never
// carried in a Snapshot. The plaintext is shown only here — store only the hash,
// exactly like project tokens. profileID must reference an existing profile (the
// store API cannot mint unbound codes; unbound exists only via legacy-DB migration).
//
// Same-name REVOKED rows are reclaimed first (in the same tx) so a device name can be re-issued
// after a revoke: Lazy soft-delete otherwise keeps the row and UNIQUE(name) blocks the INSERT.
// Active same-name rows are deliberately left untouched so a duplicate-active INSERT still fails
// UNIQUE (guards against accidentally issuing two active codes for one device).
func (s *Store) AddCacheToken(name, profileID string) (string, string, error) {
	if s.readOnly {
		return "", "", ErrReadOnly
	}
	if profileID == "" {
		return "", "", errors.New("device code requires a profile binding (profileID is empty)")
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM profiles WHERE id=?`, profileID).Scan(&n); err != nil {
		return "", "", err
	}
	if n == 0 {
		return "", "", fmt.Errorf("profile %q not found", profileID)
	}
	token, err := GenerateToken()
	if err != nil {
		return "", "", err
	}
	salt := newSalt()
	hash := HashToken([]byte(token), salt)
	id := newID()
	ts := now()
	tx, err := s.db.Begin()
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback() // no-op after Commit
	// Reclaim any same-name revoked rows so the name is free for re-issue. Active rows are
	// left in place — a duplicate-active INSERT must still hit UNIQUE.
	if _, err := tx.Exec(
		`DELETE FROM cache_tokens WHERE name=? AND status=?`,
		name, string(models.CacheTokenRevoked),
	); err != nil {
		return "", "", err
	}
	if _, err := tx.Exec(
		`INSERT INTO cache_tokens (id,name,token_hash,token_salt,token_prefix,status,profile_id,created_at,updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		id, name, hash, salt, tokenPrefix(token), string(models.CacheTokenActive), profileID, ts, ts,
	); err != nil {
		return "", "", err
	}
	if err := tx.Commit(); err != nil {
		return "", "", err
	}
	return id, token, nil
}

// BindCacheToken re-binds an ACTIVE device code to profileID (Plan 39's legacy
// repair path: pre-Plan-39 rows migrate as unbound/NULL and their pulls are
// refused with 403 until bound). Unknown device name or unknown profile errors.
func (s *Store) BindCacheToken(name, profileID string) error {
	if s.readOnly {
		return ErrReadOnly
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM profiles WHERE id=?`, profileID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("profile %q not found", profileID)
	}
	res, err := s.db.Exec(
		`UPDATE cache_tokens SET profile_id=?, updated_at=? WHERE name=? AND status=?`,
		profileID, now(), name, string(models.CacheTokenActive),
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("active cache token %q not found", name)
	}
	return nil
}

// GetCacheToken resolves a device code row by id (owner-facing fields only — never
// the hash). Mirrors GetProject: the serve layer re-queries the binding after auth
// hands it only the token id (TokenInfo.UserID). Returns (nil, nil) when absent.
func (s *Store) GetCacheToken(id string) (*models.CacheToken, error) {
	var (
		ct       models.CacheToken
		status   string
		lastPull sql.NullInt64
		created  int64
		updated  int64
	)
	err := s.db.QueryRow(
		`SELECT id,name,token_prefix,status,COALESCE(profile_id,''),last_pull_at,created_at,updated_at
		 FROM cache_tokens WHERE id=?`, id,
	).Scan(&ct.ID, &ct.Name, &ct.TokenPrefix, &status, &ct.ProfileID, &lastPull, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ct.Status = models.CacheTokenStatus(status)
	if lastPull.Valid {
		ct.LastPullAt = time.Unix(lastPull.Int64, 0)
	}
	ct.CreatedAt = time.Unix(created, 0)
	ct.UpdatedAt = time.Unix(updated, 0)
	return &ct, nil
}

// VerifyCacheToken returns the active cache token matching the plaintext, or (nil, nil) if none.
// Only status='active' admits — a revoked device code is rejected even with the correct secret
// (Lazy: takes effect on the next /snapshot fetch). Prefiltered by token_prefix so Argon2id
// (64 MiB) only runs on true candidates, mirroring VerifyToken.
func (s *Store) VerifyCacheToken(token string) (*models.CacheToken, error) {
	prefix := tokenPrefix(token)
	rows, err := s.db.Query(
		`SELECT id,name,token_hash,token_salt,token_prefix,status,COALESCE(profile_id,''),last_pull_at FROM cache_tokens WHERE token_prefix=? AND status='active'`,
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
		if err := rows.Scan(&ct.ID, &ct.Name, &hash, &salt, &ct.TokenPrefix, &status, &ct.ProfileID, &lastPull); err != nil {
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
	rows, err := s.db.Query(`SELECT id,name,token_prefix,status,COALESCE(profile_id,''),last_pull_at,created_at,updated_at FROM cache_tokens ORDER BY name`)
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
		if err := rows.Scan(&ct.ID, &ct.Name, &ct.TokenPrefix, &status, &ct.ProfileID, &lastPull, &created, &updated); err != nil {
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
	if s.readOnly {
		return ErrReadOnly
	}
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
	if s.readOnly {
		return ErrReadOnly
	}
	res, err := s.db.Exec(`UPDATE cache_tokens SET last_pull_at=? WHERE id=?`, now(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("cache token id %q not found", id)
	}
	return nil
}

// RevokedCacheTokenNameByPrefix reports the name of the most-recently-updated
// REVOKED cache token whose token_prefix matches (Plan 34 rev4 §1: the /snapshot
// 401 reason is observability-only — prefix collisions can mislabel unknown as
// revoked, which is accepted; the client never branches on the reason).
func (s *Store) RevokedCacheTokenNameByPrefix(prefix string) (string, bool, error) {
	var name string
	err := s.db.QueryRow(
		`SELECT name FROM cache_tokens WHERE token_prefix=? AND status=? ORDER BY updated_at DESC LIMIT 1`,
		prefix, string(models.CacheTokenRevoked),
	).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return name, true, nil
}
