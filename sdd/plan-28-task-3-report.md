# Plan 28 Task 3 Report — import 与 TUI 接入

**Commit:** `2254cbb` `feat: import + TUI suspected-secret metadata warnings (Plan 28 T3)` (branch `worktree-plan-28-secrethint`)
**Status:** DONE — RED→GREEN, full repo suite green, gofmt/vet clean.

## 1. What metadata each path actually carries (field-flow evidence)

### CLI `servers import` — carries ZERO user free-text metadata (proven)

| Evidence | Location |
|---|---|
| `importer.Candidate` struct = `{Name, Host string; Port int; User string; KeyPaths []string}` — nothing else exists on the type | `internal/importer/importer.go:20-25` |
| `Parse` reads only `HostName` / `Port` / `User` / `IdentityFile` from ssh_config (ssh_config has no free-text note fields) | `internal/importer/importer.go:76-98` |
| `runImport` builds `srv := &models.Server{Name, Host, Port, User}` | `internal/cli/servers_import.go:184-186` |
| Only metadata-ish write: `srv.Tags = []string{"needs-passphrase"}` — a fixed code literal, never user input | `internal/cli/servers_import.go:191` |
| `Description/Location/Hardware/Services/Role/Caveats` are never set on the import path → always `""` | (absence; grep of runImport body) |

Conclusion: a config file can never exercise a hit at CLI-import time. Per the task's fallback clause, the defensive scan is wired anyway (fires the day the importer starts populating free-text), the zero-metadata fact is documented at `scanImportServer` (`internal/cli/servers_import.go:95-108`), and the test pins the aggregate scan the import path performs (§3). The TUI supplement form is the real metadata-carrying import leg and is covered below. **Not BLOCKED** — the leg is wired and tested honestly at the aggregate-scan level the task sanctioned.

### TUI save paths

| Path | Fields carried | Save point |
|---|---|---|
| Servers-page add/edit form (`newServerForm` → `toParts`) | all 7 metadata fields from the draft; edit mode preserves `cur.Tags` (form has no tags field) | `internal/tui/forms.go:319` (add), `forms.go:334` (edit) |
| Import supplement form (`supplementForm` → `submitSupplement`) | all 7 metadata fields from the draft + tags (post `dropTag`) | `internal/tui/importflow.go:312-359`, scan at `importflow.go:364` |
| Import batch insert (`startBatch`) | name/host/port/user + fixed `needs-passphrase` tag — **zero user free-text** (same evidence as CLI; importflow reuses `importer.PickKey` + the same `Candidate` shape) | NO scan — documented rationale in `startBatch` doc comment (`internal/tui/importflow.go:185-192`) |

## 2. Wiring points

**CLI** (`internal/cli/servers_import.go`):
- New `importHint{name, findings}` (line 90), `scanImportServer` (line 109 — wraps `secrethint.ScanServer` + T2's `tagsRawForScan`), `printImportHints` (line 119 — `⚠ <alias>: <FormatWarning>` to the warn stream).
- `runImport` signature gains a `warn io.Writer` (production caller passes `cmd.ErrOrStderr()`, line 61); per-candidate scan right before `AddServerWithCredentials` (line 197); aggregated dump after the stdout report table (line 223). Insert ALWAYS proceeds; dry-run path (which `continue`s before `srv` is built) never collects hints.
- Credential/key bytes are deliberately not scanned: `PickKey`'s key content is a credential, not metadata (documented at `scanImportServer`).

**TUI** (new `internal/tui/secrethint.go`):
- `hintLines(tags, description, location, hardware, services, role, caveats string) []string` (line 25) — the parent-specified pure signature; one `⚠ ` + `FormatWarning` line per finding; nil on clean.
- `tagsScanValue` (line 41, JSON-array raw form — same discipline as CLI `tagsRawForScan`), `serverHintLines` (line 50, `models.Server` adapter), `appendHintLines` (line 59).
- Save point 1 — `submitServer` (`forms.go:319,334`): hint lines appended to the existing `actionDoneMsg` status string (`已新增/已更新 …` + `\n` + ⚠ lines, rendered in the existing ✓ footer status). Scan runs on the FINAL persisted `srv` (after tags preserve/dropTag). The `ClearCredential` early path applies no metadata flags → no scan (CLI T2 parity).
- Save point 2 — `submitSupplement` (`importflow.go:364`): after a successful `UpdateServerWithCredentials`, hint lines are appended to `f.report` (the result screen's existing report mechanism), each prefixed with the server name; the loop advances immediately — no blocking, no new screens.
- Edit-leg scan covers the whole final row (the TUI form submits all fields at once — the user re-confirms the entire row), unlike the CLI's changed-only partial scan; rationale documented in `secrethint.go:46-48`.

## 3. TDD evidence

**RED** (both packages, compile-level red as expected for new symbols):
```
internal\cli\servers_hint_test.go:214:14: undefined: scanImportServer
internal\cli\servers_hint_test.go:220:2: undefined: printImportHints
internal\cli\servers_hint_test.go:220:27: undefined: importHint
FAIL ssh-manager-mcp/internal/cli [build failed]
---
internal\tui\secrethint_test.go:26:11: undefined: hintLines
internal\tui\secrethint_test.go:52:3: undefined: tagsScanValue
FAIL ssh-manager-mcp/internal/tui [build failed]
```

**GREEN** (two separate simple commands per session-guard constraint):
```
$ go test ./internal/cli/ -count=1
ok  ssh-manager-mcp/internal/cli  10.053s
$ go test ./internal/tui/ -count=1
ok  ssh-manager-mcp/internal/tui  2.363s
```

New tests, all passing:
- CLI `internal/cli/servers_hint_test.go` (appended):
  - `TestImportWarnsOnSuspectedSecret` — the sanctioned zero-metadata shape: pins the two halves of the import wiring (`scanImportServer` on a server in the exact persisted shape + a secret-shaped tag → finding `{tags, prefix:sk-}`; `printImportHints` → line names server+field+rule, sentinel `SENTINEL-IMPORT-SK-5VQ` never echoes).
  - `TestImportNoFalsePositiveOnImportedTags` — real end-to-end CLI run (`runCaptured`, separate stdout/stderr): encrypted-key config imports with the fixed `needs-passphrase` tag and leaves stderr CLEAN (the defensive scan must not fire on legal imported state).
- TUI `internal/tui/secrethint_test.go` (new):
  - `TestHintLinesHitAndNoEcho` — 2 findings (tags prefix + caveats PEM), lines contain field+rule, both sentinels absent.
  - `TestHintLinesClean` — all 7 legal fields → zero lines.
  - `TestSubmitServerWarnsOnSuspectedSecret` / `TestSubmitServerEditWarnsOnSuspectedSecret` — save-path smoke at the real seam: direct `submitServer(...)` invocation (no bubbletea loop — same direct-call shape as neighboring forms tests; justified: the wiring is a one-line append to the existing status string, and the full TUI loop test would be disproportionate); asserts success line + hint line + persistence despite hint + no-echo.
  - `TestImportFlowSupplementWarnsOnSuspectedSecret` — drives the real flow to supplement state (`flowAtPick`/`runBatch`/`supplementTarget`), submits a PEM-shaped caveats, asserts: flow advanced to result (non-blocking), the report carries a `bare`-prefixed `field 'caveats'`/`pem-private-key` line, sentinel absent, supplement persisted.

**Regression:** `go build ./...` + `go test ./... -count=1` — zero failures (includes the pre-existing `TestServersImportFlow` suite, which exercises `runImport`'s new signature end-to-end). `gofmt -l internal/` clean; `go vet` clean.

## 4. Self-review

- 铁律 kept: no credential/key bytes reach any scan (only the 7 metadata fields are ever passed; `PickKey` key content explicitly excluded and documented both sides).
- No new dependencies (secrethint is T1's internal package; `encoding/json` stdlib).
- Non-blocking everywhere: CLI stdout table, exit codes, grant path untouched; TUI flows advance identically; zero new confirm screens.
- No-echo by construction: all warning text is `FormatWarning` (field+rule) + the server alias; every test asserts sentinel absence on the actual output surface.
- Minor accepted imprecision: a CLI-import candidate whose insert later FAILS would still contribute a collected hint (scan runs before insert per spec "入库前扫"). Advisory-only, unreachable today (no metadata can trigger), left as specified.

## 5. Concerns

1. **TUI edit re-warns on pre-existing content**: the edit leg scans the whole final row (form re-confirms all fields), so a pre-existing secret-shaped tag/description re-warns on every edit. Advisory and arguably a useful persistent reminder; differs from CLI T2's changed-only semantics — documented in `internal/tui/secrethint.go`.
2. **Supplement warning visibility is deferred**: the ⚠ lines surface on the import result screen (after the loop), not inline on the next supplement form — per the task's "existing feedback/report mechanism, no new screens" constraint. A user supplementing many servers sees the warning only at the end.
3. **CLI-import scan is dead code today** (cannot fire until the importer carries free-text). Kept defensive per task instruction; `TestImportNoFalsePositiveOnImportedTags` pins that it stays silent meanwhile.
