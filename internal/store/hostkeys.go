package store

import (
	"bytes"
	"database/sql"
	"fmt"

	"golang.org/x/crypto/ssh"
)

// hostKeyID is the storage key for a host's pinned public key. Always host:port
// (unconditional, even for :22) so same-host-different-port servers never collide.
// OpenSSH known_hosts uses bare "host" for :22 and "[host]:port" otherwise; that
// format-specific rendering lives in the known_hosts serializer, not here.
func hostKeyID(host string, port int) string {
	return fmt.Sprintf("%s:%d", host, port)
}

// Pin formats (host_keys.pin_format). A blob anchor stores the marshaled SSH
// wire-format public key in key_blob; a fingerprint anchor stores the OpenSSH
// "SHA256:<base64>" fingerprint STRING bytes there instead (Plan 48 §5).
const (
	PinFormatBlob        = "blob"
	PinFormatFingerprint = "fingerprint"
)

// Pin sources (host_keys.pin_source). 'tofu' is the column DEFAULT — every
// pre-Plan-48 anchor is an automatic first-trust blob. ('manual' — the owner
// out-of-band command — lands in a later task of the same plan.)
const (
	PinSourceTofu    = "tofu"
	PinSourceForward = "forward"
)

// Pin is a stored host-key anchor: its value bytes and format (Plan 48 §5).
type Pin struct {
	Blob   []byte
	Format string
}

// Matches reports whether the presented marshaled host key satisfies this pin
// under the dual-mode semantics — the ONE equality definition for the whole
// system (TOFU callback, ApplyForwardedHostKey, the /pin-hostkey equal
// computation, the owner --force comparison; Plan 48 §5). blob anchors compare
// bytewise; fingerprint anchors compare the presented key's SHA256 fingerprint
// with the stored string — sha256 collision resistance makes the two
// security-equivalent. An unknown/empty format (unreachable via the writers,
// possible only in a hand-edited DB) degrades to the blob comparison, exactly
// how the DEFAULTed column reads it. A nil anchor matches nothing.
func (p *Pin) Matches(presented []byte) bool {
	if p == nil {
		return false
	}
	if p.Format == PinFormatFingerprint {
		pub, err := ssh.ParsePublicKey(presented)
		if err != nil {
			return false
		}
		return ssh.FingerprintSHA256(pub) == string(p.Blob)
	}
	return bytes.Equal(p.Blob, presented)
}

// loadPin reads one anchor over dbtx (s.db on the point-read path, a tx inside
// the insert-only primitives) — the single place that knows the pin column
// set. Returns (nil, nil) when the host:port is unpinned.
func loadPin(q dbtx, hostPort string) (*Pin, error) {
	var p Pin
	err := q.QueryRow(`SELECT key_blob, pin_format FROM host_keys WHERE host_port=?`, hostPort).Scan(&p.Blob, &p.Format)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// GetHostKey returns the stored marshaled host key for host:port, or (nil, nil) if absent.
//
// Legacy byte-only view: a fingerprint-format anchor surfaces its fingerprint
// STRING bytes here, so a pre-Plan-48 consumer byte-compares and rejects — the
// documented mixed-version degradation (Plan 48 §5), not a bug.
func (s *Store) GetHostKey(host string, port int) ([]byte, error) {
	p, err := loadPin(s.db, hostKeyID(host, port))
	if err != nil || p == nil {
		return nil, err
	}
	return p.Blob, nil
}

// SaveHostKey records (trusts on first use) a marshaled host key for host:port.
// Kept as an UPSERT verbatim: the vault-side first-trust path has a single
// in-process writer and no concurrency face; Plan 48's insert-only constraint
// is implemented by the NEW primitives below, not by tightening this one.
func (s *Store) SaveHostKey(host string, port int, marshaledKey []byte) error {
	if s.readOnly {
		return ErrReadOnly
	}
	_, err := s.db.Exec(
		`INSERT INTO host_keys (host_port, key_blob, created_at) VALUES (?,?,?)
		 ON CONFLICT(host_port) DO UPDATE SET key_blob=excluded.key_blob`,
		hostKeyID(host, port), marshaledKey, now(),
	)
	return err
}

// InsertForwardedPin records a device-forwarded anchor for host:port — the
// store-side landing of POST /pin-hostkey (Plan 48 §1.2 ⑤). Insert-only: an
// existing anchor is NEVER overwritten. One transaction:
//   - free slot → INSERT (pin_format=blob, pin_source=forward, pin_device=
//     device) plus the caller's audit row, committed atomically: an audit
//     failure rolls the anchor back, so a pin never lands without its history;
//   - occupied → the existing anchor is read and dual-mode-compared INSIDE the
//     transaction (a concurrent owner --force/--clear cannot swap the anchor
//     between conflict and judgment), then the transaction rolls back — the
//     conflict path writes no audit row — and (equal, nil) carries the 409
//     semantics (equal lets the client auto-close duplicate forwards, §2.1).
func (s *Store) InsertForwardedPin(host string, port int, blob []byte, device string, audit AuditRow) (bool, error) {
	if s.readOnly {
		return false, ErrReadOnly
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback() // no-op after Commit; IS the rollback on conflict/audit-failure paths
	res, err := tx.Exec(
		`INSERT INTO host_keys (host_port, key_blob, created_at, pin_format, pin_source, pin_device)
		 VALUES (?,?,?,?,?,?)
		 ON CONFLICT(host_port) DO NOTHING`,
		hostKeyID(host, port), blob, now(), PinFormatBlob, PinSourceForward, device)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		existing, err := loadPin(tx, hostKeyID(host, port))
		if err != nil {
			return false, err
		}
		return existing.Matches(blob), nil // deferred Rollback discards the tx
	}
	if err := writeAuditTx(tx, audit); err != nil {
		return false, err // deferred Rollback: the anchor does not persist either
	}
	return false, tx.Commit()
}

// ApplyForwardedHostKey records the local receipt of a pin the broker already
// accepted via /pin-hostkey — the ONLY method allowed to write host_keys on a
// read-only (offline cache) store. The write lands in the store it is called
// on: the caller must resolve the CURRENT store (the real-time-resolution
// rule of §2.2), so a hot rebuild between the broker's 201 and this call
// cannot strand the anchor in a swapped-out generation. Contract (§2.3, T9):
//   - insert-only: a free slot inserts with the same metadata the broker
//     records for this pin (pin_format=blob, pin_source=forward, pin_device=
//     the device name set via SetForwardDevice) — the temp-DB row dies with
//     the process; the consistency only guards future consumers;
//   - an existing anchor is judged with the system-wide dual-mode semantics
//     (a fingerprint anchor matches the presented key's fingerprint): equal →
//     success with the row untouched (race tolerance); different → error;
//   - no local audit row — the broker's pin-forward row IS the history.
func (s *Store) ApplyForwardedHostKey(host string, port int, marshaledKey []byte) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op after Commit; IS the rollback on the equal path
	res, err := tx.Exec(
		`INSERT INTO host_keys (host_port, key_blob, created_at, pin_format, pin_source, pin_device)
		 VALUES (?,?,?,?,?,?)
		 ON CONFLICT(host_port) DO NOTHING`,
		hostKeyID(host, port), marshaledKey, now(), PinFormatBlob, PinSourceForward, s.forwardDevice)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		existing, err := loadPin(tx, hostKeyID(host, port))
		if err != nil {
			return err
		}
		if !existing.Matches(marshaledKey) {
			return fmt.Errorf("apply forwarded host key for %s: existing pin differs from the presented key (pull and retry)", hostKeyID(host, port))
		}
		return nil // equal → pass; deferred Rollback discards the no-op tx
	}
	return tx.Commit()
}
