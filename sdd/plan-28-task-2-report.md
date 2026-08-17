# Plan 28 Task 2 Report — CLI servers add/edit 接入

## Status: DONE (TDD RED → GREEN, full suite green, committed)

## Wiring points (file:line, post-change)

File: `internal/cli/servers.go`

| Point | Location | What |
|---|---|---|
| `servers add` scan | internal/cli/servers.go:53-61 | After `srv` construction (final trimmed values), before credential mint + `AddServerWithCredentials`. One `secrethint.ScanServer(...)` call over the final 7 metadata values; findings printed via `printSecretHints`. |
| `servers edit` scan | internal/cli/servers.go:298-305 | After the sudo-credential mint and the needs-passphrase stripTag block, immediately before `UpdateServerWithCredentials`. `scanEditMetadata(cmd, srv)` scans ONLY `Changed()` metadata flags against `srv`'s final persisted form. |
| `printSecretHints` helper | internal/cli/servers.go:351-357 | `fmt.Fprintln(cmd.ErrOrStderr(), secrethint.FormatWarning(f))` per finding — one line, stderr, no content echo. |
| `tagsRawForScan` helper | internal/cli/servers.go:359-364 | `json.Marshal(tags)` string — mirrors the DB form exactly. |
| `scanEditMetadata` helper | internal/cli/servers.go:366-392 | Changed()-gated `ScanValue` per field; maps CLI flag spellings to persisted field names (`special-handling` → `caveats`). |

Tests: `internal/cli/servers_hint_test.go` (new, 4 tests + 2 helpers `newHintEnv`/`runCaptured`).

## How tags raw form is captured

The store persists tags as `tagsJSON, _ := json.Marshal(srv.Tags)` in BOTH
`insertServerTx` (internal/store/tx.go:29) and `updateServerTx`
(internal/store/tx.go:55) — the DB `tags` TEXT column is a JSON array string
(nil slice → `null`). `tagsRawForScan` performs the identical marshal in the
CLI, so the scanner sees byte-for-byte what lands in the DB (and what
list_servers later sends to LLM providers). JSON quoting cannot break any
rule: no rule prefix or PEM marker contains `<`, `>`, `&` (the chars Go's
json.Marshal escapes), and substring matches survive quoting.

## Placement rationale

- **add**: scan after `srv` construction = final values (post-TrimSpace). It
  runs after the --password/--key mutual-exclusion check, so a rejected
  invocation never warns. It runs before the store write; a store failure
  (e.g. duplicate name) may print a warning for a row that failed to insert —
  advisory-only, acceptable (operator is about to retry the same value).
- **edit**: placed after the `--clear-credential` early return (that path
  applies NO field flags, so nothing to scan) and after the pwSet||keySet
  `stripTag` (so `--tags` + re-credential is scanned in its final post-strip
  form). Rejected edits (empty credential values, mutex violations) return
  before the scan — no warning for a write that never happens.

## TDD evidence

RED (before implementation, `go test ./internal/cli/ -run "TestServersAddWarns|TestServersEditWarns|TestServersAddNoWarning" -count=1`):

```
--- FAIL: TestServersAddWarnsOnSuspectedSecret
    servers_hint_test.go:71: stderr must carry the caveats/pem-private-key warning, got:
--- FAIL: TestServersEditWarnsOnSuspectedSecret
    servers_hint_test.go:106: stderr must carry the description/sk- warning, got:
--- FAIL: TestServersAddWarnsOnSuspectedSecretTag
    servers_hint_test.go:176: stderr must carry the tags/ghp_ warning, got:
```

(The negative test `TestServersAddNoWarningOnCleanMetadata` passed pre-implementation, as expected — nothing warned yet.)

GREEN: `go test ./internal/cli/ -count=1` → `ok ssh-manager-mcp/internal/cli 10.086s`.
Full suite: `go test ./... -count=1` → all packages ok (cli, clientops, conformance, eval, importer, mcpserver, paths, roles, secrethint, sshbroker, store, testsshd, tui, vault, vaultio).
`gofmt -l` clean, `go vet ./internal/cli/` clean, `go build ./...` clean.

### Test coverage

1. `TestServersAddWarnsOnSuspectedSecret` — `--special-handling` carrying a PEM block (real flag spelling; Finding field is `caveats`): server IS created (verified via `store.Open` + `GetServerByName`), stdout keeps `added server leaky`, stderr contains `field 'caveats'` + `pem-private-key`, sentinel `SENTINEL-PEM-BODY-7QX` absent from BOTH stdout and stderr.
2. `TestServersEditWarnsOnSuspectedSecret` — description → `sk-ant-api03-…`: stderr gets `field 'description'` + `prefix:sk-`, edit succeeds, value persists. Then an unrelated `--port` edit asserts stderr stays silent — pins the Changed()-only partial-update semantics (a pre-existing secret is NOT re-warned on unrelated edits).
3. `TestServersAddNoWarningOnCleanMetadata` — all 7 fields with legal values (incl. `8xA100` hardware text) → zero `warning:` lines.
4. `TestServersAddWarnsOnSuspectedSecretTag` — `--tags gpu,ghp_…`: warning says `field 'tags'` + `prefix:ghp_`, add succeeds — directly verifies the JSON-raw-form tags capture.

stderr is captured via `root.SetErr(stderr)` with stdout on a separate buffer (`runCaptured`), so warning-stream assertions are independent of the success line.

## Self-review

- Credential fields untouched by the scan: `--password`, `--key`, `--key-passphrase`, `--sudo-password` never enter `ScanValue`/`ScanServer`; nor do `--name/--host/--port/--user` (not free-text metadata). 铁律 upheld.
- No changes to any return value, exit code, or success-path output of add/edit — the wiring only adds `Fprintln` to stderr.
- No new dependencies (`encoding/json` stdlib; `internal/secrethint` in-repo).
- No suppression/filtering added for the `sk-` English-word false-positive class (task/risk) — hint-not-verdict semantics deferred to T4 as briefed.
- Diff is +67 lines in `servers.go`, all additive; zero modifications to existing lines beyond the two import lines.

## Concerns

1. A failed `add` (e.g. duplicate-name store error) will have already printed the hint — a warning for a row that was never persisted. Advisory-only; flagging for the final review to confirm acceptable.
2. `json.Marshal` failure path (`tagsRawForScan` ignores the error) is unreachable for `[]string` — identical to the store's own `tagsJSON, _ :=` pattern; consistent, not new risk.
3. In `edit`, `--tags` combined with re-credential scans the post-strip tag list (final persisted form) — intended, but if T4's semantics doc wants "what the operator typed" instead of "what lands", this is the spot to revisit.
