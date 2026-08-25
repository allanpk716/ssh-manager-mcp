package store

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
)

// TunnelRegistryRow mirrors a live broker-held tunnel (spec §6). last_renewed
// is a LEASE HEARTBEAT written every control tick by the owning manager — it
// is NOT traffic time (traffic time lives only in the manager's memory, spec
// §3). "row present ⇔ tunnel killable" is the state machine's foundation
// (fail-the-Open + fail-the-renewal keep it true).
type TunnelRegistryRow struct {
	TunnelID    string
	ProjectID   string
	ServerID    string
	Remote      string
	LocalAddr   string
	ListenHost  string // canonical form (spec §2)
	OpenedAt    int64
	LastRenewed int64
}

// InsertTunnelRegistry registers a just-opened tunnel. Called by the owning
// TunnelManager (fail-the-Open: an insert failure on a writable store closes
// the tunnel — spec §6).
func (s *Store) InsertTunnelRegistry(row TunnelRegistryRow) error {
	if s.readOnly {
		return ErrReadOnly
	}
	_, err := s.db.Exec(
		`INSERT INTO tunnel_registry(tunnel_id, project_id, server_id, remote, local_addr, listen_host, opened_at, last_renewed)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		row.TunnelID, row.ProjectID, row.ServerID, row.Remote, row.LocalAddr, row.ListenHost, row.OpenedAt, row.LastRenewed,
	)
	return err
}

// DeleteTunnelRegistry removes mirror rows (tunnel torn down). No-op on
// unknown ids.
func (s *Store) DeleteTunnelRegistry(ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if s.readOnly {
		return ErrReadOnly
	}
	// MaxOpenConns(1) + tiny id sets: one statement per call is fine.
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	q := `DELETE FROM tunnel_registry WHERE tunnel_id IN (` + placeholders(len(ids)) + `)`
	_, err := s.db.Exec(q, args...)
	return err
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// HasTunnelRegistryRow reports global presence (any process's tunnel) — the
// "absent target ⇒ order achieved" signal for tunnel kill orders (spec §4).
func (s *Store) HasTunnelRegistryRow(tunnelID string) (bool, error) {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM tunnel_registry WHERE tunnel_id=?`, tunnelID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// RenewTunnelHeartbeat refreshes the lease heartbeat. Returns false when the
// row is gone (zero-row ⇒ the tunnel fell out of the kill domain and must
// self-close, spec §4 duty 5).
func (s *Store) RenewTunnelHeartbeat(tunnelID string, ts int64) (bool, error) {
	if s.readOnly {
		return false, ErrReadOnly
	}
	res, err := s.db.Exec(`UPDATE tunnel_registry SET last_renewed=? WHERE tunnel_id=?`, ts, tunnelID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// CountTunnelRegistryProject counts live mirror rows of a project — the
// project kill order's completion signal (first zero-count observation marks
// the order applied, spec §4).
func (s *Store) CountTunnelRegistryProject(projectID string) (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM tunnel_registry WHERE project_id=?`, projectID).Scan(&n)
	return n, err
}

// ListTunnelRegistry returns all mirror rows (owner `tunnels ls`).
func (s *Store) ListTunnelRegistry() ([]TunnelRegistryRow, error) {
	rows, err := s.db.Query(`SELECT tunnel_id, project_id, server_id, remote, local_addr, listen_host, opened_at, last_renewed FROM tunnel_registry ORDER BY opened_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TunnelRegistryRow
	for rows.Next() {
		var r TunnelRegistryRow
		if err := rows.Scan(&r.TunnelID, &r.ProjectID, &r.ServerID, &r.Remote, &r.LocalAddr, &r.ListenHost, &r.OpenedAt, &r.LastRenewed); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GCTunnelRegistry deletes rows whose heartbeat went stale (crashed-owner
// ghosts). Idempotent; any writable broker may run it (spec §6).
func (s *Store) GCTunnelRegistry(cutoff int64) error {
	if s.readOnly {
		return ErrReadOnly
	}
	_, err := s.db.Exec(`DELETE FROM tunnel_registry WHERE last_renewed < ?`, cutoff)
	return err
}

// ExecForTest runs one raw SQL statement against the store's database. It is a
// TEST-ONLY injection seam: mcpserver's white-box mirror tests deliberately
// DROP/re-CREATE tunnel_registry to pin the manager's fail-the-Open and
// mirror-DELETE-retry failure paths — the store package deliberately exposes
// no production raw-Exec surface. The testing.Testing() gate makes the seam
// inert outside test binaries so production code cannot reach raw SQL through
// it (naming house precedent: getDACLForTest, clientops Reset*ForTest).
func (s *Store) ExecForTest(q string) error {
	if !testing.Testing() {
		return errors.New("store.ExecForTest: test-only seam, refused outside a test binary")
	}
	_, err := s.db.Exec(q)
	return err
}
