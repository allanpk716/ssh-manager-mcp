package tui

import (
	"testing"

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

func TestDraftToServer_Add(t *testing.T) {
	st := newStore(t)
	d := &serverDraft{Name: "gpu", Host: "192.0.2.10", User: "u", Port: 22, Password: "pw", Hardware: "2x3090"}
	srv, err := d.toServer(st)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddServer(srv); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetServerByName("gpu")
	if got == nil || got.Host != "192.0.2.10" || got.AuthMethod != models.AuthPassword {
		t.Fatalf("roundtrip: %+v", got)
	}
}

func TestDraftToServer_PasswordKeyMutex(t *testing.T) {
	st := newStore(t)
	d := &serverDraft{Name: "x", Host: "h", User: "u", Password: "p", KeyPath: "k"}
	if _, err := d.toServer(st); err == nil {
		t.Fatal("password+key must be rejected (CLI parity)")
	}
}

func TestSubmitServer_AddWithoutCredentialRejected(t *testing.T) {
	st := newStore(t)
	d := &serverDraft{Name: "x", Host: "h", User: "u", Port: 22} // no password, no key
	cmd := submitServer(st, nil, d)
	msg := cmd()
	if e, ok := msg.(errMsg); !ok || e.err == nil {
		t.Fatalf("add without credential must error, got %T %+v", msg, msg)
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
