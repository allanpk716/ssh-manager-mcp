# Task 3 Report: owner `ssh` 共享 120s deadline + 退出码传播

## Status: ✅ COMPLETE

## Commit: `323dc5dca4daee3197767c46773bc3f3382dfd86`

## Implementation Summary

### Changes Made

**File: `internal/cli/ssh.go`**
1. Added package-level `ownerSSHDeadline` seam (120s default, overridable for tests)
2. Added ctx creation with timeout in RunE (after error check block)
3. Replaced `context.Background()` in `sshbroker.Connect()` call with `ctx`
4. Replaced `context.Background()` and hardcoded 120s in `cli.Exec()` call with `ctx` and `ownerSSHDeadline`
5. Fixed exit code propagation: now returns cobra error with exit code message instead of nil

**File: `internal/cli/ssh_test.go`**
1. Added imports: `io`, `os`, `time`, `models`, `testsshd`
2. Added T2 review fix: store-not-created assertion in `TestOwnerSSHNoCommandErrors`
3. Added `TestOwnerSSHPropagatesRemoteExitCode`: verifies remote exit code 3 surfaces as cobra error with output printed
4. Added `TestOwnerSSHConnectDeadlineBounded`: verifies 2s deadline bounds unreachable-host connect (RFC5737 TEST-NET-3: 203.0.113.1)

### Test Results

```
=== RUN   TestOwnerSSHExecRunsCommand
--- PASS: TestOwnerSSHExecRunsCommand (0.11s)
=== RUN   TestOwnerSSHNoCommandErrors
=== RUN   TestOwnerSSHNoCommandErrors/host_only
=== RUN   TestOwnerSSHNoCommandErrors/empty_string_cmd
=== RUN   TestOwnerSSHNoCommandErrors/whitespace_cmd
--- PASS: TestOwnerSSHNoCommandErrors (0.06s)
=== RUN   TestOwnerSSHPropagatesRemoteExitCode
boom
--- PASS: TestOwnerSSHPropagatesRemoteExitCode (0.09s)
=== RUN   TestOwnerSSHConnectDeadlineBounded
Error: context deadline exceeded
    ssh_test.go:147: elapsed=2.0237912s err=context deadline exceeded
--- PASS: TestOwnerSSHConnectDeadlineBounded (2.04s)
PASS
ok  	ssh-manager-mcp/internal/cli	3.094s
```

All 4 owner SSH tests pass:
- `TestOwnerSSHExecRunsCommand` (existing smoke test)
- `TestOwnerSSHNoCommandErrors` (T2, with store-not-created assertion)
- `TestOwnerSSHPropagatesRemoteExitCode` (new: exit code propagation)
- `TestOwnerSSHConnectDeadlineBounded` (new: deadline sharing)

### Key Verification Points

1. **Exit code propagation**: Remote exit code 3 now surfaces as cobra error `fmt.Errorf("remote command exited with code %d", res.ExitCode)` while stdout is still printed before the error (verified by test assertion).

2. **Deadline sharing**: Unreachable host (203.0.113.1) connect elapsed in ~2.02s (within 15s test threshold), proving the 2s deadline bounds the connect phase. Without the shared deadline, this would hang for the OS TCP timeout (minutes).

3. **T2 review fix**: All three subtests in `TestOwnerSSHNoCommandErrors` now verify that `test.db` is NOT created when args are rejected (via `os.Stat` + `os.IsNotExist` check), ensuring validation precedes store opening.

### Code Diff Highlights

**ssh.go additions:**
- Package variable: `var ownerSSHDeadline = 120 * time.Second`
- Context setup: `ctx, cancel := context.WithTimeout(context.Background(), ownerSSHDeadline); defer cancel()`
- Connect call: `sshbroker.Connect(ctx, ...)` (was `context.Background()`)
- Exec call: `cli.Exec(ctx, commandStr, ownerSSHDeadline, 0)` (was `context.Background(), 120*time.Second`)
- Exit propagation: `return fmt.Errorf("remote command exited with code %d", res.ExitCode)` (was `return nil`)

**ssh_test.go additions:**
- T2 fix: `if _, err := os.Stat(filepath.Join(dir, "test.db")); !os.IsNotExist(err) { t.Fatalf("store must not be created for rejected args") }`
- Two new test functions with full fixture setup and assertions

## Concerns

None. Implementation follows the brief exactly; all tests pass on first run; deadline and exit code propagation verified.
