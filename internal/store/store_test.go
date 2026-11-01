package store

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// TestDefaultStorePath_UnderVaultRoot pins Plan 16 T2's contract: with no
// SSHMGR_STORE env, DefaultStorePath uses the vault directory derived from
// SSHMGR_VAULT_ROOT — NOT a user-configuration-directory path.
func TestDefaultStorePath_UnderVaultRoot(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SSHMGR_VAULT_ROOT", root)
	t.Setenv("SSHMGR_STORE", "")
	got, err := DefaultStorePath()
	if err != nil {
		t.Fatalf("DefaultStorePath: %v", err)
	}
	want := filepath.Join(root, "ssh-manager", "store.db")
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

// TestNewIDNeverStartsWithDash 钉住命令行安全规则：新生成的存储标识必须能
// 作为位置参数传入，不能让命令行把它误认成参数开关。
func TestNewIDNeverStartsWithDash(t *testing.T) {
	const count = 4096
	for i := 0; i < count; i++ {
		if id := newID(); strings.HasPrefix(id, "-") {
			t.Fatalf("newID() = %q at attempt %d; generated ids must not start with -", id, i)
		}
	}
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
