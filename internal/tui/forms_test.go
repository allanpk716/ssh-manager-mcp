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
