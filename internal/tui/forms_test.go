package tui

import (
	"reflect"
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

// TestSubmitServer_EditRecredentialDropsNeedsPassphrase (final review I-2):
// the needs-passphrase tag means "current credential lacks its passphrase" —
// a TUI edit that mints a new credential resolves it (tag dropped, other tags
// kept, empty stored as empty); an edit with NO credential change must keep
// the tag (the ⚠ stays until a real re-credential).
func TestSubmitServer_EditRecredentialDropsNeedsPassphrase(t *testing.T) {
	st := newStore(t)
	cid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("p")})
	_, _ = st.AddServer(&models.Server{
		Name: "enc", Host: "h", User: "u", AuthMethod: models.AuthPassword,
		CredentialID: cid, Tags: []string{"needs-passphrase", "gpu"},
	})
	_, _ = st.AddServer(&models.Server{
		Name: "solo", Host: "h", User: "u", AuthMethod: models.AuthPassword,
		CredentialID: cid, Tags: []string{"needs-passphrase"},
	})

	// edit without credential change: the tag survives
	cur, _ := st.GetServerByName("enc")
	d := &serverDraft{Name: "enc", Host: "h2", User: "u", Port: 22} // no secrets
	if _, ok := submitServer(st, cur, d)().(actionDoneMsg); !ok {
		t.Fatal("edit without credential change must succeed")
	}
	got, _ := st.GetServerByName("enc")
	if !hasTag(got, "needs-passphrase") || !hasTag(got, "gpu") {
		t.Fatalf("edit without re-credential must keep the tag: %+v", got.Tags)
	}

	// re-credential via new password: tag dropped, other tag kept
	cur, _ = st.GetServerByName("enc")
	d = &serverDraft{Name: "enc", Host: "h2", User: "u", Port: 22, Password: "new"}
	if _, ok := submitServer(st, cur, d)().(actionDoneMsg); !ok {
		t.Fatal("edit with new password must succeed")
	}
	got, _ = st.GetServerByName("enc")
	if hasTag(got, "needs-passphrase") || len(got.Tags) != 1 || got.Tags[0] != "gpu" {
		t.Fatalf("re-credential must drop needs-passphrase and keep other tags: %+v", got.Tags)
	}

	// tag-only list: drops to a valid empty slice
	cur, _ = st.GetServerByName("solo")
	d = &serverDraft{Name: "solo", Host: "h", User: "u", Port: 22, Password: "new"}
	if _, ok := submitServer(st, cur, d)().(actionDoneMsg); !ok {
		t.Fatal("edit with new password must succeed")
	}
	got, _ = st.GetServerByName("solo")
	if hasTag(got, "needs-passphrase") || len(got.Tags) != 0 {
		t.Fatalf("tag-only list must store empty: %+v", got.Tags)
	}
}

// TestSubmitServer_EditClearCredential (Plan 21 A2): the edit form's 清除凭据
// toggle routes submitServer through ClearServerCredential — and it takes
// priority over the password field (tick + filled password = clear wins; the
// secret fields are ignored on the clear path).
func TestSubmitServer_EditClearCredential(t *testing.T) {
	st := newStore(t)
	cid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("p")})
	_, _ = st.AddServer(&models.Server{
		Name: "t", Host: "h", User: "u", AuthMethod: models.AuthPassword,
		CredentialID: cid, Tags: []string{"needs-passphrase", "gpu"},
	})
	cur, _ := st.GetServerByName("t")

	d := &serverDraft{Name: "t", Host: "h", User: "u", Port: 22, ClearCredential: true}
	if _, ok := submitServer(st, cur, d)().(actionDoneMsg); !ok {
		t.Fatal("clear-credential submit must succeed")
	}
	got, _ := st.GetServerByName("t")
	if got.CredentialID != "" || got.AuthMethod != "" || got.SudoCredentialID != "" {
		t.Fatalf("server must be credential-less: %+v", got)
	}
	if hasTag(got, "needs-passphrase") || len(got.Tags) != 1 || got.Tags[0] != "gpu" {
		t.Fatalf("needs-passphrase must be stripped, gpu kept: %v", got.Tags)
	}
	if c, _ := st.GetCredential(cid); c != nil {
		t.Fatal("exclusively-owned credential row must be dropped")
	}

	// clear ticked + password filled: the clear path runs FIRST — password ignored
	cid2, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("p2")})
	_, _ = st.AddServer(&models.Server{Name: "t2", Host: "h", User: "u", AuthMethod: models.AuthPassword, CredentialID: cid2})
	cur2, _ := st.GetServerByName("t2")
	d2 := &serverDraft{Name: "t2", Host: "h", User: "u", Port: 22, ClearCredential: true, Password: "ignored"}
	if _, ok := submitServer(st, cur2, d2)().(actionDoneMsg); !ok {
		t.Fatal("clear+password submit must succeed via the clear path")
	}
	got2, _ := st.GetServerByName("t2")
	if got2.CredentialID != "" || got2.AuthMethod != "" {
		t.Fatalf("clear must take priority over the password field: %+v", got2)
	}
	if c, _ := st.GetCredential(cid2); c != nil {
		t.Fatal("clear path must drop the old credential row")
	}
}

// inputTitle reads a *huh.Input's title via reflection — huh exposes no
// public getter, but Title(s) stores into Input.title (an Eval[string])
// whose val field holds the literal. String() is safe on unexported
// (read-only) reflect values; only Set*/Interface() would panic.
func inputTitle(f *huh.Input) string {
	return reflect.ValueOf(f).Elem().FieldByName("title").FieldByName("val").String()
}

// TestNewServerFormFieldTitles (Plan 20 C3a regression, hardened in the fix
// round): locks the EXACT titles and order of the extracted constructors.
// Task 5 made credentials optional — the add-mode password title must keep
// saying 可选 (the extraction once regressed it to a stale pre-T5 sample
// string because the plan sketch was written from pre-T5 source). This test
// fails if any title or the structuredFields order changes.
func TestNewServerFormFieldTitles(t *testing.T) {
	d := &serverDraft{}

	want := []string{"硬件", "位置", "角色", "服务", "Caveats（agent 行动前必读）", "备注"}
	fields := structuredFields(d)
	if len(fields) != len(want) {
		t.Fatalf("structuredFields(d) returned %d fields, want %d", len(fields), len(want))
	}
	for i, f := range fields {
		in, ok := f.(*huh.Input)
		if !ok {
			t.Fatalf("structuredFields(d)[%d] is %T, want *huh.Input", i, f)
		}
		if got := inputTitle(in); got != want[i] {
			t.Errorf("structuredFields(d)[%d] title = %q, want %q", i, got, want[i])
		}
	}

	if got := inputTitle(passwordField(d, false)); got != "密码（可选，与密钥二选一）" {
		t.Errorf("passwordField(d, false) title = %q, want %q", got, "密码（可选，与密钥二选一）")
	}
	if got := inputTitle(passwordField(d, true)); got != "密码（留空=保持不变）" {
		t.Errorf("passwordField(d, true) title = %q, want %q", got, "密码（留空=保持不变）")
	}
	if got := inputTitle(sudoPasswordField(d)); got != "sudo 密码（可选）" {
		t.Errorf("sudoPasswordField(d) title = %q, want %q", got, "sudo 密码（可选）")
	}
	if got := inputTitle(keyPathField(d)); got != "私钥路径（可选，与密码互斥；编辑时留空=不变）" {
		t.Errorf("keyPathField(d) title = %q, want %q", got, "私钥路径（可选，与密码互斥；编辑时留空=不变）")
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
