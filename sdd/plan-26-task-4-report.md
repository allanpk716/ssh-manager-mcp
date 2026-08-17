# Task 4 Report: docs/backlog.md 固化 + 注释指向

**Status: DONE** — commit `033d1ba` on `worktree-plan-26-arrears`

## What was implemented

1. **Created `docs/backlog.md`** (9 lines) — the brief's Step 1 markdown block, byte-exact: title, intro line (xcheck 收敛 2026-08-16 / Plan 25), 5 numbered items. Committed as exactly 9 insertions, matching the block's 9 content lines.
2. **`internal/mcpserver/tunnels.go`** (comment only, lines 14-15): `...is a tracked backlog item (see` / `docs/backlog.md). Default 10 min per Plan 6 §T4.` — comment rewrapped; `const forwardIdleTimeout` line untouched.
3. **`internal/mcpserver/revoke_semantics_test.go`** (comment only, lines 28-29): `...kill CLI is backlog` / `(see docs/backlog.md)).` — exact string appended adjacent to "backlog" (nested parens, minimal change); function signature and body untouched.
4. **`docs/agent-access.md`** — two edits:
   - Line 108 (layer 3, the 隧道急停/backlog mention — located by grep per the brief's Files note): `（owner 侧急停命令已列 backlog。）` → `（owner 侧急停命令已列 backlog，见 docs/backlog.md。）`. Without this the Step 3 gate would fail on a bare "backlog" in a live doc.
   - New line 111 at the end of the 断连语义（四层） block (after item 4, blank-line separated, before the `rotate` paragraph): `未实现的拆除手段见 docs/backlog.md。`
5. **`docs/README.md`** — the 目录 table is a doc index (indexes all 7 sibling docs), so one row added after compat-matrix.md linking `[backlog.md](./backlog.md)` with a one-line 解决什么 description.

## Step 3 verification: full post-edit `git grep -n backlog -- internal/ docs/ README.md` hit list

| Hit | Judgment |
|---|---|
| `docs/README.md:33` — new index row `[backlog.md](./backlog.md)` | Points at docs/backlog.md. OK |
| `docs/agent-access.md:108` — `已列 backlog，见 docs/backlog.md。` | Points at docs/backlog.md. OK |
| `docs/agent-access.md:111` — `未实现的拆除手段见 docs/backlog.md。` | Points at docs/backlog.md. OK |
| `docs/superpowers/plans/2026-08-16-plan-25-...md:18, 573, 654, 792, 992` | Archived Plan 25 plan document (frozen planning record; predates backlog.md — lines 573/654/792 are quoted snapshots of the OLD comments). docs/README.md explicitly labels `superpowers/` 内部文档. These are the provenance records the backlog items cite; editing would falsify history. No edit. OK (archival) |
| `docs/superpowers/plans/2026-08-17-plan-26-...md:1, 5, 28, 35, 340, 343, 344, 349, 351, 365, 366, 370, 371, 411` | Archived Plan 26 plan document — this task's own plan; most lines literally reference `docs/backlog.md`. No edit. OK (archival) |
| `internal/mcpserver/revoke_semantics_test.go:28-29` — `(see docs/backlog.md))` | Points at docs/backlog.md. OK |
| `internal/mcpserver/tunnels.go:14-15` — `(see docs/backlog.md)` | Points at docs/backlog.md. OK |

Root `README.md`: zero hits. `docs/backlog.md` itself: no lowercase "backlog" in content (title is capitalized `# Backlog`), hence absent from the grep — expected, not an orphan.

**Conclusion:** every hit in live code/docs points at docs/backlog.md; the only non-pointing mentions are frozen plan archives under `docs/superpowers/plans/` (immutable provenance, not live references).

## Build/test proof

- `gofmt -l` on both Go files: clean (no output).
- `go build ./...`: PASS (no output).
- `go test ./internal/mcpserver/ -count=1`: `ok ssh-manager-mcp/internal/mcpserver 4.484s` — includes `TestRevokedProjectKeepsOpenTunnelForwarding` from the edited test file.

## Files changed (commit 033d1ba, 5 files, +17/−4)

- `docs/backlog.md` (new, 9 lines)
- `docs/agent-access.md` (+2 net: pointer line + blank; 1 line modified)
- `docs/README.md` (+1 index row)
- `internal/mcpserver/tunnels.go` (comment rewrap only)
- `internal/mcpserver/revoke_semantics_test.go` (comment rewrap only)

## Byte-level self-review

- **backlog.md vs brief block**: every content line was grep-compared full-line against the brief file (`C:\WorkSpace\agent\ssh-manager-mcp\.git\worktrees\plan-26-arrears\sdd\task-4-brief.md` lines 15-23); all 9 lines identical, line-number offset constant (+14). Quote characters in `拍板"暂不改行为"` / `只断"拉新"` proven ASCII `"` in BOTH files (curly-quote grep returned zero matches in both). `file` reports `UTF-8 text` (no BOM). One false alarm during verification: I initially suspected a missing blank line between intro and list — re-reading proved the blank exists in both (offset arithmetic confusion on my side, no edit was needed).
- **agent-access.md line 109** (`只断"拉新"` etc.): was inside old_string==new_string prefix of the item-4 edit, hence byte-preserved.
- **Post-edit re-reads**: tunnels.go:9-18, revoke_semantics_test.go:25-30, agent-access.md:102-115, README.md:30-35 all re-read after editing — no punctuation/whitespace drift; only the intended additions present.
- **Full `git show HEAD` diff** reviewed as the final pass: Go diffs touch comment lines only (`const` / `func` lines contextually unchanged).

## Concerns

- None blocking. Two judgment calls surfaced for the record: (1) the extra pointer edit at agent-access.md:108 — required to satisfy Step 3's per-line gate, sanctioned by the brief's Files line (「若提及隧道急停/backlog 处，grep 定位」); (2) plan archives under docs/superpowers/plans/ contain bare "backlog" mentions — treated as immutable provenance, not orphans, per the brief's "本来就是引用它" clause and docs/README.md's 内部文档 carve-out.
