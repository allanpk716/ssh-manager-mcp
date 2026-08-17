# Plan 26 Task 3 Report — `clear` 双角色补测 + 文档段落

**Status: DONE**

Commit: `86f1bd1` — `test+docs: clear dual-role enumeration pinned and documented (Plan 26 T3)` (branch `worktree-plan-26-arrears`, 2 files, +26 lines)

## What was implemented

### 1. Characterization test (verbatim from brief)

`TestEnumClearTargets_DualRoleMachine` appended to `internal/cli/clear_test.go` (now clear_test.go:166-189), placed inside the `enumClearTargets` section right after `TestEnumClearTargets_ServerMachine` so the three enum tests read as a group (full server machine → dual-role machine → empty machine).

- Reuses `withClearDirs` / `seedClearVault` / `stubClearExternals` exactly as the sibling test does (env fully pinned: SSHMGR_* + APPDATA/XDG_CONFIG_HOME).
- Seeds BOTH role.json locations via two `roles.Save` calls (RoleServer → `<vaultDir>/role.json`, RoleClient → `<userDir>/ssh-manager/role.json`). The brief's os.WriteFile fallback was NOT needed: verified `roles.Save` (roles.go:157) routes through `RolePath(s.Role)`, whose server and client branches resolve to the two different dirs `scanClearTargets` (clear.go:188-193) enumerates — matching the controller's pre-resolved ambiguity note.
- Asserts exactly 2 lines containing `role.json` (`n != 2` → fatal). No other enumerated path can contain that substring (`store.db.meta.json` / `cache.meta.json` contain `.json` but not `role.json`; temp dirs are random names).
- Test code is byte-verbatim from the brief, including the mid-test comment.

### 2. Doc paragraph (verbatim from brief Step 3)

Landed in **`docs/concepts.md:88`** — appended as the second paragraph of the dedicated section `## ssh-manager clear（角色清理，一段话版）` (concepts.md:84).

Why there and not README.md / getting-started.md (the brief's stated expectation):
- The brief itself says location is "以 `git grep -n "clear" docs/ README.md` 实际命中为准" (actual hits govern).
- The strongest hit by far is concepts.md:84 — a dedicated one-topic section describing exactly what clear does and how it enumerates ("按实际存在枚举"), which is precisely what the new paragraph elaborates. It is the page the first-run wizard points users to and the "记不清谁是谁" reference page.
- README.md's only clear mention (:92, :137) is a one-line features-list entry — a full semantics paragraph would be out of place there. docs/getting-started.md had zero clear hits.
- Consequently `git add` used `docs/concepts.md` instead of the brief Step 4's `README.md` (README.md untouched).

## Test command + output

```
$ go test ./internal/cli/ -count=1 -run TestEnumClearTargets -v
=== RUN   TestEnumClearTargets_ServerMachine
--- PASS: TestEnumClearTargets_ServerMachine (0.03s)
=== RUN   TestEnumClearTargets_DualRoleMachine
--- PASS: TestEnumClearTargets_DualRoleMachine (0.02s)
=== RUN   TestEnumClearTargets_EmptyMachine
--- PASS: TestEnumClearTargets_EmptyMachine (0.00s)
PASS
ok  	ssh-manager-mcp/internal/cli	0.848s

$ go test ./internal/cli/ -count=1
ok  	ssh-manager-mcp/internal/cli	8.028s

$ gofmt -l internal/cli/clear_test.go
(no output — clean)
```

The characterization test was GREEN on first run — current behavior matches the plan's understanding; no BLOCKED path taken.

## Files changed

- `internal/cli/clear_test.go` — +24 lines (one test, verbatim)
- `docs/concepts.md` — +2 lines (blank line + verbatim paragraph)

No changes to clear.go, roles.go, or any production code. No new deps.

## Self-review

- Completeness: test in and green; doc paragraph in; test asserts exactly two role lines. ✓
- Quality: test comment states WHY dual-role matters (mid-migration machine; scan is role-blind per scanClearTargets contract; role.json claims don't gate the scan). ✓
- Discipline: zero production-code changes; commit contains only the two intended files (the unstaged `sdd/task-1/2-report.md` deletions in the worktree are the controller's prior-report cleanup, not staged, not mine). ✓
- Testing: full cli package green, output pristine, gofmt clean. ✓

## Concerns (minor, out of scope)

`README.md:137` pre-existing text says clear does "**按角色枚举**删除本机 vault / serve / 缓存残留" — "enumerates by role" now reads contrary to the documented role-blind scan. Fixing that one phrase would be a 1-word docs edit, but it is pre-existing text outside this task's "add one paragraph" scope, so I left it. Flagging for the controller in case a follow-up tweak is wanted.
