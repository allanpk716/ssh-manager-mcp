# Plan 27 Task 4 Report — ReadServeCertFingerprint (read-only) + serve/cache doctor checks

**Status: COMPLETE.** Commits `248ec0e` (T4a) and `d066051` (T4b) on `worktree-plan-27-doctor`. Both packages green, gofmt/vet clean, working tree clean.

## What was implemented

### Part A — `mcpserver.ReadServeCertFingerprint` (commit 248ec0e)

`internal/mcpserver/cert.go`: exported read-only twin of `LoadOrCreateServeCert`, inserted between `LoadOrCreateServeCert` and `loadServeCertFingerprint`. Same three-state semantics MINUS all generation:

1. **cert present + parses** → `loadServeCertFingerprint` (same call Load uses) → returns `(certPath, keyPath, fp, nil)`.
2. **cert present but corrupt/mismatches key** → byte-identical refuse-to-start error wording as Load.
3. **cert absent + marker present** → the F10 error, verbatim Load wording ("deleted out-of-band", delete BOTH marker+cert, re-enroll all clients).
4. **both absent** → `serve cert not initialized (run `serve cert-info` once, or start serve)`.

NEVER writes: no `atomicWriteFile`, no `store.HardenACL`, no marker write anywhere on any path. Doctor relies on exactly this.

### Part B — three doctor checks (commit d066051)

`internal/cli/doctor.go`, appended to `doctorCheckFuncs` after `checkVaultOpen`:

- **`serve-cert`** — resolves `paths.ServeCertPath()` + `paths.ServeCertMarkerPath()`; both absent → INFO "serve not in use (no serve cert and no init marker)" (no error); otherwise calls `mcpserver.ReadServeCertFingerprint()` → PASS with `serve cert present (fingerprint sha256:…)` (public info — clients receive it on connect), error → FAIL with the twin's text as Detail (covers corrupt + F10, both of which embed their own recovery steps; Fix points at `serve cert-info` for verification).
- **`serve-svc`** — new seam `var serveServiceState = func() string` defaulting to runServeStatus's kardianos pattern (`service.New(&program{}, cfg)` → `s.Status()` five-state mapping; `ErrNotInstalled` → "NOT INSTALLED"; and `ErrNoServiceSystemDetected` → "NOT INSTALLED" too, the doctor simplification). Mapping: Running → PASS; Stopped → WARN (Fix: idempotent `serve install` re-run / OS service manager); NOT INSTALLED → WARN on role=server (Fix: `serve install`), INFO otherwise; anything else → WARN "indeterminate" with the raw state.
- **`client-cache`** — via `clientops.CachePaths()`: cache.bin present → sidecar matrix: DEK (`paths.CacheDekPath()`) missing → FAIL "cache undecryptable" (Fix: re-run client wizard + pull); `cache.auth.json` (`filepath.Join(dir, "cache.auth.json")`) missing → WARN "no auto-refresh credential (manual `cache pull` only)"; else PASS `cache.bin present (age 2h13m0s)` (age = `time.Since(mtime).Round(time.Minute)`). cache.bin missing: FAIL on role=client (Fix: `cache pull`), INFO otherwise.

### Test harness hardening (necessary, discovered at first GREEN attempt)

The first run of `TestDoctorServeAndCache` failed because `serve-cert: PASS` showed **the dev machine's real serve cert fingerprint**: `withDoctorDirs` pinned `SSHMGR_SERVE_CERT/KEY/MARKER` to `""`, which falls back to the REAL `VaultDir()`. Two fixes, both in `doctor_test.go` only:

1. `withDoctorDirs` now pins the three serve seams INTO the temp vault dir — honors its own documented contract ("isolates every filesystem/env location doctor READS"); T4's checks are the first to read those paths. (Production behavior unchanged — unset env still resolves the real vault dir.)
2. Every pre-existing doctor test (`TestDoctorExitCodes`, `TestDoctorEnvSeamsReported`, `TestDoctorRoleStates`, `TestDoctorVaultStructural`, `TestDoctorVaultOpen`) now stubs `serveServiceState` ("Running") — without it, serve-svc would query the host's real SCM and the exact "overall: N WARN, M FAIL" assertions would be machine-dependent (a stopped real service → surprise WARN; not-installed on a server-role leg → surprise WARN).
3. `TestDoctorVaultStructural` Case 4's client leg now seeds a healthy cache via `seedDoctorCache` — a client machine without cache.bin is a legitimate T4 FAIL, and the leg's intent (store/masterkey INFO) is preserved with a realistic client setup.

## TDD evidence

**Part A (cert_test.go, `TestReadServeCertFingerprintReadOnly`):**
- RED: `undefined: ReadServeCertFingerprint` (build failure) at the three call sites.
- GREEN after implementing the twin: `ok ssh-manager-mcp/internal/mcpserver 0.839s` (target), then full package `ok … 3.873s`.
- Coverage: ① cert+key present → pin matches the seeded Load fingerprint AND zero-write proof (mtime/size of both files via `statOrFatal`, plus file COUNT via `dirEntryCount` unchanged at 3); ② both absent (fresh dir) → error contains "not initialized" AND directory still EMPTY (0 entries — no generation, no marker); ③ marker present + cert absent → error contains "out-of-band" AND cert not re-created.

**Part B (doctor_test.go, `TestDoctorServeAndCache`, 11 legs):**
- RED: `undefined: serveServiceState` (build failure).
- GREEN after implementing the three checks + seam; final full runs: `ok ssh-manager-mcp/internal/cli 9.302s`, `ok ssh-manager-mcp/internal/mcpserver 3.812s` (two separate commands, per the session guard).
- Legs: serve-cert PASS (real cert seeded via `mcpserver.LoadOrCreateServeCert` in-test — legal; fingerprint asserted in output) / F10 FAIL (seed then rm cert, marker stays) / not-in-use INFO; serve-svc Stopped→WARN / NOT INSTALLED+server→WARN with `serve install` fix / NOT INSTALLED+standalone→INFO / Unknown→WARN indeterminate; client-cache PASS (cache.bin chtimes'd 2h13m into the past → "age 2h13m" prefix) / client+missing→FAIL `cache pull` / DEK missing→FAIL undecryptable / auth missing→WARN "no auto-refresh credential (manual `cache pull` only)" / standalone+missing→INFO.

## Files changed

- `internal/mcpserver/cert.go` — +57 lines: `ReadServeCertFingerprint`.
- `internal/mcpserver/cert_test.go` — +107 lines: `statOrFatal`, `dirEntryCount`, `TestReadServeCertFingerprintReadOnly`.
- `internal/cli/doctor.go` — +193 lines: imports (time, clientops, mcpserver, kardianos service), three checks in `doctorCheckFuncs`, `serveServiceState` seam, `checkServeCert`/`checkServeSvc`/`checkClientCache`.
- `internal/cli/doctor_test.go` — +324/-4: harness hardening (withDoctorDirs serve pins, SCM stubs in 5 tests, client-leg cache seed), `stubServeServiceState`/`seedDoctorServeCert`/`seedDoctorCache` helpers, `TestDoctorServeAndCache`, imports (time, mcpserver).

## Self-review

- **Iron rule (no secret VALUES)**: serve-cert Detail carries the SPKI fingerprint (public by design); F10/corrupt Details carry file PATHS (allowed — doctor's contract is paths/sizes/counts/ages/fingerprints). serve-svc and client-cache Details carry state/age only. No token, key, or plaintext anywhere.
- **Side-effect-free doctor path**: `ReadServeCertFingerprint` writes nothing (pinned by the mcpserver zero-write test); `serveServiceState` only queries the SCM (`Status()` is read-only); `checkClientCache` only Stats. Generation happens ONLY inside test seeding helpers.
- **Seam pattern**: `serveServiceState` follows `stubClearExternals` precedent (package-internal var, direct assignment, t.Cleanup restore). `checkServeSvc`'s `switch state := serveServiceState(); {` covers all mapped strings; the default branch catches every unmapped one.
- **Wording consistency**: corrupt + F10 errors are byte-identical to `LoadOrCreateServeCert`'s; "not initialized" matches the spec string exactly.
- `go build ./...` clean; `gofmt -l internal/` empty; `go vet` on both packages clean.

## Concerns

1. **Real-SCM exposure in production is intended**: `serve-svc` genuinely queries the host service manager when `doctor` runs for real — on a machine with the service Stopped it emits a WARN (exit stays 0). That is the check's purpose; only tests are hermetic.
2. **`serveServiceState` default constructs `&program{}`** — identical to `runServeStatus`'s own `service.New(&program{}, cfg)`; `Status()` never invokes program callbacks, so the zero-value is safe.
3. **Age formatting**: Go's `Duration.String()` renders "2h13m0s" (seconds always present ≥1s); the task's "age 2h13m" example was illustrative. Tests prefix-match "age 2h13m" so both hold.
4. **`withDoctorDirs` change touches a T1 helper** — behavior-neutral for T1–T3 assertions (verified: all five pre-existing doctor tests pass unmodified except the three deliberate hardening edits listed above; the env row merely lists three more seam NAMES).
5. Doctor `serve-cert` FAIL rows reuse the twin's embedded recovery text and add a pointer Fix ("follow the recovery steps in the detail above…") rather than duplicating remediation — deliberate, to keep one source of truth for the F10 wording.
