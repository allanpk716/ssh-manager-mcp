package store

import (
	"database/sql"
	"fmt"
	"time"

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
	// v0.8.5: created_at/updated_at join the select (were never read — the
	// projects page rendered Go zero times). Same int64→time.Unix pattern as
	// ListCacheTokens.
	rows, err := s.db.Query(`SELECT id,name,token_prefix,profile_id,status,created_at,updated_at FROM projects ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*models.Project
	for rows.Next() {
		var (
			p       models.Project
			status  string
			created int64
			updated int64
		)
		if err := rows.Scan(&p.ID, &p.Name, &p.TokenPrefix, &p.ProfileID, &status, &created, &updated); err != nil {
			return nil, err
		}
		p.Status = models.ProjectStatus(status)
		p.CreatedAt, p.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
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

// GetProject resolves a project by id (returns nil, nil when absent). Not part
// of the agent surface. (Historically used by serve mode's resolveServer to map
// an authenticated TokenInfo.UserID back to its project + profile scope; that
// HTTP path was removed with the ②a MCP-over-HTTP surface — Plan 42 批1.)
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
// this on the next mcp spawn. Errors if the id is absent. v0.8.8: revoked is
// terminal — every exit from a revoked row is refused (the CLI status commands
// had no guard: `enable` resurrected a revoked token, and `disable` → `enable`
// was a two-step bypass). Revoke-on-revoked stays allowed (idempotent).
func (s *Store) SetProjectStatus(id string, status models.ProjectStatus) error {
	if s.readOnly {
		return ErrReadOnly
	}
	var cur string
	err := s.db.QueryRow(`SELECT status FROM projects WHERE id=?`, id).Scan(&cur)
	if err == sql.ErrNoRows {
		return fmt.Errorf("project id %q not found", id)
	}
	if err != nil {
		return err
	}
	if cur == string(models.ProjectRevoked) && status != models.ProjectRevoked {
		return fmt.Errorf("project 已吊销:吊销不可逆,无法 enable/disable;需要时新建 project(TUI a / CLI projects add)")
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
// VerifyToken no longer matches it. v0.8.9: refuses a revoked row — a rotation could not
// resurrect the token (status gate), but printing a fresh token + audit ok for a credential
// that is dead on arrival is a misleading success; same absorbing state as SetProjectStatus
// (v0.8.8).
func (s *Store) RotateProject(id string) (string, error) {
	if s.readOnly {
		return "", ErrReadOnly
	}
	var cur string
	err := s.db.QueryRow(`SELECT status FROM projects WHERE id=?`, id).Scan(&cur)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("project id %q not found", id)
	}
	if err != nil {
		return "", err
	}
	if cur == string(models.ProjectRevoked) {
		return "", fmt.Errorf("project 已吊销:吊销不可逆,无法 rotate;需要时新建 project(TUI a / CLI projects add)")
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

// DeleteProject hard-deletes a project ROW — allowed ONLY on revoked rows
// (owner's v0.8.7 ruling: revoke first, then delete; two deliberate steps).
// Deleting an active row would silently equal a revoke, so it is refused
// with a pointer to the revoke step. Nothing references project rows; the
// audit sidecar keeps its history (older audit lines may resolve the id to
// no name afterwards — accepted).
func (s *Store) DeleteProject(id string) error {
	if s.readOnly {
		return ErrReadOnly
	}
	var status string
	err := s.db.QueryRow(`SELECT status FROM projects WHERE id=?`, id).Scan(&status)
	if err == sql.ErrNoRows {
		return fmt.Errorf("project id %q not found", id)
	}
	if err != nil {
		return err
	}
	if status != string(models.ProjectRevoked) {
		return fmt.Errorf("project 仍为 %s:先吊销(TUI d / CLI projects revoke)再删除", status)
	}
	res, err := s.db.Exec(`DELETE FROM projects WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("project id %q not found", id)
	}
	return nil
}
