package store

import (
	"fmt"
	"net"
	"strings"
)

// CanonicalBindIP validates a forward-bind host candidate and returns its
// canonical text form (net.IP.String()). Rules (spec §2): must be an IP
// literal — hostnames, CIDR ranges and zone-suffixed IPv6 (fe80::1%eth0) all
// fail net.ParseIP and are rejected here. Loopback/wildcard policy is the
// CALLER's (add rejects both; the forward gate always allows loopback and
// rejects wildcards).
func CanonicalBindIP(raw string) (string, error) {
	ip := net.ParseIP(strings.TrimSpace(raw))
	if ip == nil {
		return "", fmt.Errorf("listen_host %q is not an IP literal (hostnames, CIDR ranges and zoned IPv6 are not allowed)", raw)
	}
	return ip.String(), nil
}

// AddForwardBindHost owner-approves a non-loopback, non-wildcard bind IP,
// stored in canonical form. Idempotent (INSERT OR IGNORE). Spec §2.
func (s *Store) AddForwardBindHost(rawIP string) error {
	if s.readOnly {
		return ErrReadOnly
	}
	ip := net.ParseIP(strings.TrimSpace(rawIP))
	if ip == nil {
		return fmt.Errorf("%q is not an IP literal", rawIP)
	}
	if ip.IsLoopback() {
		return fmt.Errorf("%q is loopback — loopback is always allowed and needs no whitelist entry", rawIP)
	}
	if ip.IsUnspecified() {
		return fmt.Errorf("%q is a wildcard address — binding 0.0.0.0/:: is forbidden", rawIP)
	}
	_, err := s.db.Exec(`INSERT OR IGNORE INTO forward_bind_hosts(ip, created_at) VALUES (?, ?)`, ip.String(), now())
	return err
}

// RemoveForwardBindHost removes a whitelisted IP (any equivalent text form
// hits the canonical row). Returns false when no such row existed.
func (s *Store) RemoveForwardBindHost(rawIP string) (bool, error) {
	if s.readOnly {
		return false, ErrReadOnly
	}
	canonical, err := CanonicalBindIP(rawIP)
	if err != nil {
		return false, err
	}
	res, err := s.db.Exec(`DELETE FROM forward_bind_hosts WHERE ip=?`, canonical)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ListForwardBindHosts returns the whitelist in canonical form. Read path —
// works on read-only stores (empty on offline hydrated stores: the table is
// not exported into snapshots, which is the mechanism that keeps offline
// stdio loopback-only, spec §2).
func (s *Store) ListForwardBindHosts() ([]string, error) {
	rows, err := s.db.Query(`SELECT ip FROM forward_bind_hosts ORDER BY ip`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ip string
		if err := rows.Scan(&ip); err != nil {
			return nil, err
		}
		out = append(out, ip)
	}
	return out, rows.Err()
}
