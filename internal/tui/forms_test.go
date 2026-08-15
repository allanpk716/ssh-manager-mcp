package tui

import (
	"testing"

	"charm.land/huh/v2"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// newStore opens a fresh temp-dir store with a random master key (mcpserver
// tests pattern); tests only exercise non-crypto store paths.
func newStore(t *testing.T) *store.Store {
	t.Helper()
	mk, _ := store.GenerateMasterKey()
	st, err := store.Open(t.TempDir()+"/t.db", mk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// Adapted with the Plan 20 B1 switchover: toServer (which minted credentials
// via st directly) is gone; the draft now decomposes into server + credential
// pointers (toParts) and submitServer persists through the transactional APIs.
func TestSubmitServer_Add(t *testing.T) {
	st := newStore(t)
	d := &serverDraft{Name: "gpu", Host: "192.0.2.10", User: "u", Port: 22, Password: "pw", Hardware: "2x3090"}
	if _, ok := submitServer(st, nil, d)().(actionDoneMsg); !ok {
		t.Fatal("add must succeed")
	}
	got, _ := st.GetServerByName("gpu")
	if got == nil || got.Host != "192.0.2.10" || got.AuthMethod != models.AuthPassword || got.Hardware != "2x3090" {
		t.Fatalf("roundtrip: %+v", got)
	}
	if got.CredentialID == "" {
		t.Fatalf("password draft must mint a credential: %+v", got)
	}
}

func TestDraftToParts_PasswordKeyMutex(t *testing.T) {
	d := &serverDraft{Name: "x", Host: "h", User: "u", Password: "p", KeyPath: "k"}
	if _, _, _, err := d.toParts(); err == nil {
		t.Fatal("password+key must be rejected (CLI parity)")
	}
}

// TestSubmitServer_AddCredentialLess (Plan 20 C0): add mode no longer requires
// a credential — a draft with neither password nor key persists a credential-less
// server (empty CredentialID/AuthMethod; exec on it returns "no_credential"
// until one is attached).
func TestSubmitServer_AddCredentialLess(t *testing.T) {
	st := newStore(t)
	d := &serverDraft{Name: "x", Host: "h", User: "u", Port: 22} // no password, no key
	cmd := submitServer(st, nil, d)
	if _, ok := cmd().(actionDoneMsg); !ok {
		t.Fatalf("add without credential must succeed (credential-less), got non-actionDoneMsg")
	}
	got, _ := st.GetServerByName("x")
	if got == nil || got.CredentialID != "" || got.AuthMethod != "" {
		t.Fatalf("credential-less server not persisted as such: %+v", got)
	}
}

func TestSubmitServer_EditPreservesTags(t *testing.T) {
	st := newStore(t)
	cid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("p")})
	_, _ = st.AddServer(&models.Server{Name: "t", Host: "h", User: "u", AuthMethod: models.AuthPassword, CredentialID: cid, Tags: []string{"gpu"}})
	cur, _ := st.GetServerByName("t")
	d := &serverDraft{Name: "t", Host: "h2", User: "u", Port: 22} // edit host, no secrets
	cmd := submitServer(st, cur, d)
	if _, ok := cmd().(actionDoneMsg); !ok {
		t.Fatalf("edit must succeed")
	}
	got, _ := st.GetServerByName("t")
	if len(got.Tags) != 1 || got.Tags[0] != "gpu" || got.Host != "h2" {
		t.Fatalf("tags lost or host not updated: %+v", got)
	}
}

// TestSubmitServer_EditRecredentialSwaps: edit mode with a filled password
// swaps the credential via UpdateServerWithCredentials — new row in effect,
// unreferenced old row dropped in the same tx (empty-secret-keeps-existing is
// the complement, pinned by TestSubmitServer_EditPreservesTags).
func TestSubmitServer_EditRecredentialSwaps(t *testing.T) {
	st := newStore(t)
	cid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("old")})
	_, _ = st.AddServer(&models.Server{Name: "t", Host: "h", User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	cur, _ := st.GetServerByName("t")
	d := &serverDraft{Name: "t", Host: "h", User: "u", Port: 22, Password: "new"}
	if _, ok := submitServer(st, cur, d)().(actionDoneMsg); !ok {
		t.Fatal("edit with new password must succeed")
	}
	got, _ := st.GetServerByName("t")
	if got.CredentialID == cid || got.CredentialID == "" {
		t.Fatalf("credential not swapped: %+v", got)
	}
	if c, _ := st.GetCredential(cid); c != nil {
		t.Fatal("old credential row must be dropped when unreferenced")
	}
	if c, _ := st.GetCredential(got.CredentialID); c == nil || string(c.Secret) != "new" {
		t.Fatal("new credential must decrypt to the new secret")
	}
}

// TestNewServerFormFieldConstructors (Plan 20 C3a regression): verifies the
// extracted constructors produce valid, non-nil fields.
func TestNewServerFormFieldConstructors(t *testing.T) {
	d := &serverDraft{}

	// passwordField returns non-nil *huh.Input for both editing modes
	if passwordField(d, false) == nil {
		t.Error("passwordField(d, false) returned nil")
	}
	if passwordField(d, true) == nil {
		t.Error("passwordField(d, true) returned nil")
	}

	// sudoPasswordField returns non-nil *huh.Input
	if sudoPasswordField(d) == nil {
		t.Error("sudoPasswordField(d) returned nil")
	}

	// structuredFields returns exactly 6 fields, all non-nil
	fields := structuredFields(d)
	if len(fields) != 6 {
		t.Fatalf("structuredFields(d) returned %d fields, want 6", len(fields))
	}
	for i, f := range fields {
		if f == nil {
			t.Errorf("structuredFields(d)[%d] is nil", i)
		}
	}

	// All fields implement the huh.Field interface (basic type check)
	for i, f := range fields {
		var _ huh.Field = f // Interface assignment compile-time check
		_ = i
	}
}

// TestNewServerFormComposesConstructors (Plan 20 C3a): verifies that
// newServerForm successfully composes the extracted constructors.
func TestNewServerFormComposesConstructors(t *testing.T) {
	d := &serverDraft{}

	// Test add mode form
	f := newServerForm(d, false)
	if f == nil {
		t.Fatal("newServerForm(d, false) returned nil")
	}

	// Test edit mode form
	fEdit := newServerForm(d, true)
	if fEdit == nil {
		t.Fatal("newServerForm(d, true) returned nil")
	}

	// Basic smoke test: both forms should be constructable
	// (huh API limitations prevent deeper inspection)
}
