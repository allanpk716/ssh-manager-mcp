package store

import (
	"ssh-manager-mcp/internal/models"
)

// AddProject creates a project bound to profileID, returning the project id and the ONE-TIME token plaintext.
func (s *Store) AddProject(name, profileID string) (string, string, error) {
	token, err := GenerateToken()
	if err != nil {
		return "", "", err
	}
	salt := newSalt()
	hash := HashToken([]byte(token), salt)
	id := newID()
	ts := now()
	_, err = s.db.Exec(
		`INSERT INTO projects (id,name,token_hash,token_salt,token_prefix,profile_id,created_at,updated_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		id, name, hash, salt, tokenPrefix(token), profileID, ts, ts,
	)
	if err != nil {
		return "", "", err
	}
	return id, token, nil
}

// VerifyToken returns the project whose token matches, or (nil, nil) if none.
func (s *Store) VerifyToken(token string) (*models.Project, error) {
	rows, err := s.db.Query(`SELECT id,name,token_hash,token_salt,token_prefix,profile_id FROM projects`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			p          models.Project
			hash, salt []byte
		)
		if err := rows.Scan(&p.ID, &p.Name, &hash, &salt, &p.TokenPrefix, &p.ProfileID); err != nil {
			return nil, err
		}
		if verifyTokenHash([]byte(token), salt, hash) {
			return &p, nil
		}
	}
	return nil, rows.Err()
}

func (s *Store) ListProjects() ([]*models.Project, error) {
	rows, err := s.db.Query(`SELECT id,name,token_prefix,profile_id FROM projects ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*models.Project
	for rows.Next() {
		var p models.Project
		if err := rows.Scan(&p.ID, &p.Name, &p.TokenPrefix, &p.ProfileID); err != nil {
			return nil, err
		}
		out = append(out, &p)
	}
	return out, rows.Err()
}

func tokenPrefix(token string) string {
	if len(token) >= 8 {
		return token[:8]
	}
	return token
}
