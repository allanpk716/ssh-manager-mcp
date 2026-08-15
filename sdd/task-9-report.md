# Task 9 Report: TUI Form Constructor Extraction (C3a) + Review Fix Round

**Status:** DONE — original + all review fixes (C1, C2, M1, M2) applied
**Commits:** `dc11286` (original: `refactor: extract server form field constructors (zero behavior change)`) → `d8885f1` (fix round: `fix(tui): restore T5 可选 credential titles regressed by C3a extraction + real title-lock test`)
**Files:** `internal/tui/forms.go`, `internal/tui/forms_test.go`

## Original implementation (dc11286)

Extracted three constructors from `newServerForm` and rewrote the form as their composition, preserving the three groups and their field order:

- `passwordField(d *serverDraft, editing bool) *huh.Input` — password field, editing switches the title
- `sudoPasswordField(d *serverDraft) *huh.Input`
- `structuredFields(d *serverDraft) []huh.Field` — 硬件/位置/角色/服务/Caveats/备注, order = the old third group

Original regression tests: `TestNewServerFormFieldConstructors` (non-nil + len(6)) and `TestNewServerFormComposesConstructors` (non-nil form, both modes).

## Review findings and the fix round (d8885f1)

### C1 (Critical) — two form titles silently regressed to pre-T5 strings

What regressed (live source before dc11286 → what the refactor shipped):

| Field | Pre-refactor (correct, T5) | Refactored (wrong) |
|---|---|---|
| add-mode password | `密码（可选，与密钥二选一）` | `密码（与密钥二选一）` |
| key path | `私钥路径（可选，与密码互斥；编辑时留空=不变）` | `私钥路径（与密码二选一；编辑时留空=不变）` |

**Root cause: the plan sample was written from pre-T5 source.** Task 5 (earlier in Plan 20) made credentials optional and updated both titles to say 可选; the task-9 brief's code sketch predated that change in this hunk and still carried the old strings. The brief instructed "组内顺序/字段逐字节等价" (byte-identical) — the implementer satisfied that literally against the SKETCH (their stub report even claimed "Field titles match exactly") while the sketch itself had silently diverged from the live source. Lesson recorded: byte-identical-to-plan must always be cross-checked against `git show <ref> -- <file>` of the actual pre-change source, because the plan is a snapshot that can be stale.

Fix: both titles restored to the removed lines of `dc11286` (ground truth via `git show dc11286 -- internal/tui/forms.go`). The edit-mode title `密码（留空=保持不变）` was never wrong (the sketch had it right) and is untouched.

### C2 (Critical) — vacuous title-lock test replaced with real assertions

`TestNewServerFormFieldConstructors` asserted only non-nil + len(6) — C1 itself landed with that test green, proving it guarded nothing. Replaced by `TestNewServerFormFieldTitles`:

- Method: `reflect.ValueOf(f).Elem().FieldByName("title").FieldByName("val").String()`. Verified against huh v2.0.3 source: `Input.title` is an `Eval[string]` whose unexported `val` the `Title()` setter writes; `String()` is safe on read-only (unexported) reflect values — only `Interface()`/`Set*` panic.
- Asserts the six `structuredFields` titles IN ORDER (硬件 / 位置 / 角色 / 服务 / Caveats（agent 行动前必读） / 备注), `passwordField(d,false)` = `密码（可选，与密钥二选一）`, `passwordField(d,true)` = `密码（留空=保持不变）`, and (same mechanism, free) `sudoPasswordField(d)` = `sudo 密码（可选）`.
- Known limit: the key-path title is inline in `newServerForm` (not a constructor), so it is NOT reachable by this constructor-level reflection and remains unguarded (see Concerns).

### M1 — no-op loop deleted

The `for i, f := range fields { var _ huh.Field = f; _ = i }` block is gone — an interface-assignment compile check adds nothing at runtime and the loop asserted nothing.

### M2 — this report

The original round never wrote the report to the canonical `.git/worktrees/plan-20/sdd/` location; only an inaccurate stub landed untracked in the worktree's `sdd/` (it claimed "Field titles match exactly", which C1 disproves). This report replaces it in both locations.

## Test evidence

Covering tests = `internal/tui/forms_test.go` (title locks + all pre-existing forms tests).

- `go test ./internal/tui/... -count=1` → **ok** (1.2s): all 5 submitServer/toParts tests, `TestNewServerFormFieldTitles`, `TestNewServerFormComposesConstructors`, plus the wizard suite — no pre-existing test modified.
- **Mutation check (guards proven, both on a clean committed tree):**
  1. sed-flipped `密码（可选，与密钥二选一）` → `密码（与密钥二选一）` (reproducing the exact C1 regression): FAIL — `passwordField(d, false) title = "密码（与密钥二选一）", want "密码（可选，与密钥二选一）"`. Inverse-sed revert → `git diff --exit-code` empty, test PASS.
  2. sed-flipped `Title("位置")` → `Title("地址")` (structured order/lock): FAIL — `structuredFields(d)[1] title = "地址", want "位置"`. Reverted → `git diff --exit-code` empty, full suite ok.
- `go build ./...` clean; `go vet ./...` clean; `gofmt -l internal/tui/` empty.

## Self-review

- Ground-truth discipline: titles restored from the removed lines of `dc11286`, not from memory or the (stale) plan.
- The new test fails on any title change, any structured-order change, and would fail on a nil field (type assertion) — strictly stronger than everything it replaced.
- No behavior change beyond the two title strings; group order, fields, bindings, validation untouched.

## Concerns (for T10 / merge)

1. **Key-path title unguarded:** it lives inline in `newServerForm`, outside constructor reach; a future regression of `私钥路径（可选，与密码互斥；编辑时留空=不变）` would not fail any test. A render-and-grep of the form's `View()` was considered and rejected for brittleness (huh styles/wraps titles); worth a dedicated guard if T10 touches the form.
2. `TestNewServerFormComposesConstructors` still carries dc11286's comment "(huh API limitations prevent deeper inspection)" — now demonstrably false (the replacement test inspects deeper via reflection). Cosmetic; not flagged by review, left untouched.
3. Pre-existing (not this task): `internal/store/export_test.go` is not gofmt-clean as committed in T5; untouched here (`gofmt -l internal/tui/` is the scoped check and is clean).
