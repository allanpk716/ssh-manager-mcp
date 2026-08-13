package store

import (
	"bytes"
	"path/filepath"
	"runtime"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// TestDefaultStorePath_FixedLocation pins Plan 16 T2's contract: with no
// SSHMGR_STORE env, DefaultStorePath returns the program-fixed vault location
// (Win C:\ProgramData\ssh-manager\store.db; Unix /var/lib/ssh-manager/store.db)
// — NOT a UserConfigDir-based path. This is the spec §3.1/§5.1 program-fixed
// location, replacing the pre-T2 UserConfigDir default.
func TestDefaultStorePath_FixedLocation(t *testing.T) {
	t.Setenv("SSHMGR_STORE", "")
	got, err := DefaultStorePath()
	if err != nil {
		t.Fatalf("DefaultStorePath: %v", err)
	}
	dir := winOrUnix("C:\\ProgramData\\ssh-manager", "/var/lib/ssh-manager")
	want := filepath.Join(dir, "store.db")
	if got != want {
		t.Errorf("DefaultStorePath = %q, want %q", got, want)
	}
}

// TestDefaultStorePath_EnvOverride pins that SSHMGR_STORE (test/migrate env)
// is honored by DefaultStorePath — the escape hatch that keeps tests hermetic
// now that the default path lives under C:\ProgramData\... / /var/lib/...
func TestDefaultStorePath_EnvOverride(t *testing.T) {
	t.Setenv("SSHMGR_STORE", "/tmp/alt.db")
	got, err := DefaultStorePath()
	if err != nil {
		t.Fatalf("DefaultStorePath: %v", err)
	}
	if got != "/tmp/alt.db" {
		t.Errorf("env override lost: got %q want /tmp/alt.db", got)
	}
}

func winOrUnix(w, u string) string {
	if runtime.GOOS == "windows" {
		return w
	}
	return u
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	mk := make([]byte, 32)
	randRead(t, mk)
	s, err := Open(filepath.Join(t.TempDir(), "test.db"), mk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func randRead(t *testing.T, b []byte) {
	t.Helper()
	if _, err := readRand(b); err != nil {
		t.Fatal(err)
	}
}

func TestSetGetCredentialPassword(t *testing.T) {
	s := newTestStore(t)
	id, err := s.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("hunter2")})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetCredential(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != models.CredPassword {
		t.Fatalf("type = %v", got.Type)
	}
	if !bytes.Equal(got.Secret, []byte("hunter2")) {
		t.Fatalf("secret = %q, want hunter2", got.Secret)
	}
}

func TestSetGetCredentialPrivateKeyWithPassphrase(t *testing.T) {
	s := newTestStore(t)
	id, err := s.SetCredential(&models.Credential{
		Type:       models.CredPrivateKey,
		Secret:     []byte("-----BEGIN OPENSSH PRIVATE KEY-----\n...\n-----END-----"),
		Passphrase: []byte("key-pass"),
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetCredential(id)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Passphrase, []byte("key-pass")) {
		t.Fatalf("passphrase = %q, want key-pass", got.Passphrase)
	}
}

func TestGetCredentialMissing(t *testing.T) {
	s := newTestStore(t)
	got, err := s.GetCredential("nonexistent-id")
	if err != nil {
		t.Fatalf("expected nil error for missing id, got %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil credential for missing id, got %+v", got)
	}
}

func TestSetCredentialEncryptsAtRest(t *testing.T) {
	s := newTestStore(t)
	id, err := s.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("hunter2")})
	if err != nil {
		t.Fatal(err)
	}
	var blob []byte
	err = s.db.QueryRow(`SELECT secret_blob FROM credentials WHERE id = ?`, id).Scan(&blob)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(blob, []byte("hunter2")) {
		t.Fatal("secret_blob must not contain plaintext")
	}
}

func TestForeignKeyEnforcement(t *testing.T) {
	s := newTestStore(t)
	_, err := s.db.Exec(`INSERT INTO profile_servers (profile_id, server_id) VALUES ('nope', 'nope')`)
	if err == nil {
		t.Fatal("FK constraint should reject insert with nonexistent foreign keys")
	}
}
