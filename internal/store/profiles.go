package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

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
	var (
		p       models.Profile
		created int64
		updated int64
	)
	// v0.8.5: created_at/updated_at WERE never selected — every profile
	// rendered 0001-01-01 (Go zero time) in the TUI. Unix-epoch int64 scan,
	// the same pattern ListCacheTokens uses.
	err := s.db.QueryRow(
		`SELECT id,name,created_at,updated_at FROM profiles WHERE id=?`, id,
	).Scan(&p.ID, &p.Name, &created, &updated)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.CreatedAt, p.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
	ids, err := s.ServersForProfile(id)
	if err != nil {
		return nil, err
	}
	p.ServerIDs = ids
	return &p, nil
}

func (s *Store) ListProfiles() ([]*models.Profile, error) {
	rows, err := s.db.Query(`SELECT id,name,created_at,updated_at FROM profiles ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*models.Profile
	for rows.Next() {
		var (
			p       models.Profile
			created int64
			updated int64
		)
		if err := rows.Scan(&p.ID, &p.Name, &created, &updated); err != nil {
			return nil, err
		}
		p.CreatedAt, p.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
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
// v0.8.6: an ACTUAL insert (not an ignored duplicate) bumps
// profiles.updated_at in the same tx — the grant set IS profile state, and
// the TUI's 更新 column must reflect grant changes.
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
	added := 0
	for _, sid := range serverIDs {
		res, err := tx.Exec(
			`INSERT OR IGNORE INTO profile_servers (profile_id, server_id) VALUES (?,?)`,
			profileID, sid,
		)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added++
		}
	}
	if added > 0 {
		if _, err := tx.Exec(`UPDATE profiles SET updated_at=? WHERE id=?`, now(), profileID); err != nil {
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

// SyncServers REPLACES the profile's grant set with ids in one transaction:
// unchecked rows (present now, absent from ids) are removed, missing ones
// inserted — the owner's v0.8.4 ruling on grant semantics (取消勾选=移除).
// Unknown server ids fail fast exactly like GrantServers (nothing changes);
// an empty ids slice clears the grant set (a deliberate revoke-all).
// Returns (added, removed).
func (s *Store) SyncServers(profileID string, ids []string) (int, int, error) {
	if s.readOnly {
		return 0, 0, ErrReadOnly
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	var one int
	for _, sid := range ids {
		if err := tx.QueryRow(`SELECT 1 FROM servers WHERE id=?`, sid).Scan(&one); err == sql.ErrNoRows {
			return 0, 0, fmt.Errorf("server %s not found (sync aborted, nothing changed)", sid)
		} else if err != nil {
			return 0, 0, err
		}
	}
	added := 0
	for _, sid := range ids {
		res, err := tx.Exec(
			`INSERT OR IGNORE INTO profile_servers (profile_id, server_id) VALUES (?,?)`,
			profileID, sid,
		)
		if err != nil {
			return 0, 0, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added++
		}
	}
	// Go-side diff (no JSON1 dependency): delete the current rows absent
	// from ids — the removal half, the reason SyncServers exists.
	want := make(map[string]bool, len(ids))
	for _, sid := range ids {
		want[sid] = true
	}
	rows, err := tx.Query(`SELECT server_id FROM profile_servers WHERE profile_id=?`, profileID)
	if err != nil {
		return 0, 0, err
	}
	var stale []string
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			rows.Close()
			return 0, 0, err
		}
		if !want[sid] {
			stale = append(stale, sid)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, 0, err
	}
	rows.Close()
	removed := 0
	for _, sid := range stale {
		if _, err := tx.Exec(`DELETE FROM profile_servers WHERE profile_id=? AND server_id=?`, profileID, sid); err != nil {
			return 0, 0, err
		}
		removed++
	}
	// v0.8.6: the grant set IS profile state — a real change (either
	// direction) bumps updated_at in the same tx; a no-op sync keeps the old
	// timestamp (nothing changed, nothing to advertise).
	if added+removed > 0 {
		if _, err := tx.Exec(`UPDATE profiles SET updated_at=? WHERE id=?`, now(), profileID); err != nil {
			return 0, 0, err
		}
	}
	return added, removed, tx.Commit()
}

// DeleteProfile removes the profile and its grant rows in one transaction.
// It REFUSES while any project still references the profile (projects named
// in the error): silently unbinding would leave those projects' agents with
// no visible servers. Delete or re-bind the projects first. It likewise
// REFUSES while any ACTIVE device code is bound to the profile (Plan 39):
// a bound code's /snapshot authorization set IS this profile — deleting it
// underneath would strand the device. Bindings on revoked codes are inert
// and are simply cleared (SET NULL) inside the same tx.
func (s *Store) DeleteProfile(profileID string) error {
	if s.readOnly {
		return ErrReadOnly
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT name FROM projects WHERE profile_id=?`, profileID)
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(names) > 0 {
		return fmt.Errorf("profile 仍被 %d 个项目引用(%s):先删除或换绑这些项目,再删除 profile",
			len(names), strings.Join(names, ", "))
	}
	// Plan 39: same refusal for ACTIVE device bindings, by device name.
	rows, err = tx.Query(`SELECT name FROM cache_tokens WHERE profile_id=? AND status='active'`, profileID)
	if err != nil {
		return err
	}
	var devices []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		devices = append(devices, n)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(devices) > 0 {
		return fmt.Errorf("profile 仍被 %d 个设备码绑定(%s):先吊销或换绑(cache-tokens bind)这些设备码,再删除 profile",
			len(devices), strings.Join(devices, ", "))
	}
	// Inert bindings (revoked codes) would trip the FK on DELETE; clear them.
	if _, err := tx.Exec(`UPDATE cache_tokens SET profile_id=NULL WHERE profile_id=?`, profileID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM profile_servers WHERE profile_id=?`, profileID); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM profiles WHERE id=?`, profileID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("profile %s not found", profileID)
	}
	return tx.Commit()
}
