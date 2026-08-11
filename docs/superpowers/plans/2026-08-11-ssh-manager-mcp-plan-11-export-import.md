# ssh-manager-mcp Plan 11 — vault `export` / `import` (portable encrypted backup)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `ssh-manager export` and `ssh-manager import` — a portable, passphrase-encrypted, full round-trip of the vault (servers + decrypted credentials + profiles + grants + projects + host_keys + audit). The export file is self-contained and master-key-independent, so it works across machines with different master keys (backup, migration, disaster recovery).

**Architecture:** `export` reads the whole vault into a versioned DTO (`store.Snapshot`), **decrypting** each credential under the source vault's master key (so the DTO holds plaintext, master-key-independent), JSON-marshals the DTO, and encrypts the JSON with a key derived from a user **passphrase** (Argon2id → AES-256-GCM) via a new `internal/vaultio` envelope. `import` reverses it: passphrase-decrypt → unmarshal → write rows into the target vault in FK order inside one transaction, **re-sealing** each credential under the TARGET vault's master key. The format is the keystone for later phases (Synology auto-backup Plan 13, client cache) — they reuse `store.Snapshot` + `vaultio`.

**Tech Stack:** Go 1.24; `golang.org/x/crypto/argon2` (already a transitive dep via the vault's passphrase mode) for Argon2id; stdlib `crypto/aes`/`crypto/cipher`/`crypto/rand`/`encoding/json`; existing `cobra` CLI. **No new external dependencies.**

## Global Constraints

- **Agent surface unchanged.** `internal/mcpserver/*`, `internal/sshbroker/*`, `NewServer`, the broker tools, the iron rule, the stdio `mcp` command, and `serve` are NOT modified. This plan is **owner-CLI + store-layer + a new `vaultio` package** only.
- **No new external dependencies.** Argon2id via `golang.org/x/crypto/argon2` (already used by `internal/store/masterkey.go` DeriveFromPassphrase); AES-GCM + JSON via stdlib.
- **Round-trip is the load-bearing test (Plan 11's correctness bar):** export from store A (master key mk1) → import into an EMPTY store B (a DIFFERENT master key mk2) → every table's rows match A, AND a project's original plaintext token (from A's `AddProject`) still validates on B (`st.VerifyToken(token)` returns the project). This proves master-key-independence + token-hash preservation.
- **Export file = KeePass model.** Plaintext credentials ride inside a passphrase-encrypted file → **offline brute-force surface** (file + Argon2id salt are crackable if the file leaks). Mitigation = strong passphrase (documented); the file is no stronger than its passphrase. Same trade-off as a KeePass database; document it honestly.
- **Master-key-independence:** the export file's passphrase is the ONLY secret needed to restore. The source vault must be **unlocked** to export (decrypts creds); the target vault must be **unlocked** to import (re-seals creds under its own master key). The two vaults' master keys are independent and never leave their hosts.
- **Empty-target guard:** `import` refuses a non-empty target (`ErrVaultNotEmpty`) — no silent clobber. To restore over an existing vault, the operator deletes `store.db` first (getting a fresh empty vault). No `--force` in MVP (keeps it safe).
- **Token preservation:** `projects.token_hash/token_salt/token_prefix` are carried verbatim (raw SQL — the existing `ListProjects`/`GetProject` omit the hash). The original plaintext token is never stored and never recovers; it keeps validating after import because the hash row is preserved.
- **NULL normalization (benign):** nullable TEXT columns (`servers.sudo_credential_id`, `servers.tags`, `audit_log.command/project_id/server_id/status`, `credentials.passphrase_blob`) are read via `COALESCE(...,'')` and written as their value (empty string for formerly-NULL). For this schema empty-string is functionally equivalent to NULL (sudo_credential_id '' matches no UUID → no sudo; empty tags → none). Documented.
- **Timestamps:** DTO timestamp fields are `int64` (the raw DB INTEGER value), read and written verbatim — sidesteps the seconds-vs-millis question, preserves exactly.
- **Hygiene:** `.gitattributes` LF; `gofmt -l .` empty on touched files; `go vet ./...` clean; one logical commit per task; messages end `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.
- **Branch:** `worktree-export-import` (already created), base master HEAD.
- **Roadmap note:** this is Plan 11 = export/import. The multi-machine roadmap phases formerly labeled "Plan 11–14" (client cache / replication / Synology backup / migration+enroll) are renumbered to 12+ and **reuse the `store.Snapshot` + `vaultio` format defined here** (Synology backup = the same Snapshot encrypted with the server DEK instead of a passphrase; client cache = the same Snapshot pulled read-only).

---

## Scope decisions (surfaced for plan review)

1. **Separate DTO, not `models.*`.** `models` structs lack json tags and use `time.Time` (unit-ambiguous round-trip). The export DTO (`store.Snapshot*`) has explicit json tags + `int64` timestamps + base64 `[]byte` fields (Go json auto-base64s `[]byte`). Exact, versioned, portable.
2. **Raw SQL for Export/Import, not the existing Add*/Get* getters.** `AddServer`/`AddProfile`/`SetCredential`/`AddProject` all **generate new ids** (and `AddProject` regenerates the token) → they would break FK integrity + invalidate the original token on import. Import must insert rows id-preserving in FK order inside one tx. Raw SQL is the clean way. (Read side also needs raw SQL for `credentials` sealed blobs and `projects.token_hash/salt`, which no getter exposes.)
3. **New `internal/vaultio` package owns the file envelope** (`Encrypt`/`Decrypt` passphrase → Argon2id → AES-GCM, layout `magic(8)‖salt(16)‖nonce(12)‖ct`). It uses `golang.org/x/crypto/argon2` **directly** (same params as `store.DeriveFromPassphrase`: time=1, memory=64MiB, parallelism=4, len=32) — it does NOT import `store` (keeps the layer clean: `store` owns data+DTO, `vaultio` owns byte-encryption, `cli` orchestrates). Re-implementing ~15 lines of AES-GCM in `vaultio` is intentional (different key context than the vault's credential sealing) and is **not** duplication of concern.
4. **`store.Snapshot` lives in `internal/store`** (Export/Import are `*Store` methods needing `s.db` + `s.masterKey`). The CLI marshals/unmarshals `store.Snapshot` and hands bytes to `vaultio`.
5. **Passphrase prompt is a testable seam** mirroring `internal/cli/unlock.go`'s `var passphrasePrompt = func() ([]byte, error){…}`. `export` prompts twice with confirm (a typo would lock the backup out); `import` prompts once. Tests swap the seam.
6. **Audit + host_keys ARE included** in the snapshot (whole-vault). Audit could grow large; acceptable for a backup. (A future `--skip-audit` is easy; out of MVP.)
7. **`ListHostKeys` is added** (hostkeys.go only has point lookups today) — needed by Export; reusable later.
8. **`import` writes to the live store** via `vault.OpenStore()` (target must be unlocked). It does NOT rotate/rekey anything — it inserts rows.

---

## File Structure

**New:**
- `internal/store/export.go` — `Snapshot` + `SnapshotCredential/Server/Profile/Grant/Project/HostKey/Audit` DTO types (json-tagged, int64 timestamps); `ListHostKeys()`; `ExportSnapshot() (*Snapshot, error)`; `ImportSnapshot(*Snapshot) error`; `ErrVaultNotEmpty`.
- `internal/store/export_test.go` — DTO round-trip + ListHostKeys + export-captures-all + import-empty-guard + cross-master-key round-trip + original-token-validates.
- `internal/vaultio/vaultio.go` — `Encrypt(passphrase, plaintext []byte) ([]byte, error)`; `Decrypt(passphrase, blob []byte) ([]byte, error)`; `ErrBadMagic`/`ErrBadFormat`.
- `internal/vaultio/vaultio_test.go` — round-trip; wrong-passphrase fails; tampered ciphertext fails; truncated blob fails.
- `internal/cli/export.go` — `newExportCmd()`; `var passphrasePrompt` seam (confirm-on-export).
- `internal/cli/import.go` — `newImportCmd()`.
- `internal/cli/export_import_smoke_test.go` — end-to-end CLI export→import→verify (+ token validates).
- `docs/备份与迁移.md` — operator guide (usage, security model, vs copying `store.db`, scenarios).

**Modified:**
- `internal/cli/root.go` — add `newExportCmd(), newImportCmd()` to the `AddCommand` line.
- `README.md` — cross-link to `docs/备份与迁移.md`.

---

## Task 1: `Snapshot` DTO + `ListHostKeys` + `ExportSnapshot` (read side)

**Goal:** Capture every table into a master-key-independent DTO. No file/crypto yet.

**Files:**
- Create: `internal/store/export.go`, `internal/store/export_test.go`
- Read for patterns: `internal/store/store.go` (schema 207-274), `crypto.go` (`open`/`seal`), `hostkeys.go`.

**Interfaces:**
- Consumes: the unexported `open(s.masterKey, blob)` (`crypto.go:45`) to decrypt credentials.
- Produces: `type Snapshot ...`; `(*Store) ListHostKeys() ([]SnapshotHostKey, error)`; `(*Store) ExportSnapshot() (*Snapshot, error)`.

- [ ] **Step 1: Write the failing tests** (`internal/store/export_test.go`).

```go
package store

import (
	"bytes"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// TestExportSnapshot_CapturesAllTables seeds one of each row kind, exports,
// and asserts the DTO carries every row with DECRYPTED credential plaintext.
func TestExportSnapshot_CapturesAllTables(t *testing.T) {
	s := newTestStore(t) // store_test.go:11 — fresh store w/ random 32-byte master key

	// seed: profile, server (+ its credential), grant, project (hash retained in DB), host key, audit row
	credID, err := s.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("s3cr3t")})
	if err != nil {
		t.Fatal(err)
	}
	// (AddServer via the existing method is fine for SEEDING — it generates ids, which is what we want here)
	srv := &models.Server{Name: "gpu", Host: "192.0.2.10", Port: 22, User: "deploy",
		AuthMethod: models.AuthPassword, CredentialID: credID, Tags: []string{"prod"}}
	srvID, err := s.AddServer(srv)
	if err != nil {
		t.Fatal(err)
	}
	profID, err := s.AddProfile("team-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.GrantServers(profID, []string{srvID}); err != nil {
		t.Fatal(err)
	}
	projID, _, err := s.AddProject("my-agent", profID) // plaintext token discarded here
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveHostKey("192.0.2.10", 22, []byte("fake-host-key-blob")); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteAudit(AuditRow{Action: "exec", ProjectID: projID, ServerID: srvID, Status: "ok"}); err != nil {
		t.Fatal(err)
	}

	snap, err := s.ExportSnapshot()
	if err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	if snap.Version != 1 {
		t.Errorf("Version = %d, want 1", snap.Version)
	}
	if len(snap.Credentials) != 1 || !bytes.Equal(snap.Credentials[0].Secret, []byte("s3cr3t")) {
		t.Errorf("credentials not captured/decrypted: %+v", snap.Credentials)
	}
	if len(snap.Servers) != 1 || snap.Servers[0].Name != "gpu" {
		t.Errorf("servers not captured: %+v", snap.Servers)
	}
	if len(snap.Profiles) != 1 || snap.Profiles[0].Name != "team-a" {
		t.Errorf("profiles not captured: %+v", snap.Profiles)
	}
	if len(snap.Grants) != 1 || snap.Grants[0].ProfileID != profID || snap.Grants[0].ServerID != srvID {
		t.Errorf("grants not captured: %+v", snap.Grants)
	}
	// CRITICAL: token_hash/salt ARE captured (the whole point — raw SQL, not ListProjects)
	if len(snap.Projects) != 1 || len(snap.Projects[0].TokenHash) == 0 || len(snap.Projects[0].TokenSalt) == 0 {
		t.Errorf("projects hash/salt not captured: %+v", snap.Projects)
	}
	if len(snap.HostKeys) != 1 || snap.HostKeys[0].HostPort != "192.0.2.10:22" {
		t.Errorf("host_keys not captured: %+v", snap.HostKeys)
	}
	if len(snap.Audit) != 1 || snap.Audit[0].Action != "exec" {
		t.Errorf("audit not captured: %+v", snap.Audit)
	}
}
```

- [ ] **Step 2: Run to fail** — `ExportSnapshot`/`ListHostKeys`/DTO types undefined.

Run: `go test ./internal/store/ -run TestExportSnapshot_CapturesAllTables -v`
Expected: FAIL (compile error — undefined symbols).

- [ ] **Step 3: Implement DTO + ListHostKeys + ExportSnapshot** (`internal/store/export.go`).

```go
package store

import (
	"database/sql"
	"fmt"
)

// Snapshot is a portable, master-key-independent capture of the entire vault.
// Credential Secret/Passphrase hold DECRYPTED plaintext; the serialized form
// MUST be encrypted (vaultio.Encrypt) before touching disk. Version = format version.
type Snapshot struct {
	Version     int                  `json:"version"`
	Credentials []SnapshotCredential `json:"credentials"`
	Servers     []SnapshotServer     `json:"servers"`
	Profiles    []SnapshotProfile    `json:"profiles"`
	Grants      []SnapshotGrant      `json:"grants"`
	Projects    []SnapshotProject    `json:"projects"`
	HostKeys    []SnapshotHostKey    `json:"host_keys"`
	Audit       []SnapshotAudit      `json:"audit"`
}

type SnapshotCredential struct {
	ID         string `json:"id"`
	Type       string `json:"type"`        // models.CredentialType string value
	Secret     []byte `json:"secret"`      // DECRYPTED plaintext
	Passphrase []byte `json:"passphrase"`  // DECRYPTED plaintext; nil/empty if none
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`
}

type SnapshotServer struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	Host              string   `json:"host"`
	Port              int      `json:"port"`
	User              string   `json:"user"`
	AuthMethod        string   `json:"auth_method"`
	CredentialID      string   `json:"credential_id"`
	SudoCredentialID  string   `json:"sudo_credential_id"` // "" if none (NULL coalesced)
	TagsRaw           string   `json:"tags"`                // raw DB TEXT (JSON array string) — preserved verbatim
	Description       string   `json:"description"`
	Location          string   `json:"location"`
	Hardware          string   `json:"hardware"`
	Services          string   `json:"services"`
	Role              string   `json:"role"`
	Caveats           string   `json:"caveats"`
	CreatedAt         int64    `json:"created_at"`
	UpdatedAt         int64    `json:"updated_at"`
}

type SnapshotProfile struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

type SnapshotGrant struct {
	ProfileID string `json:"profile_id"`
	ServerID  string `json:"server_id"`
}

type SnapshotProject struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	TokenHash   []byte `json:"token_hash"`   // verbatim — preserves original-token validity
	TokenSalt   []byte `json:"token_salt"`
	TokenPrefix string `json:"token_prefix"`
	ProfileID   string `json:"profile_id"`
	Status      string `json:"status"` // models.ProjectStatus string value
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
}

type SnapshotHostKey struct {
	HostPort  string `json:"host_port"` // "{host}:{port}"
	KeyBlob   []byte `json:"key_blob"`
	CreatedAt int64  `json:"created_at"`
}

type SnapshotAudit struct {
	ID         int64  `json:"id"`
	TS         int64  `json:"ts"`
	ProjectID  string `json:"project_id"`
	ServerID   string `json:"server_id"`
	Action     string `json:"action"`
	Command    string `json:"command"`
	Sudo       bool   `json:"sudo"`
	Status     string `json:"status"`
	ExitCode   int    `json:"exit_code"`
	DurationMS int64  `json:"duration_ms"`
}

// ListHostKeys returns every host_keys row (point lookups already exist; this is the dump path).
func (s *Store) ListHostKeys() ([]SnapshotHostKey, error) {
	rows, err := s.db.Query(`SELECT host_port, key_blob, created_at FROM host_keys ORDER BY host_port`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SnapshotHostKey
	for rows.Next() {
		var h SnapshotHostKey
		if err := rows.Scan(&h.HostPort, &h.KeyBlob, &h.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ExportSnapshot captures every table. Credentials are DECRYPTED under s.masterKey.
// Requires the store to be unlocked. The caller must encrypt the serialized form.
func (s *Store) ExportSnapshot() (*Snapshot, error) {
	snap := &Snapshot{Version: 1}

	// servers (COALESCE the two nullable text cols to '')
	rs, err := s.db.Query(`SELECT id,name,host,port,user,auth_method,credential_id,COALESCE(sudo_credential_id,''),COALESCE(tags,''),description,location,hardware,services,role,caveats,created_at,updated_at FROM servers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	for rs.Next() {
		var sv SnapshotServer
		if err := rs.Scan(&sv.ID, &sv.Name, &sv.Host, &sv.Port, &sv.User, &sv.AuthMethod,
			&sv.CredentialID, &sv.SudoCredentialID, &sv.TagsRaw, &sv.Description, &sv.Location,
			&sv.Hardware, &sv.Services, &sv.Role, &sv.Caveats, &sv.CreatedAt, &sv.UpdatedAt); err != nil {
			rs.Close()
			return nil, err
		}
		snap.Servers = append(snap.Servers, sv)
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return nil, err
	}

	// credentials (decrypt each blob under s.masterKey)
	rc, err := s.db.Query(`SELECT id,type,secret_blob,COALESCE(passphrase_blob,''),created_at,updated_at FROM credentials`)
	if err != nil {
		return nil, err
	}
	for rc.Next() {
		var c SnapshotCredential
		var secretBlob, passBlob []byte
		if err := rc.Scan(&c.ID, &c.Type, &secretBlob, &passBlob, &c.CreatedAt, &c.UpdatedAt); err != nil {
			rc.Close()
			return nil, err
		}
		pt, err := open(s.masterKey, secretBlob)
		if err != nil {
			rc.Close()
			return nil, fmt.Errorf("decrypt credential %s: %w", c.ID, err)
		}
		c.Secret = pt
		if len(passBlob) > 0 {
			pp, err := open(s.masterKey, passBlob)
			if err != nil {
				rc.Close()
				return nil, fmt.Errorf("decrypt passphrase %s: %w", c.ID, err)
			}
			c.Passphrase = pp
		}
		snap.Credentials = append(snap.Credentials, c)
	}
	rc.Close()
	if err := rc.Err(); err != nil {
		return nil, err
	}

	// profiles
	rp, err := s.db.Query(`SELECT id,name,created_at,updated_at FROM profiles ORDER BY name`)
	if err != nil {
		return nil, err
	}
	for rp.Next() {
		var p SnapshotProfile
		if err := rp.Scan(&p.ID, &p.Name, &p.CreatedAt, &p.UpdatedAt); err != nil {
			rp.Close()
			return nil, err
		}
		snap.Profiles = append(snap.Profiles, p)
	}
	rp.Close()
	if err := rp.Err(); err != nil {
		return nil, err
	}

	// grants (profile_servers)
	rg, err := s.db.Query(`SELECT profile_id, server_id FROM profile_servers`)
	if err != nil {
		return nil, err
	}
	for rg.Next() {
		var g SnapshotGrant
		if err := rg.Scan(&g.ProfileID, &g.ServerID); err != nil {
			rg.Close()
			return nil, err
		}
		snap.Grants = append(snap.Grants, g)
	}
	rg.Close()
	if err := rg.Err(); err != nil {
		return nil, err
	}

	// projects — RAW SQL for token_hash/salt (ListProjects/GetProject omit them)
	rj, err := s.db.Query(`SELECT id,name,token_hash,token_salt,token_prefix,profile_id,status,created_at,updated_at FROM projects`)
	if err != nil {
		return nil, err
	}
	for rj.Next() {
		var p SnapshotProject
		if err := rj.Scan(&p.ID, &p.Name, &p.TokenHash, &p.TokenSalt, &p.TokenPrefix, &p.ProfileID, &p.Status, &p.CreatedAt, &p.UpdatedAt); err != nil {
			rj.Close()
			return nil, err
		}
		snap.Projects = append(snap.Projects, p)
	}
	rj.Close()
	if err := rj.Err(); err != nil {
		return nil, err
	}

	// host_keys
	snap.HostKeys, err = s.ListHostKeys()
	if err != nil {
		return nil, err
	}

	// audit — RAW SQL (AuditRows clamps limit<=0 to 1; dump all)
	ra, err := s.db.Query(`SELECT id,ts,COALESCE(project_id,''),COALESCE(server_id,''),action,COALESCE(command,''),sudo,COALESCE(status,''),COALESCE(exit_code,0),COALESCE(duration_ms,0) FROM audit_log ORDER BY id`)
	if err != nil {
		return nil, err
	}
	for ra.Next() {
		var a SnapshotAudit
		if err := ra.Scan(&a.ID, &a.TS, &a.ProjectID, &a.ServerID, &a.Action, &a.Command, &a.Sudo, &a.Status, &a.ExitCode, &a.DurationMS); err != nil {
			ra.Close()
			return nil, err
		}
		snap.Audit = append(snap.Audit, a)
	}
	ra.Close()
	if err := ra.Err(); err != nil {
		return nil, err
	}

	_ = sql.Drivers() // keep database/sql import if no other use; REMOVE this line if the import is otherwise used
	return snap, nil
}
```

> **Implementer note:** if `database/sql` is not otherwise used after writing the above (it likely isn't — `s.db.Query` doesn't need the import in this file if `sql.Rows`/`sql.NullString` aren't referenced), drop the import and the `_ = sql.Drivers()` placeholder line. Run `gofmt`/`go vet` to confirm. The placeholder exists only so the import example compiles standalone; the real file uses whatever imports it actually needs (`fmt`, and `database/sql` only if `sql.Null*` is referenced).

- [ ] **Step 4: Run test to verify it passes.**

Run: `go test ./internal/store/ -run TestExportSnapshot_CapturesAllTables -v`
Expected: PASS.

- [ ] **Step 5: Commit** — `feat(store): Snapshot DTO + ExportSnapshot + ListHostKeys (Plan 11 T1)` + Co-Authored-By.

---

## Task 2: `ImportSnapshot` (write side — id-preserving, re-seal, empty-guard)

**Goal:** Restore a Snapshot into THIS store, re-sealing credentials under this store's master key. The Plan-11 correctness bar lives here.

**Files:** `internal/store/export.go` (append), `internal/store/export_test.go` (append).

**Interfaces:**
- Consumes: `Snapshot` (from T1); the unexported `seal(s.masterKey, plaintext)` (`crypto.go:23`).
- Produces: `var ErrVaultNotEmpty`; `(*Store) ImportSnapshot(*Snapshot) error`.

- [ ] **Step 1: Write the failing round-trip test (THE load-bearing test).**

```go
// TestImportSnapshot_RoundTrip_CrossMasterKey exports from store A (mk1), imports
// into a SECOND store B with a DIFFERENT master key, and asserts every table
// matches AND the original project plaintext token still validates on B.
func TestImportSnapshot_RoundTrip_CrossMasterKey(t *testing.T) {
	a := newTestStore(t) // mk1

	// seed A: one credential, one server (sudo too, to exercise SudoCredentialID),
	// one profile + grant, one project (capture the plaintext token!), one host key, one audit row.
	credID, _ := a.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw-A")})
	sudoID, _ := a.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("sudo-A")})
	srvID, _ := a.AddServer(&models.Server{Name: "gpu", Host: "192.0.2.10", Port: 22, User: "deploy",
		AuthMethod: models.AuthPassword, CredentialID: credID, SudoCredentialID: sudoID, Tags: []string{"prod"}, Description: "box"})
	profID, _ := a.AddProfile("team-a")
	a.GrantServers(profID, []string{srvID})
	projID, token, err := a.AddProject("my-agent", profID) // keep `token` — the proof
	if err != nil {
		t.Fatal(err)
	}
	a.SaveHostKey("192.0.2.10", 22, []byte("hk-blob"))
	a.WriteAudit(AuditRow{Action: "exec", ProjectID: projID, ServerID: srvID, Status: "ok"})

	snap, err := a.ExportSnapshot()
	if err != nil {
		t.Fatalf("export A: %v", err)
	}

	// B: fresh EMPTY store with a DIFFERENT master key (newTestStore mints a new random key).
	b := newTestStore(t)

	if err := b.ImportSnapshot(snap); err != nil {
		t.Fatalf("import into B: %v", err)
	}

	// servers match (same id — proves id-preserving insert)
	got, err := b.GetServer(srvID)
	if err != nil || got == nil || got.Name != "gpu" || got.Host != "192.0.2.10" || got.SudoCredentialID != sudoID {
		t.Fatalf("server mismatch on B: got=%+v err=%v", got, err)
	}
	// credential re-sealed under B's key AND decrypts to the original plaintext
	gc, err := b.GetCredential(credID)
	if err != nil || gc == nil || string(gc.Secret) != "pw-A" {
		t.Fatalf("credential not re-sealed/decrypted under B's key: %+v err=%v", gc, err)
	}
	// grants + profiles
	if ids, _ := b.ServersForProfile(profID); len(ids) != 1 || ids[0] != srvID {
		t.Fatalf("grants not restored on B: %v", ids)
	}
	// host keys
	hk, _ := b.GetHostKey("192.0.2.10", 22)
	if !bytes.Equal(hk, []byte("hk-blob")) {
		t.Fatalf("host key not restored: %v", hk)
	}
	// THE PROOF — original plaintext token from A still validates on B (hash preserved verbatim)
	pj, err := b.VerifyToken(token)
	if err != nil || pj == nil || pj.ID != projID {
		t.Fatalf("ORIGINAL TOKEN DOES NOT VALIDATE ON B after import: pj=%+v err=%v", pj, err)
	}
}

// TestImportSnapshot_RefusesNonEmpty guards against silent clobber.
func TestImportSnapshot_RefusesNonEmpty(t *testing.T) {
	a := newTestStore(t)
	a.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("x")})
	a.AddServer(&models.Server{Name: "s", Host: "1.2.3.4", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: ""}) // any server makes it non-empty
	snap, _ := a.ExportSnapshot()

	b := newTestStore(t)
	b.AddServer(&models.Server{Name: "existing", Host: "9.9.9.9", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: ""})
	if err := b.ImportSnapshot(snap); err != ErrVaultNotEmpty {
		t.Fatalf("import into non-empty: err=%v, want ErrVaultNotEmpty", err)
	}
}
```

> The second test seeds a server without a credential for simplicity (`CredentialID: ""`). If the schema's FK on `servers.credential_id` rejects empty (NOT NULL + FK), seed a real credential first. Match whatever `AddServer` requires (read its validation in `servers.go`); the test's intent is "B has ≥1 server → import refused."

- [ ] **Step 2: Run to fail** — `ImportSnapshot`/`ErrVaultNotEmpty` undefined.

Run: `go test ./internal/store/ -run 'TestImportSnapshot' -v`
Expected: FAIL (undefined).

- [ ] **Step 3: Implement `ImportSnapshot`** (append to `internal/store/export.go`).

```go
import "errors" // add to the import block

// ErrVaultNotEmpty is returned by ImportSnapshot when the target store already
// has servers. Import is restore-into-empty only — no silent clobber. Delete
// store.db (or point SSHMGR_STORE at a fresh path) to get an empty vault.
var ErrVaultNotEmpty = errors.New("vault is not empty; import into a fresh/empty store (move/delete store.db first)")

// ImportSnapshot restores a Snapshot into THIS store in one transaction:
//   - credentials are RE-SEALED under this store's master key (master-key-independent file);
//   - rows are inserted id-preserving in FK order (credentials → servers → profiles → profile_servers → projects → host_keys → audit);
//   - projects.token_hash/salt/prefix are written verbatim so the original plaintext token keeps validating.
// Refuses a non-empty target.
func (s *Store) ImportSnapshot(snap *Snapshot) error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM servers`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrVaultNotEmpty
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op after Commit

	// 1. credentials (re-seal under THIS store's master key)
	for _, c := range snap.Credentials {
		secretBlob, err := seal(s.masterKey, c.Secret)
		if err != nil {
			return fmt.Errorf("seal credential %s: %w", c.ID, err)
		}
		var passArg any // nil → SQL NULL
		if len(c.Passphrase) > 0 {
			pb, err := seal(s.masterKey, c.Passphrase)
			if err != nil {
				return fmt.Errorf("seal passphrase %s: %w", c.ID, err)
			}
			passArg = pb
		}
		if _, err := tx.Exec(`INSERT INTO credentials(id,type,secret_blob,passphrase_blob,created_at,updated_at) VALUES(?,?,?,?,?,?)`,
			c.ID, c.Type, secretBlob, passArg, c.CreatedAt, c.UpdatedAt); err != nil {
			return fmt.Errorf("insert credential %s: %w", c.ID, err)
		}
	}

	// 2. servers (sudo_credential_id "" → NULL to mirror the original NULL semantics for empty)
	for _, sv := range snap.Servers {
		var sudoArg any
		if sv.SudoCredentialID != "" {
			sudoArg = sv.SudoCredentialID
		}
		if _, err := tx.Exec(`INSERT INTO servers(id,name,host,port,user,auth_method,credential_id,sudo_credential_id,tags,description,location,hardware,services,role,caveats,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			sv.ID, sv.Name, sv.Host, sv.Port, sv.User, sv.AuthMethod, sv.CredentialID, sudoArg, sv.TagsRaw, sv.Description, sv.Location, sv.Hardware, sv.Services, sv.Role, sv.Caveats, sv.CreatedAt, sv.UpdatedAt); err != nil {
			return fmt.Errorf("insert server %s: %w", sv.ID, err)
		}
	}

	// 3. profiles
	for _, p := range snap.Profiles {
		if _, err := tx.Exec(`INSERT INTO profiles(id,name,created_at,updated_at) VALUES(?,?,?,?)`, p.ID, p.Name, p.CreatedAt, p.UpdatedAt); err != nil {
			return fmt.Errorf("insert profile %s: %w", p.ID, err)
		}
	}

	// 4. grants (profile_servers)
	for _, g := range snap.Grants {
		if _, err := tx.Exec(`INSERT INTO profile_servers(profile_id,server_id) VALUES(?,?)`, g.ProfileID, g.ServerID); err != nil {
			return fmt.Errorf("insert grant %s/%s: %w", g.ProfileID, g.ServerID, err)
		}
	}

	// 5. projects — hash/salt/prefix VERBATIM (original plaintext token keeps validating)
	for _, p := range snap.Projects {
		if _, err := tx.Exec(`INSERT INTO projects(id,name,token_hash,token_salt,token_prefix,profile_id,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`,
			p.ID, p.Name, p.TokenHash, p.TokenSalt, p.TokenPrefix, p.ProfileID, p.Status, p.CreatedAt, p.UpdatedAt); err != nil {
			return fmt.Errorf("insert project %s: %w", p.ID, err)
		}
	}

	// 6. host_keys
	for _, h := range snap.HostKeys {
		if _, err := tx.Exec(`INSERT INTO host_keys(host_port,key_blob,created_at) VALUES(?,?,?)`, h.HostPort, h.KeyBlob, h.CreatedAt); err != nil {
			return fmt.Errorf("insert host_key %s: %w", h.HostPort, err)
		}
	}

	// 7. audit (id autoincrement — insert without id, letting the target assign; ts preserved)
	for _, a := range snap.Audit {
		if _, err := tx.Exec(`INSERT INTO audit_log(ts,project_id,server_id,action,command,sudo,status,exit_code,duration_ms) VALUES(?,?,?,?,?,?,?,?,?)`,
			a.TS, nullIfEmpty(a.ProjectID), nullIfEmpty(a.ServerID), a.Action, nullIfEmpty(a.Command), a.Sudo, nullIfEmpty(a.Status), a.ExitCode, a.DurationMS); err != nil {
			return fmt.Errorf("insert audit: %w", err)
		}
	}

	return tx.Commit()
}

// nullIfEmpty returns nil (→ SQL NULL) for "", else s. Mirrors COALESCE-on-read.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
```

> **Implementer note on audit id:** the source `audit_log.id` (autoincrement INTEGER PK) is NOT preserved (the target assigns its own). This is fine — audit ids are not referenced by other tables. `ts` (and all other columns) ARE preserved. If exact id preservation is later wanted, add `id` to the INSERT; for MVP, dropping it avoids autoincrement-sequence collisions.

- [ ] **Step 4: Run tests to verify they pass** — including the cross-master-key round-trip and the original-token-validates proof.

Run: `go test ./internal/store/ -run 'TestImportSnapshot|TestExportSnapshot' -v`
Expected: PASS — round-trip matches, original token validates on B, non-empty refused.

- [ ] **Step 5: Commit** — `feat(store): ImportSnapshot (id-preserving, re-seal, empty-guard) (Plan 11 T2)` + Co-Authored-By. Note in the body: this is the master-key-independence + token-preservation proof point.

---

## Task 3: `internal/vaultio` — passphrase envelope (Argon2id → AES-256-GCM)

**Goal:** Self-contained byte envelope: `Encrypt(passphrase, data) → magic‖salt‖nonce‖ct`; `Decrypt` reverses. Wrong passphrase / tamper → error (GCM auth). Standalone (no `store` import).

**Files:** Create `internal/vaultio/vaultio.go`, `internal/vaultio/vaultio_test.go`.

**Interfaces:**
- Produces: `Encrypt(passphrase, plaintext []byte) ([]byte, error)`; `Decrypt(passphrase, blob []byte) ([]byte, error)`; `ErrBadMagic`, `ErrTruncated`.

- [ ] **Step 1: Write the failing tests.**

```go
package vaultio

import (
	"bytes"
	"errors"
	"testing"
)

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	pt := []byte(`{"version":1,"servers":[]}`)
	out, err := Encrypt([]byte("correct horse battery staple"), pt)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	got, err := Decrypt([]byte("correct horse battery staple"), out)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, pt) {
		t.Fatalf("round-trip mismatch: got %q want %q", got, pt)
	}
}

func TestDecrypt_WrongPassphraseFails(t *testing.T) {
	out, _ := Encrypt([]byte("right"), []byte("secret"))
	if _, err := Decrypt([]byte("wrong"), out); err == nil {
		t.Fatal("Decrypt with wrong passphrase must fail (GCM auth)")
	}
}

func TestDecrypt_TamperedFails(t *testing.T) {
	out, _ := Encrypt([]byte("pw"), []byte("secret"))
	out[len(out)-1] ^= 0xFF // flip a ciphertext byte
	if _, err := Decrypt([]byte("pw"), out); err == nil {
		t.Fatal("Decrypt of tampered ciphertext must fail")
	}
}

func TestDecrypt_TruncatedFails(t *testing.T) {
	out, _ := Encrypt([]byte("pw"), []byte("x"))
	if _, err := Decrypt([]byte("pw"), out[:len(magic)+4]); !errors.Is(err, ErrTruncated) {
		t.Fatalf("truncated blob: err=%v, want ErrTruncated", err)
	}
}

func TestDecrypt_BadMagicFails(t *testing.T) {
	bad := append([]byte("XXXXXXXX"), make([]byte, 40)...)
	if _, err := Decrypt([]byte("pw"), bad); !errors.Is(err, ErrBadMagic) {
		t.Fatalf("bad magic: err=%v, want ErrBadMagic", err)
	}
}

func TestEncrypt_DifferentSaltsProduceDifferentCiphertext(t *testing.T) {
	// randomness sanity: two encrypts of the same plaintext differ (random salt+nonce)
	a, _ := Encrypt([]byte("pw"), []byte("x"))
	b, _ := Encrypt([]byte("pw"), []byte("x"))
	if bytes.Equal(a, b) {
		t.Fatal("two Encrypt calls produced identical output (salt/nonce not random)")
	}
}
```

- [ ] **Step 2: Run to fail** — package undefined.

Run: `go test ./internal/vaultio/ -v`
Expected: FAIL (no such package).

- [ ] **Step 3: Implement** (`internal/vaultio/vaultio.go`).

```go
// Package vaultio is the portable, passphrase-encrypted envelope for vault
// exports (and, later, Synology backups / client-cache pulls). It is deliberately
// independent of internal/store: callers hand it plaintext bytes + a passphrase,
// get back magic‖salt‖nonce‖ciphertext. The key is Argon2id(passphrase, salt)
// with the SAME parameters the vault's own passphrase mode uses
// (internal/store/masterkey.go DeriveFromPassphrase: time=1, memory=64MiB,
// parallelism=4, 32-byte out), so a uniform cost is applied across the project.
package vaultio

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"io"

	"golang.org/x/crypto/argon2"
)

var (
	magic = []byte("SSHMGRV1") // 8 bytes — format identifier + future versioning

	// ErrBadMagic: blob does not start with the expected magic header.
	ErrBadMagic = errors.New("vaultio: bad magic (not an ssh-manager export)")
	// ErrTruncated: blob too short to contain magic+salt+nonce+tag.
	ErrTruncated = errors.New("vaultio: truncated blob")
)

const (
	saltLen   = 16
	nonceLen  = 12 // AES-GCM standard nonce
	keyLen    = 32 // AES-256
	argonTime = 1
	argonMem  = 64 * 1024 // KiB → 64 MiB
	argonPar  = 4
)

// Encrypt derives a key from passphrase+random-salt (Argon2id), AES-256-GCM-seals
// plaintext, and returns magic‖salt‖nonce‖ciphertext.
func Encrypt(passphrase, plaintext []byte) ([]byte, error) {
	salt := make([]byte, saltLen)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, err
	}
	return sealWithSalt(passphrase, salt, plaintext)
}

// sealWithSalt is split out so tests can fix the salt for determinism if needed.
func sealWithSalt(passphrase, salt, plaintext []byte) ([]byte, error) {
	key := argon2.IDKey(passphrase, salt, argonTime, argonMem, argonPar, keyLen)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, nonceLen)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	ct := gcm.Seal(nil, nonce, plaintext, nil)
	out := make([]byte, 0, len(magic)+saltLen+nonceLen+len(ct))
	out = append(out, magic...)
	out = append(out, salt...)
	out = append(out, nonce...)
	out = append(out, ct...)
	return out, nil
}

// Decrypt parses magic‖salt‖nonce‖ciphertext, re-derives the key, and AES-GCM-opens.
// Wrong passphrase or any tampering → a non-nil error (GCM authentication failure).
func Decrypt(passphrase, blob []byte) ([]byte, error) {
	minLen := len(magic) + saltLen + nonceLen // + at least the 16-byte GCM tag, checked implicitly by Open
	if len(blob) < minLen {
		return nil, ErrTruncated
	}
	if !bytes.Equal(blob[:len(magic)], magic) {
		return nil, ErrBadMagic
	}
	off := len(magic)
	salt := blob[off : off+saltLen]
	off += saltLen
	nonce := blob[off : off+nonceLen]
	off += nonceLen
	ct := blob[off:]

	key := argon2.IDKey(passphrase, salt, argonTime, argonMem, argonPar, keyLen)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, err // wrong passphrase OR tamper — do not distinguish (no oracle)
	}
	return pt, nil
}
```

- [ ] **Step 4: Run tests to verify they pass.**

Run: `go test ./internal/vaultio/ -v`
Expected: all 6 PASS.

- [ ] **Step 5: Commit** — `feat(vaultio): passphrase envelope (Argon2id→AES-GCM) (Plan 11 T3)` + Co-Authored-By.

---

## Task 4: CLI `export` / `import` commands + register

**Goal:** Wire it up. `ssh-manager export --out file` (prompts passphrase twice) and `ssh-manager import <file>` (prompts once). Passphrase prompt is a testable seam.

**Files:** Create `internal/cli/export.go`, `internal/cli/import.go`, `internal/cli/export_import_smoke_test.go`; modify `internal/cli/root.go`.

**Interfaces:**
- Consumes: `vault.OpenStore()` (`openUnlockedStore()`), `store.ExportSnapshot`/`ImportSnapshot`, `vaultio.Encrypt`/`Decrypt`, `encoding/json`.
- Produces: `newExportCmd()`, `newImportCmd()`; registered in `root.go`.

- [ ] **Step 1: Write the failing CLI smoke test** (`internal/cli/export_import_smoke_test.go`). Mirror the pattern in `internal/cli/cli_smoke_test.go` (`withEnv` + build a root with `SSHMGR_STORE` + `SSHMGR_MASTERKEY_HEX`, capture stdout, `Execute()`).

```go
package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// passphrasePrompt is a testable seam (mirrors unlock.go). Tests swap it.
// Default reads from stdin (term.ReadPassword); tests inject a fixed value.

func TestExportImport_CLIRoundTrip(t *testing.T) {
	dir := t.TempDir()
	dbA := filepath.Join(dir, "a.db")
	dbB := filepath.Join(dir, "b.db")
	outFile := filepath.Join(dir, "vault.export")

	// --- seed store A via the CLI (servers add) + capture a project token ---
	mk, _ := store.GenerateMasterKey()
	withEnv(t, map[string]string{
		"SSHMGR_STORE":          dbA,
		"SSHMGR_MASTERKEY_HEX":  hexEncode(mk), // enc.go:5
	})
	// swap the passphrase seam to a fixed value for export
	orig := passphrasePrompt
	passphrasePrompt = func() ([]byte, error) { return []byte("strong-passphrase-123"), nil }
	origConfirm := passphraseConfirmPrompt
	passphraseConfirmPrompt = func() ([]byte, error) { return []byte("strong-passphrase-123"), nil }
	t.Cleanup(func() { passphrasePrompt = orig; passphraseConfirmPrompt = origConfirm })

	mustCliA := func(args ...string) *bytes.Buffer {
		root := NewRootCmd(); root.SetArgs(args); out := &bytes.Buffer{}
		root.SetOut(out); root.SetErr(out)
		if err := root.Execute(); err != nil { t.Fatalf("A %v: %v", args, err) }
		return out
	}
	// add a credential+server (reuse servers add), a profile, grant, project
	// (Exact flags per servers.go — read serversAddCmd; this sketch uses the known ones.)
	mustCliA("servers", "add", "--name", "gpu", "--host", "192.0.2.10", "--user", "deploy", "--password", "pw-A")
	// ... add profile/grant/project via the CLI, OR seed directly via store.Open(dbA,mk).
	//  Simpler: seed A directly for the project token capture:
	stA, _ := store.Open(dbA, mk)
	profID, _ := stA.AddProfile("team-a")
	srv, _ := stA.GetServerByName("gpu")
	stA.GrantServers(profID, []string{srv.ID})
	_, token, _ := stA.AddProject("my-agent", profID)
	stA.Close()

	// --- export from A ---
	mustCliA("export", "--out", outFile)
	if _, err := os.Stat(outFile); err != nil { t.Fatalf("export file not written: %v", err) }

	// --- import into a FRESH store B (different master key) ---
	mk2, _ := store.GenerateMasterKey()
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         dbB,
		"SSHMGR_MASTERKEY_HEX": hexEncode(mk2),
	})
	// import uses passphrasePrompt (single) — same seam value
	root := NewRootCmd(); root.SetArgs([]string{"import", outFile})
	if err := root.Execute(); err != nil { t.Fatalf("import: %v", err) }

	// --- verify B: server present + original token validates ---
	stB, _ := store.Open(dbB, mk2)
	defer stB.Close()
	got, _ := stB.GetServerByName("gpu")
	if got == nil || got.Host != "192.0.2.10" { t.Fatalf("server not imported into B: %+v", got) }
	if pj, err := stB.VerifyToken(token); err != nil || pj == nil {
		t.Fatalf("ORIGINAL TOKEN does not validate on B after CLI import: %v", pj)
	}
	_ = models.AuthPassword // keep import if used
}
```

> **Implementer note:** the exact `servers add` flag set and `hexEncode`/`withEnv` helper locations are in `internal/cli/servers.go` and `cli_smoke_test.go` — read them and match. The sketch above is the shape; fill the real flag names. The test's load-bearing assertions are: (a) the export file exists, (b) `GetServerByName("gpu")` returns the server on B, (c) `VerifyToken(token)` succeeds on B. If seeding A directly via `store.Open` (skipping CLI for setup) is cleaner, do that — the test exercises the `export` and `import` commands themselves, not `servers add`.

- [ ] **Step 2: Run to fail** — `newExportCmd`/`newImportCmd`/`passphrasePrompt` undefined; commands not registered.

Run: `go test ./internal/cli/ -run TestExportImport_CLIRoundTrip -v`
Expected: FAIL.

- [ ] **Step 3: Implement `internal/cli/export.go`.**

```go
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/vaultio"
)

// passphrasePrompt and passphraseConfirmPrompt are testable seams (mirror
// unlock.go's passphrasePrompt). Defaults read from the terminal; tests inject.
var (
	passphrasePrompt       = func() ([]byte, error) { return readPassphrase("Export passphrase: ") }
	passphraseConfirmPrompt = func() ([]byte, error) { return readPassphrase("Confirm passphrase: ") }
)

// readPassphrase is the default terminal prompt (defined in unlock.go or a shared
// helper — reuse term.ReadPassword(int(os.Stdin.Fd())) like unlock.go:17).
// If unlock.go's passphrasePrompt already exposes this, reuse it instead.

func newExportCmd() *cobra.Command {
	var outPath string
	c := &cobra.Command{
		Use:   "export",
		Short: "Export the entire vault to a passphrase-encrypted portable file",
		Long: `Export every server, credential, profile, project, host key, and audit row to a
portable, passphrase-encrypted file. Credentials are decrypted then re-encrypted
with a key derived from YOUR passphrase (Argon2id + AES-256-GCM) — the file is
independent of this machine's vault master key, so it restores on any machine.

The file is only as strong as its passphrase (offline brute-force is possible if
the file leaks — like a KeePass database). Use a strong passphrase.

To restore: ssh-manager import <file> (into a fresh/empty vault).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer st.Close()

			snap, err := st.ExportSnapshot()
			if err != nil {
				return err
			}
			plaintext, err := json.MarshalIndent(snap, "", "  ")
			if err != nil {
				return err
			}

			pw, err := passphrasePrompt()
			if err != nil {
				return err
			}
			pw2, err := passphraseConfirmPrompt()
			if err != nil {
				return err
			}
			if string(pw) != string(pw2) {
				return fmt.Errorf("passphrases do not match")
			}
			if len(pw) == 0 {
				return fmt.Errorf("passphrase must not be empty")
			}

			blob, err := vaultio.Encrypt(pw, plaintext)
			if err != nil {
				return err
			}

			var w io.Writer
			if outPath == "-" || outPath == "" {
				w = cmd.OutOrStdout()
			} else {
				f, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
				if err != nil {
					return err
				}
				defer f.Close()
				w = f
			}
			if _, err := w.Write(blob); err != nil {
				return err
			}
			if outPath != "-" && outPath != "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "exported %d servers / %d credentials to %s\n", len(snap.Servers), len(snap.Credentials), outPath)
			}
			return nil
		},
	}
	c.Flags().StringVar(&outPath, "out", "", "output file (use '-' for stdout; required for a real file)")
	return c
}
```

> **Implementer note:** `readPassphrase` should reuse `unlock.go`'s terminal-reading code (`term.ReadPassword(int(os.Stdin.Fd()))`). If `unlock.go`'s `passphrasePrompt` already does exactly this, point `cli/export.go`'s seams at the same underlying reader (or factor a shared `readPassphrase(prompt string)` helper). Don't duplicate the terminal logic — share it.

- [ ] **Step 4: Implement `internal/cli/import.go`.**

```go
package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vaultio"
)

func newImportCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "import <file>",
		Short: "Restore a vault from a passphrase-encrypted export file",
		Long: `Restore an export file into THIS vault. The target vault must be EMPTY (import
never clobbers — move/delete store.db to get a fresh empty vault first) and
UNLOCKED (credentials are re-sealed under this vault's master key).

Project tokens carry their hash, so the original plaintext token (from when the
export was made) still validates after import.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			blob, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			pw, err := passphrasePrompt()
			if err != nil {
				return err
			}
			plaintext, err := vaultio.Decrypt(pw, blob)
			if err != nil {
				return err
			}
			var snap store.Snapshot
			if err := json.Unmarshal(plaintext, &snap); err != nil {
				return err
			}
			st, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer st.Close()
			if err := st.ImportSnapshot(&snap); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "imported %d servers / %d credentials\n", len(snap.Servers), len(snap.Credentials))
			return nil
		},
	}
	return c
}
```

- [ ] **Step 5: Register in `internal/cli/root.go`** — add `newExportCmd(), newImportCmd()` to the `root.AddCommand(...)` line (root.go:22).

- [ ] **Step 6: Run tests to verify they pass** + full no-regression.

Run: `go test ./internal/cli/ ./internal/store/ ./internal/vaultio/ -v` then `go test ./...`
Expected: PASS — CLI round-trip, original token validates on B; all packages green.

- [ ] **Step 7: Commit** — `feat(cli): ssh-manager export/import commands (Plan 11 T4)` + Co-Authored-By.

---

## Task 5: `docs/备份与迁移.md` + README cross-link + verify + review + merge

**Goal:** Document the feature; final no-regression + review + merge.

**Files:** Create `docs/备份与迁移.md`; modify `README.md` (cross-link); modify `docs/README.md` (index row).

- [ ] **Step 1: Write `docs/备份与迁移.md`** (Chinese, operator-facing, matching the style of `docs/multi-machine.md` / `getting-started.md`). Cover:
  - **What it is**: `export` → a portable, passphrase-encrypted file with the WHOLE vault (servers + 密码/密钥 + profiles + projects + host keys + audit). `import` → restore into an empty vault. Cross-machine (master-key-independent).
  - **Usage**:
    ```bash
    ssh-manager export --out vault.sme                  # 提示输口令（输两次确认）
    ssh-manager import vault.sme                         # 在目标机（空 vault、已 unlock）上恢复
    ```
  - **安全模型（必读）**: 文件是 KeePass 式——`Argon2id(口令) → AES-256-GCM`；文件泄露 + 弱口令 = 凭据被爆破。**必须用强口令**。口令丢了 = 文件无法恢复（无法找回）。明文凭据在文件里（被口令加密保护），不同于 `store.db`（凭据按 vault master key 加密，需原机 keychain）。
  - **与「直接复制 `store.db`」的对比**: `store.db` 不可移植（恢复需原机 keychain 的 master key）；`export` 文件只要口令即可，跨机、可入新 vault。
  - **使用场景**: ① 备份/存档（定期 export 收好）；② 迁移（旧机 export → 新机 import）；③ 灾难恢复（vault 损毁，从 export 恢复到一个干净 store.db）。
  - **限制**: import 只入空 vault（不覆盖既有）；不增量同步（要同步见 multi-machine serve 模式）；audit 的自增 id 不保留（时间戳保留）；原 project token 在 import 后仍有效（hash 保留）。
  - **格式**: `internal/vaultio` envelope (`SSHMGRV1` magic + Argon2id + AES-GCM) + `store.Snapshot` JSON（version 1）。该格式后续会被 Synology 自动备份等复用。
- [ ] **Step 2: README cross-link** — add to the 中文 docs table a row for `备份与迁移.md`, AND add a one-liner under the "Managing servers & projects" section: "Back up / migrate the whole vault: `ssh-manager export` / `import` — see `docs/备份与迁移.md`." Add `docs/README.md` index row too (mirror the multi-machine.md row).
- [ ] **Step 3: Verify** — `go test ./...` green; `gofmt -l .` empty on touched files; `go vet ./...` clean. Manual smoke: `ssh-manager export --out /tmp/x.sme` (passphrase) → `ssh-manager import /tmp/x.sme` into a fresh `SSHMGR_STORE` → `ssh-manager servers ls` shows the imported servers.
- [ ] **Step 4: Final whole-branch review** — focus on: (a) credentials are NEVER written to disk unencrypted (the file is always vaultio.Encrypt'd; the DTO plaintext only exists in memory between ExportSnapshot and Encrypt); (b) the round-trip test genuinely proves cross-master-key + original-token-validates; (c) empty-target guard has no bypass; (d) agent surface untouched. Resolve findings in one fix wave.
- [ ] **Step 5: Merge** to master per the user's finishing choice (`--no-ff`, matching Plan 10).

---

## Self-Review (run before handoff)

1. **Spec coverage (from the grill):** export whole vault incl. credentials (✓ T1/T2 — decrypted in DTO, re-sealed on import); passphrase-encrypted portable file Argon2id (✓ T3 vaultio); full round-trip incl. import (✓ T2/T4); KeePass-style strong-passphrase/offline-crack documented (✓ T5 docs); `docs/备份与迁移.md` (✓ T5). Format reusability for backup/cache flagged (✓ vaultio + Snapshot are standalone). Agent surface unchanged (✓ — no mcpserver/sshbroker/NewServer/serve edits).
2. **Placeholder scan:** the `readPassphrase`/`servers add` flag details are "verify on read" with the source file named (unlock.go, servers.go) — the contracts are fixed, only the exact existing symbol names are deferred (Plan-8 precedent). No `<...>`/TODO in code blocks. The T1 `_ = sql.Drivers()` line is an explicit implementer note to drop, not a placeholder left in committed code.
3. **Type consistency:** `store.Snapshot` → `json.Marshal` (T4 export) → `json.Unmarshal` into `store.Snapshot` (T4 import) — same type both ends. `vaultio.Encrypt(passphrase, plaintext)` ↔ `vaultio.Decrypt(passphrase, blob)`. `ExportSnapshot() (*Snapshot, error)` ↔ `ImportSnapshot(*Snapshot) error`. `ListHostKeys() ([]SnapshotHostKey, error)` produced and consumed in the same Snapshot round-trip.
4. **Security (the load-bearing concerns):** (a) credentials at rest in the file are ALWAYS vaultio-encrypted (the only plaintext-cred window is in-memory between ExportSnapshot and Encrypt / between Decrypt and ImportSnapshot — both short, both in the unlocked process); (b) master-key-independence proven by the cross-master-key round-trip test (T2); (c) original-token-validates-after-import proven (T2) — token hash carried verbatim; (d) wrong-passphrase/tamper → GCM auth failure (T3 tests); (e) empty-target guard (T2) prevents clobber. The ONE residual risk to state clearly in docs: the file is offline-brute-forceable (KeePass model) → strong passphrase is the mitigation, not optional.

---

## Execution Handoff

**Subagent-Driven (recommended):** T1–T4 sonnet (pure Go store/vaultio/cli + tests, no LLM, no conformance Docker). T5 sonnet docs + a final **opus** whole-branch review focused on Self-Review §4 (credentials-never-unencrypted-at-rest; cross-master-key + token-preservation proof; empty-guard no bypass). **No Fable-5/$ required** — correctness is proven by the round-trip + envelope tests, not the §12 eval. Merge per the user's choice (`--no-ff` to master).

**Honest scope note:** Plan 11 is the **second** standalone plan in the multi-machine arc (after Plan 10 serve mode). It delivers portable backup/migration NOW (single-machine users' most-asked gap), AND defines the `Snapshot` + `vaultio` serialization that Plan 12+ (Synology auto-backup, client cache) will reuse. The round-trip test in T2 is the correctness keystone — if it fails, nothing else matters.
