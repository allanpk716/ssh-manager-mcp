package store

import (
	"database/sql"
	"fmt"
	"strings"

	"ssh-manager-mcp/internal/models"
)

func (s *Store) AddProfile(name string) (string, error) {
	if s.readOnly {
		return "", ErrReadOnly
	}
	id := newID()
	ts := now()
	_, err := s.db.Exec(
		`INSERT INTO profiles (id,name,created_at,updated_at) VALUES (?,?,?,?)`,
		id, name, ts, ts,
	)
	if err != nil {
		// Localize the name-collision error — the raw driver text leaks SQLite
		// jargon into TUI/CLI surfaces (same wrap as AddServer).
		if strings.Contains(err.Error(), "UNIQUE constraint failed: profiles.name") {
			return "", fmt.Errorf("profile name %q already exists", name)
		}
		return "", err
	}
	return id, nil
}

func (s *Store) GetProfile(id string) (*models.Profile, error) {
	var p models.Profile
	err := s.db.QueryRow(`SELECT id,name FROM profiles WHERE id=?`, id).Scan(&p.ID, &p.Name)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ids, err := s.ServersForProfile(id)
	if err != nil {
		return nil, err
	}
	p.ServerIDs = ids
	return &p, nil
}

func (s *Store) ListProfiles() ([]*models.Profile, error) {
	rows, err := s.db.Query(`SELECT id,name FROM profiles ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*models.Profile
	for rows.Next() {
		var p models.Profile
		if err := rows.Scan(&p.ID, &p.Name); err != nil {
			return nil, err
		}
		out = append(out, &p)
	}
	return out, rows.Err()
}

// GrantServers adds serverIDs to the profile inside one transaction.
// Duplicate (profile_id, server_id) pairs are ignored (INSERT OR IGNORE).
// Unknown server ids are rejected by an in-transaction precheck BEFORE any
// INSERT runs — fail-fast with the offending id named, so nothing is granted
// and no half-granted profile is left for a caller retry to orphan. The FK
// constraint stays as the fail-closed backstop behind the precheck.
func (s *Store) GrantServers(profileID string, serverIDs []string) error {
	if s.readOnly {
		return ErrReadOnly
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, sid := range serverIDs {
		var one int
		if err := tx.QueryRow(`SELECT 1 FROM servers WHERE id=?`, sid).Scan(&one); err == sql.ErrNoRows {
			return fmt.Errorf("server %s not found (grant aborted, nothing changed)", sid)
		} else if err != nil {
			return err
		}
	}
	for _, sid := range serverIDs {
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO profile_servers (profile_id, server_id) VALUES (?,?)`,
			profileID, sid,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ServersForProfile(profileID string) ([]string, error) {
	rows, err := s.db.Query(`SELECT server_id FROM profile_servers WHERE profile_id=?`, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
