package store

import (
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// countRows is the raw table-row counter the atomicity assertions need (same
// package, so it can reach s.db).
func countRows(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// mustGetCred fails the test unless credential id is still present.
func mustGetCred(t *testing.T, s *Store, id string) *models.Credential {
	t.Helper()
	c, err := s.GetCredential(id)
	if err != nil {
		t.Fatal(err)
	}
	if c == nil {
		t.Fatalf("credential %q vanished", id)
	}
	return c
}

// TestAddServerWithCredentialsAtomic: server + cred + sudo land in ONE
// transaction; a failure at ANY point leaves zero credential orphans (G6).
func TestAddServerWithCredentialsAtomic(t *testing.T) {
	st := newTestStore(t)

	// Success path: cred + sudo + server row, exactly 2 credential rows.
	id, err := st.AddServerWithCredentials(
		&models.Server{Name: "gpu", Host: "h", Port: 22, User: "u"},
		&models.Credential{Type: models.CredPassword, Secret: []byte("pw")},
		&models.Credential{Type: models.CredPassword, Secret: []byte("sudo")},
	)
	if err != nil {
		t.Fatalf("AddServerWithCredentials: %v", err)
	}
	got, err := st.GetServer(id)
	if err != nil {
		t.Fatalf("GetServer: %v", err)
	}
	if got.CredentialID == "" || got.AuthMethod != models.AuthPassword || got.SudoCredentialID == "" {
		t.Fatalf("server not wired to its credentials: %+v", got)
	}
	if got.CredentialID == got.SudoCredentialID {
		t.Fatalf("cred and sudo must be distinct rows: %+v", got)
	}
	if n := countRows(t, st, "credentials"); n != 2 {
		t.Fatalf("credentials rows = %d, want 2 (cred + sudo)", n)
	}

	// Failure path A — fail-fast validation (pre-tx): oversized description.
	big := &models.Server{Name: "big", Host: "h", Port: 22, User: "u",
		Description: strings.Repeat("x", maxServerTextFieldBytes+1)}
	if _, err := st.AddServerWithCredentials(big,
		&models.Credential{Type: models.CredPassword, Secret: []byte("pw")}, nil); err == nil {
		t.Fatal("oversized description must be rejected")
	}
	if n := countRows(t, st, "credentials"); n != 2 {
		t.Fatalf("fail-fast rejection left residue: credentials rows = %d, want 2", n)
	}

	// Failure path B — mid-tx failure: duplicate name trips the server INSERT
	// AFTER both credential INSERTs already ran inside the tx. Rollback must
	// leave ZERO new rows — this is the G6 zero-orphan atomicity assertion.
	if _, err := st.AddServerWithCredentials(
		&models.Server{Name: "gpu", Host: "h2", Port: 22, User: "u"},
		&models.Credential{Type: models.CredPassword, Secret: []byte("pw2")},
		&models.Credential{Type: models.CredPassword, Secret: []byte("sudo2")},
	); err == nil {
		t.Fatal("duplicate name must fail the insert")
	}
	if n := countRows(t, st, "credentials"); n != 2 {
		t.Fatalf("mid-tx failure leaked credential orphans: rows = %d, want 2", n)
	}
	if n := countRows(t, st, "servers"); n != 1 {
		t.Fatalf("mid-tx failure leaked server row: rows = %d, want 1", n)
	}
	if n, err := st.CountOrphanCredentials(); err != nil || n != 0 {
		t.Fatalf("orphan count after failed tx = %d (err %v), want 0", n, err)
	}
}

// TestAddServerWithCredentialsReuseID: cred.ID already set means REUSE the
// existing row (batch dedup contract, T8) — no row is minted.
func TestAddServerWithCredentialsReuseID(t *testing.T) {
	st := newTestStore(t)
	cred := &models.Credential{Type: models.CredPassword, Secret: []byte("pw")}
	idA, err := st.AddServerWithCredentials(&models.Server{Name: "a", Host: "h", Port: 22, User: "u"}, cred, nil)
	if err != nil {
		t.Fatal(err)
	}
	before := countRows(t, st, "credentials")

	reused := &models.Credential{ID: cred.ID, Type: models.CredPassword, Secret: []byte("ignored-on-reuse")}
	idB, err := st.AddServerWithCredentials(&models.Server{Name: "b", Host: "h", Port: 22, User: "u"}, reused, nil)
	if err != nil {
		t.Fatalf("reuse: %v", err)
	}
	if n := countRows(t, st, "credentials"); n != before {
		t.Fatalf("reuse must not mint a row: credentials rows = %d, want %d", n, before)
	}
	a, _ := st.GetServer(idA)
	b, _ := st.GetServer(idB)
	if b.CredentialID != a.CredentialID {
		t.Fatalf("server b not pointed at the reused credential: %q vs %q", b.CredentialID, a.CredentialID)
	}
}

// TestDeleteServerCascadingSharedCredential: cascade delete respects BOTH
// reference columns — a credential survives while ANY other server references
// it via credential_id OR sudo_credential_id, and dies with its last reference.
func TestDeleteServerCascadingSharedCredential(t *testing.T) {
	st := newTestStore(t)

	// Login column: a and b share one credential.
	cred := &models.Credential{Type: models.CredPassword, Secret: []byte("shared")}
	idA, err := st.AddServerWithCredentials(&models.Server{Name: "a", Host: "h", Port: 22, User: "u"}, cred, nil)
	if err != nil {
		t.Fatal(err)
	}
	idB, err := st.AddServerWithCredentials(&models.Server{Name: "b", Host: "h", Port: 22, User: "u"},
		&models.Credential{ID: cred.ID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteServerCascading(idA); err != nil {
		t.Fatalf("delete a: %v", err)
	}
	mustGetCred(t, st, cred.ID) // b still references it → row survives
	if err := st.DeleteServerCascading(idB); err != nil {
		t.Fatalf("delete b: %v", err)
	}
	if c, _ := st.GetCredential(cred.ID); c != nil {
		t.Fatal("last reference gone — credential row must be dropped")
	}

	// Sudo column + the two-column CROSS check: sudoCred is x's LOGIN
	// credential and y's SUDO credential. Deleting x must not drop it (the
	// sudo_credential_id reference counts too); deleting y must.
	sudoCred := &models.Credential{Type: models.CredPassword, Secret: []byte("s")}
	idX, err := st.AddServerWithCredentials(&models.Server{Name: "x", Host: "h", Port: 22, User: "u"}, sudoCred, nil)
	if err != nil {
		t.Fatal(err)
	}
	idY, err := st.AddServerWithCredentials(&models.Server{Name: "y", Host: "h", Port: 22, User: "u"},
		nil, &models.Credential{ID: sudoCred.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteServerCascading(idX); err != nil {
		t.Fatalf("delete x: %v", err)
	}
	mustGetCred(t, st, sudoCred.ID) // y's sudo reference keeps it alive
	if err := st.DeleteServerCascading(idY); err != nil {
		t.Fatalf("delete y: %v", err)
	}
	if c, _ := st.GetCredential(sudoCred.ID); c != nil {
		t.Fatal("sudo-shared credential must be dropped once both references are gone")
	}

	// Absent id: idempotent no-op (matches legacy DeleteServer semantics).
	if err := st.DeleteServerCascading("no-such-id"); err != nil {
		t.Fatalf("absent id must be a no-op, got %v", err)
	}
}

// TestUpdateServerWithCredentialsReplacesOld: nil keeps current; a swap points
// at the new row and deletes the old one ONLY when no other server references
// it (two-column check, same for sudo).
func TestUpdateServerWithCredentialsReplacesOld(t *testing.T) {
	st := newTestStore(t)
	srv := &models.Server{Name: "gpu", Host: "h", Port: 22, User: "u"}
	id, err := st.AddServerWithCredentials(srv,
		&models.Credential{Type: models.CredPassword, Secret: []byte("old")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := st.GetServer(id)
	if err != nil {
		t.Fatal(err)
	}
	oldCredID := loaded.CredentialID

	// nil cred: field edits keep the current credential + auth method.
	loaded.Host = "h2"
	if err := st.UpdateServerWithCredentials(loaded, nil, nil); err != nil {
		t.Fatal(err)
	}
	kept, _ := st.GetServer(id)
	if kept.CredentialID != oldCredID || kept.AuthMethod != models.AuthPassword || kept.Host != "h2" {
		t.Fatalf("nil cred must keep the existing credential: %+v", kept)
	}

	// Swap to a new password: old row (unreferenced) dropped in the same tx.
	if err := st.UpdateServerWithCredentials(kept,
		&models.Credential{Type: models.CredPassword, Secret: []byte("new")}, nil); err != nil {
		t.Fatal(err)
	}
	swapped, _ := st.GetServer(id)
	if swapped.CredentialID == oldCredID || swapped.CredentialID == "" {
		t.Fatalf("credential not swapped: %+v", swapped)
	}
	if c, _ := st.GetCredential(oldCredID); c != nil {
		t.Fatal("replaced old credential (unreferenced) must be deleted in the same tx")
	}
	if c := mustGetCred(t, st, swapped.CredentialID); string(c.Secret) != "new" {
		t.Fatalf("new credential must decrypt to the new secret, got %q", c.Secret)
	}

	// Shared old credential: s2 still references it → repoint only, keep the row.
	shared := &models.Credential{Type: models.CredPassword, Secret: []byte("shared")}
	idS3, err := st.AddServerWithCredentials(&models.Server{Name: "s3", Host: "h", Port: 22, User: "u"}, shared, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddServerWithCredentials(&models.Server{Name: "s2", Host: "h", Port: 22, User: "u"},
		&models.Credential{ID: shared.ID}, nil); err != nil {
		t.Fatal(err)
	}
	s3row, _ := st.GetServer(idS3)
	if err := st.UpdateServerWithCredentials(s3row,
		&models.Credential{Type: models.CredPassword, Secret: []byte("solo")}, nil); err != nil {
		t.Fatal(err)
	}
	mustGetCred(t, st, shared.ID) // s2's reference protects the old row

	// Sudo column: replacing an unreferenced sudo credential drops the old row.
	srow := &models.Server{Name: "sudo-srv", Host: "h", Port: 22, User: "u"}
	idS, err := st.AddServerWithCredentials(srow, nil,
		&models.Credential{Type: models.CredPassword, Secret: []byte("sudo-old")})
	if err != nil {
		t.Fatal(err)
	}
	sLoaded, _ := st.GetServer(idS)
	oldSudo := sLoaded.SudoCredentialID
	if err := st.UpdateServerWithCredentials(sLoaded, nil,
		&models.Credential{Type: models.CredPassword, Secret: []byte("sudo-new")}); err != nil {
		t.Fatal(err)
	}
	if c, _ := st.GetCredential(oldSudo); c != nil {
		t.Fatal("replaced sudo credential (unreferenced) must be deleted in the same tx")
	}

	// Net result: the swaps left zero orphans behind.
	if n, err := st.CountOrphanCredentials(); err != nil || n != 0 {
		t.Fatalf("orphan count after swaps = %d (err %v), want 0", n, err)
	}
}

// TestClearServerCredential (Plan 21 A2): ClearServerCredential resets a
// server to the credential-less form in ONE tx — credential/sudo references
// cleared, auth_method blanked, the needs-passphrase tag stripped (meaningless
// without a credential), exclusively-owned credential rows cascade-deleted
// (two-column guard — shared rows survive). Absent id = idempotent no-op.
func TestClearServerCredential(t *testing.T) {
	st := newTestStore(t)

	// a: shared login credential + EXCLUSIVE sudo credential + the tag.
	// b: shares a's login credential via the reuse-.ID contract.
	cred := &models.Credential{Type: models.CredPassword, Secret: []byte("shared")}
	idA, err := st.AddServerWithCredentials(
		&models.Server{Name: "a", Host: "h", Port: 22, User: "u", Tags: []string{"needs-passphrase", "gpu"}},
		cred, &models.Credential{Type: models.CredPassword, Secret: []byte("sudo")},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddServerWithCredentials(&models.Server{Name: "b", Host: "h", Port: 22, User: "u"},
		&models.Credential{ID: cred.ID}, nil); err != nil {
		t.Fatal(err)
	}
	a, _ := st.GetServer(idA)
	sudoID := a.SudoCredentialID
	before := countRows(t, st, "credentials")

	if err := st.ClearServerCredential(idA); err != nil {
		t.Fatalf("ClearServerCredential: %v", err)
	}

	// 1) a is credential-less: no login cred, no sudo cred, no auth method.
	got, err := st.GetServer(idA)
	if err != nil {
		t.Fatal(err)
	}
	if got.CredentialID != "" || got.AuthMethod != "" || got.SudoCredentialID != "" {
		t.Fatalf("server must be fully de-referenced: %+v", got)
	}
	for _, tg := range got.Tags {
		if tg == "needs-passphrase" {
			t.Fatalf("needs-passphrase must be stripped, got %v", got.Tags)
		}
	}
	if len(got.Tags) != 1 || got.Tags[0] != "gpu" {
		t.Fatalf("other tags must survive: %v", got.Tags)
	}

	// 2) two-column guard: b still references the login credential → row
	// survives; a's sudo credential was exclusively owned → row dropped.
	mustGetCred(t, st, cred.ID)
	if c, _ := st.GetCredential(sudoID); c != nil {
		t.Fatal("exclusively-owned sudo credential must be cascade-deleted")
	}
	if n := countRows(t, st, "credentials"); n != before-1 {
		t.Fatalf("credentials rows = %d, want %d (shared kept, exclusive dropped)", n, before-1)
	}
	if n, err := st.CountOrphanCredentials(); err != nil || n != 0 {
		t.Fatalf("orphan count after clear = %d (err %v), want 0", n, err)
	}

	// 3) absent id: idempotent no-op (DeleteServerCascading semantics).
	if err := st.ClearServerCredential("no-such-id"); err != nil {
		t.Fatalf("absent id must be a no-op, got %v", err)
	}
}
