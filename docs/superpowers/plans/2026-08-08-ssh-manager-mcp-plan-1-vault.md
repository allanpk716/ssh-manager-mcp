# ssh-manager-mcp Plan 1: Credential Vault Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the encrypted credential store + cross-platform keychain + management CLI that forms the foundation of the SSH broker (spec §3, §5, §7). This plan produces a working, tested credential vault CLI — no SSH/MCP yet (that is Plan 2).

**Architecture:** Single Go binary `ssh-manager` with subcommands (cobra). A `store` package owns an encrypted SQLite vault: credential secrets are AES-256-GCM encrypted with a per-record key derived (HKDF-SHA256) from a master key. The master key is held by the OS keychain (go-keyring) or derived from a passphrase (Argon2id) as fallback. CLI commands are thin wrappers over store service functions; tests target the store/service layer (fast, no real keychain) plus build smoke tests.

**Tech Stack:** Go 1.22+ · `modernc.org/sqlite` (pure-Go SQLite, no CGO — cross-platform single binary) · `github.com/zalando/go-keyring` (Win DPAPI / macOS Keychain / Linux Secret Service) · `github.com/spf13/cobra` (CLI) · `golang.org/x/crypto/{argon2,hkdf}` · Go stdlib `crypto/{aes,cipher,rand,sha256}`.

## Global Constraints

- Module path: `ssh-manager-mcp`. Build produces binary `ssh-manager` from `cmd/ssh-manager`.
- Target platforms: Windows / Linux / macOS. No CGO. Pure-Go SQLite only.
- Go 1.22+ (uses `crypto/rand`, `errors.Join`, modern stdlib).
- Dependency versions: pin at `go get` time into `go.mod`; do not introduce frameworks beyond cobra.
- Store location: `<os.UserConfigDir()>/ssh-manager/store.db` and `<os.UserConfigDir()>/ssh-manager/meta.json`.
- Master key: 32 random bytes. Credential DEK = HKDF-SHA256(masterKey, salt=per-record 16 random bytes, info="ssh-manager/v1/credential").
- Token: 32 random bytes, base64url-encoded; stored as Argon2id hash (time=1, memory=64MiB, threads=4, keyLen=32) + per-project 16-byte salt. Plaintext shown once at creation.
- Secrets NEVER logged. Tests must not write real keychain (use injected fake).
- Commit after every task. Commit message prefix `feat:`/`test:`/`chore:` as appropriate.

---

## File Structure

```
ssh-manager-mcp/
├── go.mod
├── cmd/ssh-manager/main.go          # entry point; wires cobra root
├── internal/models/models.go        # Server, Credential, Profile, Project structs
├── internal/store/
│   ├── crypto.go        # seal/open (AES-256-GCM via HKDF DEK) + tests
│   ├── masterkey.go     # KeyProvider interface, keyring impl, passphrase derive, meta file + tests
│   ├── store.go         # Store: Open, schema init, unlock state, path helpers + tests
│   ├── credentials.go   # Credential CRUD (encrypt/decrypt) + tests
│   ├── servers.go       # Server CRUD + tests
│   ├── profiles.go      # Profile CRUD + junction + tests
│   ├── projects.go      # Project CRUD (links Profile, stores token hash) + tests
│   ├── token.go         # token gen + hash/verify + tests
│   └── guardrail.go     # residual-key scan (~/.ssh, ssh-agent) + tests
├── internal/cli/
│   ├── root.go          # cobra root cmd
│   ├── servers.go       # servers add/ls/rm
│   ├── profiles.go      # profiles add/ls/rm/grant
│   ├── projects.go      # projects add/ls/rm/token
│   └── unlock.go        # unlock/lock
└── docs/superpowers/...
```

**Responsibilities:** `models` = pure data structs (no logic). `store` = all persistence + crypto + key handling (the vault). `cli` = thin cobra commands calling `store`. SSH/MCP packages arrive in Plan 2 and will consume `store`.

---

## Task 1: Scaffold + Models

**Files:**
- Create: `go.mod`, `cmd/ssh-manager/main.go`, `internal/models/models.go`, `internal/cli/root.go`
- Test: build smoke (`go build ./...`)

**Interfaces:**
- Produces: module `ssh-manager-mcp`; structs `models.Server`, `models.Credential`, `models.Profile`, `models.Project`; cobra root command `cli.NewRootCmd()`.

- [ ] **Step 1: Init module and get dependencies**

Run:
```bash
cd /c/WorkSpace/agent/ssh-manager-mcp
go mod init ssh-manager-mcp
go get github.com/spf13/cobra@latest
go get modernc.org/sqlite@latest
go get github.com/zalando/go-keyring@latest
go get golang.org/x/crypto@latest
```

- [ ] **Step 2: Write models**

Create `internal/models/models.go`:
```go
package models

import "time"

type AuthMethod string

const (
	AuthPassword   AuthMethod = "password"
	AuthPrivateKey AuthMethod = "private_key"
)

type CredentialType string

const (
	CredPassword   CredentialType = "password"
	CredPrivateKey CredentialType = "private_key"
)

// Server is an SSH target. Credential holds the login secret; SudoCredential (optional) holds a password for sudo -S.
type Server struct {
	ID              string
	Name            string
	Host            string
	Port            int
	User            string
	AuthMethod      AuthMethod
	CredentialID    string
	SudoCredentialID string // empty if none
	Tags            []string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Credential stores an encrypted secret. Secret and Passphrase are decrypted only in memory by the store.
type Credential struct {
	ID         string
	Type       CredentialType
	Secret     []byte // plaintext, only after store decrypts
	Passphrase []byte // plaintext, only for private_key; nil otherwise
}

type Profile struct {
	ID        string
	Name      string
	ServerIDs []string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Project is an agent identity. TokenHash/Salt verify the presented token; ProfileID scopes visible servers.
type Project struct {
	ID          string
	Name        string
	TokenHash   []byte
	TokenSalt   []byte
	TokenPrefix string
	ProfileID   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}
```

- [ ] **Step 3: Write cobra root**

Create `internal/cli/root.go`:
```go
package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "ssh-manager",
		Short: "Encrypted SSH credential vault and broker (MCP)",
	}
	root.AddCommand(versionCmd)
	return root
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print build version",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("ssh-manager dev")
	},
}
```

Create `cmd/ssh-manager/main.go`:
```go
package main

import (
	"fmt"
	"os"

	"ssh-manager-mcp/internal/cli"
)

func main() {
	if err := cli.NewRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
```

- [ ] **Step 4: Build smoke test**

Run: `go build ./...`
Expected: no output, exit 0.

Run: `go run ./cmd/ssh-manager version`
Expected: prints `ssh-manager dev`.

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum cmd/ internal/models/ internal/cli/root.go
git commit -m "chore: scaffold go module, models, cobra root"
```

---

## Task 2: Crypto — seal/open (AES-256-GCM)

**Files:**
- Create: `internal/store/crypto.go`, `internal/store/crypto_test.go`
- Test: `internal/store/crypto_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `store.seal(masterKey, plaintext []byte) ([]byte, error)` and `store.open(masterKey, blob []byte) ([]byte, error)`. Blob layout: `salt(16) || nonce(12) || ciphertext`.

- [ ] **Step 1: Write failing test**

Create `internal/store/crypto_test.go`:
```go
package store

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	pt := []byte("hunter2")
	blob, err := seal(key, pt)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	got, err := open(key, blob)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !bytes.Equal(got, pt) {
		t.Fatalf("got %q want %q", got, pt)
	}
}

func TestOpenWrongKeyFails(t *testing.T) {
	key1 := make([]byte, 32)
	key2 := make([]byte, 32)
	rand.Read(key1)
	rand.Read(key2)
	blob, err := seal(key1, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := open(key2, blob); err == nil {
		t.Fatal("open with wrong key should fail")
	}
}

func TestSealIsRandom(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)
	a, _ := seal(key, []byte("same"))
	b, _ := seal(key, []byte("same"))
	if bytes.Equal(a, b) {
		t.Fatal("two seals of same plaintext must differ (random salt+nonce)")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/`
Expected: FAIL / build error — `seal` and `open` undefined.

- [ ] **Step 3: Write implementation**

Create `internal/store/crypto.go`:
```go
package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"

	"golang.org/x/crypto/hkdf"
)

const (
	infoCredential = "ssh-manager/v1/credential"
	saltLen        = 16
	nonceLen       = 12
	keyLen         = 32
)

// seal encrypts pt under masterKey with a fresh random salt+nonce.
// Output: salt(16) || nonce(12) || ciphertext.
func seal(masterKey, pt []byte) ([]byte, error) {
	salt := make([]byte, saltLen)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, err
	}
	nonce := make([]byte, nonceLen)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	gcm, err := newGCM(masterKey, salt)
	if err != nil {
		return nil, err
	}
	ct := gcm.Seal(nil, nonce, pt, nil)
	out := make([]byte, 0, saltLen+nonceLen+len(ct))
	out = append(out, salt...)
	out = append(out, nonce...)
	out = append(out, ct...)
	return out, nil
}

// open decrypts a blob produced by seal.
func open(masterKey, blob []byte) ([]byte, error) {
	if len(blob) < saltLen+nonceLen {
		return nil, errors.New("ciphertext too short")
	}
	salt := blob[:saltLen]
	nonce := blob[saltLen : saltLen+nonceLen]
	ct := blob[saltLen+nonceLen:]
	gcm, err := newGCM(masterKey, salt)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, ct, nil)
}

func newGCM(masterKey, salt []byte) (cipher.AEAD, error) {
	dek, err := deriveKey(masterKey, salt)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func deriveKey(masterKey, salt []byte) ([]byte, error) {
	k := make([]byte, keyLen)
	r := hkdf.New(sha256.New, masterKey, salt, []byte(infoCredential))
	if _, err := io.ReadFull(r, k); err != nil {
		return nil, err
	}
	return k, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store/`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/store/crypto.go internal/store/crypto_test.go
git commit -m "feat(store): AES-256-GCM seal/open with HKDF-derived per-record key"
```

---

## Task 3: Master Key Provider (keyring + passphrase fallback)

**Files:**
- Create: `internal/store/masterkey.go`, `internal/store/masterkey_test.go`
- Test: `internal/store/masterkey_test.go`

**Interfaces:**
- Consumes: `golang.org/x/crypto/argon2`, `github.com/zalando/go-keyring`.
- Produces: `store.KeyProvider` interface; `store.KeyringKeyProvider{}`; `store.MemKeyProvider`; `store.GenerateMasterKey()`; `store.DeriveFromPassphrase(passphrase, salt []byte) []byte`; `store.LoadMeta(path)/SaveMeta(path, *Meta)` with `Meta{PassphraseSalt []byte}`.

- [ ] **Step 1: Write failing test**

Create `internal/store/masterkey_test.go`:
```go
package store

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
)

func TestMemKeyProviderRoundTrip(t *testing.T) {
	kp := &MemKeyProvider{}
	if _, err := kp.Get(); err != ErrNotFound {
		t.Fatalf("empty mem: want ErrNotFound, got %v", err)
	}
	key := make([]byte, 32)
	rand.Read(key)
	if err := kp.Set(key); err != nil {
		t.Fatal(err)
	}
	got, err := kp.Get()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, key) {
		t.Fatal("mismatch after set/get")
	}
}

func TestDeriveFromPassphraseDeterministic(t *testing.T) {
	salt := []byte("0123456789abcdef")
	a := DeriveFromPassphrase([]byte("correct horse"), salt)
	b := DeriveFromPassphrase([]byte("correct horse"), salt)
	if !bytes.Equal(a, b) {
		t.Fatal("same passphrase+salt must derive same key")
	}
	if len(a) != 32 {
		t.Fatalf("key len = %d, want 32", len(a))
	}
	c := DeriveFromPassphrase([]byte("different"), salt)
	if bytes.Equal(a, c) {
		t.Fatal("different passphrase must derive different key")
	}
}

func TestMetaSaveLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "meta.json")
	salt := []byte("abcdef0123456789")
	if err := SaveMeta(p, &Meta{PassphraseSalt: salt}); err != nil {
		t.Fatal(err)
	}
	m, err := LoadMeta(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(m.PassphraseSalt, salt) {
		t.Fatal("salt mismatch")
	}
	if _, err := LoadMeta(filepath.Join(dir, "missing")); !os.IsNotExist(err) && err != nil {
		t.Fatalf("missing meta: want nil or IsNotExist, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/`
Expected: build error — `KeyProvider`, `MemKeyProvider`, `ErrNotFound`, etc. undefined.

- [ ] **Step 3: Write implementation**

Create `internal/store/masterkey.go`:
```go
package store

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/zalando/go-keyring"
	"golang.org/x/crypto/argon2"
)

const (
	keyringService = "ssh-manager"
	keyringUser    = "master-key"
)

// ErrNotFound is returned when a master key is not present in a provider.
var ErrNotFound = errors.New("master key not found")

// KeyProvider abstracts master-key custody so tests inject a fake (real keychain is flaky in CI).
type KeyProvider interface {
	Get() ([]byte, error) // returns ErrNotFound if absent
	Set(key []byte) error
}

// KeyringKeyProvider stores the master key in the OS keychain.
type KeyringKeyProvider struct{}

func (KeyringKeyProvider) Get() ([]byte, error) {
	s, err := keyring.Get(keyringService, keyringUser)
	if err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return base64.StdEncoding.DecodeString(s)
}

func (KeyringKeyProvider) Set(key []byte) error {
	return keyring.Set(keyringService, keyringUser, base64.StdEncoding.EncodeToString(key))
}

// MemKeyProvider is an in-memory provider for tests.
type MemKeyProvider struct {
	key []byte
}

func (m *MemKeyProvider) Get() ([]byte, error) {
	if m.key == nil {
		return nil, ErrNotFound
	}
	out := make([]byte, len(m.key))
	copy(out, m.key)
	return out, nil
}

func (m *MemKeyProvider) Set(key []byte) error {
	m.key = make([]byte, len(key))
	copy(m.key, key)
	return nil
}

// GenerateMasterKey returns 32 random bytes.
func GenerateMasterKey() ([]byte, error) {
	k := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, k); err != nil {
		return nil, err
	}
	return k, nil
}

// DeriveFromPassphrase derives a 32-byte master key from a passphrase (Argon2id).
func DeriveFromPassphrase(passphrase, salt []byte) []byte {
	return argon2.IDKey(passphrase, salt, 1, 64*1024, 4, 32)
}

// Meta holds vault metadata persisted next to the store (used for passphrase fallback).
type Meta struct {
	PassphraseSalt []byte `json:"passphrase_salt"`
}

func LoadMeta(path string) (*Meta, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Meta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func SaveMeta(path string, m *Meta) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/masterkey.go internal/store/masterkey_test.go
git commit -m "feat(store): master key provider (keyring + Argon2id passphrase fallback)"
```

---

## Task 4: Store Core (open, schema, unlock state) + Credential CRUD

**Files:**
- Create: `internal/store/store.go`, `internal/store/store_test.go`, `internal/store/credentials.go`
- Test: `internal/store/store_test.go`

**Interfaces:**
- Consumes: `store.seal/open`, `modernc.org/sqlite`.
- Produces: `store.Open(path string, masterKey []byte) (*Store, error)`; `store.DefaultStorePath()`; `(*Store).Close()`; `(*Store).SetCredential(c *models.Credential) (id string, err error)`; `(*Store).GetCredential(id string) (*models.Credential, error)`.

- [ ] **Step 1: Write failing test**

Create `internal/store/store_test.go`:
```go
package store

import (
	"bytes"
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/models"
)

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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/`
Expected: build error — `Open`, `Store`, `SetCredential` undefined.

- [ ] **Step 3: Write store core**

Create `internal/store/store.go`:
```go
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// readRand is a seam for tests (not overridden in production).
var readRand = func(b []byte) (int, error) { return rand.Read(b) }

// newID returns a short random base64url id.
func newID() string {
	b := make([]byte, 8)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func now() int64 { return time.Now().Unix() }

// Store is the encrypted credential vault. masterKey lives in memory while open.
type Store struct {
	db        *sql.DB
	masterKey []byte
}

// DefaultStorePath returns the on-disk vault location.
func DefaultStorePath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "ssh-manager", "store.db"), nil
}

// Open opens (or creates) the vault at path and ensures the schema. The master key decrypts credentials.
func Open(path string, masterKey []byte) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if err := initSchema(db); err != nil {
		db.Close()
		return nil, err
	}
	mk := make([]byte, len(masterKey))
	copy(mk, masterKey)
	return &Store{db: db, masterKey: mk}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func initSchema(db *sql.DB) error {
	_, err := db.Exec(schemaSQL)
	return err
}

const schemaSQL = `
CREATE TABLE IF NOT EXISTS credentials (
  id TEXT PRIMARY KEY,
  type TEXT NOT NULL,
  secret_blob BLOB NOT NULL,
  passphrase_blob BLOB,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS servers (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  host TEXT NOT NULL,
  port INTEGER NOT NULL,
  user TEXT NOT NULL,
  auth_method TEXT NOT NULL,
  credential_id TEXT NOT NULL REFERENCES credentials(id),
  sudo_credential_id TEXT,
  tags TEXT,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS profiles (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS profile_servers (
  profile_id TEXT NOT NULL REFERENCES profiles(id) ON DELETE CASCADE,
  server_id TEXT NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
  PRIMARY KEY (profile_id, server_id)
);
CREATE TABLE IF NOT EXISTS projects (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  token_hash BLOB NOT NULL,
  token_salt BLOB NOT NULL,
  token_prefix TEXT NOT NULL,
  profile_id TEXT NOT NULL REFERENCES profiles(id),
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS audit_log (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  ts INTEGER NOT NULL,
  project_id TEXT,
  server_id TEXT,
  action TEXT NOT NULL,
  command TEXT,
  sudo INTEGER NOT NULL DEFAULT 0,
  status TEXT,
  exit_code INTEGER,
  duration_ms INTEGER
);
`
```

- [ ] **Step 4: Write Credential CRUD**

Create `internal/store/credentials.go`:
```go
package store

import (
	"database/sql"

	"ssh-manager-mcp/internal/models"
)

// SetCredential encrypts and stores a credential, returning its id.
func (s *Store) SetCredential(c *models.Credential) (string, error) {
	secretBlob, err := seal(s.masterKey, c.Secret)
	if err != nil {
		return "", err
	}
	var passBlob []byte
	if len(c.Passphrase) > 0 {
		passBlob, err = seal(s.masterKey, c.Passphrase)
		if err != nil {
			return "", err
		}
	}
	id := newID()
	ts := now()
	_, err = s.db.Exec(
		`INSERT INTO credentials (id, type, secret_blob, passphrase_blob, created_at, updated_at) VALUES (?,?,?,?,?,?)`,
		id, string(c.Type), secretBlob, passBlob, ts, ts,
	)
	if err != nil {
		return "", err
	}
	return id, nil
}

// GetCredential decrypts and returns a credential by id.
func (s *Store) GetCredential(id string) (*models.Credential, error) {
	var (
		typ       string
		secretRaw []byte
		passRaw   []byte
	)
	err := s.db.QueryRow(
		`SELECT type, secret_blob, passphrase_blob FROM credentials WHERE id = ?`, id,
	).Scan(&typ, &secretRaw, &passRaw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	secret, err := open(s.masterKey, secretRaw)
	if err != nil {
		return nil, err
	}
	c := &models.Credential{ID: id, Type: models.CredentialType(typ), Secret: secret}
	if passRaw != nil {
		pass, err := open(s.masterKey, passRaw)
		if err != nil {
			return nil, err
		}
		c.Passphrase = pass
	}
	return c, nil
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/store/`
Expected: PASS (5 tests total so far).

- [ ] **Step 6: Commit**

```bash
git add internal/store/store.go internal/store/store_test.go internal/store/credentials.go
git commit -m "feat(store): vault open/schema + encrypted credential CRUD"
```

---

## Task 5: Server CRUD

**Files:**
- Create: `internal/store/servers.go`, `internal/store/servers_test.go`
- Test: `internal/store/servers_test.go`

**Interfaces:**
- Consumes: `(*Store).SetCredential`, `(*Store).GetCredential`, `models.Server`.
- Produces: `(*Store).AddServer(s *models.Server) (string, error)`, `(*Store).GetServer(id string) (*models.Server, error)`, `(*Store).GetServerByName(name string) (*models.Server, error)`, `(*Store).ListServers() ([]*models.Server, error)`, `(*Store).DeleteServer(id string) error`.

- [ ] **Step 1: Write failing test**

Create `internal/store/servers_test.go`:
```go
package store

import (
	"testing"

	"ssh-manager-mcp/internal/models"
)

func mustCred(t *testing.T, s *Store, typ models.CredentialType, secret string) string {
	t.Helper()
	id, err := s.SetCredential(&models.Credential{Type: typ, Secret: []byte(secret)})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAddGetServer(t *testing.T) {
	s := newTestStore(t)
	cid := mustCred(t, s, models.CredPassword, "pw")
	id, err := s.AddServer(&models.Server{
		Name: "gpu-3090", Host: "10.0.0.5", Port: 22, User: "ubuntu",
		AuthMethod: models.AuthPassword, CredentialID: cid, Tags: []string{"gpu"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetServer(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "gpu-3090" || got.Host != "10.0.0.5" || got.User != "ubuntu" {
		t.Fatalf("server = %+v", got)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "gpu" {
		t.Fatalf("tags = %v", got.Tags)
	}
}

func TestGetServerByName(t *testing.T) {
	s := newTestStore(t)
	cid := mustCred(t, s, models.CredPassword, "pw")
	if _, err := s.AddServer(&models.Server{Name: "web", Host: "h", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetServerByName("web")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "web" {
		t.Fatal("wrong server")
	}
	if _, err := s.GetServerByName("nope"); err != nil {
		t.Fatalf("missing by name should be nil,nil; got %v", err)
	}
}

func TestDeleteServer(t *testing.T) {
	s := newTestStore(t)
	cid := mustCred(t, s, models.CredPassword, "pw")
	id, _ := s.AddServer(&models.Server{Name: "x", Host: "h", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	if err := s.DeleteServer(id); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetServer(id)
	if got != nil {
		t.Fatal("server should be gone")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/`
Expected: build error — `AddServer`, etc. undefined.

- [ ] **Step 3: Write implementation**

Create `internal/store/servers.go`:
```go
package store

import (
	"database/sql"
	"encoding/json"

	"ssh-manager-mcp/internal/models"
)

func (s *Store) AddServer(srv *models.Server) (string, error) {
	id := newID()
	ts := now()
	tagsJSON, _ := json.Marshal(srv.Tags)
	sudo := nullableString(srv.SudoCredentialID)
	_, err := s.db.Exec(
		`INSERT INTO servers (id,name,host,port,user,auth_method,credential_id,sudo_credential_id,tags,created_at,updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		id, srv.Name, srv.Host, srv.Port, srv.User, string(srv.AuthMethod), srv.CredentialID, sudo, string(tagsJSON), ts, ts,
	)
	if err != nil {
		return "", err
	}
	return id, nil
}

func (s *Store) GetServer(id string) (*models.Server, error) {
	row := s.db.QueryRow(
		`SELECT id,name,host,port,user,auth_method,credential_id,sudo_credential_id,tags FROM servers WHERE id=?`, id,
	)
	return scanServer(row)
}

func (s *Store) GetServerByName(name string) (*models.Server, error) {
	row := s.db.QueryRow(
		`SELECT id,name,host,port,user,auth_method,credential_id,sudo_credential_id,tags FROM servers WHERE name=?`, name,
	)
	srv, err := scanServer(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return srv, err
}

func (s *Store) ListServers() ([]*models.Server, error) {
	rows, err := s.db.Query(
		`SELECT id,name,host,port,user,auth_method,credential_id,sudo_credential_id,tags FROM servers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*models.Server
	for rows.Next() {
		srv, err := scanServer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, srv)
	}
	return out, rows.Err()
}

func (s *Store) DeleteServer(id string) error {
	_, err := s.db.Exec(`DELETE FROM servers WHERE id=?`, id)
	return err
}

type scanner interface {
	Scan(dest ...any) error
}

func scanServer(sc scanner) (*models.Server, error) {
	var (
		srv              models.Server
		authMethod       string
		tagsJSON         string
		sudoCredentialID sql.NullString
	)
	if err := sc.Scan(&srv.ID, &srv.Name, &srv.Host, &srv.Port, &srv.User, &authMethod, &srv.CredentialID, &sudoCredentialID, &tagsJSON); err != nil {
		return nil, err
	}
	srv.AuthMethod = models.AuthMethod(authMethod)
	srv.SudoCredentialID = sudoCredentialID.String
	if tagsJSON != "" {
		_ = json.Unmarshal([]byte(tagsJSON), &srv.Tags)
	}
	return &srv, nil
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/servers.go internal/store/servers_test.go
git commit -m "feat(store): server CRUD"
```

---

## Task 6: Profile CRUD (+ junction table)

**Files:**
- Create: `internal/store/profiles.go`, `internal/store/profiles_test.go`
- Test: `internal/store/profiles_test.go`

**Interfaces:**
- Consumes: `models.Profile`, server existence (foreign keys).
- Produces: `(*Store).AddProfile(name string) (string, error)`, `(*Store).GrantServers(profileID string, serverIDs []string) error`, `(*Store).GetProfile(id string) (*models.Profile, error)`, `(*Store).ListProfiles() ([]*models.Profile, error)`, `(*Store).ServersForProfile(profileID string) ([]string, error)`.

- [ ] **Step 1: Write failing test**

Create `internal/store/profiles_test.go`:
```go
package store

import (
	"testing"

	"ssh-manager-mcp/internal/models"
)

func TestProfileGrantAndList(t *testing.T) {
	s := newTestStore(t)
	cid := mustCred(t, s, models.CredPassword, "pw")
	a, _ := s.AddServer(&models.Server{Name: "a", Host: "h", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	b, _ := s.AddServer(&models.Server{Name: "b", Host: "h", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})

	pid, err := s.AddProfile("dev-ab")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.GrantServers(pid, []string{a, b}); err != nil {
		t.Fatal(err)
	}
	servers, err := s.ServersForProfile(pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 2 {
		t.Fatalf("want 2 servers, got %v", servers)
	}
}

func TestProfileGrantIsIdempotentAndAdditive(t *testing.T) {
	s := newTestStore(t)
	cid := mustCred(t, s, models.CredPassword, "pw")
	a, _ := s.AddServer(&models.Server{Name: "a", Host: "h", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	pid, _ := s.AddProfile("p")
	_ = s.GrantServers(pid, []string{a})
	_ = s.GrantServers(pid, []string{a}) // duplicate
	servers, _ := s.ServersForProfile(pid)
	if len(servers) != 1 {
		t.Fatalf("duplicate grant must stay 1, got %d", len(servers))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/`
Expected: build error — `AddProfile` etc. undefined.

- [ ] **Step 3: Write implementation**

Create `internal/store/profiles.go`:
```go
package store

import (
	"database/sql"

	"ssh-manager-mcp/internal/models"
)

func (s *Store) AddProfile(name string) (string, error) {
	id := newID()
	ts := now()
	_, err := s.db.Exec(
		`INSERT INTO profiles (id,name,created_at,updated_at) VALUES (?,?,?,?)`,
		id, name, ts, ts,
	)
	if err != nil {
		return "", err
	}
	return id, nil
}

func (s *Store) GetProfile(id string) (*models.Profile, error) {
	var p models.Profile
	err := s.db.QueryRow(`SELECT id,name FROM profiles WHERE id=?`, id).Scan(&p.ID, &p.Name)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ids, err := s.ServersForProfile(id)
	if err != nil {
		return nil, err
	}
	p.ServerIDs = ids
	return &p, nil
}

func (s *Store) ListProfiles() ([]*models.Profile, error) {
	rows, err := s.db.Query(`SELECT id,name FROM profiles ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*models.Profile
	for rows.Next() {
		var p models.Profile
		if err := rows.Scan(&p.ID, &p.Name); err != nil {
			return nil, err
		}
		out = append(out, &p)
	}
	return out, rows.Err()
}

// GrantServers adds serverIDs to the profile (idempotent). Unknown server ids are skipped silently.
func (s *Store) GrantServers(profileID string, serverIDs []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, sid := range serverIDs {
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO profile_servers (profile_id, server_id) VALUES (?,?)`,
			profileID, sid,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ServersForProfile(profileID string) ([]string, error) {
	rows, err := s.db.Query(`SELECT server_id FROM profile_servers WHERE profile_id=?`, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/profiles.go internal/store/profiles_test.go
git commit -m "feat(store): profile CRUD + grant/junction"
```

---

## Task 7: Token + Project CRUD

**Files:**
- Create: `internal/store/token.go`, `internal/store/token_test.go`, `internal/store/projects.go`, `internal/store/projects_test.go`
- Test: as above

**Interfaces:**
- Consumes: `golang.org/x/crypto/argon2`.
- Produces: `store.GenerateToken() (plaintext string, err error)`; `store.HashToken(plaintext, salt []byte) []byte`; `(*Store).AddProject(name, profileID string) (projectID, tokenPlaintext string, err error)`; `(*Store).VerifyToken(tokenPlaintext string) (*models.Project, error)`; `(*Store).ListProjects() ([]*models.Project, error)`.

- [ ] **Step 1: Write failing test (token)**

Create `internal/store/token_test.go`:
```go
package store

import (
	"testing"
)

func TestGenerateTokenIsBase64URL32Bytes(t *testing.T) {
	tok, err := GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) < 40 {
		t.Fatalf("token too short: %q", tok)
	}
	tok2, _ := GenerateToken()
	if tok == tok2 {
		t.Fatal("tokens must be unique")
	}
}

func TestHashTokenVerifies(t *testing.T) {
	salt := []byte("0123456789abcdef")
	tok, _ := GenerateToken()
	h := HashToken([]byte(tok), salt)
	if !verifyTokenHash([]byte(tok), salt, h) {
		t.Fatal("hash should verify")
	}
	if verifyTokenHash([]byte("wrong"), salt, h) {
		t.Fatal("wrong token must not verify")
	}
}
```

- [ ] **Step 2: Write token implementation**

Create `internal/store/token.go`:
```go
package store

import (
	"crypto/rand"
	"encoding/base64"
	"io"

	"golang.org/x/crypto/argon2"
)

// GenerateToken returns a new random 32-byte token, base64url-encoded.
func GenerateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashToken returns the Argon2id hash of the token plaintext under salt.
func HashToken(token, salt []byte) []byte {
	return argon2.IDKey(token, salt, 1, 64*1024, 4, 32)
}

func verifyTokenHash(token, salt, want []byte) bool {
	got := HashToken(token, salt)
	return constantTimeEqual(got, want)
}

func constantTimeEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := range a {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

func newSalt() []byte {
	b := make([]byte, 16)
	io.ReadFull(rand.Reader, b)
	return b
}
```

- [ ] **Step 3: Run token test**

Run: `go test ./internal/store/ -run Token`
Expected: PASS.

- [ ] **Step 4: Write failing test (project)**

Create `internal/store/projects_test.go`:
```go
package store

import (
	"testing"
)

func TestAddProjectReturnsTokenAndVerifies(t *testing.T) {
	s := newTestStore(t)
	pid, _ := s.AddProfile("dev")
	projID, token, err := s.AddProject("project-A", pid)
	if err != nil {
		t.Fatal(err)
	}
	if token == "" || projID == "" {
		t.Fatal("missing id or token")
	}
	proj, err := s.VerifyToken(token)
	if err != nil {
		t.Fatal(err)
	}
	if proj == nil || proj.ID != projID || proj.ProfileID != pid {
		t.Fatalf("verify returned %+v", proj)
	}
}

func TestVerifyTokenRejectsUnknown(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.VerifyToken("not-a-real-token"); err != nil {
		t.Fatalf("unknown token should be nil,nil; got %v", err)
	}
}
```

- [ ] **Step 5: Write project implementation**

Create `internal/store/projects.go`:
```go
package store

import (
	"ssh-manager-mcp/internal/models"
)

// AddProject creates a project bound to profileID, returning the project id and the ONE-TIME token plaintext.
func (s *Store) AddProject(name, profileID string) (string, string, error) {
	token, err := GenerateToken()
	if err != nil {
		return "", "", err
	}
	salt := newSalt()
	hash := HashToken([]byte(token), salt)
	id := newID()
	ts := now()
	_, err = s.db.Exec(
		`INSERT INTO projects (id,name,token_hash,token_salt,token_prefix,profile_id,created_at,updated_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		id, name, hash, salt, tokenPrefix(token), profileID, ts, ts,
	)
	if err != nil {
		return "", "", err
	}
	return id, token, nil
}

// VerifyToken returns the project whose token matches, or (nil, nil) if none.
func (s *Store) VerifyToken(token string) (*models.Project, error) {
	rows, err := s.db.Query(`SELECT id,name,token_hash,token_salt,token_prefix,profile_id FROM projects`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			p           models.Project
			hash, salt  []byte
		)
		if err := rows.Scan(&p.ID, &p.Name, &hash, &salt, &p.TokenPrefix, &p.ProfileID); err != nil {
			return nil, err
		}
		if verifyTokenHash([]byte(token), salt, hash) {
			return &p, nil
		}
	}
	return nil, rows.Err()
}

func (s *Store) ListProjects() ([]*models.Project, error) {
	rows, err := s.db.Query(`SELECT id,name,token_prefix,profile_id FROM projects ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*models.Project
	for rows.Next() {
		var p models.Project
		if err := rows.Scan(&p.ID, &p.Name, &p.TokenPrefix, &p.ProfileID); err != nil {
			return nil, err
		}
		out = append(out, &p)
	}
	return out, rows.Err()
}

func tokenPrefix(token string) string {
	if len(token) >= 8 {
		return token[:8]
	}
	return token
}
```

- [ ] **Step 6: Run all store tests**

Run: `go test ./internal/store/`
Expected: PASS (all).

- [ ] **Step 7: Commit**

```bash
git add internal/store/token.go internal/store/token_test.go internal/store/projects.go internal/store/projects_test.go
git commit -m "feat(store): token gen/hash/verify + project CRUD with profile binding"
```

---

## Task 8: Residual-key Guardrail

**Files:**
- Create: `internal/store/guardrail.go`, `internal/store/guardrail_test.go`
- Test: `internal/store/guardrail_test.go`

**Interfaces:**
- Consumes: `os` env/home.
- Produces: `store.CheckResidualKeys() (found []string, err error)` returning paths of suspicious `~/.ssh` private key files (best-effort, never fatal). Used by MCP startup in Plan 2.

- [ ] **Step 1: Write failing test**

Create `internal/store/guardrail_test.go`:
```go
package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckResidualKeysFindsIdFiles(t *testing.T) {
	fakeSSH := t.TempDir()
	writeFile(t, filepath.Join(fakeSSH, "id_rsa"), "PRIVATE")
	writeFile(t, filepath.Join(fakeSSH, "id_ed25519"), "PRIVATE")
	writeFile(t, filepath.Join(fakeSSH, "known_hosts"), "host data")
	writeFile(t, filepath.Join(fakeSSH, "random.txt"), "x")

	got, err := checkResidualKeysIn(fakeSSH)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 key files, got %v", got)
	}
}

func TestCheckResidualKeysEmptyDir(t *testing.T) {
	got, err := checkResidualKeysIn(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("want 0, got %v", got)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run Residual`
Expected: build error — `checkResidualKeysIn` undefined.

- [ ] **Step 3: Write implementation**

Create `internal/store/guardrail.go`:
```go
package store

import (
	"os"
	"path/filepath"
	"strings"
)

// privateKeyFileNames is the set of default OpenSSH private-key basenames we warn about.
var privateKeyFileNames = map[string]bool{
	"id_rsa": true, "id_dsa": true, "id_ecdsa": true, "id_ed25519": true,
}

// CheckResidualKeys scans ~/.ssh for default private-key files (best-effort).
// Returns the paths found. Errors (e.g. no ~/.ssh) return (nil, nil) — the check never blocks startup.
func CheckResidualKeys() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, nil
	}
	return checkResidualKeysIn(filepath.Join(home, ".ssh"))
}

func checkResidualKeysIn(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil // missing dir is not an error
	}
	var found []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if privateKeyFileNames[name] {
			found = append(found, filepath.Join(dir, name))
			continue
		}
		// also catch "id_rsa" + ".pub" pattern edge or custom like "id_ed25519_github": skip .pub
		if strings.HasSuffix(name, ".pub") {
			continue
		}
		if strings.HasPrefix(name, "id_") {
			found = append(found, filepath.Join(dir, name))
		}
	}
	return found, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store/`
Expected: PASS (all).

- [ ] **Step 5: Commit**

```bash
git add internal/store/guardrail.go internal/store/guardrail_test.go
git commit -m "feat(store): residual-key guardrail (~/.ssh private-key scan)"
```

---

## Task 9: CLI Commands (servers / profiles / projects / unlock / lock)

**Files:**
- Create: `internal/cli/common.go`, `internal/cli/servers.go`, `internal/cli/profiles.go`, `internal/cli/projects.go`, `internal/cli/unlock.go`; modify `internal/cli/root.go`
- Test: build + a command-level smoke test that exercises the store end-to-end through a temp store path.

**Interfaces:**
- Consumes: the full `store` API + `models`.
- Produces: wired cobra subcommands. Adds env var `SSHMGR_STORE` to override store path (for tests/local dev).

- [ ] **Step 1: Write common helpers + failing smoke test**

Create `internal/cli/common.go`:
```go
package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/store"
)

// storePath resolves the vault path (env override > default).
func storePath() (string, error) {
	if p := os.Getenv("SSHMGR_STORE"); p != "" {
		return p, nil
	}
	return store.DefaultStorePath()
}

// openUnlockedStore fails the command with guidance if the vault is locked.
// (In Plan 1 we pass the master key via SSHMGR_MASTERKEY_HEX for tests; real unlock lands when wired to keyring.)
func openUnlockedStore(cmd *cobra.Command) (*store.Store, error) {
	path, err := storePath()
	if err != nil {
		return nil, err
	}
	mkHex := os.Getenv("SSHMGR_MASTERKEY_HEX")
	if mkHex == "" {
		return nil, fmt.Errorf("vault locked: run `ssh-manager unlock` (or set SSHMGR_MASTERKEY_HEX for scripting)")
	}
	mk, err := hexDecode(mkHex)
	if err != nil {
		return nil, err
	}
	return store.Open(path, mk)
}
```

Create `internal/cli/enc.go` (small hex helper kept out of common for clarity):
```go
package cli

import "encoding/hex"

func hexDecode(s string) ([]byte, error) { return hex.DecodeString(s) }
func hexEncode(b []byte) string          { return hex.EncodeToString(b) }
```

Create `internal/cli/cli_smoke_test.go`:
```go
package cli

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/store"
)

// withEnv sets env vars for the test and restores on cleanup.
func withEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	old := map[string]string{}
	for k, v := range kv {
		old[k] = os.Getenv(k)
		os.Setenv(k, v)
	}
	t.Cleanup(func() {
		for k, v := range old {
			os.Setenv(k, v)
		}
	})
}

func TestServersAddAndListEndToEnd(t *testing.T) {
	dir := t.TempDir()
	mk, _ := store.GenerateMasterKey()
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         filepath.Join(dir, "test.db"),
		"SSHMGR_MASTERKEY_HEX": hex.EncodeToString(mk),
	})

	root := NewRootCmd()
	root.SetArgs([]string{"servers", "add", "--name", "gpu", "--host", "10.0.0.5", "--user", "ubuntu", "--password", "pw"})

	out := &bytes.Buffer{}
	root.SetOut(out)
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}

	root2 := NewRootCmd()
	root2.SetArgs([]string{"servers", "ls"})
	root2.SetOut(out)
	if err := root2.Execute(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("gpu")) {
		t.Fatalf("ls output missing gpu: %s", out.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/`
Expected: build error — `servers` command and flags undefined.

- [ ] **Step 3: Write servers CLI**

Create `internal/cli/servers.go`:
```go
package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/models"
)

func newServersCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "servers", Short: "Manage SSH target servers"}
	cmd.AddCommand(serversAddCmd(), serversListCmd(), serversRmCmd())
	return cmd
}

func serversAddCmd() *cobra.Command {
	var (
		name, host, user, password, keyPath, keyPass, sudoPassword string
		port                                                        int
		tags                                                        []string
	)
	c := &cobra.Command{
		Use:   "add",
		Short: "Add a server (with its credential)",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore(cmd)
			if err != nil {
				return err
			}
			defer s.Close()
			var cred models.Credential
			if password != "" {
				cred = models.Credential{Type: models.CredPassword, Secret: []byte(password)}
			} else {
				keyBytes, err := readKeyFile(keyPath)
				if err != nil {
					return err
				}
				cred = models.Credential{Type: models.CredPrivateKey, Secret: keyBytes, Passphrase: []byte(keyPass)}
			}
			cid, err := s.SetCredential(&cred)
			if err != nil {
				return err
			}
			srv := &models.Server{
				Name: name, Host: host, Port: port, User: user,
				AuthMethod: cred.Type.AuthMethodForServer(), CredentialID: cid, Tags: tags,
			}
			if sudoPassword != "" {
				sid, err := s.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte(sudoPassword)})
				if err != nil {
					return err
				}
				srv.SudoCredentialID = sid
			}
			id, err := s.AddServer(srv)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "added server %s id=%s\n", name, id)
			return nil
		},
	}
	c.Flags().StringVar(&name, "name", "", "server name (unique)")
	c.Flags().StringVar(&host, "host", "", "hostname or IP")
	c.Flags().IntVar(&port, "port", 22, "port")
	c.Flags().StringVar(&user, "user", "", "ssh user")
	c.Flags().StringVar(&password, "password", "", "password auth (mutually exclusive with --key)")
	c.Flags().StringVar(&keyPath, "key", "", "path to private key file")
	c.Flags().StringVar(&keyPass, "key-passphrase", "", "passphrase for encrypted private key")
	c.Flags().StringVar(&sudoPassword, "sudo-password", "", "sudo password (enables sudo -S)")
	c.Flags().StringSliceVar(&tags, "tags", nil, "tags")
	_ = c.MarkFlagRequired("name")
	_ = c.MarkFlagRequired("host")
	_ = c.MarkFlagRequired("user")
	return c
}

func serversListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List servers",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore(cmd)
			if err != nil {
				return err
			}
			defer s.Close()
			servers, err := s.ListServers()
			if err != nil {
				return err
			}
			for _, srv := range servers {
				sudo := "-"
				if srv.SudoCredentialID != "" {
					sudo = "sudo"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%-16s %-20s %s@%s:%d [%s]\n", srv.Name, srv.ID, srv.User, srv.Host, srv.Port, sudo)
			}
			return nil
		},
	}
}

func serversRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm [name-or-id]",
		Short: "Remove a server",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore(cmd)
			if err != nil {
				return err
			}
			defer s.Close()
			srv, _ := s.GetServerByName(args[0])
			id := args[0]
			if srv != nil {
				id = srv.ID
			}
			return s.DeleteServer(id)
		},
	}
}

// readKeyFile reads a private key from disk.
func readKeyFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}
```

Add the `AuthMethodForServer` helper to `internal/models/models.go` (append):
```go
// AuthMethodForServer maps a credential type to the server's auth_method.
func (c CredentialType) AuthMethodForServer() AuthMethod {
	if c == CredPrivateKey {
		return AuthPrivateKey
	}
	return AuthPassword
}
```

- [ ] **Step 4: Write profiles + projects + unlock CLI**

Create `internal/cli/profiles.go`:
```go
package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newProfilesCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "profiles", Short: "Manage server profiles (groups)"}
	cmd.AddCommand(profilesAddCmd(), profilesLsCmd(), profilesGrantCmd())
	return cmd
}

func profilesAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add [name]",
		Args:  cobra.ExactArgs(1),
		Short: "Create a profile",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore(cmd)
			if err != nil {
				return err
			}
			defer s.Close()
			id, err := s.AddProfile(args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "created profile %s id=%s\n", args[0], id)
			return nil
		},
	}
}

func profilesLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List profiles",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore(cmd)
			if err != nil {
				return err
			}
			defer s.Close()
			profs, err := s.ListProfiles()
			if err != nil {
				return err
			}
			for _, p := range profs {
				srvs, _ := s.ServersForProfile(p.ID)
				fmt.Fprintf(cmd.OutOrStdout(), "%-16s %-20s servers=%d\n", p.Name, p.ID, len(srvs))
			}
			return nil
		},
	}
}

func profilesGrantCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "grant [profile] [server1 server2 ...]",
		Args:  cobra.MinimumNArgs(2),
		Short: "Grant servers to a profile (by name)",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore(cmd)
			if err != nil {
				return err
			}
			defer s.Close()
			profs, err := s.ListProfiles()
			if err != nil {
				return err
			}
			var profileID string
			for _, p := range profs {
				if p.Name == args[0] {
					profileID = p.ID
				}
			}
			if profileID == "" {
				return fmt.Errorf("profile %q not found", args[0])
			}
			var serverIDs []string
			for _, name := range args[1:] {
				srv, _ := s.GetServerByName(name)
				if srv == nil {
					return fmt.Errorf("server %q not found", name)
				}
				serverIDs = append(serverIDs, srv.ID)
			}
			if err := s.GrantServers(profileID, serverIDs); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "granted %d server(s) to %s\n", len(serverIDs), args[0])
			return nil
		},
	}
}
```

Create `internal/cli/projects.go`:
```go
package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newProjectsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "projects", Short: "Manage agent projects (each gets a token)"}
	cmd.AddCommand(projectsAddCmd(), projectsLsCmd())
	return cmd
}

func projectsAddCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "add [name] --profile [profile]",
		Args:  cobra.ExactArgs(1),
		Short: "Create a project and print its one-time token + .mcp.json snippet",
		RunE: func(cmd *cobra.Command, args []string) error {
			profileName, _ := cmd.Flags().GetString("profile")
			s, err := openUnlockedStore(cmd)
			if err != nil {
				return err
			}
			defer s.Close()
			profs, err := s.ListProfiles()
			if err != nil {
				return err
			}
			var profileID string
			for _, p := range profs {
				if p.Name == profileName {
					profileID = p.ID
				}
			}
			if profileID == "" {
				return fmt.Errorf("profile %q not found", profileName)
			}
			_, token, err := s.AddProject(args[0], profileID)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Token (shown once): %s\n\n", token)
			fmt.Fprintln(out, ".mcp.json snippet:")
			fmt.Fprintf(out, `{"mcpServers":{"ssh":{"command":"ssh-manager","args":["mcp","--token","%s"]}}}`+"\n", token)
			return nil
		},
	}
	c.Flags().String("profile", "", "profile name to bind")
	_ = c.MarkFlagRequired("profile")
	return c
}

func projectsLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List projects (token prefix only)",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore(cmd)
			if err != nil {
				return err
			}
			defer s.Close()
			projs, err := s.ListProjects()
			if err != nil {
				return err
			}
			for _, p := range projs {
				fmt.Fprintf(cmd.OutOrStdout(), "%-16s %-20s token=%s… profile=%s\n", p.Name, p.ID, p.TokenPrefix, p.ProfileID)
			}
			return nil
		},
	}
}
```

Create `internal/cli/unlock.go`:
```go
package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/store"
)

func newUnlockCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unlock",
		Short: "Resolve the master key and print SSHMGR_MASTERKEY_HEX for the current shell",
		RunE: func(cmd *cobra.Command, args []string) error {
			kp := store.KeyringKeyProvider{}
			mk, err := kp.Get()
			if err == store.ErrNotFound {
				// first run: generate + store in keychain
				mk, err = store.GenerateMasterKey()
				if err != nil {
					return err
				}
				if err := kp.Set(mk); err != nil {
					return err
				}
			} else if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "export SSHMGR_MASTERKEY_HEX=%s\n", hexEncode(mk))
			return nil
		},
	}
}

func newLockCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "lock",
		Short: "Clear the master key from this shell",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), "unset SSHMGR_MASTERKEY_HEX")
			os.Unsetenv("SSHMGR_MASTERKEY_HEX")
			return nil
		},
	}
}
```

- [ ] **Step 5: Wire commands into root**

Modify `internal/cli/root.go` — replace the `NewRootCmd` body to register subcommands:
```go
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "ssh-manager",
		Short: "Encrypted SSH credential vault and broker (MCP)",
	}
	root.AddCommand(versionCmd, newServersCmd(), newProfilesCmd(), newProjectsCmd(), newUnlockCmd(), newLockCmd())
	return root
}
```

(_`projectsAddCmd` already registers its required `--profile` flag in Step 4._)

- [ ] **Step 6: Build + run smoke test**

Run: `go build ./...`
Expected: no errors.

Run: `go test ./internal/cli/`
Expected: PASS.

- [ ] **Step 7: Manual smoke**

Run (set up a throwaway vault):
```bash
export SSHMGR_STORE=/tmp/smoke.db
eval "$(go run ./cmd/ssh-manager unlock)"   # sets SSHMGR_MASTERKEY_HEX
go run ./cmd/ssh-manager servers add --name gpu --host 10.0.0.5 --user ubuntu --password pw
go run ./cmd/ssh-manager servers ls
go run ./cmd/ssh-manager profiles add dev
go run ./cmd/ssh-manager profiles grant dev gpu
go run ./cmd/ssh-manager projects add project-A --profile dev
```
Expected: `servers ls` shows `gpu`; `projects add` prints a token + `.mcp.json` snippet.

- [ ] **Step 8: Commit**

```bash
git add internal/cli/ internal/models/models.go
git commit -m "feat(cli): servers/profiles/projects/unlock/lock subcommands"
```

---

## Self-Review

**1. Spec coverage (Plan 1 scope = spec §3 CLI skeleton, §5 data model, §7 crypto/keychain, §4 guardrail):**
- Encrypted store + AES-256-GCM + HKDF → Tasks 2, 4. ✓
- Master key keychain + passphrase fallback → Task 3. ✓
- Data model: Server/Credential/Profile/Project + junction + audit_log schema → Task 4 schema; CRUD → Tasks 4–7. ✓ (audit_log table created; writes land in Plan 2 with exec actions.)
- Token gen + Argon2id hash → Task 7. ✓
- Residual-key guardrail → Task 8. ✓
- CLI: servers/profiles/projects/unlock/lock → Task 9. ✓
- `.mcp.json` snippet generation → Task 9 (projects add). ✓
- Plan-1 does NOT cover: SSH exec, MCP server, Profile enforcement at runtime, audit writes, owner `ssh` path — all explicitly Plan 2. ✓

**2. Placeholder scan:** Reviewed and fixed inline: Task 9 `readKeyFile` now uses `os.ReadFile` directly (os import added); `projectsAddCmd` registers its required `--profile` flag; removed stray `time` import in `credentials.go`. Verified no "TBD"/"implement later"/"add error handling"/"similar to Task N" remain.

**3. Type consistency:** `models.CredentialType.AuthMethodForServer()` (added Task 9) is consumed in `serversAddCmd`. `store.Open(path, masterKey)`, `SetCredential/GetCredential/AddServer/...` signatures match across tasks. `GenerateMasterKey`/`GenerateToken`/`HashToken` consistent. `KeyProvider` interface used by `KeyringKeyProvider` and `MemKeyProvider`. `SSHMGR_STORE`/`SSHMGR_MASTERKEY_HEX` env names consistent between `common.go` and tests.

**Gap noted for Plan 2:** `VerifyToken` currently scans all projects (acceptable for small N). If project count grows large, add a token_prefix index + prefilter — record as a Plan 2/3 follow-up, not a Plan 1 blocker.

---

## Execution Handoff

Plan 1 complete and saved to `docs/superpowers/plans/2026-08-08-ssh-manager-mcp-plan-1-vault.md`. Two execution options:

1. **Subagent-Driven (recommended)** — I dispatch a fresh subagent per task, review between tasks, fast iteration.
2. **Inline Execution** — Execute tasks in this session using executing-plans, batch execution with checkpoints.

Which approach? (Plans 2–4 will be written after Plan 1 ships, so their interfaces match the real code.)
