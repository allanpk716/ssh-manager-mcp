# ssh-manager-mcp Plan 8 — Management CLI (server notes, edit, project lifecycle)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the owner-side management gaps surfaced in the `/grill-me` session. The vault + broker + MCP agent surface (Plans 1–6) are unchanged; this plan adds **owner CLI** affordances the user found missing:
1. **Server notes** — a free-text `description` on each server (hardware/purpose), shown in `servers ls`.
2. **`servers edit`** — change any field and/or re-credential **in place** (server id + profile bindings preserved; today the only path is `rm`+`add`, which rotates the id and breaks `profile_servers`).
3. **Project lifecycle (Lazy)** — `projects rotate` (re-key a token in place), `disable` / `enable` (suspend/resume), `revoke` (permanent), plus `projects show <name>` (the agent→profile→servers view the user asked for) and a `status` column in `projects ls`.

**Architecture:** Pure owner-CLI + store-layer work. **No broker, no MCP, no daemon, no web.** Two SQLite columns added with idempotent migration; the agent surface (`BrokerTools`, the §12 scorers, the iron rule) is **untouched**. The only agent-visible effect is intended: a revoked/disabled/rotated project's token stops resolving at the next `mcp` spawn (`VerifyToken` gains a `status='active'` filter) — a running session is **not** live-killed (Lazy, the user's Q1 choice).

**Tech Stack:** Go 1.24; the existing SQLite store (`modernc.org/sqlite`), `cobra` CLI, `argon2` token hashing (already in `internal/store/token.go`). No new deps.

## Global Constraints

- **Agent surface unchanged.** `BrokerTools`, `internal/mcpserver/*`, `internal/sshbroker/*`, `internal/eval/*` are NOT modified by this plan. The §12 gate / scorers are irrelevant here. **No-regression bar = `go test ./...` green** (no LLM, no conformance Docker needed).
- **Lazy lifecycle.** `revoke`/`disable`/`enable`/`rotate` take effect at the next `mcp` process spawn (`RunStdio` → `VerifyToken`). A currently-running agent session keeps its access until Claude Code restarts its MCP child. This is the deliberate Q1 choice; a live hard-kill is OUT OF SCOPE.
- **Soft delete.** `revoke` sets `status='revoked'` (row kept for audit, hidden from default `projects ls`, shown with `--all`). The user's mental model is "gone"; soft-delete gives the same UX (token dead, hidden) + keeps the audit row. Hard-delete is a one-line change later if ever wanted.
- **`rotate` is in-place.** Same project `id` + `profile_id`; only `token_hash`/`token_salt`/`token_prefix` are replaced. Audit continuity preserved.
- **Idempotent migration.** `CREATE TABLE IF NOT EXISTS` does not add columns to existing tables, so `migrate()` adds the two new columns via guarded `ALTER TABLE … ADD COLUMN` (check `PRAGMA table_info` first). A fresh DB already has the columns via `schemaSQL`; a pre-Plan-8 DB gets them via `migrate()`. No data loss; existing rows get `description=''` and `status='active'`.
- **Self-use.** The management surface has no auth of its own — it runs locally after `unlock` (the vault master key is the gate). No multi-user concerns (user's Q5).
- **Lifecycle events audited.** `rotate`/`disable`/`enable`/`revoke` write an `audit_log` row (`action='project.rotate'|'project.disable'|…`, `project_id` set, `server_id` NULL, `status='ok'`). Owner-side `servers add/edit` are NOT audited (matches the current pattern — only broker tool calls + now the security-relevant project lifecycle are).
- **Hygiene:** `.gitattributes` LF; `gofmt -l .` empty; `go vet ./...` clean; one logical commit per task; messages end `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.
- **Branch:** `plan-8-management-cli`, base master HEAD.

---

## Scope decisions (surfaced for plan review)

1. **`servers edit` = full-row update with partial-flag merge.** The CLI loads the existing server, applies only the flags the operator passed (cobra `cmd.Flags().Changed("x")`), then `UpdateServer` writes the whole row. No partial-SQL complexity. `name` is mutable (rename supported). Re-credential (`--password`/`--key`) creates a new `credentials` row and repoints `credential_id`; the old credential row is **orphaned** (encrypted, unreferenced — harmless). Garbage-collecting orphan creds is a possible follow-up, not this plan.
2. **Password/key still mutually exclusive on `edit`.** Same rule as `add`; enforced in the CLI handler.
3. **`projects show <name>`** resolves project → profile name → `[]server{name, host, user}` and prints a readable tree. It does NOT reveal credentials (the iron rule holds on the owner side too — `show` never reads secret bytes).
4. **`status` values:** `active` | `disabled` | `revoked`. `VerifyToken` admits only `active`. `disable`→`disabled`, `enable`→`active`, `revoke`→`revoked`. `rotate` leaves `status` untouched (a rotated project is still active — you wanted a new key, not a suspension).
5. **`projects ls` default hides `revoked`**, shows `disabled` (so you notice a suspended agent). `--all` shows everything including revoked. (Revoked = "deleted" in the user's model → hidden by default.)
6. **No `profiles revoke <server>`** (the "shrink a profile" lever). The user's Q2 answer was "杀" (kill whole project) only; narrowing a live profile is OUT OF SCOPE. (`profiles grant` stays the only profile-mutation command.)
7. **Agent `list_servers` output unchanged.** `description` is owner-side metadata; it is **not** leaked to the agent. (The agent sees `id/name/host/user/has_sudo` only, as today.) If the owner later wants the agent to see notes, that's a separate, iron-rule-relevant decision — not here.

---

## File Structure

**Modified:**
- `internal/models/models.go` — `Server.Description`; new `ProjectStatus` type + consts; `Project.Status`.
- `internal/store/store.go` — `schemaSQL` adds `servers.description`, `projects.status`; `migrate()` adds the two columns idempotently (`addColumnIfMissing` helper).
- `internal/store/servers.go` — `AddServer` + all `SELECT`s + `scanServer` carry `description`; new `UpdateServer(srv)`.
- `internal/store/projects.go` — `VerifyToken` gains `status='active'` filter; `ListProjects` reads `status`; new `GetProjectByName`, `SetProjectStatus`, `RotateProject`.
- `internal/cli/servers.go` — `add` gains `--description`; new `edit` subcommand; `ls` shows a description column.
- `internal/cli/projects.go` — new `show` / `rotate` / `disable` / `enable` / `revoke` subcommands; `ls` gains `--all` + `status` column.
- `internal/cli/root.go` — register the new subcommands.
- `README.md` — "Manage servers & projects" section: `servers edit`, the `projects` lifecycle, `projects show`.

**New tests (modify existing `*_test.go`):**
- `internal/store/store_test.go` (or a new `migrate_test.go`) — migration idempotency + fresh-DB-has-columns.
- `internal/store/servers_test.go` — `description` round-trip; `UpdateServer` changes fields + preserves id; re-credential repoints `credential_id`.
- `internal/store/projects_test.go` — `VerifyToken` rejects `disabled`/`revoked`; `enable` re-admits; `RotateProject` invalidates old token, admits new, id/profile unchanged; `SetProjectStatus`.
- `internal/cli/ssh_smoke_test.go` (or `servers` smoke) — `servers edit` + `ls` description; `projects show` / `rotate` / `disable`/`enable`/`revoke` smoke.

---

## Task 1: Schema migration + model fields (`description`, `status`)

**Goal:** Add the two columns to the schema + an idempotent migration, and the model fields. No behavior change yet.

**Files:** `internal/models/models.go`, `internal/store/store.go`, `internal/store/store_test.go` (or new `migrate_test.go`).

- [ ] **Step 1: Write the failing migration test.** Open a fresh DB (existing test helper), assert `servers.description` and `projects.status` columns exist via `PRAGMA table_info`; open a pre-Plan-8 DB (create with the OLD schema sans the two columns, close, reopen via `Open`) and assert the columns appear after `migrate()` runs AND that re-opening (running migrate twice) does not error ("duplicate column").
- [ ] **Step 2: Run to fail** — columns absent on the old-shape DB.
- [ ] **Step 3: models.go** — add `Description string` to `Server`; add `type ProjectStatus string` with `ProjectActive="active"`, `ProjectDisabled="disabled"`, `ProjectRevoked="revoked"`; add `Status ProjectStatus` to `Project`.
- [ ] **Step 4: store.go** — add `description TEXT,` to the `servers` block and `status TEXT NOT NULL DEFAULT 'active',` to the `projects` block of `schemaSQL`. Add `addColumnIfMissing(db, table, col, decl)` and call it in `migrate()` for both columns (after the existing `host_keys` migration).
- [ ] **Step 5: Test passes** + `go test ./internal/store/` green + `gofmt`/`vet` clean.
- [ ] **Step 6: Commit** — `feat(store): servers.description + projects.status cols (idempotent migrate) (Plan 8 T1)` + Co-Authored-By.

---

## Task 2: Store — server `description` round-trip + `UpdateServer`

**Goal:** `description` flows through add/get/list; a full-row `UpdateServer` exists for `edit`.

**Files:** `internal/store/servers.go`, `internal/store/servers_test.go`.

- [ ] **Step 1: Failing tests** — `description` set on `AddServer` is returned by `GetServer`/`GetServerByName`/`ListServers`; `UpdateServer` changes `host`/`port`/`user`/`description`/`tags`/`name` and the row keeps the same `id`; `UpdateServer` repoints `credential_id` when the caller set a new one (re-credential simulation: caller creates a new cred, sets `srv.CredentialID`, calls `UpdateServer`).
- [ ] **Step 2: Run to fail** — `UpdateServer` undefined; `description` not scanned.
- [ ] **Step 3: servers.go** — add `description` to the `INSERT` in `AddServer` and to every `SELECT` (`GetServer`, `GetServerByName`, `ListServers`); scan it in `scanServer`; implement:
```go
func (s *Store) UpdateServer(srv *models.Server) error {
    tagsJSON, _ := json.Marshal(srv.Tags)
    sudo := nullableString(srv.SudoCredentialID)
    res, err := s.db.Exec(
        `UPDATE servers SET name=?,host=?,port=?,user=?,auth_method=?,credential_id=?,sudo_credential_id=?,tags=?,description=?,updated_at=? WHERE id=?`,
        srv.Name, srv.Host, srv.Port, srv.User, string(srv.AuthMethod), srv.CredentialID, sudo, string(tagsJSON), srv.Description, now(), srv.ID,
    )
    if err != nil { return err }
    n, _ := res.RowsAffected()
    if n == 0 { return fmt.Errorf("server %q not found", srv.ID) }
    return nil
}
```
- [ ] **Step 4: Tests pass** + package green.
- [ ] **Step 5: Commit** — `feat(store): Server.Description round-trip + UpdateServer (Plan 8 T2)` + Co-Authored-By.

---

## Task 3: Store — project `status` enforcement + lifecycle + rotate

**Goal:** `VerifyToken` admits only `active`; lifecycle + rotate functions exist. **This is the load-bearing security task** (adversarial tests, per the project's iron-rule discipline).

**Files:** `internal/store/projects.go`, `internal/store/projects_test.go`.

- [ ] **Step 1: Failing adversarial tests:**
  - `VerifyToken` on an `active` project → returns the project (existing behavior).
  - `SetProjectStatus(id, disabled)` → `VerifyToken` now returns `nil` (token rejected) **even with the correct token**.
  - `SetProjectStatus(id, active)` (enable) → `VerifyToken` admits again.
  - `SetProjectStatus(id, revoked)` → `VerifyToken` returns `nil`.
  - `RotateProject(id)` → returns a new token; the **old** token → `VerifyToken` returns `nil`; the **new** token → admits; the project `id` and `profile_id` are unchanged (assert via a new `GetProject(id)`/`GetProjectByName`).
  - `GetProjectByName(name)` resolves (returns `nil, nil` when absent, mirroring `GetServerByName`).
- [ ] **Step 2: Run to fail** — new symbols undefined; `VerifyToken` still admits disabled.
- [ ] **Step 3: projects.go** — change `VerifyToken` query to `… WHERE token_prefix=? AND status='active'`; add `status` to `ListProjects` `SELECT` + scan; implement `GetProjectByName`, `SetProjectStatus(id string, status models.ProjectStatus)`, and:
```go
func (s *Store) RotateProject(id string) (string, error) {
    token, err := GenerateToken()
    if err != nil { return "", err }
    salt := newSalt()
    hash := HashToken([]byte(token), salt)
    prefix := tokenPrefix(token)
    res, err := s.db.Exec(`UPDATE projects SET token_hash=?,token_salt=?,token_prefix=?,updated_at=? WHERE id=?`,
        hash, salt, prefix, now(), id)
    if err != nil { return "", err }
    if n, _ := res.RowsAffected(); n == 0 { return "", fmt.Errorf("project %q not found", id) }
    return token, nil
}
```
- [ ] **Step 4: Adversarial tests pass** (esp. disabled-with-correct-token → rejected). Package green.
- [ ] **Step 5: Commit** — `feat(store): project status enforcement + rotate/lifecycle (Plan 8 T3)` + Co-Authored-By. Note in the body: this is the Lazy enforcement point — `VerifyToken` now filters `status='active'`.

---

## Task 4: CLI — `servers add --description`, `servers edit`, `ls` column

**Goal:** Owner can annotate, edit (incl. re-credential), and see notes.

**Files:** `internal/cli/servers.go`, `internal/cli/root.go`, `internal/cli/ssh_smoke_test.go` (or a `servers` smoke test).

- [ ] **Step 1: Failing CLI tests** — `servers add … --description "X"` → `servers ls` shows "X"; `servers edit <name> --host <new>` → `GetServerByName` reflects new host, **same id**, still in its profile (grant a server to a profile, edit it, assert `ServersForProfile` still lists it); `servers edit <name> --password <new>` → re-credential succeeds, id unchanged.
- [ ] **Step 2: Run to fail** — `edit` undefined; `--description` unknown.
- [ ] **Step 3: servers.go** — add `--description` flag to `add`; implement `serversEditCmd` using `cmd.Flags().Changed("x")` to merge only provided flags onto the loaded server (load via `GetServerByName`, apply, `UpdateServer`); re-credential block mirrors `add` (mutual-exclusion check, `SetCredential`, repoint `srv.CredentialID` + `srv.AuthMethod`); add a truncated `description` field to the `servers ls` line. Register `serversEditCmd()` in `newServersCmd`.
- [ ] **Step 4: Tests pass.**
- [ ] **Step 5: Commit** — `feat(cli): servers edit + --description + ls notes column (Plan 8 T4)` + Co-Authored-By.

---

## Task 5: CLI — `projects show` / `rotate` / `disable` / `enable` / `revoke` / `ls --all` (+ lifecycle audit)

**Goal:** The full project lifecycle + the agent→profile→servers view, each security-relevant op audited.

**Files:** `internal/cli/projects.go`, `internal/cli/root.go`, `internal/store/audit.go` (consume existing writer), CLI tests.

- [ ] **Step 1: Read `internal/store/audit.go`** to get the exact audit-writer signature (record fields + func name) the broker uses; the CLI will call the same writer with `project_id` set, `server_id` NULL, `action`/`status` per op.
- [ ] **Step 2: Failing CLI tests** — `projects rotate <name>` prints a new token + `.mcp.json` snippet, and the OLD token (captured from the original `projects add`) no longer resolves (`VerifyToken` → nil) while the new one does; `projects disable` → `VerifyToken` nil; `enable` → admits; `revoke` → nil + hidden from default `projects ls`, visible with `--all`; `projects show <name>` prints the profile name + each granted server's name/host/user (no secrets); each of rotate/disable/enable/revoke writes one `audit_log` row with the expected `action`.
- [ ] **Step 3: Run to fail.**
- [ ] **Step 4: projects.go** — `projectsShowCmd` (resolve project→profile→`ServersForProfile`, print tree, no cred reads); `projectsRotateCmd` (`GetProjectByName` → `RotateProject` → print token + snippet → audit `project.rotate`); `projectsDisableCmd`/`projectsEnableCmd` (`SetProjectStatus` → audit); `projectsRevokeCmd` (`SetProjectStatus` revoked → audit); `projects ls --all` + a `status` column. Register all in `newProjectsCmd`.
- [ ] **Step 5: Tests pass** + `go test ./...` green (full no-regression bar).
- [ ] **Step 6: Commit** — `feat(cli): projects show/rotate/disable/enable/revoke + lifecycle audit (Plan 8 T5)` + Co-Authored-By.

---

## Task 6: README docs + verify + review + merge

**Goal:** Document the management surface, confirm green, final review, merge.

**Files:** `README.md`.

- [ ] **Step 1: README** — add a "Manage servers & projects" section: `servers add --description` / `servers edit` (incl. re-credential, id preserved), `projects show <name>`, the lifecycle (`rotate`/`disable`/`enable`/`revoke`) with the **Lazy** caveat (takes effect at next `mcp` spawn; a running session is not live-killed), and `projects ls [--all]`. State that `description` is owner-only (not surfaced to the agent).
- [ ] **Step 2: Verify** — `go test ./...` green; `gofmt -l .` empty; `go vet ./...` clean.
- [ ] **Step 3: Final whole-branch review** — focus on: T3 `VerifyToken` `status='active'` filter is the only enforcement path (no bypass); `rotate` truly in-place (id/profile unchanged); migration idempotent across fresh + old-shape DBs; `servers edit` re-credential orphans the old cred cleanly; lifecycle audit rows are written. Resolve findings in one fix wave.
- [ ] **Step 4: Merge to master (`--no-ff`)** per the user's finishing choice.

---

## Self-Review (run before handoff)

1. **Spec coverage (from the grill):** server notes (✓ T1/T2/T4), `servers edit` incl. re-credential with id/bindings preserved (✓ T2/T4), `projects show` view (✓ T5), project lifecycle disable/enable/revoke + rotate in-place (✓ T3/T5), Lazy enforcement (✓ T3 `status='active'` filter), self-use/no-web (✓ — no surface added). Agent surface UNCHANGED (no BrokerTools/MCP/broker/eval edits).
2. **Placeholder scan:** the exact `audit.go` writer signature (T5 S1) and the test-helper names are "verify on read" items with the contracts specified — not TBDs. No `<...>` placeholders in code blocks.
3. **Type consistency:** `models.ProjectStatus` → `Project.Status` → `projects.status` column → `VerifyToken` filter + `SetProjectStatus`. `Server.Description` → `servers.description` → `UpdateServer`/`scanServer`/CLI flag. `RotateProject` returns `(token, error)` consumed by `projectsRotateCmd`.
4. **Scope:** 6 tasks. T1 (schema+models), T2 (server store), T3 (project store — load-bearing security), T4 (server CLI), T5 (project CLI + audit), T6 (docs+merge). No LLM, no conformance Docker, no §12 eval. The load-bearing risk is T3's enforcement — reviewers must confirm a disabled/revoked project cannot resolve its token by any path (only `VerifyToken` admits, and it now filters `status='active'`).

---

## Execution Handoff

**Subagent-Driven (recommended):** T1–T5 sonnet (pure Go store/CLI + tests, no LLM); T6 sonnet docs + a final opus whole-branch review focused on T3 enforcement + migration idempotency. **No Fable-5/$ required** — correctness is proven by unit/adversarial tests, not the §12 gate (the agent surface is untouched). Merge per the user's choice (`--no-ff` to master, matching Plan 5c/5d/5e/6).

**Honest scope note:** this is the smallest plan in the series by risk — it adds no agent surface and no crypto. The one security-relevant change is T3's `status='active'` filter in `VerifyToken`; everything else is owner ergonomics. If a smaller cut is wanted: T1+T2+T4 (server notes + edit) deliver the most-felt gap (re-credential without `rm`+`add`); T3+T5 (lifecycle) can follow. Recommend the full plan (the lifecycle was the user's explicit Q4 discussion outcome).
