# Server Structured Metadata Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add 5 structured metadata fields (Location/Hardware/Services/Role/Caveats) to each server and surface them — plus existing Tags + Description — to the agent via `list_servers`, reversing Plan-8's owner-only rule.

**Architecture:** Extend-in-place following the Tags/Description precedent: new columns on `servers` (nullable TEXT, idempotent migration), new fields on `models.Server` + `mcpserver.ServerInfo`, new CLI flags. No new MCP tool, no eval/baseline/e2e scorer changes. A 4 KB store-layer cap bounds the agent-context blast radius.

**Tech Stack:** Go (module `ssh-manager-mcp`), SQLite via `modernc.org/sqlite`, CLI via `github.com/spf13/cobra`, MCP via `github.com/modelcontextprotocol/go-sdk/mcp`. Tests use stdlib `testing` (no testify).

## Global Constraints

- **4 KB byte cap** on each of Description/Location/Hardware/Services/Role/Caveats and each individual Tag — enforced at the store layer (`AddServer`/`UpdateServer`).
- **No new MCP tool** — `BrokerTools` stays 6; all new fields embed in `list_servers`'s `ServerInfo`.
- **Zero eval/baseline/e2e scorer changes** — T6/T8 are immune to `ServerInfo` field additions (verified).
- **New columns are nullable `TEXT`**, added via idempotent `addColumnIfMissing` (`ALTER TABLE ADD COLUMN`).
- **Explicit `""`, no `omitempty`** on new `ServerInfo` fields (agent sees a consistent schema; `""` = "explicitly none").
- **Docs use sanitized examples** (RFC5737 IPs; no real hostnames/locations/hardware) — the repo is public.
- **Commit messages** end with `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.
- **Trim leading/trailing whitespace** on the 5 new flags + `--description` at parse; preserve internal newlines.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/models/models.go` | `Server` struct — add 5 fields; flip Description comments |
| `internal/store/store.go` | schema + migration + `validateServerText` helper |
| `internal/store/servers.go` | CRUD plumbing (INSERT/UPDATE/SELECT/scanServer) + validation calls |
| `internal/store/servers_test.go` | round-trip + 4 KB cap tests |
| `internal/store/migrate_test.go` | old-shape migration fixture extended for 5 new columns |
| `internal/mcpserver/types.go` | `ServerInfo` — add 7 fields with jsonschema tags |
| `internal/mcpserver/core.go` | `ListServersForProfile` — populate new fields |
| `internal/mcpserver/server.go` | `list_servers` tool description wording |
| `internal/mcpserver/listmetadata_test.go` | NEW — assert list_servers surfaces all fields |
| `internal/cli/servers.go` | add/edit flags, ls format, `truncate` helper |
| `internal/cli/cli_smoke_test.go` | update ls assertion for new format + new flags |
| `docs/managing-servers.md` | structured-fields coaching + secret warning |
| `docs/agent-access.md` | field list update |
| `docs/superpowers/plans/2026-08-10-ssh-manager-mcp-plan-8-management-cli.md` | annotate scope decision #7 (reversal) |

---

### Task 1: Server model fields + storage columns + CRUD plumbing

**Files:**
- Modify: `internal/models/models.go:40-53`
- Modify: `internal/store/store.go:116-122` (migrate), `:168-181` (schemaSQL)
- Modify: `internal/store/servers.go:17-21` (INSERT), `:34-37` (UPDATE), `:49,56,67` (3× SELECT), `:101` (scanServer Scan)
- Test: `internal/store/servers_test.go`

**Interfaces:**
- Consumes: nothing (foundation).
- Produces: `models.Server` with fields `Location, Hardware, Services, Role, Caveats string`; store CRUD that persists + reads them round-trip.

- [ ] **Step 1: Write the failing round-trip test**

Add to `internal/store/servers_test.go` (after `TestServerDescriptionRoundTrip`):

```go
func TestServerStructuredFieldsRoundTrip(t *testing.T) {
	s := newTestStore(t)
	cid := mustCred(t, s, models.CredPassword, "pw")
	const (
		location = "dc2 rack14"
		hardware = "8x A100 80GB, 1TB RAM"
		services = "postgres primary, prometheus"
		role     = "prod pg primary"
		caveats  = "do not reboot 02-03:00\nfailover is manual"
	)
	id, err := s.AddServer(&models.Server{
		Name: "db1", Host: "10.0.0.5", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: cid,
		Location: location, Hardware: hardware, Services: services, Role: role, Caveats: caveats,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetServer(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Location != location || got.Hardware != hardware || got.Services != services ||
		got.Role != role || got.Caveats != caveats {
		t.Fatalf("structured fields lost:\nlocation=%q hardware=%q services=%q role=%q caveats=%q",
			got.Location, got.Hardware, got.Services, got.Role, got.Caveats)
	}
	byName, _ := s.GetServerByName("db1")
	if byName.Role != role || byName.Caveats != caveats {
		t.Fatalf("GetServerByName lost fields: %+v", byName)
	}
	list, _ := s.ListServers()
	if list[0].Services != services {
		t.Fatalf("ListServers lost services: %q", list[0].Services)
	}

	// UpdateServer persists edits to the structured fields.
	loaded, _ := s.GetServer(id)
	loaded.Role = "prod pg replica"
	loaded.Caveats = "drained"
	if err := s.UpdateServer(loaded); err != nil {
		t.Fatalf("UpdateServer: %v", err)
	}
	updated, _ := s.GetServer(id)
	if updated.Role != "prod pg replica" || updated.Caveats != "drained" {
		t.Fatalf("UpdateServer did not persist structured fields: %+v", updated)
	}

	// Empty fields stay empty (nullable columns, scan as "").
	empty, _ := s.AddServer(&models.Server{
		Name: "bare", Host: "h", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: cid,
	})
	gotEmpty, _ := s.GetServer(empty)
	if gotEmpty.Location != "" || gotEmpty.Caveats != "" {
		t.Fatalf("unset fields should be empty: %+v", gotEmpty)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails (compile error)**

Run: `go test ./internal/store/ -run TestServerStructuredFieldsRoundTrip -v`
Expected: FAIL — compile error `unknown field 'Location' in struct literal of type models.Server` (the struct does not yet have the fields).

- [ ] **Step 3: Add the 5 fields to `models.Server`**

In `internal/models/models.go`, replace the `Server` struct block (lines 40-53) with:

```go
// Server is an SSH target. Credential holds the login secret; SudoCredential (optional) holds a password for sudo -S.
// Structured metadata fields (Location/Hardware/Services/Role/Caveats) plus Tags and Description are surfaced to
// the agent via list_servers (full-open, reversing Plan-8's owner-only rule — see
// docs/superpowers/specs/2026-08-11-server-structured-metadata-design.md).
type Server struct {
	ID                string
	Name              string
	Host              string
	Port              int
	User              string
	AuthMethod        AuthMethod
	CredentialID      string
	SudoCredentialID string // empty if none
	Tags              []string
	Description       string // owner free-text notes; surfaced to agent (supplementary to structured fields below)
	Location          string // where deployed: datacenter/region/rack/tenant
	Hardware          string // hardware config: CPU/RAM/disk/GPU
	Services          string // what is deployed/running here
	Role              string // this server's purpose (e.g. "prod pg primary")
	Caveats           string // operational gotchas; agent reads before acting
	CreatedAt         time.Time
	UpdatedAt         time.Time
}
```

(Removes the old line-39 "owner-only — never surfaced" sentence and the line-50 "not exposed via MCP tools" comment.)

- [ ] **Step 4: Add 5 columns to `schemaSQL` (servers block)**

In `internal/store/store.go`, in the `CREATE TABLE servers` block, replace:
```sql
  tags TEXT,
  description TEXT,
  created_at INTEGER NOT NULL,
```
with:
```sql
  tags TEXT,
  description TEXT,
  location TEXT,
  hardware TEXT,
  services TEXT,
  role TEXT,
  caveats TEXT,
  created_at INTEGER NOT NULL,
```

- [ ] **Step 5: Add 5 `addColumnIfMissing` calls to `migrate()`**

In `internal/store/store.go` `migrate()`, replace:
```go
	if err := addColumnIfMissing(db, "servers", "description", "TEXT"); err != nil {
		return err
	}
	if err := addColumnIfMissing(db, "projects", "status", "TEXT NOT NULL DEFAULT 'active'"); err != nil {
		return err
	}
```
with:
```go
	if err := addColumnIfMissing(db, "servers", "description", "TEXT"); err != nil {
		return err
	}
	if err := addColumnIfMissing(db, "servers", "location", "TEXT"); err != nil {
		return err
	}
	if err := addColumnIfMissing(db, "servers", "hardware", "TEXT"); err != nil {
		return err
	}
	if err := addColumnIfMissing(db, "servers", "services", "TEXT"); err != nil {
		return err
	}
	if err := addColumnIfMissing(db, "servers", "role", "TEXT"); err != nil {
		return err
	}
	if err := addColumnIfMissing(db, "servers", "caveats", "TEXT"); err != nil {
		return err
	}
	if err := addColumnIfMissing(db, "projects", "status", "TEXT NOT NULL DEFAULT 'active'"); err != nil {
		return err
	}
```

- [ ] **Step 6: Extend `scanServer` to read the 5 new columns**

In `internal/store/servers.go`, replace the `Scan` line (line 101):
```go
	if err := sc.Scan(&srv.ID, &srv.Name, &srv.Host, &srv.Port, &srv.User, &authMethod, &srv.CredentialID, &sudoCredentialID, &tagsJSON, &srv.Description, &createdAt, &updatedAt); err != nil {
```
with:
```go
	if err := sc.Scan(&srv.ID, &srv.Name, &srv.Host, &srv.Port, &srv.User, &authMethod, &srv.CredentialID, &sudoCredentialID, &tagsJSON, &srv.Description, &srv.Location, &srv.Hardware, &srv.Services, &srv.Role, &srv.Caveats, &createdAt, &updatedAt); err != nil {
```

- [ ] **Step 7: Extend `INSERT` in `AddServer`**

In `internal/store/servers.go`, replace the INSERT block (lines 17-21):
```go
	_, err := s.db.Exec(
		`INSERT INTO servers (id,name,host,port,user,auth_method,credential_id,sudo_credential_id,tags,description,created_at,updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, srv.Name, srv.Host, srv.Port, srv.User, string(srv.AuthMethod), srv.CredentialID, sudo, string(tagsJSON), srv.Description, ts, ts,
	)
```
with:
```go
	_, err := s.db.Exec(
		`INSERT INTO servers (id,name,host,port,user,auth_method,credential_id,sudo_credential_id,tags,description,location,hardware,services,role,caveats,created_at,updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, srv.Name, srv.Host, srv.Port, srv.User, string(srv.AuthMethod), srv.CredentialID, sudo, string(tagsJSON), srv.Description,
		srv.Location, srv.Hardware, srv.Services, srv.Role, srv.Caveats, ts, ts,
	)
```

- [ ] **Step 8: Extend `UPDATE` in `UpdateServer`**

In `internal/store/servers.go`, replace the UPDATE block (lines 34-37):
```go
	res, err := s.db.Exec(
		`UPDATE servers SET name=?,host=?,port=?,user=?,auth_method=?,credential_id=?,sudo_credential_id=?,tags=?,description=?,updated_at=? WHERE id=?`,
		srv.Name, srv.Host, srv.Port, srv.User, string(srv.AuthMethod), srv.CredentialID, sudo, string(tagsJSON), srv.Description, now(), srv.ID,
	)
```
with:
```go
	res, err := s.db.Exec(
		`UPDATE servers SET name=?,host=?,port=?,user=?,auth_method=?,credential_id=?,sudo_credential_id=?,tags=?,description=?,location=?,hardware=?,services=?,role=?,caveats=?,updated_at=? WHERE id=?`,
		srv.Name, srv.Host, srv.Port, srv.User, string(srv.AuthMethod), srv.CredentialID, sudo, string(tagsJSON), srv.Description,
		srv.Location, srv.Hardware, srv.Services, srv.Role, srv.Caveats, now(), srv.ID,
	)
```

- [ ] **Step 9: Extend all 3 `SELECT` column lists**

In `internal/store/servers.go`, replace the column list in each of the 3 SELECT statements (lines 49, 56, 67). Each currently reads:
```go
`SELECT id,name,host,port,user,auth_method,credential_id,sudo_credential_id,tags,description,created_at,updated_at FROM servers ...
```
Replace with:
```go
`SELECT id,name,host,port,user,auth_method,credential_id,sudo_credential_id,tags,description,location,hardware,services,role,caveats,created_at,updated_at FROM servers ...
```
(Three identical replacements — `GetServer`, `GetServerByName`, `ListServers`. The `FROM servers WHERE id=?` / `WHERE name=?` / `ORDER BY name` suffixes stay unchanged.)

- [ ] **Step 10: Run the test to verify it passes**

Run: `go test ./internal/store/ -run TestServerStructuredFieldsRoundTrip -v`
Expected: PASS.

- [ ] **Step 11: Run the full store package to confirm no regressions**

Run: `go test ./internal/store/ -v`
Expected: PASS (all existing tests still green — the new columns are nullable, old fixtures unaffected).

- [ ] **Step 12: Commit**

```bash
git add internal/models/models.go internal/store/store.go internal/store/servers.go internal/store/servers_test.go
git commit -m "feat(store): add structured server metadata fields (location/hardware/services/role/caveats)" -m "Persisted as nullable TEXT columns on servers (idempotent migration). Round-trip via AddServer/GetServer/GetServerByName/ListServers/UpdateServer. No agent surface change yet." -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: Migration idempotency for the 5 new columns

**Files:**
- Modify: `internal/store/migrate_test.go:11-42` (extend the old-shape fixture), `:66-75` (fresh-schema test)
- Consumes: Task 1's `migrate()` + `schemaSQL`.
- Produces: proof that an old-shape DB migrates to all 5 new columns, and a fresh DB has them.

- [ ] **Step 1: Extend `TestFreshSchemaHasNewColumns` to assert the 5 new columns**

In `internal/store/migrate_test.go`, replace `TestFreshSchemaHasNewColumns` (lines 67-75):
```go
func TestFreshSchemaHasNewColumns(t *testing.T) {
	s := newTestStore(t)
	if !hasColumn(t, s.db, "servers", "description") {
		t.Fatal("fresh servers table missing description column")
	}
	if !hasColumn(t, s.db, "projects", "status") {
		t.Fatal("fresh projects table missing status column")
	}
}
```
with:
```go
func TestFreshSchemaHasNewColumns(t *testing.T) {
	s := newTestStore(t)
	for _, col := range []string{"description", "location", "hardware", "services", "role", "caveats"} {
		if !hasColumn(t, s.db, "servers", col) {
			t.Fatalf("fresh servers table missing %s column", col)
		}
	}
	if !hasColumn(t, s.db, "projects", "status") {
		t.Fatal("fresh projects table missing status column")
	}
}
```

- [ ] **Step 2: Extend `TestMigrateAddsColumnsToOldShape` to assert the 5 new columns**

In `internal/store/migrate_test.go`, replace the assertion tail of `TestMigrateAddsColumnsToOldShape` (lines 96-101):
```go
	if !hasColumn(t, s.db, "servers", "description") {
		t.Fatal("migrate did not add servers.description")
	}
	if !hasColumn(t, s.db, "projects", "status") {
		t.Fatal("migrate did not add projects.status")
	}
```
with:
```go
	for _, col := range []string{"description", "location", "hardware", "services", "role", "caveats"} {
		if !hasColumn(t, s.db, "servers", col) {
			t.Fatalf("migrate did not add servers.%s", col)
		}
	}
	if !hasColumn(t, s.db, "projects", "status") {
		t.Fatal("migrate did not add projects.status")
	}
```

(The existing `oldShapeServersProjects` fixture already lacks the new columns — it models the pre-Plan-8 shape — so it doubles as the "missing all 5" fixture. No change to the const needed.)

- [ ] **Step 3: Run the migration tests**

Run: `go test ./internal/store/ -run 'TestFreshSchema|TestMigrate' -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/store/migrate_test.go
git commit -m "test(store): assert migration adds the 5 structured-metadata columns" -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: 4 KB field validation at the store layer

**Files:**
- Modify: `internal/store/servers.go` (add `validateServerText`, wire into `AddServer` + `UpdateServer`)
- Test: `internal/store/servers_test.go`
- Consumes: Task 1's `models.Server` fields.
- Produces: `AddServer`/`UpdateServer` reject any over-limit field with a named error.

- [ ] **Step 1: Write the failing test**

Add to `internal/store/servers_test.go`:

```go
func TestServerFieldSizeCap(t *testing.T) {
	s := newTestStore(t)
	cid := mustCred(t, s, models.CredPassword, "pw")

	// Exactly 4096 bytes is allowed.
	atLimit := strings.Repeat("x", 4096)
	id, err := s.AddServer(&models.Server{
		Name: "ok", Host: "h", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: cid, Caveats: atLimit,
	})
	if err != nil {
		t.Fatalf("at-limit field should be accepted: %v", err)
	}

	// 4097 bytes is rejected with a field-named error.
	overLimit := strings.Repeat("x", 4097)
	_, err = s.AddServer(&models.Server{
		Name: "big", Host: "h", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: cid, Caveats: overLimit,
	})
	if err == nil {
		t.Fatal("over-limit caveats should be rejected")
	}
	if !strings.Contains(err.Error(), "caveats") {
		t.Fatalf("error should name the field, got: %v", err)
	}

	// UpdateServer also enforces the cap.
	loaded, _ := s.GetServer(id)
	loaded.Hardware = strings.Repeat("h", 4097)
	if err := s.UpdateServer(loaded); err == nil {
		t.Fatal("UpdateServer over-limit hardware should be rejected")
	}

	// Per-tag cap.
	_, err = s.AddServer(&models.Server{
		Name: "taggy", Host: "h", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: cid, Tags: []string{strings.Repeat("t", 4097)},
	})
	if err == nil || !strings.Contains(err.Error(), "tag") {
		t.Fatalf("over-limit tag should be rejected with a tag-named error, got: %v", err)
	}
}
```

Add `"strings"` to the import block of `servers_test.go` (currently imports only `"testing"` and `"ssh-manager-mcp/internal/models"`).

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/store/ -run TestServerFieldSizeCap -v`
Expected: FAIL — the at-limit/over-limit AddServer calls both succeed (no validation yet); the first assertion that expects rejection (`over-limit caveats should be rejected`) fails.

- [ ] **Step 3: Add `validateServerText`**

Append to `internal/store/servers.go` (after `nullableString`):

```go
// maxServerTextFieldBytes caps each free-text server field and each individual tag.
// All of these fields flow into the agent's context on every list_servers call, so an
// uncapped field is a context-window DoS vector (intentional or accidental). Bytes —
// not runes — because bytes are the real context-budget boundary.
const maxServerTextFieldBytes = 4096

// validateServerText enforces the per-field/per-tag byte cap. Called by AddServer and
// UpdateServer before the write. No content/charset validation (free text); no tag count limit.
func validateServerText(srv *models.Server) error {
	for _, f := range []struct{ name, val string }{
		{"description", srv.Description},
		{"location", srv.Location},
		{"hardware", srv.Hardware},
		{"services", srv.Services},
		{"role", srv.Role},
		{"caveats", srv.Caveats},
	} {
		if len(f.val) > maxServerTextFieldBytes {
			return fmt.Errorf("server field %q exceeds %d-byte limit (%d)", f.name, maxServerTextFieldBytes, len(f.val))
		}
	}
	for i, tag := range srv.Tags {
		if len(tag) > maxServerTextFieldBytes {
			return fmt.Errorf("server tag[%d] exceeds %d-byte limit (%d)", i, maxServerTextFieldBytes, len(tag))
		}
	}
	return nil
}
```

- [ ] **Step 4: Wire validation into `AddServer`**

In `internal/store/servers.go` `AddServer`, after the `sudo := nullableString(srv.SudoCredentialID)` line (line 16) and before the INSERT, insert:
```go
	if err := validateServerText(srv); err != nil {
		return "", err
	}
```

- [ ] **Step 5: Wire validation into `UpdateServer`**

In `internal/store/servers.go` `UpdateServer`, after the `sudo := nullableString(srv.SudoCredentialID)` line (line 33) and before the UPDATE, insert:
```go
	if err := validateServerText(srv); err != nil {
		return err
	}
```

- [ ] **Step 6: Run the test to verify it passes**

Run: `go test ./internal/store/ -run TestServerFieldSizeCap -v`
Expected: PASS.

- [ ] **Step 7: Run the full store package**

Run: `go test ./internal/store/ -v`
Expected: PASS (Tasks 1-3 all green).

- [ ] **Step 8: Commit**

```bash
git add internal/store/servers.go internal/store/servers_test.go
git commit -m "feat(store): cap server text fields at 4KB (agent-context DoS bound)" -m "validateServerText runs in AddServer/UpdateServer; rejects over-limit fields/tags with a named error. Bytes, not runes." -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: MCP surface — ServerInfo + ListServersForProfile + list_servers description

**Files:**
- Modify: `internal/mcpserver/types.go:6-12` (`ServerInfo`)
- Modify: `internal/mcpserver/core.go:41-44` (populate)
- Modify: `internal/mcpserver/server.go:50` (tool description)
- Create: `internal/mcpserver/listmetadata_test.go`
- Consumes: Task 1's `models.Server` fields.
- Produces: `list_servers` returns all 12 fields per server to the agent.

- [ ] **Step 1: Write the failing test**

Create `internal/mcpserver/listmetadata_test.go`:

```go
package mcpserver

import (
	"bytes"
	"encoding/json"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// TestListServersSurfacesMetadata verifies the structured fields + Tags + Description
// are projected into ServerInfo (and thus into the agent's list_servers payload).
// Lightweight: no sshd needed — ListServersForProfile only reads the store.
func TestListServersSurfacesMetadata(t *testing.T) {
	st := newStore(t)
	cid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	sid, err := st.AddServer(&models.Server{
		Name: "gpu", Host: "10.0.0.5", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: cid,
		Role: "prod ml", Services: "jupyter, trainer",
		Caveats: "do not reboot 02-03:00",
		Location: "dc2 rack14", Hardware: "8x A100",
		Tags: []string{"gpu"}, Description: "owner notes",
	})
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := st.AddProfile("p")
	if err := st.GrantServers(pid, []string{sid}); err != nil {
		t.Fatal(err)
	}

	infos, err := ListServersForProfile(st, pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 {
		t.Fatalf("len = %d, want 1", len(infos))
	}
	info := infos[0]
	if info.Role != "prod ml" || info.Services != "jupyter, trainer" ||
		info.Caveats != "do not reboot 02-03:00" || info.Location != "dc2 rack14" ||
		info.Hardware != "8x A100" || info.Description != "owner notes" {
		t.Fatalf("ServerInfo missing structured fields: %+v", info)
	}
	if len(info.Tags) != 1 || info.Tags[0] != "gpu" {
		t.Fatalf("Tags = %v", info.Tags)
	}

	// snake_case JSON keys reach the agent.
	b, _ := json.Marshal(info)
	for _, key := range []string{`"role"`, `"services"`, `"caveats"`, `"location"`, `"hardware"`, `"tags"`, `"description"`} {
		if !bytes.Contains(b, []byte(key)) {
			t.Fatalf("JSON missing %s: %s", key, b)
		}
	}
}

// TestListServersHidesOutOfProfileMetadata: a server NOT granted to the profile must
// not appear at all (iron rule intact — fields ride the existing profile projection).
func TestListServersHidesOutOfProfileMetadata(t *testing.T) {
	st := newStore(t)
	cid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	inID, _ := st.AddServer(&models.Server{
		Name: "in", Host: "h", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: cid, Role: "visible", Caveats: "in-caveat",
	})
	_, _ = st.AddServer(&models.Server{
		Name: "out", Host: "h", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: cid, Role: "hidden", Caveats: "out-caveat",
	})
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{inID})

	infos, _ := ListServersForProfile(st, pid)
	if len(infos) != 1 || infos[0].Name != "in" {
		t.Fatalf("only the granted server should appear: %+v", infos)
	}
	if infos[0].Role != "visible" {
		t.Fatalf("granted server's fields missing: %+v", infos[0])
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/mcpserver/ -run 'TestListServersSurfacesMetadata|TestListServersHidesOutOfProfileMetadata' -v`
Expected: FAIL — `info.Role` / `info.Caveats` etc. do not exist on `ServerInfo` yet (compile error: `unknown field 'Role'`).

- [ ] **Step 3: Add 7 fields to `ServerInfo`**

In `internal/mcpserver/types.go`, replace the `ServerInfo` struct (lines 6-12):
```go
// ServerInfo is a Profile-scoped server as seen by the agent (no credentials).
type ServerInfo struct {
	ID      string `json:"id" jsonschema:"stable server id (use this in exec_command)"`
	Name    string `json:"name" jsonschema:"human-friendly server name"`
	Host    string `json:"host" jsonschema:"server host"`
	User    string `json:"user" jsonschema:"ssh user"`
	HasSudo bool   `json:"has_sudo" jsonschema:"true if sudo=true is supported on this server"`
}
```
with:
```go
// ServerInfo is a Profile-scoped server as seen by the agent (no credentials).
// Structured metadata fields (Role/Services/Caveats/Location/Hardware) plus Tags and
// Description are surfaced so the agent grasps each server's full picture. Caveats is
// placed before Location/Hardware so it reads prominently; empty strings mean "none".
type ServerInfo struct {
	ID      string `json:"id" jsonschema:"stable server id (use this in exec_command)"`
	Name    string `json:"name" jsonschema:"human-friendly server name"`
	Host    string `json:"host" jsonschema:"server host"`
	User    string `json:"user" jsonschema:"ssh user"`
	HasSudo bool   `json:"has_sudo" jsonschema:"true if sudo=true is supported on this server"`
	Role        string   `json:"role" jsonschema:"this server's purpose/role (e.g. 'prod pg primary')"`
	Services    string   `json:"services" jsonschema:"what is deployed/running on this server"`
	Caveats     string   `json:"caveats" jsonschema:"operational gotchas & special handling rules — READ BEFORE acting on this server; empty means none"`
	Location    string   `json:"location" jsonschema:"where this server is deployed (datacenter/region/rack/tenant)"`
	Hardware    string   `json:"hardware" jsonschema:"hardware configuration (CPU/RAM/disk/GPU)"`
	Tags        []string `json:"tags" jsonschema:"free-form labels"`
	Description string   `json:"description" jsonschema:"owner's free-text notes (supplementary; prefer structured fields above)"`
}
```

- [ ] **Step 4: Populate the new fields in `ListServersForProfile`**

In `internal/mcpserver/core.go`, replace the `ServerInfo` literal (lines 41-44):
```go
		out = append(out, ServerInfo{
			ID: srv.ID, Name: srv.Name, Host: srv.Host, User: srv.User,
			HasSudo: srv.SudoCredentialID != "",
		})
```
with:
```go
		out = append(out, ServerInfo{
			ID:      srv.ID,
			Name:    srv.Name,
			Host:    srv.Host,
			User:    srv.User,
			HasSudo: srv.SudoCredentialID != "",
			Role:        srv.Role,
			Services:    srv.Services,
			Caveats:     srv.Caveats,
			Location:    srv.Location,
			Hardware:    srv.Hardware,
			Tags:        srv.Tags,
			Description: srv.Description,
		})
```

- [ ] **Step 5: Update the `list_servers` tool description**

In `internal/mcpserver/server.go`, replace the `Description` of the `list_servers` tool (line 50):
```go
			Description: "List the SSH servers you may use. ALWAYS call this first to discover server ids and capabilities before exec_command. Returns id/name/host/user/has_sudo — never credentials.",
```
with:
```go
			Description: "List the SSH servers you may use. ALWAYS call this first to discover server ids and capabilities before exec_command. Returns id/name/host/user/has_sudo, plus owner-provided context: role, services (what's deployed), location, hardware, caveats (special handling — read before acting), tags, description. Never includes credentials.",
```

- [ ] **Step 6: Run the new tests to verify they pass**

Run: `go test ./internal/mcpserver/ -run 'TestListServersSurfacesMetadata|TestListServersHidesOutOfProfileMetadata' -v`
Expected: PASS.

- [ ] **Step 7: Run the full mcpserver package (incl. e2e)**

Run: `go test ./internal/mcpserver/ -v`
Expected: PASS — including the existing `TestE2EIronRule` (profile-authz unaffected; the seeded servers there carry no structured fields, so they surface as empty strings without breaking the `allowed`/`forbidden` substring assertions).

- [ ] **Step 8: Commit**

```bash
git add internal/mcpserver/types.go internal/mcpserver/core.go internal/mcpserver/server.go internal/mcpserver/listmetadata_test.go
git commit -m "feat(mcp): surface structured server metadata to the agent via list_servers" -m "ServerInfo gains Role/Services/Caveats/Location/Hardware + Tags + Description (full-open, reversing Plan-8 owner-only rule). Profile-scoping intact; zero eval/baseline/scorer changes." -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: CLI — add/edit flags, ls format

**Files:**
- Modify: `internal/cli/servers.go` (add command 18-86, edit 151-238, ls 88-124)
- Modify: `internal/cli/cli_smoke_test.go:57-138` (update `TestServersEditAndDescription`)
- Consumes: Tasks 1 + 3's `models.Server` fields + validation.
- Produces: `--location/--hardware/--services/--role/--caveats` flags on add/edit; ls shows role + caveats.

- [ ] **Step 1: Update `TestServersEditAndDescription` for the new flags + ls format**

In `internal/cli/cli_smoke_test.go`, replace the `add` + `ls` portion of `TestServersEditAndDescription` (lines 78-85):
```go
	// add WITH a description
	mustCli("servers", "add", "--name", "gpu", "--host", "h", "--user", "u",
		"--password", "pw", "--description", "8x A100")

	// ls surfaces the description (owner-only metadata)
	if out := mustCli("servers", "ls"); !bytes.Contains(out.Bytes(), []byte("8x A100")) {
		t.Fatalf("ls missing description: %s", out.String())
	}
```
with:
```go
	// add WITH structured fields + description
	mustCli("servers", "add", "--name", "gpu", "--host", "h", "--user", "u",
		"--password", "pw",
		"--role", "prod ml", "--caveats", "do not reboot 02-03:00",
		"--location", "dc2", "--hardware", "8x A100",
		"--services", "jupyter", "--description", "owner note")

	// ls now shows role + truncated caveats (description no longer on the ls line)
	if out := mustCli("servers", "ls"); !bytes.Contains(out.Bytes(), []byte("prod ml")) || !bytes.Contains(out.Bytes(), []byte("do not reboot 02-03:00")) {
		t.Fatalf("ls missing role/caveats: %s", out.String())
	}
	if out := mustCli("servers", "ls"); bytes.Contains(out.Bytes(), []byte("owner note")) {
		t.Fatalf("ls should not show description anymore: %s", out.String())
	}
```

Update the `before` inspection (lines 101-103) — `add` now sets the description to `"owner note"` (not `"8x A100"`) and the structured fields too. Replace:
```go
	if before.Description != "8x A100" {
		t.Fatalf("description = %q", before.Description)
	}
```
with:
```go
	if before.Description != "owner note" {
		t.Fatalf("description = %q", before.Description)
	}
	if before.Role != "prod ml" || before.Caveats != "do not reboot 02-03:00" {
		t.Fatalf("structured fields not stored: %+v", before)
	}
```

Also update the later `edit` assertion (lines 107-111) so the structured fields are exercised. Replace:
```go
	// edit host + description; id + cred preserved (no re-credential flag given)
	mustCli("servers", "edit", "gpu", "--host", "newhost", "--description", "9x A100")
	after := inspect()
	if after.Host != "newhost" || after.Description != "9x A100" {
		t.Fatalf("edit did not apply: %+v", after)
	}
```
with:
```go
	// edit host + role + clear caveats (pass empty); id + cred preserved (no re-credential flag given)
	mustCli("servers", "edit", "gpu", "--host", "newhost", "--role", "prod ml replica", "--caveats", "")
	after := inspect()
	if after.Host != "newhost" || after.Role != "prod ml replica" || after.Caveats != "" {
		t.Fatalf("edit did not apply: %+v", after)
	}
	if after.Description != "owner note" {
		t.Fatalf("description should be unchanged when --description not passed: %q", after.Description)
	}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/cli/ -run TestServersEditAndDescription -v`
Expected: FAIL — `unknown flag: --role` (cobra rejects the not-yet-registered flag).

- [ ] **Step 3: Add `"strings"` import + flags to the `add` command**

In `internal/cli/servers.go`, add `"strings"` to the import block (currently `fmt`, `os`, `cobra`, `models`):
```go
import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/models"
)
```

In `serversAddCmd`, extend the var block (lines 19-23) — replace:
```go
	var (
		name, host, user, password, keyPath, keyPass, sudoPassword, description string
		port                                                                    int
		tags                                                                    []string
	)
```
with:
```go
	var (
		name, host, user, password, keyPath, keyPass, sudoPassword, description string
		location, hardware, services, role, caveats                            string
		port                                                                    int
		tags                                                                    []string
	)
```

Replace the `Server` literal in `add` (lines 53-56):
```go
			srv := &models.Server{
				Name: name, Host: host, Port: port, User: user,
				AuthMethod: cred.Type.AuthMethodForServer(), CredentialID: cid, Tags: tags, Description: description,
			}
```
with:
```go
			srv := &models.Server{
				Name: name, Host: host, Port: port, User: user,
				AuthMethod: cred.Type.AuthMethodForServer(), CredentialID: cid, Tags: tags,
				Description: strings.TrimSpace(description),
				Location:    strings.TrimSpace(location),
				Hardware:    strings.TrimSpace(hardware),
				Services:    strings.TrimSpace(services),
				Role:        strings.TrimSpace(role),
				Caveats:     strings.TrimSpace(caveats),
			}
```

After the `--description` flag registration (line 81), add the 5 new flags and flip the `--description` help. Replace:
```go
	c.Flags().StringVar(&description, "description", "", "owner notes — hardware/purpose (NOT shown to the agent)")
```
with:
```go
	c.Flags().StringVar(&description, "description", "", "owner notes (shown to the agent); prefer structured fields below")
	c.Flags().StringVar(&location, "location", "", "where deployed (datacenter/region/rack/tenant) — shown to the agent")
	c.Flags().StringVar(&hardware, "hardware", "", "hardware config (CPU/RAM/disk/GPU) — shown to the agent")
	c.Flags().StringVar(&services, "services", "", "what is deployed/running here — shown to the agent")
	c.Flags().StringVar(&role, "role", "", "this server's purpose (e.g. 'prod pg primary') — shown to the agent")
	c.Flags().StringVar(&caveats, "special-handling", "", "operational gotchas / special handling rules — the agent reads this BEFORE acting")
```

> **Note:** the flag for caveats is named `--special-handling` (the field is `Caveats`), matching the user-facing concept "special situations to watch." The struct field stays `Caveats`.

- [ ] **Step 4: Add flags + Changed-merge to the `edit` command**

In `serversEditCmd`, extend the var block (lines 152-156) — replace:
```go
	var (
		newName, host, user, password, keyPath, keyPass, sudoPassword, description string
		port                                                                       int
		tags                                                                       []string
	)
```
with:
```go
	var (
		newName, host, user, password, keyPath, keyPass, sudoPassword, description string
		location, hardware, services, role, caveats                               string
		port                                                                       int
		tags                                                                       []string
	)
```

Add merge clauses after the existing `description`/`tags` handling (after line 188). Replace:
```go
			if cmd.Flags().Changed("description") {
				srv.Description = description
			}
			if cmd.Flags().Changed("tags") {
				srv.Tags = tags
			}
```
with:
```go
			if cmd.Flags().Changed("description") {
				srv.Description = strings.TrimSpace(description)
			}
			if cmd.Flags().Changed("tags") {
				srv.Tags = tags
			}
			if cmd.Flags().Changed("location") {
				srv.Location = strings.TrimSpace(location)
			}
			if cmd.Flags().Changed("hardware") {
				srv.Hardware = strings.TrimSpace(hardware)
			}
			if cmd.Flags().Changed("services") {
				srv.Services = strings.TrimSpace(services)
			}
			if cmd.Flags().Changed("role") {
				srv.Role = strings.TrimSpace(role)
			}
			if cmd.Flags().Changed("special-handling") {
				srv.Caveats = strings.TrimSpace(caveats)
			}
```

In the edit flag registration block, flip `--description` help and add the 5 flags. Replace (line 231):
```go
	c.Flags().StringVar(&description, "description", "", "owner notes — hardware/purpose (NOT shown to the agent)")
```
with:
```go
	c.Flags().StringVar(&description, "description", "", "owner notes (shown to the agent); prefer structured fields below")
	c.Flags().StringVar(&location, "location", "", "where deployed (datacenter/region/rack/tenant) — shown to the agent")
	c.Flags().StringVar(&hardware, "hardware", "", "hardware config (CPU/RAM/disk/GPU) — shown to the agent")
	c.Flags().StringVar(&services, "services", "", "what is deployed/running here — shown to the agent")
	c.Flags().StringVar(&role, "role", "", "this server's purpose — shown to the agent")
	c.Flags().StringVar(&caveats, "special-handling", "", "operational gotchas / special handling rules — pass \"\" to clear")
```

- [ ] **Step 5: Rework `ls` to show role + caveats, and rename `truncateDesc` → `truncate`**

In `serversListCmd`, replace the print loop body (lines 102-109):
```go
			for _, srv := range servers {
				sudo := "-"
				if srv.SudoCredentialID != "" {
					sudo = "sudo"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%-16s %-20s %s@%s:%d [%s] %s\n",
					srv.Name, srv.ID, srv.User, srv.Host, srv.Port, sudo, truncateDesc(srv.Description))
			}
```
with:
```go
			for _, srv := range servers {
				sudo := "-"
				if srv.SudoCredentialID != "" {
					sudo = "sudo"
				}
				role := srv.Role
				if role == "" {
					role = "-"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%-16s %-20s %s@%s:%d [%s] (%s) · %s\n",
					srv.Name, srv.ID, srv.User, srv.Host, srv.Port, sudo, role, truncate(srv.Caveats))
			}
```

Rename the helper (lines 115-124) — replace:
```go
// truncateDesc clips a description for the ls line (rune-safe; "" → "-").
func truncateDesc(d string) string {
	if d == "" {
		return "-"
	}
	if r := []rune(d); len(r) > 40 {
		return string(r[:37]) + "..."
	}
	return d
}
```
with:
```go
// truncate clips a free-text field for the ls line (rune-safe; "" → "-").
func truncate(s string) string {
	if s == "" {
		return "-"
	}
	if r := []rune(s); len(r) > 40 {
		return string(r[:37]) + "..."
	}
	return s
}
```

- [ ] **Step 6: Run the CLI tests to verify they pass**

Run: `go test ./internal/cli/ -v`
Expected: PASS — `TestServersEditAndDescription` (updated) + `TestServersAddAndListEndToEnd`.

- [ ] **Step 7: Commit**

```bash
git add internal/cli/servers.go internal/cli/cli_smoke_test.go
git commit -m "feat(cli): structured-metadata flags + ls shows role/caveats" -m "add/edit gain --location/--hardware/--services/--role/--special-handling; --description help flipped (now agent-visible). ls shows role + truncated caveats; description dropped from the ls line." -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 6: Docs (managing-servers, agent-access, Plan-8 annotation) + full build/test

**Files:**
- Modify: `docs/managing-servers.md`
- Modify: `docs/agent-access.md` (around line 95)
- Modify: `docs/superpowers/plans/2026-08-10-ssh-manager-mcp-plan-8-management-cli.md` (scope decision #7, line 36)
- Consumes: Tasks 1-5.
- Produces: owner + agent-facing docs aligned with full-open; Plan-8 reversal annotated.

- [ ] **Step 1: Update `docs/managing-servers.md`**

Find the section that coaches putting hardware/purpose/contact into `--description` (around line 64) and replace that coaching with structured-fields guidance. Insert/replace with:

```markdown
### Structured server metadata

Each server carries structured fields, all shown to the agent via `list_servers` so it
understands what each box is and how to act safely:

| Flag | Field | Example |
|---|---|---|
| `--location` | where deployed | `dc2 rack14` / `us-east-1a` |
| `--hardware` | hardware config | `8x A100 80GB, 1TB RAM` |
| `--services` | what's deployed/running | `postgres primary, prometheus` |
| `--role` | this server's purpose | `prod pg primary` |
| `--special-handling` | operational gotchas the agent must heed | `do not reboot 02-03:00; failover is manual` |

`--description` is supplementary free-text; `--tags` is free-form labels. Both are also
shown to the agent.

> ⚠️ **Do not put secrets in any of these fields.** Keys, tokens, and PII entered here
> travel into the agent's context and the upstream LLM provider on every `list_servers`
> call. Use the credential vault (`--password` / `--key`) for secrets, never these fields.

Each field is capped at 4 KB. Edit any field with `ssh-manager servers edit <name> --<flag> ...`;
pass an empty value (`--special-handling ""`) to clear.
```

(Remove the old "put hardware/purpose/owner/expiry in `--description`" sentence — that advice is now superseded by the structured fields. Keep all RFC5737 example IPs as-is.)

- [ ] **Step 2: Update `docs/agent-access.md` field list**

Find the line listing the `list_servers` return fields (around line 95), currently naming `id / name / host / user / has_sudo`, and replace the field list with:

```markdown
返回你 grant 给那个 profile 的 server 列表（`id` / `name` / `host` / `user` / `has_sudo`，加上 owner 提供的上下文：`role` / `services`（部署了什么）/ `location` / `hardware` / `caveats`（特殊处理——动手前先读）/ `tags` / `description`，**无凭据**）。
```

- [ ] **Step 3: Annotate the Plan-8 reversal**

In `docs/superpowers/plans/2026-08-10-ssh-manager-mcp-plan-8-management-cli.md`, scope decision #7 (around line 36), append to that decision's text:

```markdown

> **Update (2026-08-11):** the deferred "later decision" has now been made — reversed to
> **full-open**. Structured fields (location/hardware/services/role/caveats) plus tags and
> description are surfaced to the agent via `list_servers`. See
> `docs/superpowers/specs/2026-08-11-server-structured-metadata-design.md` and its
> implementation plan.
```

- [ ] **Step 4: Build + vet**

Run: `go build ./... && go vet ./...`
Expected: no output (success).

- [ ] **Step 5: Full test suite**

Run: `go test ./...`
Expected: PASS — store, mcpserver (incl. e2e), cli all green.

- [ ] **Step 6: Commit**

```bash
git add docs/managing-servers.md docs/agent-access.md docs/superpowers/plans/2026-08-10-ssh-manager-mcp-plan-8-management-cli.md
git commit -m "docs: structured server metadata + secret warning + Plan-8 reversal note" -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Self-Review (completed)

**1. Spec coverage:** Spec §5.1 → Task 1 (struct). §5.2 → Task 1 (schema/migrate/CRUD). §5.3 → Task 3 (4 KB cap). §5.4 → Task 4 (ServerInfo + populate + tool description). §5.5 → Task 5 (CLI). §5.6 → Tasks 1/3 (migration + validation error handling). §5.7 → Task 6 (docs/security). §6 (no eval changes) → Task 4 Step 7 verifies e2e still green. §7 testing → Tasks 1-5 each carry their tests. No spec section is unaddressed.

**2. Placeholder scan:** None — every code step shows full code; every test step shows a runnable test. The only prose-only steps (Task 6 docs) supply the actual text to insert.

**3. Type consistency:** Field names uniform across tasks: `Location/Hardware/Services/Role/Caveats` (Go) ↔ `location/hardware/services/role/caveats` (SQL/JSON). The CLI caveats flag is `--special-handling` mapping to field `Caveats` — called out explicitly in Task 5 Step 3 and used consistently in Steps 4-5 and the test. `validateServerText`, `truncate`, `maxServerTextFieldBytes`, `TestListServersSurfacesMetadata` referenced identically wherever they appear. The existing `TestServersEditAndDescription` update (Task 5 Step 1) is consistent with the new `ls` format (Task 5 Step 5).
