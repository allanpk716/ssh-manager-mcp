# Server Structured Metadata — Design Spec

**Date:** 2026-08-11
**Status:** Design — pending implementation plan
**Worktree/branch:** `allanpk716/增加记录服务器信息记录`

## 1. Problem

`Server.Description` (added in Plan 8) is a single free-text field that absorbs every kind of owner knowledge — hardware, purpose, contact, expiry, caveats. Operators coach themselves to stuff it all there (`docs/managing-servers.md`). Two consequences:

1. **Mixed signal** — no structure, so neither humans nor tooling can distinguish "this box has 8x H100" from "don't reboot 02–03:00."
2. **Agent blindness** — Plan 8 deliberately made `Description` (and `Tags`) **owner-only — never surfaced to the agent** (`internal/models/models.go:39,50`; Plan-8 scope decision #7). The MCP agent operating on a server has zero context about what it is, what runs on it, or what special handling it needs, and guesses when it hits something unusual.

## 2. Goal

Give every server **structured metadata fields**, surfaced to the agent via `list_servers`, so the agent grasps the full picture of each server it operates on — without mixing disparate facts into one free-text blob.

## 3. Key decision — reversing the Plan-8 owner-only rule

This design **reverses** Plan 8's "Description/Tags are owner-only" policy. Plan 8 anticipated exactly this: *"If the owner later wants the agent to see notes, that's a separate, iron-rule-relevant decision — not here"* (`docs/superpowers/plans/2026-08-10-ssh-manager-mcp-plan-8-management-cli.md:36`). This spec is that deferred decision.

**Decision: full-open.** All structured fields + `Tags` + `Description` become agent-visible. Rationale (owner's): the agent will actively manage servers and needs full context; the marginal risk of ambient exposure is accepted in exchange for the agent not guessing at special situations.

**Exposure-channel note (recorded, accepted):** surfacing a field via `list_servers` is *ambient* — every listing, into agent context + upstream LLM provider, with no per-read audit. This is distinct from the agent discovering the same fact on demand via an audited `exec_command`. The owner judges the convenience (one fewer round-trip, always-present context) worth the ambient-exposure cost. The agent already holds SSH power; the information is not secret-from-agent, only ambient-vs-on-demand.

## 4. Non-goals (out of scope, v1)

- **Agent write-back** — agent cannot mutate metadata (read-only v1). A `set_server_info` tool is v2.
- **Dedicated `get_server_info` tool** — all fields embed in `list_servers`. If fleet-scale token cost bites later, add a dedicated read tool then (this re-opens the T8-coverage question — see §9).
- **Secret-pattern lint** on field contents — light discipline only (doc + schema wording). v2 if needed.
- **Per-field visibility flags / field-level encryption** — all new fields are plaintext columns (same as `description`/`tags` today); no per-field agent-visibility toggle (full-open is a static split: everything visible).

## 5. Design

### 5.1 Data model — `Server` struct (`internal/models/models.go:40-53`)

Add 5 free-text fields after `Description` (line 50), before `CreatedAt`. No `json:`/`yaml:` tags — `Server` is persisted positionally, not serialized (matches existing style). Flip the two `Description` comments (lines 39, 50) to reflect that it is now surfaced.

```go
type Server struct {
    ID                string
    Name              string
    Host              string
    Port              int
    User              string
    AuthMethod        AuthMethod
    CredentialID      string
    SudoCredentialID  string // empty if none
    Tags              []string
    Description       string // owner free-text notes; NOW surfaced to agent (full-open)
    Location          string // where deployed: datacenter/region/rack/tenant
    Hardware          string // hardware config: CPU/RAM/disk/GPU
    Services          string // what is deployed/running here
    Role              string // this server's purpose (e.g. "prod pg primary")
    Caveats           string // operational gotchas; agent reads before acting
    CreatedAt         time.Time
    UpdatedAt         time.Time
}
```

### 5.2 Storage (`internal/store`)

**Schema (`store.go:168-181`, the `servers` CREATE block):** add 5 nullable `TEXT` columns after `description` (line 178):
```sql
location TEXT,
hardware TEXT,
services TEXT,
role TEXT,
caveats TEXT,
```

**Migration (`store.go:86-123`, `migrate()`):** add 5 guarded `addColumnIfMissing` calls alongside the existing `servers.description` one (line 116). Idempotent — existing DBs get empty columns; fresh DBs get them via `CREATE TABLE` (`migrate` runs before `initSchema`; `addColumnIfMissing` no-ops when the table is absent).

**Store layer (`servers.go`):** extend the column list in all 5 sites, inserting the 5 new columns between `description` and `created_at`:
- `INSERT` (lines 18–21) + its args (line 20)
- `UPDATE` (lines 34–37) + its args (line 36)
- 3× `SELECT` (lines 49, 56, 67)
- `scanServer` (lines 92–112): add `&srv.Location` … `&srv.Caveats` to the `Scan` (line 101), positioned after `&srv.Description`.

### 5.3 Field length cap — 4 KB, enforced at store layer

To bound the agent-context blast radius (every `list_servers` carries all fields × all in-profile servers), cap each text field at **4096 bytes**:

- **Scope:** `Description`, `Location`, `Hardware`, `Services`, `Role`, `Caveats`, and each individual `Tags` entry.
- **Unit:** bytes — the real context-budget boundary (for CJK, 4 KB ≈ 1300 chars, still generous).
- **Enforcement:** single chokepoint in the store layer — a private `validateServerText(srv *models.Server) error` called by both `AddServer` and `UpdateServer` before the write. Returns e.g. `fmt.Errorf("server field %q exceeds 4096-byte limit (%d)", name, len)`. The CLI surfaces it as a non-zero exit; no special handling.
- **No content/charset validation** (free text); no `Tags` count limit.

### 5.4 MCP surface — `ServerInfo` (`internal/mcpserver/types.go:6-12`)

Add 7 fields after `HasSudo`, each with a `jsonschema` description (these become the agent-facing property descriptions in the tool schema). Field order = agent reading order (identity → purpose → safety → context → supplementary). Explicit values, **no `omitempty`** — the agent sees a consistent schema, and `""` means "explicitly none" (meaningful especially for `caveats`).

```go
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

**Construction (`core.go:41-44`):** the single `ServerInfo{...}` literal — populate the 7 new fields from `srv` (`srv.Role`, `srv.Services`, `srv.Caveats`, `srv.Location`, `srv.Hardware`, `srv.Tags`, `srv.Description`).

**`list_servers` tool description (`server.go:50`):** update the final sentence. Current ends *"...Returns id/name/host/user/has_sudo — never credentials."* New ending: *"...Returns id/name/host/user/has_sudo plus owner-provided context: role, services (what's deployed), location, hardware, caveats (special handling — read before acting), tags, description. Never includes credentials."*

### 5.5 CLI (`internal/cli/servers.go`)

**`add` (lines 18–86):** add 5 string vars + 5 flags (`--location`, `--hardware`, `--services`, `--role`, `--caveats`); populate them in the `Server` literal (lines 53–56). Flip the `--description` flag help (line 81) from `"owner notes — hardware/purpose (NOT shown to the agent)"` to `"owner notes (shown to the agent); prefer structured fields --location/--hardware/--services/--role/--caveats"`.

**`edit` (lines 151–238):** same 5 vars + flags (in the 227–236 block); add `if cmd.Flags().Changed("<field>")` merge clauses mirroring `description`/`tags` (lines 183–188). Flip `--description` help (line 231) same as `add`. Clearing semantics: pass empty string (`--caveats ""`), consistent with existing `description`/`tags` behavior under `Changed()`.

**`ls` (lines 88–113):** surface the operationally-relevant fields. Replace the `truncateDesc(srv.Description)` tail with role + truncated caveats. Proposed format:
```
%-16s %-20s %s@%s:%d [%s] (%s) · %s
```
mapping to `name, id, user, host, port, sudo, role-or-"-", truncate(caveats)`. `Description` drops out of `ls` (it is supplementary; visible to the agent via `list_servers` and to the owner via `edit`). Rename/generalize `truncateDesc` → `truncate` (rune-safe clip to 40, `""` → `"-"`).

**Whitespace:** trim leading/trailing whitespace on all 5 new flags at parse (do the same for `--description` for consistency); preserve internal newlines (`caveats` may be multi-line).

### 5.6 Error handling

- **Migration failure:** existing behavior — `migrate()` errors bubble up through `store.Open`, which closes the DB. The new `addColumnIfMissing` calls are idempotent `ALTER TABLE ADD COLUMN` (SQLite-native, safe); no new failure mode.
- **4 KB violation:** store-layer `validateServerText` error → CLI non-zero exit with the field-named message. No retry, no silent truncation (the owner must choose what to cut).
- **Empty new columns on old rows:** nullable `TEXT`, scan as `""`. `ServerInfo` surfaces `""`. No nil-handling needed (strings, not pointers).

### 5.7 Security considerations

- **Full-open exposure:** all metadata fields now flow into agent context on every `list_servers`. The exposure is ambient (every call, upstream to the LLM provider) — see §3. Accepted by design decision.
- **Public repo:** no sample server config file exists in the repo (servers live only in operator-local gitignored `store.db`). Doc examples already use RFC5737 IPs (commit `5c3fec8`). The doc update (§5.8) must keep examples sanitized — no real hostnames/locations/hardware in committed docs.
- **Caveats discipline (light):** `docs/managing-servers.md` adds an explicit warning — *do not put secrets (keys/tokens/PII) in any agent-visible field; they enter agent context and the LLM provider upstream*. Reinforced by the `Caveats` jsonschema wording. No v1 content lint.
- **Encryption:** unchanged. New fields are plaintext columns, same as `description`/`tags`. The vault (`store.db`) stays behind the OS-keychain master key at the file level; no field-level encryption (none exists for `description`/`tags` either).

## 6. What does NOT change (verified against verbatim source)

- **Eval scorers T6 / T8 (zero-tolerance gates):** immune. T6 checks literal credential bytes in broker-tool results (`internal/eval/score.go:712`); T8 checks cross-profile `exec`/`download`/`upload` reach (`score.go:835`). Neither inspects `ServerInfo` field contents.
- **Baselines (`internal/eval/baseline*.json`):** store only pass/m counts per task; no field sets, no tool names. No edit.
- **`e2e_test.go:62`:** asserts profile-authorization (out-of-profile server `"forbidden"` absent, in-profile `"allowed"` present) — not field content. Adding fields does not break it; the only risk would be fixture content literally containing `"forbidden"`, which fixtures do not.
- **`BrokerTools` cardinality:** no new tool → stays 6. (No cardinality assertion exists in code anyway — only prose references.)
- **Profile-scoping:** new fields ride the existing `ListServersForProfile` → `ServersForProfile` projection; out-of-profile servers (and their fields) remain hidden. Iron rule intact.

## 7. Testing

- **Store round-trip (`internal/store/servers_test.go`):** set a `Server` with all 5 new fields + `Tags` + `Description`, `AddServer`, `GetServer`, assert all equal — including multi-line `caveats`, CJK content, and empty-string fields.
- **Migration idempotency (`internal/store/migrate_test.go`):** open a DB with the pre-feature `servers` shape (description+tags, no new columns), run `Open` (which migrates), assert the 5 new columns exist and old rows scan with empty strings. Run `Open` twice — second is a no-op.
- **4 KB cap (new test in `servers_test.go`):** `AddServer`/`UpdateServer` with a 4097-byte field → expect the named error; with exactly 4096 → ok. Per-tag cap for `Tags`.
- **CLI smoke (`internal/cli/cli_smoke_test.go`):** `add` with `--location/--hardware/--services/--role/--caveats`; `edit` each via `Changed`-merge + clear-via-empty; `ls` shows role + caveats.
- **MCP surface assertion (new test in `internal/mcpserver/`):** build a `Server` with all fields, call `ListServersForProfile`, marshal to JSON, assert all 7 new fields present with correct values (the mirror of the old "don't leak" — now "do surface"). Keep the existing profile-authz assertion (a forbidden server's fields don't leak either).

## 8. Implementation touch-set (file-by-file)

| File | Change |
|---|---|
| `internal/models/models.go` | +5 fields on `Server` (after line 50); flip `Description` comments (39, 50) |
| `internal/store/store.go` | +5 columns in `servers` `schemaSQL` (178); +5 `addColumnIfMissing` in `migrate` (116); `validateServerText` helper |
| `internal/store/servers.go` | extend `INSERT`(18)/`UPDATE`(34)/3×`SELECT`(49,56,67)/`scanServer`(101); call `validateServerText` in `AddServer`+`UpdateServer` |
| `internal/mcpserver/types.go` | +7 fields on `ServerInfo` (12) with jsonschema tags |
| `internal/mcpserver/core.go` | populate 7 fields in the `ServerInfo` literal (41-44) |
| `internal/mcpserver/server.go` | update `list_servers` Description final sentence (50) |
| `internal/cli/servers.go` | +5 flags on `add`(72-81) & `edit`(227-236); populate/merge; flip `--description` help (81, 231); `ls` format (107) + generalize `truncateDesc` |
| `docs/managing-servers.md` | replace "stuff description" coaching with structured-fields guidance + secret warning |
| `docs/agent-access.md` | update field list (95) |
| `docs/superpowers/plans/2026-08-10-...plan-8-management-cli.md` | annotate scope decision #7 (36): the deferred "later decision" is now made (reversal) |

## 9. Future work (explicitly deferred)

- `get_server_info` (dedicated read tool) and `set_server_info` (write tool) — if fleet-scale token cost or agent write-back becomes a real need. Adding a tool re-opens T8 coverage: T8's tool list is hardcoded to `exec`/`download`/`upload` (`score.go:848`), so a new tool must self-gate `…ForProfile` and be folded into `scoreT8`.
- Secret-pattern lint on field contents.
- Per-field visibility toggle or field-level encryption (not needed under static full-open).

## 10. References

- Plan-8 scope decision #7: `docs/superpowers/plans/2026-08-10-ssh-manager-mcp-plan-8-management-cli.md:36`
- Pre-public scrub (RFC5737 doc IPs, `.gitignore` hardening): commit `5c3fec8`
- Eval scorer semantics: `internal/eval/score.go` T6 (712), T8 (835); baselines `internal/eval/baseline*.json`
