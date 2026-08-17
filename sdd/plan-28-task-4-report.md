# Plan 28 Task 4 Report: 文档 + 全量验证（收官）

Commit: `e339965` — `docs: suspected-secret metadata warning semantics (Plan 28 T4)`
Branch: `worktree-plan-28-secrethint`

## 1. Doc placement

### docs/managing-servers.md — :91-108 (new blockquote, +18 lines)

Inserted immediately **after** the pre-existing "⚠️ Do not put secrets in any of these fields" blockquote (ends :89) and **before** "Each field is capped at 4 KB" (:110). Why here: this is the metadata section's secrets paragraph — the exact spot the plan named (the "travel into the agent's context" line is :88) — so the warning semantics sit directly under the rule they explain. Language matches the surrounding section (English; that section is English while the rest of the file is Chinese).

Content covers all required points:
- All four write paths scan: `servers add` / `servers edit` / `servers import` (per-machine supplement) / TUI add-edit + import-supplement save points.
- 9 rules: 8 prefixes (`sk-`, `ghp_`, `gho_`, `github_pat_`, `AKIA`, `xoxb-`, `xoxp-`, `eyJ`) + PEM tightened to require `-----BEGIN` AND `PRIVATE KEY` co-occurring (public certs don't match).
- High-entropy deliberately not implemented (2.9% FP measured; 9 rules pass 0-FP corpus regression).
- Non-blocking: write always succeeds, exit code untouched, flagged content never echoed.
- Hints not verdicts: FP on ordinary words expected ("task-force" contains `sk-`).
- Fix path: edit the field, or ignore if intentionally a public fingerprint.
- Quotes the exact FormatWarning output shape (verified byte-for-byte, see §4).

### README.md — :104 (one sentence added)

grep `metadata` found exactly one natural anchor: the "Managing servers & projects (owner CLI)" paragraph, which already carries the "⚠️ Never put secrets in these fields" warning sentence. Added one sentence after it (not skipped): backstop warning on every metadata write path, non-blocking, 9 token shapes, never echoes content. One sentence only — README style is compact, details live in managing-servers.md.

### docs/backlog.md — NOT modified

grep for `P5|secrethint|suspected|疑似|hint` found zero matches — no live P5-related line exists. No forced entry added, per task instruction.

## 2. Smoke output (verbatim)

Temp vault seeded via env-prefix on a single command line (no export, no compound): `SSHMGR_STORE=<temp>/store.db SSHMGR_MASTERKEY_HEX=<32-byte hex> SSHMGR_FILEKEY_PATH=<temp>/no-such-master.key`. The FILEKEY_PATH deliberately points at a non-existent file so tier-1 (injected FileKeyProvider) returns ErrNotFound and tier-2 (env hex) wins — the dev machine's real master.key is never touched. Same recipe as the T2 test helper `newHintEnv` (internal/cli/servers_hint_test.go:24-38). Binary built fresh from this worktree into the temp dir.

Run 1 — dirty description (`--description "key sk-test-abcdef0123456789abcdef"`), exit 0:

```
warning: server metadata may contain a secret — field 'description' matched rule 'prefix:sk-' (content not shown; this text would be sent to LLM providers on every list_servers — edit the server to fix, or ignore if intentional)
added server hint-smoke id=1m8vy_oaQWs
```

Run 2 — clean description (`--description "gpu box for nightly training"`), same vault, exit 0:

```
added server hint-clean id=hE4pClUUWxQ
```

Persistence proof (`servers ls` on the temp vault — server landed despite the warning):

```
hint-clean       hE4pClUUWxQ          root@127.0.0.1:22 [-] (-) · -
hint-smoke       1m8vy_oaQWs          root@127.0.0.1:22 [-] (-) · -
```

Observations:
- Warning fires on stderr before the store write and does NOT block it; exit code 0 in both runs.
- The flagged value `sk-test-abcdef0123456789abcdef` appears nowhere in any output (no-echo held in the real binary, not just tests).
- Clean metadata produced zero warning lines.
- Temp dir (`%LOCALAPPDATA%\Temp\p28-t4-smoke`, incl. store.db, binary, no-such key) deleted after the run.

Note: the task brief's example command used positional args (`servers add hint-smoke 127.0.0.1`); the actual CLI requires flags (`--name`/`--host`/`--user`, checked in internal/cli/servers.go:87-104). The corrected form was used.

## 3. Verification (each a separate command)

| Command | Result |
|---|---|
| `go build ./...` | clean (no output, exit 0) |
| `go vet ./...` | clean (no output, exit 0) |
| `gofmt -l .` | empty (zero files) |
| `go test ./... -count=1` | all 17 packages `ok` (incl. `internal/secrethint` 0.164s, `internal/cli` 10.906s, `internal/tui` 2.653s); no FAIL |

## 4. Byte-review (prior one-byte Unicode regression guard)

- Both edited files pass `iconv -f UTF-8 -t UTF-8` roundtrip (exit 0) — whole files valid UTF-8, no mojibake introduced.
- The doc's quoted warning vs FormatWarning (internal/secrethint/secrethint.go:81-85), byte-dumped via `grep -o | od -tx1`:
  - em-dash `secret — field`: doc `e2 80 94` == Go source `e2 80 94` (U+2014).
  - second em-dash `list_servers — edit`: doc `e2 80 94` == source.
  - apostrophes around field/rule names: doc `27` (ASCII `'`) == source ASCII `'`.
- No pre-existing Chinese lines touched: diff is +18 blockquote lines in managing-servers.md and a single rewritten paragraph line in README.md (only ASCII + em-dash added; the README line's pre-existing Chinese-free English is unchanged around the insertion).
- New non-ASCII introduced: em-dashes (verified above) and one 🔎 marker (valid 4-byte UTF-8, consistent with the file's existing 💡/⚠️ blockquote markers).

## 5. Concerns

- None blocking. Minor: the brief asked for "3-5 lines" in managing-servers.md; the block is 18 source-wrapped lines but a single logical blockquote paragraph — it had to carry six mandated semantic points plus the verbatim warning quote required by the plan's acceptance ("文档与 FormatWarning 实际输出一致"). Rendering is ~1 screen paragraph, proportionate to the neighboring import-section warnings.
- The 2.9%/0-FP claims in the doc mirror the secrethint.go package comment and T1 testdata exactly (verified against source before writing).
- README anchor existed, so README was edited rather than skipped (reporting which, per task).
