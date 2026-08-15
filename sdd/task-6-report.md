# Task 6 Report — 非破坏性 standalone→server 升级 `[u]`

**Status: COMPLETE.** Commit: see git log (`feat(tui): non-destructive standalone→server upgrade [u]`).

## What was built

### `internal/tui/upgrade.go` (new)
The serve-segment mini state machine owned by the broker App, reusing T4's pieces verbatim:

`upgAddr` (wizAddrForm LAN picker) → `upgAdmin` (serveAdminNotice) → `upgInstall` (installServeStep, binds 0.0.0.0:7878) → `upgProbe` (probeServe) → `upgResult` (serveResultScreen: install banner + manual command on failure, probe verdict) → `upgClientName` (one-field huh form, 客户端名 default hostname) → `upgDeviceIssue` (cert-first/code-second mint, same idempotency discipline as the wizard's) → `upgDeviceCode` (wizTokenScreen one-time screen with cache-pull merged-token usage line) → `upgAccessCard` (accessCard) → `upgradeComplete`.

- `upgradeSegment` is heap-allocated once (same stale-pointer rationale as `wizardData`).
- **Install-failure asymmetry (documented in code):** the walkthrough completes (manual command shown, device code already minted) but `roles.Save` is SKIPPED — machine stays standalone so `[u]` retries. Only a clean install writes `{server, setup_complete:true}`.
- Esc/empty answers on the two forms cancel the segment cleanly (`cancelUpgrade`).
- In-flight steps (install/probe/issue) show no overlay — the console stays visible; msgs advance the machine.

### `internal/tui/app.go` (modified)
- `App` gains `role roles.Role` + `upg *upgradeSegment`.
- `NewBrokerApp` populates role via `detectBrokerRole()`: role.json (Load) first; nil-state falls back to `roles.ResolveMode("")` (accepting only standalone/server answers — a broker App can never be client); undecided → `RoleStandalone` (safe default: only adds the `[u]` affordance).
- Footer appends `[u]升级为 server` while role == standalone; drops it the moment `upgradeComplete` flips `a.role`.
- Key dispatch: `u` starts the segment (guarded on standalone + no segment running).
- `formDoneMsg` routes into `upgradeFormDone` while the segment is live; new `serveInstalledMsg` / `serveProbeMsg` / `deviceCodeIssuedMsg` cases advance it (defensively no-op when no segment); `errMsg` aborts the segment back to the standalone console with the error visible.

### Deviation from the brief (one, deliberate)
The brief's flow list omitted the LAN-address picker, but `probeServe` and `accessCard` both require a real address (value=display discipline from T4). The segment therefore opens with the reused `wizAddrForm` picker as step 0. Everything else follows the brief's sequence exactly.

## Tests (`internal/tui/upgrade_test.go`) — TDD red→green
1. `TestUpgrade_KeyOpensSegment` — standalone App + `[u]` → overlay non-nil, segment at upgAddr; server-role App ignores `[u]`.
2. `TestUpgrade_FullFlowPersistsRoleKeepsVault` — fake installer via the package-level `serveInstall` seam (what `SetServeInstaller` wires): full simulated walkthrough → role.json `{server, setup_complete:true}`, App role flipped (footer drops `[u]`), status banner 「已升级为 server」, install hook saw `0.0.0.0:7878`, vault servers/profiles/projects counts unchanged, and exactly ONE cache token named after the client — the upgrade's only vault write.
3. `TestUpgrade_FooterRoleGated` — `[u]升级为 server` in standalone footer only.
4. `TestUpgrade_InstallFailureKeepsStandalone` — failed install shows manual command, completes walkthrough, role.json stays standalone, `[u]` still available.
5. `TestUpgrade_ErrMsgAbortsSegment` — a failed segment action aborts cleanly with the error surfaced.

## Verification
`go build ./... && go vet ./... && go test ./... -count=1` — ALL packages green. gofmt clean.

## Non-destructiveness argument
The segment's only possible writes: serve service registration, serve cert (idempotent), one device code, role.json. No code path calls AddServer/AddProfile/AddProject/GrantServers/Delete*/Rotate*. The device code IS a new vault entity by design (the flow's purpose) — the test pins servers/profiles/projects as the untouchable set, matching the brief's wording.

## Notes / residual edges
- Retry after a failed install re-mints a device code under the (possibly same) client name; if the earlier code survives with the same name, `AddCacheToken` surfaces the active-name collision as an errMsg → segment aborts with the error. Operator can revoke the stale code on the 设备码 page or pick a new name. Same behavior as the wizard path.
- Pressing `q` mid-segment quits (role.json untouched, at most one minted code remains — revocable from the 设备码 page).

## Fix round: Esc-cancel

Reviewer F1/F2/F3, one commit on this branch.

**F1 (Important) — Esc-cancel on the upgrade segment's form steps was broken.** The old detection (empty answers) could never fire: `wizAddrForm`'s select pre-commits its default (`*chosen = def`), the client-name form prefills the hostname, and `formOverlay` converted Esc to a bare `formDoneMsg{}` — so Esc ADVANCED the machine (first screen → admin notice → privileged install; name form → a real device code minted). Fix: `formDoneMsg` gains `aborted bool`, set true in `formOverlay.Update`'s Esc-intercept AND huh `StateAborted` branch; `upgradeFormDone(m formDoneMsg)` cancels the whole segment on `m.aborted` before any step dispatch. The wrong code comment (claiming huh aborts "before any preset default is committed") is replaced with the aborted-flag description. Audit of other `formDoneMsg` consumers: wizard never uses `formOverlay` (static screens / huh directly, Esc intercepted pre-form at wizard.go:417 → Quit) — no change; clientpage's `editConnForm` dismissal closes the form and runs `after` (nil on abort) — already correct; `secretView` emits bare `formDoneMsg{}` — untouched.

**F2 (Minor) — page action keys suppressed in-flight.** `a/e/d/g` dispatch in `App.Update` now returns early when `a.upg != nil`, so the overlay-less install/probe/deviceIssue windows can't have a form clobbered by segment msgs. (`u` already carried the same guard.)

**F3 (Minor) — broker role load fails closed.** `detectBrokerRole` now returns `(roles.Role, error)`; `roles.Load()` errors propagate out of `NewBrokerApp` instead of silently defaulting to standalone — matching roles' fail-closed design (unreachable in practice: `ResolveMode` gates the launch path earlier).

**Tests added:**
- `TestUpgrade_EscCancelsSegment` (upgrade_test.go) — drives the real path (Esc KeyPressMsg → `formOverlay` → `formDoneMsg{aborted}` → `App.Update`): scenario 1 Esc on the addr form → segment cancelled, status 「已取消升级」, fake `SetServeInstaller` hook never called; scenario 2 Esc on the client-name form → no device code (`ListCacheTokens` count 0).
- `TestUpgrade_PageKeysSuppressedInFlight` (upgrade_test.go) — segment at `upgInstall` with overlay nil, 'a' press → no overlay opened, segment untouched.
- `TestBrokerApp_RoleLoadFailsClosed` (app_test.go) — corrupt role.json + `NewBrokerApp` → non-nil error.

**Verification:** `go build ./... && go vet ./... && go test ./... -count=1` all green; gofmt clean; the three new tests pass individually (verified with `-run ... -v`).
