package store

import (
	"database/sql"
	"fmt"

	"ssh-manager-mcp/internal/models"
)

// AddProject creates a project bound to profileID, returning the project id and the ONE-TIME token plaintext.
func (s *Store) AddProject(name, profileID string) (string, string, error) {
	if s.readOnly {
		return "", "", ErrReadOnly
	}
	token, err := GenerateToken()
	if err != nil {
		return "", "", err
	}
	salt := newSalt()
	hash := HashToken([]byte(token), salt)
	id := newID()
	ts := now()
	_, err = s.db.Exec(
		`INSERT INTO projects (id,name,token_hash,token_salt,token_prefix,profile_id,status,created_at,updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		id, name, hash, salt, tokenPrefix(token), profileID, string(models.ProjectActive), ts, ts,
	)
	if err != nil {
		return "", "", err
	}
	return id, token, nil
}

// VerifyToken returns the project whose token matches AND is active, or (nil, nil) if none.
// Only status='active' admits — the Lazy gate: a disabled/revoked project is rejected EVEN
// with the correct secret (takes effect at the next mcp spawn). Prefiltering by token_prefix
// (first 8 chars) bounds work to the rare matching row, so Argon2id (64 MiB) only runs on
// true candidates.
func (s *Store) VerifyToken(token string) (*models.Project, error) {
	prefix := tokenPrefix(token)
	rows, err := s.db.Query(
		`SELECT id,name,token_hash,token_salt,token_prefix,profile_id,status FROM projects WHERE token_prefix=? AND status='active'`,
		prefix,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			p          models.Project
			hash, salt []byte
			status     string
		)
		if err := rows.Scan(&p.ID, &p.Name, &hash, &salt, &p.TokenPrefix, &p.ProfileID, &status); err != nil {
			return nil, err
		}
		if verifyTokenHash([]byte(token), salt, hash) {
			p.Status = models.ProjectStatus(status)
			return &p, nil
		}
	}
	return nil, rows.Err()
}

func (s *Store) ListProjects() ([]*models.Project, error) {
	rows, err := s.db.Query(`SELECT id,name,token_prefix,profile_id,status FROM projects ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*models.Project
	for rows.Next() {
		var (
			p      models.Project
			status string
		)
		if err := rows.Scan(&p.ID, &p.Name, &p.TokenPrefix, &p.ProfileID, &status); err != nil {
			return nil, err
		}
		p.Status = models.ProjectStatus(status)
		out = append(out, &p)
	}
	return out, rows.Err()
}

// GetProjectByName resolves a project by name (returns nil, nil when absent).
func (s *Store) GetProjectByName(name string) (*models.Project, error) {
	var (
		p      models.Project
		status string
	)
	err := s.db.QueryRow(
		`SELECT id,name,token_prefix,profile_id,status FROM projects WHERE name=?`, name,
	).Scan(&p.ID, &p.Name, &p.TokenPrefix, &p.ProfileID, &status)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.Status = models.ProjectStatus(status)
	return &p, nil
}

// GetProject resolves a project by id (returns nil, nil when absent). Used by
// serve mode's resolveServer to map an authenticated TokenInfo.UserID back to
// its project + profile scope. Not part of the agent surface.
func (s *Store) GetProject(id string) (*models.Project, error) {
	var (
		p      models.Project
		status string
	)
	err := s.db.QueryRow(
		`SELECT id,name,token_prefix,profile_id,status FROM projects WHERE id=?`, id,
	).Scan(&p.ID, &p.Name, &p.TokenPrefix, &p.ProfileID, &status)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.Status = models.ProjectStatus(status)
	return &p, nil
}

// SetProjectStatus sets the lifecycle status (active/disabled/revoked). VerifyToken reads
// this on the next mcp spawn. Errors if the id is absent.
func (s *Store) SetProjectStatus(id string, status models.ProjectStatus) error {
	if s.readOnly {
		return ErrReadOnly
	}
	res, err := s.db.Exec(`UPDATE projects SET status=?, updated_at=? WHERE id=?`, string(status), now(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("project id %q not found", id)
	}
	return nil
}

// RotateProject replaces the token IN PLACE (same id, profile_id, status) and returns the new
// plaintext token. The old token stops verifying immediately — its hash is overwritten, so
// VerifyToken no longer matches it.
func (s *Store) RotateProject(id string) (string, error) {
	if s.readOnly {
		return "", ErrReadOnly
	}
	token, err := GenerateToken()
	if err != nil {
		return "", err
	}
	salt := newSalt()
	hash := HashToken([]byte(token), salt)
	prefix := tokenPrefix(token)
	res, err := s.db.Exec(
		`UPDATE projects SET token_hash=?, token_salt=?, token_prefix=?, updated_at=? WHERE id=?`,
		hash, salt, prefix, now(), id,
	)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", fmt.Errorf("project id %q not found", id)
	}
	return token, nil
}

func tokenPrefix(token string) string {
	if len(token) >= 8 {
		return token[:8]
	}
	return token
}
