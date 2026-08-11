# Plan 12 — Offline Read-Only Cache (merged P12 + P13) — Design Spec

**Date:** 2026-08-11
**Status:** Design — pending implementation plan
**Worktree/branch:** `worktree-plan-12-offline-cache`

> Absorbs the former Plan 13 ("vault 复制：服务器→工作机同步机制"). The roadmap table in
> `docs/multi-machine.md` is renumbered by this plan: old P14 (群晖自动备份) → **P13**,
> old P15 (迁移+enroll) → **P14**.

## 1. Problem

A work machine that uses the VLAN `serve` broker (Plan 10) for its credential list is **dead
when the broker is unreachable** — broker down, VLAN split, laptop taken off-site. Plan 11's
`export`/`import` gives a portable backup, but it is (a) **manual**, (b) loads into a
**writable** vault (no read-only guarantee — an agent can mutate a stale copy and believe it
persisted), and (c) **passphrase**-encrypted (a cache that re-prompts per use is unworkable;
passphrase-derived DEK was explicitly rejected during the 2026-08-11 grill). There is no
"offline" today — `docs/multi-machine.md` limitation #2 says so outright.

## 2. Goal

Each work machine holds an **encrypted, read-only snapshot of the whole vault**, **auto-refreshed
from the broker**, served to the local agent over stdio exactly like the online broker — same
tools, same profile scoping, same iron rule — so the agent's reach **offline == online**, and the
only thing that changes is that **mutations are refused**.

## 3. Key decisions (from the 2026-08-11 brainstorm; recording the why)

1. **Merge P12 + P13.** Pull transport + cache + read-only serving + auto-refresh delivered as
   one plan. The user chose the bigger scope over the incremental "cache-only, manual load"
   option, accepting a larger plan in exchange for no half-built intermediate state.

2. **Pull auth = a new, owner-issued per-device "authorization code"** (`cache-tokens`), CLI-managed
   (`add`/`ls`/`revoke`), **not** a project token. The endpoint returns the **whole** `Snapshot`.
   - *Why:* a project token is profile-scoped by the iron rule; broadening it to dump the whole
     vault would let any agent token exfiltrate every credential. The cache pull is an **owner**
     operation, so it gets its own disjoint credential type. Two gates, never bridged.

3. **Cache encryption = a random DEK in the work machine's OS keychain** (same mechanism as the
   existing master key via `KeyringKeyProvider`, distinct service/user slot). **Passphrase-derived
   DEK was rejected** (re-prompt is unworkable for a cache); the DEK is high-entropy, so no KDF.

4. **Auto-refresh via the OS scheduler** (systemd timer / Windows Task Scheduler / launchd) running
   `cache pull` periodically — **not** an in-process daemon. `cache pull` is already one-shot and
   idempotent; the OS is the "background". Mirrors how `serve` itself is run under systemd.

5. **Read-only enforced at the store layer** (defense in depth): a `readOnly` flag on `*Store`
   makes every mutation method return `ErrReadOnly`. The broker literally cannot mutate even by
   accident. Audit is the **one** exception — routed to a local sidecar (decision 7).

6. **Cache-mode stdio still takes `--token` and verifies it against the cache.** The snapshot
   carries `projects` rows verbatim (token hash/salt preserved — proven by Plan 11's round-trip
   test). So the **same project token** the agent uses online validates offline against the cache,
   profile scoping + iron rule stay intact, and the agent's reach is identical. This is the
   invariant that makes the agent surface genuinely unaware of online vs offline.

7. **Offline audit → local sidecar** (`~/.ssh-manager/cache-audit.log`, JSONL, append). **Not**
   auto-merged back (single-direction, zero-merge — a grilled-in constraint). Offline audit is
   per-machine; the owner greps it.

8. **Unknown host key offline → reject (fail-closed).** Read-only mode cannot pin new keys; a
   refresh resolves it. This is stricter than online TOFU and intentionally so (no write path =
   no pinning path = no MITM-then-pin).

## 4. Non-goals (out of scope, v1)

- **Live hot-reload** of a running stdio broker when the cache file is refreshed. A refresh takes
  effect on the **next `mcp` spawn** (stdio brokers are per-session, short-lived). v2 if needed.
- **Online/offline auto-fallback** in `.mcp.json`. Switching is manual: online points Claude Code
  at the `serve` URL; offline points it at the local stdio cache broker (same project token).
- **Owner-bypass / full-vault access in cache mode.** v1 requires a project token (consistent,
  scoped). An `--owner` cache flag is v2 if the owner wants unscoped cache access.
- **Delta/incremental sync.** Every pull is a **full snapshot** (zero-merge). v2 if the vault grows
  large enough to matter.
- **Multi-server fanout / hub replication.** One authoritative broker, pulled by N work machines.
- **`cache load <export-file>`** (manual population from a Plan-11 export). Deliberately omitted so
  the cache model stays "cache = pulled". Disaster-recovery enrollment that can't reach the broker
  uses Plan-11 `import` into a normal writable vault instead (documented).

## 5. Design

### 5.1 Data model — new `cache_tokens` table (`internal/store/store.go` schema + migrate)

Mirrors `projects`'s token model exactly (reuse `GenerateToken`/`HashToken`/`verifyTokenHash`/`tokenPrefix`
from `token.go` + `projects.go`):

```sql
CREATE TABLE IF NOT EXISTS cache_tokens (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  token_hash BLOB NOT NULL,
  token_salt BLOB NOT NULL,
  token_prefix TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'active',   -- 'active' | 'revoked' (Lazy: revoked rejects even w/ correct secret)
  last_pull_at INTEGER,                     -- NULL until first pull; updated on each /snapshot fetch
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
```

`status` uses the same Lazy semantics as `projects.status`: `VerifyCacheToken` filters
`status='active'`; `revoke` sets `status='revoked'` and the next pull is rejected. `last_pull_at`
is bumped server-side on a successful `/snapshot` (operational visibility — "when did the laptop
last pull?"). No FKs (cache tokens are owner-level, not scoped to a profile).

### 5.2 Store layer — new `internal/store/cachetoken.go` (mirrors `projects.go`)

- `AddCacheToken(name string) (id, plaintext string, err error)` — `GenerateToken` →
  `HashToken`/`newSalt` → INSERT → return one-time plaintext (same shape as `AddProject`).
- `VerifyCacheToken(token string) (*CacheToken, error)` — prefix-filtered scan, constant-time
  hash compare, active-only. Returns `nil, nil` on no match (caller → 401).
- `ListCacheTokens() ([]*CacheToken, error)` — `id/name/token_prefix/status/last_pull_at/created_at`
  (never the hash).
- `RevokeCacheToken(name string) error` — `UPDATE … SET status='revoked'` (idempotent on name).
- `TouchCacheToken(id string) error` — `UPDATE … SET last_pull_at=?` (called by `/snapshot`).
- `CacheToken` model in `internal/models/models.go` (`ID/Name/TokenPrefix/Status/LastPullAt/…`,
  no hash field — owner-facing).

### 5.3 Store layer — read-only mode (`internal/store/store.go` + mutation sites)

Add two unexported fields to `*Store`:

```go
type Store struct {
    db          *sql.DB
    masterKey   []byte
    readOnly    bool        // cache mode: refuse every mutation except audit→sidecar
    auditSidecar *os.File   // when set, WriteAudit appends JSONL here instead of touching db
}
```

- **`ErrReadOnly`** (`errors.New("store is read-only (offline cache); connect to the server to mutate")`).
- **Guard every mutation method** with `if s.readOnly { return ErrReadOnly }` (or `0, ErrReadOnly`
  where a count/id is returned). The full list (verified against current source):
  `AddServer`, `UpdateServer`, `DeleteServer`, `SetCredential`, `DeleteCredential`,
  `AddProfile`, `DeleteProfile`, `GrantServers`/`RevokeServers`, `AddProject`, `RotateProject`,
  `SetProjectStatus`, `SaveHostKey`, `AddCacheToken`, `RevokeCacheToken`, `TouchCacheToken`.
  Each is a 2-line guard at the top of the method — mechanical, reviewable in one pass.
- **`WriteAudit`** is special: if `s.auditSidecar != nil`, append a JSONL line
  (`{ts, project_id, server_id, action, command, sudo, status, exit_code, duration_ms}`) and
  return `nil` — never touch `s.db`. This is the **only** "write" permitted in read-only mode,
  and it lands in the sidecar, not the cache. Set together (`readOnly=true; auditSidecar=f`) when
  hydrating a cache store.
- **Read methods are untouched** (`GetServer*`, `GetCredential`, `VerifyToken`,
  `ServersForProfile`/`ListServersForProfile`, `GetHostKey`, `ListProjects`, etc.) — the broker
  reads the cache exactly as it reads a live store.

A `OpenReadOnly( (*Store, error)` helper isn't needed; hydration goes through
`Open(":memory:", dek-decoded-snapshot)`-style — see 5.6.

### 5.4 Server side — `/snapshot` endpoint (`internal/mcpserver/serve.go`)

A **new HTTP route on the existing `serve` listener**, with a **disjoint auth verifier**:

- `verifyCacheToken(ctx, token, req) (*auth.TokenInfo, error)` — calls `st.VerifyCacheToken`,
  returns a `TokenInfo{UserID: ct.ID}` with the same nominal far-future `Expiration` as
  `verifyToken` (same SDK-shape reason). Distinct verifier function — **never** passed to the MCP
  handler's `RequireBearerToken`, and `verifyToken` (project) is **never** passed to `/snapshot`'s.
- **Routing:** `ServeRunner.HTTPHandler()` becomes a path-mux:
  - `GET /snapshot` → `auth.RequireBearerToken(verifyCacheToken, …)` → handler that calls
    `st.ExportSnapshot()`, JSON-marshals, writes 200. On success (after the body is written),
    `st.TouchCacheToken(ct.ID)` (best-effort — a touch failure is logged, not fatal).
  - **everything else** → the existing `auth.RequireBearerToken(verifyToken) → resolveServer → mcpHandler`.
  - The mux is a small `http.HandlerFunc` switching on `req.URL.Path`. The two auth chains share
    nothing but the listener and TLS.
- **Response body** = `store.Snapshot` JSON (Plan-11 format, verbatim — credentials decrypted under
  the server's master key into the DTO). **`cache_tokens` are NEVER in the `Snapshot`** —
  `ExportSnapshot` (Plan 11) already omits them (the table didn't exist then), and this plan does
  not add them to it. Device auth codes stay server-side only; a pulled cache holds zero of them.
  Plaintext **inside TLS** on the wire (same threat model as every other cred-bearing response the broker sends); the **client** DEK-encrypts at rest (5.5).
  No additional envelope on the wire — TLS is the transport crypto, DEK is the at-rest crypto.
- **`ExportSnapshot` reuse:** the `/snapshot` handler calls the **exact** Plan-11 method — no new
  read code. This is the format-reuse payoff Plan 11 was built for.

### 5.5 Client side — `cache` commands (`internal/cli/cache.go`, new)

- **`ssh-manager cache pull --url https://serve:7878 --token <device-auth-code>`**
  1. `GET <url>/snapshot` with `Authorization: Bearer <token>` (reuse `net/http`).
     - On 401/403: clear error ("authorization code invalid or revoked"). On network error:
       non-zero exit, **existing cache untouched** (a failed pull never corrupts the cache).
  2. `json.Unmarshal` the body into `store.Snapshot`.
  3. Load (or generate) the **DEK** from the OS keychain — same `ssh-manager` service, but a
     **distinct user slot `cache-dek`** (vs the master key's `master-key` slot). First pull:
     `GenerateMasterKey()` (32 random bytes) → store it; subsequent pulls: read it back. This needs
     a small generalization of `KeyringKeyProvider` (today it hardcodes `keyringUser="master-key"`)
     to take a `User` field defaulting to `master-key` — see touch-set row for `masterkey.go`. The
     DEK never leaves the keychain; only its holder (the work machine) can decrypt the cache.
  4. `vaultio.EncryptWithKey(dek, jsonBytes)` → `magic‖nonce‖ct` (see 5.6) — **atomic write**:
     write `cache.bin.tmp` (0600), `os.Rename` over `~/.ssh-manager/cache.bin`.
  5. Print to STDERR: snapshot timestamp (from server `updated_at` max), server URL, count.
- **`ssh-manager cache status`** — reads `cache.bin` (DEK-decrypt), prints: present Y/N, snapshot
  server-timestamp + age ("3.5h old"), server URL (stored in a sidecar `cache.meta.json`),
  last-local-pull time, # servers/creds. Exits non-zero if absent / corrupt / DEK missing.
- **No `cache load`** in v1 (non-goal §4).

### 5.6 `internal/vaultio` — add a raw-key envelope variant

`vaultio.Encrypt(passphrase, …)` runs Argon2id — correct for a low-entropy human passphrase,
**wrong** for a high-entropy 32-byte DEK (a KDF on already-random input is wasted cost). Add:

```go
// EncryptWithKey AES-256-GCM-seals plaintext under a raw 32-byte key (a DEK), returning
// magic‖nonce‖ct (no salt — the key is already high-entropy). Use for the offline cache's
// local at-rest encryption; use Encrypt (Argon2id) for human passphrases.
func EncryptWithKey(key, plaintext []byte) ([]byte, error)
func DecryptWithKey(key, blob []byte) ([]byte, error)   // same ErrBadMagic/ErrTruncated
```

Same `SSHMGRV1` magic, same AES-256-GCM, **no salt, no Argon2id** — `key` feeds `aes.NewCipher`
directly. Distinct from the passphrase path; both documented. (Layout differs only by the absence
of the 16-byte salt — `Decrypt` vs `DecryptWithKey` are distinguished by the caller, not the bytes;
that's fine since the caller knows which key it holds. If future ambiguity matters, a `SSHMGRK1`
magic for the key variant is a one-line change.)

### 5.7 Client side — `mcp --cache` (cache-mode stdio)

`internal/cli/mcp.go` gains `--cache` (bool). When set:

1. **Require `--token`** still (the same project token the agent uses online).
2. **Hydrate the cache**: read `~/.ssh-manager/cache.bin`, DEK-decrypt (`vaultio.DecryptWithKey`),
   `json.Unmarshal` → `store.Snapshot`. Then (per 5.8): `mk := store.GenerateMasterKey()` (a
   throwaway in-memory key) → `store.Open(":memory:", mk)` → `initSchema` runs in `Open` →
   `ImportSnapshot(snap)` (re-seals creds under `mk`) → set `readOnly=true` + open
   `~/.ssh-manager/cache-audit.log` as `auditSidecar`.
3. `st.VerifyToken(token)` against the cache — **the same gate as online stdio** (`run.go:20`).
   Rejects unknown/disabled tokens identically. Returns the project + profile.
4. `mcpserver.NewServer(st, project.ProfileID, project.ID)` — **unchanged constructor**; the broker
   now reads from the read-only hydrated store. Same 6 tools, same scoping, same iron rule.
5. `srv.Run(StdioTransport)` — same as `RunStdio`.

Net agent-surface change: **none**. The agent sees the same tools, the same per-server reach, the
same token. The only behavioral delta: mutation tools never existed on the broker surface anyway
(the broker is read-exec only); **owner-CLI mutations are what's blocked**, and those don't run in
`mcp` mode regardless. So for the **agent**, cache mode is **observationally indistinguishable**
from online — except an out-of-profile server is still rejected, and an unknown host key is
rejected instead of pinned (5.9).

### 5.8 Hydration detail — avoiding a redundant re-seal

`ImportSnapshot` re-seals each credential under the target store's master key. For a `:memory:`
read-only cache, that re-seal is wasted work (we never write the db back). Two clean options:

- **(A) Give the `:memory:` store a throwaway random master key** and let `ImportSnapshot` re-seal
  normally — simple, reuses the proven path, costs one seal/cred at startup (negligible). Reads
  then `open(masterKey, blob)`-decrypt normally. **Preferred — zero new code in the read path.**
- (B) Add a "plaintext-cred snapshot hydration" path that skips sealing. More code, no measurable
  win. Rejected.

Go with (A): `mk := store.GenerateMasterKey()` → `store.Open(":memory:", mk)` → `ImportSnapshot`
→ `readOnly=true`. The throwaway `mk` dies with the process.

### 5.9 Host-key behavior in cache mode (`internal/sshbroker/hostkey.go`)

The broker dials SSH and checks the presented host key against `GetHostKey(host, port)` (read from
cache). Online, a miss TOFU-pins via `SaveHostKey`. In cache mode `SaveHostKey` returns
`ErrReadOnly` — so the dial must **fail closed** with a clear message
("unknown host key; offline cache cannot pin — run `cache pull` to refresh"). One check at the
broker's host-key callback: if `SaveHostKey` returns `ErrReadOnly`, surface it as a hard refusal,
not a silent skip. MITM-then-pin is impossible offline.

### 5.10 CLI — `cache-tokens` (owner, server-side) (`internal/cli/cache_tokens.go`, new)

Mirror `projects.go`'s CLI shape:

```
ssh-manager cache-tokens add --name laptop        # prints one-time authorization code
ssh-manager cache-tokens ls                       # name / prefix / status / last_pull_at
ssh-manager cache-tokens revoke <name>            # Lazy: next pull rejected
```

Registered in `internal/cli/root.go` alongside `newProjectsCmd()`.

### 5.11 Auto-refresh — OS scheduler (docs only; no code)

`cache pull` is one-shot + idempotent. The OS scheduler runs it every N minutes. `docs/multi-machine.md`
(or a new `docs/缓存.md`) ships templates:

- **Linux (systemd timer)** — `ssh-manager-cache-pull.{service,timer}`, `OnCalendar=*:0/30`,
  `Environment=SSHMGR_CACHE_URL=… SSHMGR_CACHE_TOKEN=…` (or a small `~/.ssh-manager/cache.conf`
  the timer passes through).
- **Windows (Task Scheduler)** — `schtasks /Create /SC MINUTE /MO 30 …`.
- **macOS (launchd)** — `~/Library/LaunchAgents/ssh-manager-cache-pull.plist`, `StartInterval=1800`.

Default cadence **30 min** (documented as tunable). The token/url can live in a 0600 config file
the scheduler reads, so they're not in the unit's `Environment=` in plaintext-at-rest — OR the
owner accepts the unit-file risk on the work machine (it's the owner's own machine). Document both,
recommend the config-file path.

## 6. What does NOT change (verified against source)

- **Agent tool surface — zero change.** `BrokerTools` (`server.go:24`) stays 6 tools with identical
  names/descriptions. `NewServer` (`server.go:39`) signature unchanged. The cache path calls it with
  a read-only hydrated store; the agent can't tell.
- **Iron rule + profile scoping — intact offline.** Cache-mode stdio verifies `--token` against the
  cached `projects` rows (hash preserved verbatim, Plan-11-proven) and scopes via the same
  `NewServer(profileID, projectID)` → `…ForProfile` projection. Out-of-profile servers stay hidden.
- **Eval scorers T6/T8 — immune.** T6 (literal cred bytes in tool results) and T8 (cross-profile
  exec/download/upload reach) don't touch `cache_tokens`, the snapshot endpoint, or store `readOnly`.
  Cache mode preserves the exact scoping T8 asserts.
- **`serve` project-token path — untouched.** `verifyToken` / `resolveServer` / the MCP handler are
  the same; `/snapshot` is an additive sibling route with a disjoint verifier.
- **`export`/`import` (Plan 11) — untouched.** `ExportSnapshot` is *reused* by `/snapshot`; the
  passphrase envelope and CLI are unchanged.
- **`BrokerTools` cardinality / no new MCP tool** — the agent gets no new capability; the new
  surface is owner-CLI (`cache-tokens`, `cache`) + one HTTP route, none of it agent-facing.

## 7. Security considerations

- **Two disjoint auth gates (the keystone).** A project token MUST NOT authenticate `/snapshot`, and
  a cache token MUST NOT authenticate the MCP endpoint. Cross-isolation is a load-bearing test (§8).
  Without this, a project token could dump the whole vault — the exact breach the iron rule exists
  to prevent.
- **`/snapshot` returns plaintext creds over TLS.** Same threat model as every cred-bearing broker
  response. TLS (recommended, warned when absent on non-loopback) is the transport crypto. At rest
  on the work machine, the DEK (keychain) is the crypto. The plaintext window is in-memory on both
  ends, as with `export`/`import`.
- **DEK custody = OS keychain.** Machine theft → owner `cache-tokens revoke <machine>` server-side;
  the stolen DEK can't pull anymore (the cached file is inert post-revoke for refresh, though the
  stolen file's cached creds remain decryptable by the stolen DEK — document: revoke + rotate any
  cached credentials that were sensitive, same as a stolen `store.db`).
- **`last_pull_at` is operational metadata**, not sensitive (no creds); surfaced in `cache-tokens ls`.
- **Public repo hygiene:** doc examples use RFC5737 IPs + placeholder tokens (matching the
  `5c3fec8` scrub precedent); no real hosts/tokens in committed docs.
- **Read-only fail-closed:** unknown host key (5.9), every mutation (5.3). No silent degrade.

## 8. Testing

- **`internal/store/cachetoken_test.go`** — add→verify (active)→revoke→verify-rejects (Lazy);
  prefix-filter; constant-time compare path; `TouchCacheToken` updates `last_pull_at`.
- **`internal/store/readonly_test.go`** — with `readOnly=true`, **every** mutation method returns
  `ErrReadOnly` (table-driven over the full list in 5.3); `WriteAudit` with `auditSidecar` set
  appends a JSONL line to the file and returns nil, **without** inserting into `audit_log` (assert
  the table row count is unchanged); reads still work.
- **`internal/mcpserver/serve_snapshot_test.go`** — (a) GET `/snapshot` with a valid cache token →
  200 + full `Snapshot` (assert it matches `ExportSnapshot()` directly); (b) **cross-auth
  isolation**: project token → `/snapshot` 401; cache token → MCP endpoint 401/403 (the keystone);
  (c) revoked cache token → `/snapshot` 401; (d) `last_pull_at` bumped after a successful pull.
- **`internal/vaultio/vaultio_key_test.go`** — `EncryptWithKey`/`DecryptWithKey` round-trip; wrong
  key → GCM auth fail; tamper → fail; truncated → `ErrTruncated`; two encrypts differ (random nonce).
- **`internal/cli/cache_test.go`** — `cache pull` against an in-process test `serve` (reuse the
  `httptest` pattern) writes `cache.bin`, DEK in keychain (test `MemKeyProvider`); re-pull is
  idempotent + atomic (kill mid-write does not corrupt the prior cache — simulate by a failing
  rename); `cache status` reports snapshot age from the stored timestamp. **No network in CI** —
  drive against a loopback test server.
- **`internal/cli/mcp_cache_test.go`** — `mcp --cache --token <project-token>`: token verified
  against cache; in-profile `exec_command` (against `testsshd`) works; out-of-profile server
  rejected; **mutation attempt** (there's no agent mutation tool, so assert via the owner path:
  `servers add` while pointed at the cache store → `ErrReadOnly`); unknown host key → exec refused.
- **Hydrate round-trip** — `ExportSnapshot` → `Open(":memory:", throwaway-mk)` → `ImportSnapshot`
  → `readOnly=true` → `GetServer`/`GetCredential` (decrypts under throwaway mk)/`VerifyToken`
  (original project token validates)/`ListServersForProfile`/`GetHostKey` all return cached data.
- **No-regression:** `go test ./...` green; `gofmt -l .` empty on touched files; `go vet ./...` clean.

## 9. Implementation touch-set (file-by-file)

| File | Change |
|---|---|
| `internal/store/store.go` | +`cache_tokens` CREATE TABLE + `addColumnIfMissing`-style migration (new table → guarded CREATE only); +`readOnly`/`auditSidecar` fields on `*Store`; `ErrReadOnly`. |
| `internal/store/cachetoken.go` (**new**) | `AddCacheToken`/`VerifyCacheToken`/`ListCacheTokens`/`RevokeCacheToken`/`TouchCacheToken` (mirror `projects.go` + `token.go`). |
| `internal/store/{servers,credentials,profiles,projects,hostkeys}.go` | 2-line `ErrReadOnly` guard at the top of every mutation method (full list in 5.3). |
| `internal/store/audit.go` | `WriteAudit`: if `auditSidecar != nil` → JSONL append, return nil. |
| `internal/models/models.go` | +`CacheToken` struct (owner-facing fields, no hash). |
| `internal/store/masterkey.go` | generalize `KeyringKeyProvider` with a `User` field (default `"master-key"`); the cache DEK uses `User:"cache-dek"`. |
| `internal/vaultio/vaultio.go` | +`EncryptWithKey`/`DecryptWithKey` (raw-key AES-GCM, no Argon2id). |
| `internal/mcpserver/serve.go` | +`verifyCacheToken`; `HTTPHandler()` → path-mux (`/snapshot` vs MCP); `/snapshot` handler (`ExportSnapshot` + `TouchCacheToken`). |
| `internal/cli/cache_tokens.go` (**new**) | `cache-tokens add/ls/revoke`; registered in `root.go`. |
| `internal/cli/cache.go` (**new**) | `cache pull`/`cache status`; DEK keychain (generalized `KeyProvider` w/ user slot); atomic cache write. |
| `internal/cli/mcp.go` | +`--cache` flag; hydrate (`:memory:` + `ImportSnapshot` + `readOnly` + `auditSidecar`) → `VerifyToken` → `NewServer` → Run. |
| `internal/cli/root.go` | register `cache-tokens`, `cache`. |
| `internal/sshbroker/hostkey.go` | cache-mode unknown-host-key → fail closed (surface `ErrReadOnly` from `SaveHostKey` as a hard refusal). |
| `docs/multi-machine.md` | renumber roadmap (P13=群晖, P14=enroll); limitation #2 "无离线缓存"→"已实现 (Plan 12)"; new "离线只读缓存" section (pull / cache-tokens / auto-refresh / offline use / limits). |
| `docs/backup-restore.md` | update the "格式后续会被复用：客户端只读缓存" note → now done (Plan 12). |
| `README.md` + `docs/README.md` | cross-link the new cache section. |
| (new) `docs/缓存.md` or fold into `multi-machine.md` | operator guide: enroll a machine (`cache-tokens add` → `cache pull`), scheduler templates, going offline, revocation, limitations. |

## 10. Scope note (honest)

This is the **largest plan since the broker itself** — it touches `serve` (new route + new auth
verifier), `store` (new table + `readOnly` across ~14 mutation methods), the CLI (`cache-tokens` +
`cache` + `mcp --cache`), `mcpserver` (cache run path), `vaultio` (key-variant envelope),
`sshbroker` (host-key fail-closed), and docs (roadmap renumber + new operator guide). The agent
tool surface is nonetheless **unchanged** — that invariant (§6) is what keeps the eval and the
iron rule untouched, and it is the single most important thing to preserve during implementation.

The correctness keystone is the **two-disjoint-gates** test (§8: a project token must not reach
`/snapshot`, a cache token must not reach MCP) — if that fails, nothing else matters.

## 11. Future work (explicitly deferred)

- Live hot-reload of a running stdio broker on cache refresh.
- `.mcp.json` online/offline auto-fallback.
- `--owner` unscoped cache access.
- Delta/incremental pull; multi-server hub fanout.
- `cache load <export-file>` manual-population escape hatch (if pull-only proves limiting).
- Full enroll flow (now P14): DEK distribution / machine provisioning beyond the manual
  `cache-tokens add` + `cache pull` two-step.

## 12. References

- Multi-machine design (grilled 2026-08-11, 路线乙): `docs/multi-machine.md`
- Plan 11 (export/import — `Snapshot` + `vaultio`, the format this reuses):
  `docs/superpowers/plans/2026-08-11-ssh-manager-mcp-plan-11-export-import.md`, `internal/store/export.go`,
  `internal/vaultio/vaultio.go`
- Serve HTTP auth + routing (the mux this extends): `internal/mcpserver/serve.go`
- Token model (cache-tokens mirror this): `internal/store/projects.go`, `internal/store/token.go`
- Master-key custody (DEK mirrors this): `internal/store/masterkey.go`
