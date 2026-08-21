package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"ssh-manager-mcp/internal/models"
)

// dbtx is the surface *sql.DB and *sql.Tx share, so the row-level helpers
// below serve both the legacy tx-less methods (AddServer/UpdateServer/
// GetServer delegate) and the transactional API in this file — one copy of
// each SQL statement, no behavior change on the legacy paths.
type dbtx interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
}

// insertServerTx inserts a full server row on db (a *sql.Tx inside the
// transactional API, s.db on the legacy path) and returns the new id. The
// NULL-for-"" mapping on credential_id/sudo_credential_id (Plan 20 C0
// credential-less form) lives here exactly once.
func insertServerTx(db dbtx, srv *models.Server) (string, error) {
	id := newID()
	ts := now()
	tagsJSON, _ := json.Marshal(srv.Tags)
	sudo := nullableString(srv.SudoCredentialID)
	// Credential-less servers (Plan 20 C0): "" must land as NULL — the FK on
	// credential_id has no '' row to reference, so a literal '' would violate it.
	cred := nullableString(srv.CredentialID)
	_, err := db.Exec(
		`INSERT INTO servers (id,name,host,port,user,auth_method,credential_id,sudo_credential_id,tags,description,location,hardware,services,role,caveats,expose_host,created_at,updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, srv.Name, srv.Host, srv.Port, srv.User, string(srv.AuthMethod), cred, sudo, string(tagsJSON), srv.Description,
		srv.Location, srv.Hardware, srv.Services, srv.Role, srv.Caveats, srv.ExposeHost, ts, ts,
	)
	if err != nil {
		// Localize the name-collision error — the raw driver text
		// ("constraint failed: UNIQUE constraint failed: servers.name (2067)")
		// leaks SQLite jargon into TUI/CLI surfaces.
		if strings.Contains(err.Error(), "UNIQUE constraint failed: servers.name") {
			return "", fmt.Errorf("server name %q already exists", srv.Name)
		}
		return "", err
	}
	return id, nil
}

// updateServerTx writes the full row (id-preserving) on db. Errors when the id
// is absent (RowsAffected==0) — same contract as the legacy UpdateServer.
func updateServerTx(db dbtx, srv *models.Server) error {
	tagsJSON, _ := json.Marshal(srv.Tags)
	sudo := nullableString(srv.SudoCredentialID)
	// Same NULL-for-"" mapping as insertServerTx (Plan 20 C0 credential-less form).
	cred := nullableString(srv.CredentialID)
	res, err := db.Exec(
		`UPDATE servers SET name=?,host=?,port=?,user=?,auth_method=?,credential_id=?,sudo_credential_id=?,tags=?,description=?,location=?,hardware=?,services=?,role=?,caveats=?,expose_host=?,updated_at=? WHERE id=?`,
		srv.Name, srv.Host, srv.Port, srv.User, string(srv.AuthMethod), cred, sudo, string(tagsJSON), srv.Description,
		srv.Location, srv.Hardware, srv.Services, srv.Role, srv.Caveats, srv.ExposeHost, now(), srv.ID,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("server id %q not found", srv.ID)
	}
	return nil
}

// getServerTx loads one server row by id on db.
func getServerTx(db dbtx, id string) (*models.Server, error) {
	return scanServer(db.QueryRow(
		`SELECT id,name,host,port,user,auth_method,credential_id,sudo_credential_id,tags,description,location,hardware,services,role,caveats,expose_host,created_at,updated_at FROM servers WHERE id=?`, id,
	))
}

// insertCredentialTx seals+inserts c inside tx. When c.ID is already set the
// row is assumed to exist (batch dedup reuse) and is NOT inserted again; the
// same id is returned.
func insertCredentialTx(tx *sql.Tx, masterKey []byte, c *models.Credential) (string, error) {
	if c.ID != "" {
		return c.ID, nil
	}
	secretBlob, err := seal(masterKey, c.Secret)
	if err != nil {
		return "", err
	}
	var passBlob []byte
	if len(c.Passphrase) > 0 {
		if passBlob, err = seal(masterKey, c.Passphrase); err != nil {
			return "", err
		}
	}
	id := newID()
	ts := now()
	if _, err := tx.Exec(
		`INSERT INTO credentials (id,type,secret_blob,passphrase_blob,created_at,updated_at) VALUES (?,?,?,?,?,?)`,
		id, string(c.Type), secretBlob, passBlob, ts, ts,
	); err != nil {
		return "", err
	}
	c.ID = id
	return id, nil
}

// credentialReferencedElseBy counts servers rows referencing credID via EITHER
// credential_id or sudo_credential_id, excluding the server row excludeID.
func credentialReferencedElseBy(tx *sql.Tx, credID, excludeID string) (int, error) {
	var n int
	err := tx.QueryRow(
		`SELECT COUNT(*) FROM servers
		  WHERE (credential_id=? OR sudo_credential_id=?) AND id<>?`,
		credID, credID, excludeID,
	).Scan(&n)
	return n, err
}

// AddServerWithCredentials inserts srv and (optionally) its credentials in ONE
// transaction — a mid-way failure leaves zero credential orphans (Plan 20
// B1/G6). cred/sudo may be nil (credential-less server). cred.ID != "" (same
// for sudo) means REUSE the existing credential row — no new row is minted;
// that is the batch-dedup contract the snapshot import (T8) relies on.
func (s *Store) AddServerWithCredentials(srv *models.Server, cred, sudo *models.Credential) (string, error) {
	if s.readOnly {
		return "", ErrReadOnly
	}
	if err := validateServerText(srv); err != nil {
		return "", err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback() // no-op after Commit
	srv.CredentialID, srv.AuthMethod = "", ""
	if cred != nil {
		cid, err := insertCredentialTx(tx, s.masterKey, cred)
		if err != nil {
			return "", err
		}
		srv.CredentialID, srv.AuthMethod = cid, cred.Type.AuthMethodForServer()
	}
	if sudo != nil {
		sid, err := insertCredentialTx(tx, s.masterKey, sudo)
		if err != nil {
			return "", err
		}
		srv.SudoCredentialID = sid
	}
	id, err := insertServerTx(tx, srv)
	if err != nil {
		return "", err
	}
	return id, tx.Commit()
}

// UpdateServerWithCredentials updates srv and swaps credentials atomically:
// nil/empty cred = keep current; cred.ID set = point at it; else mint new and
// delete the replaced one when nothing else references it (two-column check).
// Same rules for sudo.
func (s *Store) UpdateServerWithCredentials(srv *models.Server, cred, sudo *models.Credential) error {
	if s.readOnly {
		return ErrReadOnly
	}
	if err := validateServerText(srv); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op after Commit
	old, err := getServerTx(tx, srv.ID)
	if err != nil {
		return err
	}
	var dropOldCred, dropOldSudo string
	if cred != nil {
		cid, err := insertCredentialTx(tx, s.masterKey, cred)
		if err != nil {
			return err
		}
		if old.CredentialID != "" && old.CredentialID != cid {
			// Fail-closed: if the reference-count query itself errors we ABORT
			// the whole tx rather than guess whether the old credential is
			// shared — deleting a shared row would strand the referencing
			// server, and unconditionally keeping it after an unknown count is
			// a silent policy fork mid-write (gc exists for genuine leaks).
			n, err := credentialReferencedElseBy(tx, old.CredentialID, srv.ID)
			if err != nil {
				return err
			}
			if n == 0 {
				dropOldCred = old.CredentialID
			}
		}
		srv.CredentialID, srv.AuthMethod = cid, cred.Type.AuthMethodForServer()
	} else {
		srv.CredentialID, srv.AuthMethod = old.CredentialID, old.AuthMethod
	}
	if sudo != nil {
		sid, err := insertCredentialTx(tx, s.masterKey, sudo)
		if err != nil {
			return err
		}
		if old.SudoCredentialID != "" && old.SudoCredentialID != sid {
			n, err := credentialReferencedElseBy(tx, old.SudoCredentialID, srv.ID)
			if err != nil {
				return err // fail-closed, same rationale as the credential branch
			}
			if n == 0 {
				dropOldSudo = old.SudoCredentialID
			}
		}
		srv.SudoCredentialID = sid
	} else {
		srv.SudoCredentialID = old.SudoCredentialID
	}
	if err := updateServerTx(tx, srv); err != nil {
		return err
	}
	for _, id := range []string{dropOldCred, dropOldSudo} {
		if id == "" {
			continue
		}
		if _, err := tx.Exec(`DELETE FROM credentials WHERE id=?`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteServerCascading removes the server row and any credentials it EXCLUSIVELY
// owned (two-column reference check — shared credentials survive). Deleting an
// absent id is an idempotent no-op, matching the legacy DeleteServer.
func (s *Store) DeleteServerCascading(id string) error {
	if s.readOnly {
		return ErrReadOnly
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op after Commit
	srv, err := getServerTx(tx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil // already gone — nothing to cascade
		}
		return err
	}
	if _, err := tx.Exec(`DELETE FROM servers WHERE id=?`, id); err != nil {
		return err
	}
	for _, cid := range []string{srv.CredentialID, srv.SudoCredentialID} {
		if cid == "" {
			continue
		}
		if n, err := credentialReferencedElseBy(tx, cid, id); err != nil {
			return err // fail-closed: unknown reference state must not delete
		} else if n == 0 {
			if _, err := tx.Exec(`DELETE FROM credentials WHERE id=?`, cid); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// dropTagFrom returns tags minus every occurrence of tag (fresh slice, valid
// empty when the list empties). The store-package twin of the TUI dropTag /
// CLI stripTag helpers — kept unexported and local to the one call site.
func dropTagFrom(tags []string, tag string) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if t != tag {
			out = append(out, t)
		}
	}
	return out
}

// ClearServerCredential resets the server to the credential-less form in one
// transaction: credential/sudo references cleared, the needs-passphrase tag
// stripped (meaningless without a credential), and exclusively-owned credential
// rows deleted (two-column guard — shared rows survive). Absent id = no-op.
func (s *Store) ClearServerCredential(id string) error {
	if s.readOnly {
		return ErrReadOnly
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	srv, err := getServerTx(tx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil // idempotent no-op (DeleteServerCascading semantics)
		}
		return err
	}
	oldCred, oldSudo := srv.CredentialID, srv.SudoCredentialID
	srv.CredentialID, srv.AuthMethod, srv.SudoCredentialID = "", "", ""
	srv.Tags = dropTagFrom(srv.Tags, "needs-passphrase")
	if err := updateServerTx(tx, srv); err != nil {
		return err
	}
	for _, cid := range []string{oldCred, oldSudo} {
		if cid == "" {
			continue
		}
		n, err := credentialReferencedElseBy(tx, cid, id)
		if err != nil {
			return err // fail-closed
		}
		if n == 0 {
			if _, err := tx.Exec(`DELETE FROM credentials WHERE id=?`, cid); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// CountOrphanCredentials counts credential rows referenced by NEITHER column.
// Read-only-safe (pure SELECT) — gc can dry-run against an offline cache.
func (s *Store) CountOrphanCredentials() (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM credentials
		  WHERE id NOT IN (SELECT credential_id FROM servers WHERE credential_id IS NOT NULL)
		    AND id NOT IN (SELECT sudo_credential_id FROM servers WHERE sudo_credential_id IS NOT NULL)`,
	).Scan(&n)
	return n, err
}

// DeleteOrphanCredentials removes exactly those rows (gc --apply). It never
// touches servers, host_keys, or cache_tokens — the WHERE clause is the same
// two-column reference check as CountOrphanCredentials.
func (s *Store) DeleteOrphanCredentials() (int64, error) {
	if s.readOnly {
		return 0, ErrReadOnly
	}
	res, err := s.db.Exec(
		`DELETE FROM credentials
		  WHERE id NOT IN (SELECT credential_id FROM servers WHERE credential_id IS NOT NULL)
		    AND id NOT IN (SELECT sudo_credential_id FROM servers WHERE sudo_credential_id IS NOT NULL)`,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
