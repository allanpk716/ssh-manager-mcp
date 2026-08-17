# Plan 27 Task 1 Report — 检查框架 + role/env 检查 + 命令注册

**Status: DONE** (one flagged concern for the orchestrator, see Concerns)

## What was implemented

`ssh-manager doctor` skeleton per Plan 27 T1:

- `internal/cli/doctor.go` (new, ~230 lines): the check framework (`doctorCheck`/`checkStatus`), the checks table, the renderer, the exit-code error contract, the `env` and `role` checks, `newDoctorCmd` (with the 0/1/2 exit-code contract in `--help` per the plan's global constraint "稳定退出码写进 --help").
- `internal/cli/doctor_test.go` (new): `withDoctorDirs` (env-pinning fixture, withClearDirs discipline + the two extra cache-cred seams), `driveDoctor` (driveClear out-buffer pattern), `TestDoctorExitCodes`, `TestDoctorEnvSeamsReported`, `TestDoctorRoleStates`.
- `internal/cli/root.go`: one-line `newDoctorCmd()` addition to the AddCommand chain.

## TDD evidence

**RED** (Step 2) — `go test ./internal/cli/ -count=1 -run TestDoctor`:

```
internal\cli\doctor_test.go:84:21: undefined: errDoctorFindings
internal\cli\doctor_test.go:99:12: undefined: doctorExitCode
internal\cli\doctor_test.go:102:12: undefined: doctorExitCode
...
FAIL	ssh-manager-mcp/internal/cli [build failed]
```

**GREEN** (Step 4):

```
=== RUN   TestDoctorExitCodes
--- PASS: TestDoctorExitCodes (0.05s)
=== RUN   TestDoctorEnvSeamsReported
--- PASS: TestDoctorEnvSeamsReported (0.03s)
=== RUN   TestDoctorRoleStates
--- PASS: TestDoctorRoleStates (0.09s)
ok  	ssh-manager-mcp/internal/cli	1.014s
```

Full verification: `go test ./internal/cli/ -count=1` ok (8.3s), `go vet ./internal/cli/` clean, `gofmt -l internal/cli` empty, `go build ./...` clean.

**Manual smoke** (`go run ./cmd/ssh-manager doctor`, this dev machine):

```
ssh-manager doctor (dev)
env:  PASS  no SSHMGR_* environment overrides in effect
role:  INFO  no role.json — fresh machine, run the wizard
overall: 0 WARN, 0 FAIL
```

With `SSHMGR_MASTERKEY_HEX=4141` (exit stays 0 — WARN never changes the code; the value `4141` does NOT appear, name only):

```
env:  WARN  SSHMGR_MASTERKEY_HEX is set (dev/test affordance — production should not rely on it)
       fix: unset SSHMGR_MASTERKEY_HEX and provide the master key via the key file instead
role:  INFO  no role.json — fresh machine, run the wizard
overall: 1 WARN, 0 FAIL
```

This also demonstrates the "唯 SSHMGR_MASTERKEY_HEX → WARN instead of INFO" substitution: only-masterkey-set yields exactly one WARN row (no INFO row, no PASS row).

## Files changed

- Created `C:\WorkSpace\agent\ssh-manager-mcp\.claude\worktrees\plan-27-doctor\internal\cli\doctor.go`
- Created `C:\WorkSpace\agent\ssh-manager-mcp\.claude\worktrees\plan-27-doctor\internal\cli\doctor_test.go`
- Modified `C:\WorkSpace\agent\ssh-manager-mcp\.claude\worktrees\plan-27-doctor\internal\cli\root.go` (AddCommand + newDoctorCmd)

## Commits

- `c292b27` feat(cli): doctor skeleton — check framework, exit codes, env/role checks (Plan 27 T1)
- `c35d8d3` fix(cli): doctor env INFO wording — SSHMGR_* prefix, not SSHMgr (Plan 27 T1) [self-review polish]

## Shapes landed (T2/T3/T4 mount on these — verbatim)

```go
type checkStatus string

const (
	statusPass checkStatus = "PASS"
	statusWarn checkStatus = "WARN"
	statusFail checkStatus = "FAIL"
	statusInfo checkStatus = "INFO"
)

type doctorCheck struct {
	Name   string
	Status checkStatus
	Detail string // one human-readable line; NO secrets
	Fix    string // remediation; may be empty for PASS/INFO
}

// The checks table T2/T3/T4 append to (currently: checkEnv, checkRole):
var doctorCheckFuncs = []func() []doctorCheck{ ... }

// runDoctor: renders header `ssh-manager doctor (<buildinfo.Version>)`,
// rows `name:  STATUS  detail` (two spaces around STATUS), fix lines
// `       fix: <Fix>` (7 spaces) rendered ONLY for WARN/FAIL, tail
// `overall: N WARN, M FAIL`; returns nil / wrapped errDoctorFindings.
func runDoctor(cmd *cobra.Command, _ []string) error

var errDoctorFindings = errors.New("doctor: FAIL findings detected")

// doctorExitCode: nil→0, errDoctorFindings (wrapped incl.)→1, other→2.
func doctorExitCode(err error) int
```

Check-function naming convention: `checkEnv`, `checkRole` — T2's `checkStore`/`checkMasterKey`, T3's `vault-open`, T4's serve/cache checks follow as plain funcs returning `[]doctorCheck`, appended to `doctorCheckFuncs`. Rendering/counting/exit logic needs no changes from them.

## Semantics decisions worth knowing

- **role FAIL branch (addition beyond the brief's three states)**: `roles.Load()` can return an error (corrupt role.json / invalid role value — the exact broken state `ssh-manager clear` exists for). This is a FAIL with fix "run `ssh-manager clear` (writes a vault safety-net backup first), then re-run the wizard". It is also T1's FAIL fixture for the exit-1 test (see Deviations). Dual-residue Detail reports the LOADED state (vault location wins per `roles.Load` precedence) plus both-location presence.
- **env check two-row shape**: masterkey set → WARN row ("SSHMGR_MASTERKEY_HEX is set (dev/test affordance — production should not rely on it)" + fix); any other seams set → one INFO row listing NAMES joined ", " + "(values not shown)"; nothing set at all → single PASS row. When only masterkey is set: WARN replaces INFO entirely (matches the plan's substitution phrasing).
- **`withDoctorDirs`** = withClearDirs's full list (STORE/FILEKEY_PATH/MASTERKEY_HEX("")/CACHE_DIR("")/CACHE_DEK/SERVE_LOG/SERVE_CERT("")/SERVE_KEY("")/SERVE_MARKER("")/APPDATA/XDG_CONFIG_HOME) **plus** `SSHMGR_CACHE_URL=""` and `SSHMGR_CACHE_TOKEN=""` — seams the env check enumerates but clear never touched; without the reset a dev shell carrying a live cache token would break hermeticity.
- **Value-leak tests**: TestDoctorEnvSeamsReported asserts neither the 64-char hex value NOR the SSHMGR_STORE temp path value appears anywhere in the output.

## Deviations from the brief (deliberate, flagged)

1. **FAIL fixture**: the brief sketched "store.db path missing + role=server → errDoctorFindings" — that is T2's store check, not implementable in T1 without scope creep. Used the role check's own FAIL branch (corrupt role.json → `roles.Load()` error) instead: same exit-code contract exercised end-to-end through cobra.
2. **Third test**: added `TestDoctorRoleStates` (beyond the two named tests) to pin the role WARN-incomplete / PASS-complete / dual-residue branches — binding design decisions that would otherwise ship untested.
3. **`roleFilePresent` helper**: `fileExists` already exists in `migrate_path.go` with signature `(bool, error)`; the helper narrows it to bool rather than redefining the name.

## Self-review findings

- Completeness: all 11 env seams enumerated; three exit states asserted (incl. wrapped-sentinel); all five role states pinned; header/fix-line/overall-line formats asserted.
- Quality: smoke output above; every WARN/FAIL carries an actionable fix (tui resume, clear+wizard, unset env).
- Discipline: ONLY env+role checks — no store/masterkey/probe/serve/cache (T2-T4 untouched); zero MCP surface change; main.go NOT touched.
- Testing: RED→GREEN evidenced; pristine output (vet/gofmt/build clean, no t.Log noise).

## Concerns

1. **Exit code 2 is not reachable from the binary yet.** `cmd/ssh-manager/main.go` maps every cobra error to exit 1. Today runDoctor's only possible returns are nil and wrapped `errDoctorFindings` (check funcs cannot error), so observable behavior already matches the 0/1 convention on every real fixture — and the plan's four acceptance fixtures (空机/健康server/健康client/错配key) never exercise 2. The 2 branch lives in the pure, tested `doctorExitCode`. **For T5**: either wire main.go (~2 lines: a doctor-internal sentinel + `cli.ExitCode(err)` defaulting to 1, so non-doctor commands stay byte-identical) or phrase the docs so exit 2 isn't promised as observable. I did not touch main.go — it is outside T1's file list and changing every command's failure code was not mine to decide.
2. Deviation 1 above (FAIL fixture substitution) — reviewer should confirm the role-FAIL branch wording/approach is accepted as the plan's intent.
