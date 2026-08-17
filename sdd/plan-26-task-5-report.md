# Task 5 Report: cosmetic 批（Plan 25 残留 4 处）

## Status: DONE

## Commit: `b96de92` — docs(cosmetic): creation-based reclaim wording, broker-filesystem parentheticals, idle-sweeper naming (Plan 25 leftovers, Plan 26 T5)

5 files changed, 15 insertions(+), 15 deletions(-). Zero behavior change — every hunk is a comment or a `t.Log` string.

## Edits (located by grepping the brief's quoted text, never by line number)

### Group 1 — internal/eval/summary_test.go (1 edit; grep `idle-timeout`)

| Old | New |
|---|---|
| `t.Log("forward_port + close_port (ssh -L, stateful TunnelManager, ~10 min idle-timeout + close_port +")` | `t.Log("forward_port + close_port (ssh -L, stateful TunnelManager, ~10 min after-creation auto-close + close_port +")` |

Only the fragment `~10 min idle-timeout` → `~10 min after-creation auto-close`; rest of line byte-identical. Pure ASCII, no assertion touches this string.

### Group 2 — internal/mcpserver/types.go (1 edit; grep `\(= the agent's\)`)

| Old | New |
|---|---|
| `// UploadInput is the upload_file tool input. LocalPath is read from the broker's`<br>`// (= the agent's) filesystem; RemotePath is the destination on the server.` | `// UploadInput is the upload_file tool input. LocalPath is read from the broker's`<br>`// filesystem; RemotePath is the destination on the server.` |

Deleted exactly `(= the agent's) ` (parenthetical + one trailing space) from the wrapped comment line. Everything else byte-identical (line 48 untouched, incl. ASCII apostrophe in `broker's`). The jsonschema line's "on the machine the broker runs on" already covers serve mode.

### Group 3 — internal/mcpserver/core.go:289 (1 edit; grep `\(= the agent's\)`)

| Old | New |
|---|---|
| `// localPath is read from the broker's (= the agent's) filesystem; remotePath is` | `// localPath is read from the broker's filesystem; remotePath is` |

Deleted exactly ` (= the agent's)` — single line, rest byte-identical.

### Group 4 — "idle sweeper" wording → "tunnel sweeper" (10 edits; grep `idle sweeper` + `idle-sweeper`)

Brief named core.go:412 and run.go:43 explicitly; for tunnels.go it said 命中处 ("the hits"). I interpreted 字样 to cover all occurrences of the wording in tunnels.go — space-separated, hyphenated (`idle-sweeper`), and the line-broken one (`The idle` / `sweeper goroutine` at old :47-48) — because fixing only the 3 space-separated hits would leave the file half-renamed and self-inconsistent, violating the brief's stated principle (注释不得暗示活动感知). Identifiers `forwardSweepInterval`, `SweepIdle`, `forwardIdleTimeout` untouched everywhere.

| File:line (old) | Old | New |
|---|---|---|
| core.go:412 | `// reclaimed by close_port (CloseForwardForProfile), the idle sweeper` | `// reclaimed by close_port (CloseForwardForProfile), the tunnel sweeper` |
| run.go:43 | `	// leaked SSH connections. The idle sweeper goroutine is also stopped. (The` | `	// leaked SSH connections. The tunnel sweeper goroutine is also stopped. (The` |
| tunnels.go:18 | `// forwardSweepInterval is the ticker period for the idle sweeper goroutine.` | `// forwardSweepInterval is the ticker period for the tunnel sweeper goroutine.` |
| tunnels.go:19-20 | `// One minute is fine-grained enough that a 10-min idle tunnel is reaped within`<br>`// ~10–11 min, and coarse enough that the sweeper is idle work in steady state.` | `// One minute is fine-grained enough that a tunnel is reaped within ~10–11 min`<br>`// of creation, and coarse enough that the sweeper is idle work in steady state.` |
| tunnels.go:27 | `// until close_port, the idle sweeper, or MCP shutdown tears it down.` | `// until close_port, the tunnel sweeper, or MCP shutdown tears it down.` |
| tunnels.go:36 | `// closes; the idle sweeper (SweepIdle) closes tunnels whose lastActivity is` | `// closes; the tunnel sweeper (SweepIdle) closes tunnels whose lastActivity is` |
| tunnels.go:47 | `// The struct is safe for concurrent use (every method takes mu). The idle` | `// The struct is safe for concurrent use (every method takes mu). The tunnel` |
| tunnels.go:60 | `// NewTunnelManager returns an empty TunnelManager. The idle-sweeper goroutine` | `// NewTunnelManager returns an empty TunnelManager. The tunnel-sweeper goroutine` |
| tunnels.go:71 | `// StartSweeper launches the idle-sweeper goroutine at most once. It is a no-op` | `// StartSweeper launches the tunnel-sweeper goroutine at most once. It is a no-op` |
| tunnels.go:110 | `// Touch refreshes a tunnel's lastActivity to now, deferring idle-sweeper` | `// Touch refreshes a tunnel's lastActivity to now, deferring tunnel-sweeper` |
| tunnels.go:164 | `// the idle-sweeper goroutine if it was started. This is the MCP-shutdown path:` | `// the tunnel-sweeper goroutine if it was started. This is the MCP-shutdown path:` |

The tunnels.go:19-20 edit is the one non-literal-字样 change: old line 19 said "a 10-min idle tunnel is reaped within ~10–11 min" — a direct 无活动回收 statement inside the same comment block as named hit :18, condemned by the brief's principle. Reworded to "a tunnel is reaped within ~10–11 min of creation". The pre-existing en-dash U+2013 in `~10–11` was carried over verbatim (verified below).

## git diff hunk list (14 hunks + 4 pre-existing sdd deletions, not staged)

- `internal/eval/summary_test.go` @@ -134,7 +134,7 @@ — 1 line, group 1
- `internal/mcpserver/core.go` @@ -286,7 +286,7 @@ — 1 line, group 3
- `internal/mcpserver/core.go` @@ -409,7 +409,7 @@ — 1 line, group 4
- `internal/mcpserver/run.go` @@ -40,7 +40,7 @@ — 1 line, group 4
- `internal/mcpserver/tunnels.go` @@ -15,16 +15,16 @@ — lines 18, 19-20, 27 (3 changes)
- `internal/mcpserver/tunnels.go` @@ -33,7 +33,7 @@ — line 36
- `internal/mcpserver/tunnels.go` @@ -44,7 +44,7 @@ — line 47
- `internal/mcpserver/tunnels.go` @@ -57,7 +57,7 @@ — line 60
- `internal/mcpserver/tunnels.go` @@ -68,7 +68,7 @@ — line 71
- `internal/mcpserver/tunnels.go` @@ -107,7 +107,7 @@ — line 110
- `internal/mcpserver/tunnels.go` @@ -161,7 +161,7 @@ — line 164
- `internal/mcpserver/types.go` @@ -46,7 +46,7 @@ — 1 line, group 2
- `sdd/task-{1,2,3,4}-report.md` deletions — pre-existing working-tree state before this task started (orchestration cleanup of earlier task reports); NOT staged, NOT committed.

## Byte-level review (Step 2)

`git diff` read hunk-by-hunk before staging. Checks:

- Every hunk contains only the intended change; context lines (including the em-dashes on core.go:288, run.go:40-42, tunnels.go:11-15/23-26) untouched.
- All changed lines are pure ASCII except the moved `~10–11` (en-dash U+2013), which was copied verbatim from the Read output. Post-edit grep `~10–11` (en-dash) matches tunnels.go:19; grep `~10-11` (ASCII hyphen — the corruption signature) returns zero matches across internal/. No smart-quote substitution anywhere (Plan 25 T4 regression class).
- Apostrophes in `broker's` / `agent's` remain ASCII U+0027.
- run.go tab indent preserved (diff context whitespace identical).
- No reformatting side effects: `gofmt -l .` empty.

## Verification (Step 3) — each as a separate simple command

| Command | Result |
|---|---|
| `go build ./...` | no output (success) |
| `go vet ./...` | no output (success) |
| `gofmt -l .` | no output (nothing to format) |
| `go test ./... -count=1` | all 14 test packages `ok` (cli 10.3s, clientops 10.9s, conformance 0.8s, **eval 2.2s**, importer 0.2s, **mcpserver 7.5s**, paths 0.1s, roles 2.5s, sshbroker 6.8s, store 4.0s, testsshd 2.2s, tui 4.1s, vault 1.8s, vaultio 1.9s) — zero FAIL |

(Bash guard note: package paths containing "eval" cannot be named in commands, so the test-package proof ran via `go test ./... -count=1`, which covers internal/eval.)

## Commit notes

- The brief's Step 4 `git add` line (a) contains the guard-blocked substring and (b) omits `internal/mcpserver/tunnels.go` even though tunnels.go is in the task's Files list and Step 1.4 requires editing it. Staged via `git add internal` (exactly the five edited files, verified with `git status` before committing); commit message taken verbatim from the brief plus the required Co-Authored-By trailer.
- Commit: `b96de92` on branch `worktree-plan-26-arrears`. Working tree after commit: only the pre-existing unstaged sdd deletions.

## Concerns

1. **Residual "idle" mentions outside this task's file list** (plan-level acceptance #3 material, not edited — scope discipline; the brief's Files list is authoritative):
   - `internal/mcpserver/server.go:55` — `tunnels.StartSweeper() // background idle-reaper (closes tunnels idle > forwardIdleTimeout)` — this is the clearest remaining 无活动回收 phrasing in the repo; recommend a follow-up one-word fix ("closes tunnels older than forwardIdleTimeout" / "tunnel sweeper").
   - `internal/mcpserver/tunnels.go:81` — `// sweepLoop is the idle-reaper: every forwardSweepInterval it calls SweepIdle.` — different noun ("reaper", not the brief's 字样 "idle sweeper"); mechanism description only, borderline.
   - `internal/mcpserver/core_test.go:961` — `// TestTunnelManagerSweepIdleReapsStaleTunnels verifies the idle-sweeper's` — test-file comment naming.
   - Left deliberately untouched as non-misleading: tunnels.go:101-102 "the MVP idle signal = open time" and :111 "(idle = open-duration)" both explicitly define idle as creation-time (anti-ambiguity clarifiers); tunnels.go:20 "the sweeper is idle work" refers to the sweeper doing little work, not tunnel idleness; `forwardIdleTimeout`/`SweepIdle` identifiers; unrelated "idle" in serve.go:259 and cli/backup.go:68-70.
   If the plan verifier wants acceptance #3 strictly clean, server.go:55 is the one that matters.
2. The brief's git add line omission of tunnels.go (noted above) — resolved by staging `internal`.
