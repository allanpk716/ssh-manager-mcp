package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// TunnelOrder is an owner kill order (spec §4). Exactly one of TunnelID /
// ProjectID is set (enforced app-side here + by a CHECK constraint).
// outcome ∈ {NULL(pending), 'applied'} — the 'expired' terminal state was
// removed in spec rev3 (unreachable by construction).
type TunnelOrder struct {
	ID        int64
	TunnelID  string
	ProjectID string
	CreatedBy string
	CreatedAt int64
	AppliedAt *int64
	Outcome   *string
}

// CreateTunnelOrder places a kill order. createdBy is the OS user running the
// CLI (owner-action traceability, spec §7).
func (s *Store) CreateTunnelOrder(tunnelID, projectID, createdBy string) (int64, error) {
	if s.readOnly {
		return 0, ErrReadOnly
	}
	if (tunnelID == "") == (projectID == "") {
		return 0, errors.New("exactly one of tunnel_id / project_id must be set")
	}
	res, err := s.db.Exec(
		`INSERT INTO tunnel_orders(tunnel_id, project_id, created_by, created_at) VALUES (?, ?, ?, ?)`,
		nullableString(tunnelID), nullableString(projectID), createdBy, now(),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func scanTunnelOrder(row interface{ Scan(...any) error }) (*TunnelOrder, error) {
	var o TunnelOrder
	var tun, proj sql.NullString
	if err := row.Scan(&o.ID, &tun, &proj, &o.CreatedBy, &o.CreatedAt, &o.AppliedAt, &o.Outcome); err != nil {
		return nil, err
	}
	o.TunnelID, o.ProjectID = tun.String, proj.String
	return &o, nil
}

const tunnelOrderCols = `id, tunnel_id, project_id, created_by, created_at, applied_at, outcome`

// GetTunnelOrder returns the order by id (nil, nil when absent) — the kill
// CLI polls it after placing an order.
func (s *Store) GetTunnelOrder(id int64) (*TunnelOrder, error) {
	o, err := scanTunnelOrder(s.db.QueryRow(`SELECT `+tunnelOrderCols+` FROM tunnel_orders WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return o, nil
}

// PendingTunnelOrders returns orders with outcome IS NULL (spec §4 — every
// read AND every marking UPDATE carries this guard).
func (s *Store) PendingTunnelOrders() ([]TunnelOrder, error) {
	rows, err := s.db.Query(`SELECT ` + tunnelOrderCols + ` FROM tunnel_orders WHERE outcome IS NULL ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TunnelOrder
	for rows.Next() {
		o, err := scanTunnelOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *o)
	}
	return out, rows.Err()
}

// MarkTunnelOrderApplied flips a pending order to its only terminal state.
// Returns false when the order was not pending (already applied).
func (s *Store) MarkTunnelOrderApplied(id int64) (bool, error) {
	if s.readOnly {
		return false, ErrReadOnly
	}
	res, err := s.db.Exec(
		`UPDATE tunnel_orders SET applied_at=?, outcome='applied' WHERE id=? AND outcome IS NULL`,
		now(), id,
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// CleanupTunnelOrders (spec §4 rev4, three statements — the naive
// "delete all old rows" tautology bug is gone): applied rows after 7d;
// pending rows only when their TARGET is also absent from tunnel_registry
// (an order whose target rows still exist — e.g. a project order grinding
// through an agent's continuous reopens — is still in effect and is never
// silently dropped).
func (s *Store) CleanupTunnelOrders() error {
	if s.readOnly {
		return ErrReadOnly
	}
	cutoff := now() - 7*24*3600
	stmts := []string{
		`DELETE FROM tunnel_orders WHERE outcome IS NOT NULL AND created_at < ?`,
		`DELETE FROM tunnel_orders WHERE outcome IS NULL AND created_at < ?
		   AND tunnel_id IS NOT NULL
		   AND NOT EXISTS (SELECT 1 FROM tunnel_registry WHERE tunnel_registry.tunnel_id = tunnel_orders.tunnel_id)`,
		`DELETE FROM tunnel_orders WHERE outcome IS NULL AND created_at < ?
		   AND project_id IS NOT NULL
		   AND NOT EXISTS (SELECT 1 FROM tunnel_registry WHERE tunnel_registry.project_id = tunnel_orders.project_id)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q, cutoff); err != nil {
			return fmt.Errorf("tunnel_orders cleanup: %w", err)
		}
	}
	return nil
}
