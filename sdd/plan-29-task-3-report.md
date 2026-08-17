# Plan 29 Task 3 Report — app.go 接线 + 回归

**Status: DONE** — commit `e720660` on `worktree-plan-29-editpicker` (never master).
**Verification: `go test ./internal/tui/ -count=1` → `ok ssh-manager-mcp/internal/tui` (full package, all tests, after gofmt); `go vet ./internal/tui/`, `go build ./...`, `gofmt -l internal/tui/` all clean. No new deps.**

## Exact wiring diff

`internal/tui/app.go` — `serversKey`'s `"e"` branch (the only production change in this task):

```diff
 	case "e":
 		if cur := sp.current(); cur != nil {
-			draft := prefill(cur)
-			a.overlay = newFormOverlay("编辑服务器", newServerForm(draft, true), func() tea.Cmd {
-				return submitServer(a.st, cur, draft)
-			})
+			// Plan 29 T3: the field-picker edit page replaces the three-group
+			// long form — same overlay lifecycle (closes on formDoneMsg, runs
+			// submitServer via its after) and same whole-draft semantics, but
+			// entry is a paginated field list. Width comes from the App's
+			// WindowSizeMsg state (0 pre-report; the page floors itself).
+			a.overlay = newServerEditPage(a.st, cur, prefill(cur), a.width)
 		}
```

Net: 5 lines of huh-form construction → 1 constructor call (plus the rationale comment).

## Width field used

**`a.width`** — verified directly against the `App` struct (app.go:71): `width int // terminal width from WindowSizeMsg (0 = not yet reported)`, set at app.go:340-342 (`case tea.WindowSizeMsg: a.width, a.height = ...`). This is the panelization branch's width state (the same value `View()` feeds `sizedPage.Render(a.width, ...)` and `clip(a.width, ...)`). No other width field exists in `App`; `panels.go` holds no separate width state. Pre-`WindowSizeMsg` the value is 0 and `serverEditPage` floors itself (list width `max(width-2, 20)` = 20; `clipLines` renders unclipped) — T2's documented graceful path.

## Existing-test adaptations (before → after)

Only one existing test touched: `TestServersPageDispatch` in `internal/tui/app_test.go` (the sole App-level `e`-key test; written against the huh long form).

1. **Title expectation**: `{"e", "编辑服务器", false}` → `{"e", "编辑服务器: gpu", false}`. The page's `Title()` is the list title `"编辑服务器: " + orig.Name` (editpage.go:81, T2), so the exact-match assertion pins the target's name too — stronger, not weaker.

2. **Init-cmd expectation**: the test previously required a non-nil `Init` cmd for every overlay key. `formOverlay.Init()` returns huh's focus cmd (non-nil), but `serverEditPage.Init()` returns nil **by design** (editpage.go:123 — its list needs no async focus). Added a `wantInitCmd bool` column: `a`/`d`/`i` still assert a non-nil Init cmd (their formOverlay path is untouched); `e` asserts `false`; no-op rows (`g`, empty-list `e`/`d`) carry `false` and exit before that check anyway. Doc comment updated to explain the column and the Plan 29 title change.

Nothing else in app_test.go needed changes — no other test drives the servers `e` key (verified by grep: `编辑服务器` appears only in the dispatch table among app-level tests; forms_test.go's `newServerForm(d, true)` composition test is add/edit-form-level and stays valid since `newServerForm` remains).

## New test (TDD Step 1 — the failing test)

`TestServersEditKeyOpensFieldPicker` (app_test.go, right after the dispatch test):

- Builds the app, sends `tea.WindowSizeMsg{Width: 80, Height: 24}` through the real Update path, then presses `e`.
- Asserts the overlay is `*serverEditPage` (type-level: the field-picker page, not `*tui.formOverlay`) **and** `p.width == 80` — pins that the App's width state is what the page was built from.
- Walks `↓` through `App.Update` (which forwards KeyPressMsg to the overlay) one press past every item — the walk crosses all picker pages (16 rows paginate at the fixed height) — accumulating `View().Content`:
  - present: header 「编辑服务器: gpu」, four field labels 「名称」「Host」「端口」「SSH 用户」 (task asked ≥3), save sentinel 「✓ 保存并退出」;
  - absent: old long-form-only wordings 「名称（唯一）」 and 「Host / IP」 — those strings now exist only inside single-field field-state forms, never in the picker list.
- Comment notes that `a`/`d`/`i` regression rides on `TestServersPageDispatch`.

## TDD evidence

- **RED** (before the app.go change, new tests in, production code untouched):
  ```
  === RUN   TestServersPageDispatch
      app_test.go:121: key "e" opened "编辑服务器", want "编辑服务器: gpu"
  --- FAIL: TestServersPageDispatch (0.05s)
  === RUN   TestServersEditKeyOpensFieldPicker
      app_test.go:149: e must open the field-picker page (serverEditPage), got *tui.formOverlay
  --- FAIL: TestServersEditKeyOpensFieldPicker (0.02s)
  ```
  Both failed for exactly the reasons the wiring addresses — old title, old overlay type.
- **Baseline** before touching anything: full package `ok` (3s), so the RED came only from the new expectations.
- **GREEN**: after the 1-line wiring, `go test ./internal/tui/ -count=1` → `ok ssh-manager-mcp/internal/tui` (full package, includes all T1/T2 editfields/editpage suites, wizard, importflow, servers, panels, etc.).
- Post-gofmt re-run: still `ok` (gofmt only realigned one comment column).

## Untouched paths (constraint check)

- `a` (新增): `newFormOverlay("新增服务器", newServerForm(draft, false), ...)` — byte-identical.
- `i` (导入): `newImportFlow(a.st)` — byte-identical.
- `d` (删除), `!`, wizard (`wizardsteps.go` uses `newServerForm(d, false)`), importflow (`prefill(f.srv)`) — byte-identical.
- The App-level form-cmd routing bug stays out of scope (tracked separately), per the task constraints.
- `newServerForm` survives: add flow (app.go:445), wizard (wizardsteps.go:64), forms_test.go. The `editing=true` variant is now exercised only by its composition test — expected, since the only `true` call site was the `e` branch this task replaces.
- Import set of app.go unchanged (`huh` still used by `d`/profiles/projects/tokens branches; `fmt`/`strings` unaffected).

## Self-review

- Lifecycle compatibility re-verified against the App's `formDoneMsg` case (app.go:359-364): `a.overlay = nil; return a, m.after` — the page's save rides `formDoneMsg{after: submitServer(...)}` and abort rides `formDoneMsg{aborted: true}`, both already pinned end-to-end by T2's `TestEditPageSaveEndToEnd` / `TestEditPageListEscAbortsNoWrite`. T3 adds the App-level entry assertion; the combination covers the full chain.
- Empty-list guard preserved: `if cur := sp.current(); cur != nil` wraps the new constructor — `e` on an empty list stays a silent no-op (dispatch table's emptyList rows, still passing).
- Width plumbing asserted, not assumed (`p.width != 80` check) — catches a future field rename or a wiring regression that passes a literal.
- The walk loop `i <= len(p.fields)` mirrors T2's own ↓-walk bound (one press past the last item), so the sentinel on the final page is guaranteed to surface.

## Concerns

- None blocking. Two observations for the record:
  1. Pressing `e` before any `WindowSizeMsg` builds the page at width 0 → narrow-but-functional 20-col list (T2's documented floor). Not new in T3; acceptable per T2 design.
  2. The dispatch test's `e` row no longer asserts a non-nil Init cmd. That is intentional and documented (`serverEditPage.Init()` is a deliberate no-op); the picker's interactivity is instead pinned structurally (type + width + view-walk) in the new test.
