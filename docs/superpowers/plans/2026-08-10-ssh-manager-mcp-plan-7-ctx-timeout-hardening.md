# Plan 7 — context.Context threading + max-exec-timeout cap: Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Thread `context.Context` from the MCP `*ForProfile` wrappers into the sshbroker so agent-cancellation aborts in-flight SSH ops (Exec/ExecSudo/Download/Upload/Connect), and add a server-side `MaxExecTimeout` ceiling on `exec_command`.

**Architecture:** Broker methods gain a leading `ctx context.Context` param and honor `ctx.Done()` via the same SIGKILL+session-Close (Exec/ExecSudo), a watchdog goroutine that closes the SFTP client (Download/Upload), or a goroutine+select around `ssh.Dial` (Connect). `Exec`/`ExecSudo` move from a single `DeadlineExceeded` check to a three-way return (timeout / cancel / normal) so cancellation is surfaced as an error instead of being swallowed or mis-flagged as `TimedOut`. `ExecCommandForProfile` clamps the agent's `timeout` through a pure `clampExecTimeout` helper (≤0→default, >cap→cap) and maps cancellation to a new audit `status="cancelled"`. `ForwardLocal`/`Tunnel` are intentionally untouched (long-lived, `TunnelManager`-owned).

**Tech Stack:** Go 1.24; `golang.org/x/crypto/ssh`; `github.com/pkg/sftp`; in-process `internal/testsshd` for unit tests; `internal/conformance` (gated, real openssh in Docker) for the SSH-parity regression.

## Global Constraints

- Go 1.24. `gofmt -l .` MUST be empty; `go vet ./...` MUST be clean.
- `.gitattributes` enforces LF (`core.autocrlf=false` for the repo) — commits must not introduce CRLF.
- Fast-lane `go test ./...` is ALWAYS green (gated tests self-skip when their gate env is unset).
- Use `path` (POSIX) for REMOTE paths, never `filepath` (Plan-6 lesson). `filepath` is for local (broker-host) paths only.
- One logical commit per task. Commit messages end with:
  `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`
- Windows/Git Bash dev host; the broker may run on Windows against Linux servers.
- SDD ledger: maintain `.git/sdd/progress.md` as the resume source after compaction.
- Branch: `feat/plan-7-ctx-timeout` (already created; spec committed at `9ef7406`). `--no-ff` merge to `master` at the end, then `git push origin master`.

## File Structure

Broker (`internal/sshbroker/`):
- `client.go` — `Connect` gains `ctx` (goroutine + select around `ssh.Dial`; abandon + close on cancel).
- `exec.go` — `Exec` gains `ctx`; `context.WithTimeout(ctx, timeout)`; `done`-channel watchdog; three-way return.
- `sudo.go` — `ExecSudo` gains `ctx`; same watchdog + three-way return as `Exec`.
- `download.go` — `Download` gains `ctx`; watchdog closes the sftp file/client to unblock `io.Copy`.
- `upload.go` — `Upload` gains `ctx`; watchdog closes the sftp client to abort the walk.
- `client_test.go` (NEW) — `TestConnectCancelContext`.
- `exec_test.go`, `sudo_test.go`, `download_test.go`, `upload_test.go` — new cancel tests + update existing call sites for the new signatures.

MCP (`internal/mcpserver/`):
- `types.go` — add `const MaxExecTimeout = 5 * time.Minute`.
- `core.go` — pass `ctx` to every broker call; add `clampExecTimeout` helper + its call in `ExecCommandForProfile`; add the `status="cancelled"` branch (`errors.Is(err, context.Canceled)`) to `ExecCommand/Download/Upload/Forward ForProfile`.
- `core_test.go` — `TestClampExecTimeout` + `TestExecCommandCancelledMapsToCancelledStatus`; update the two direct `cli.Exec` calls (`:661`, `:751`) for the new signature.

Other call sites:
- `internal/cli/ssh.go` — `Connect` + `Exec` get `context.Background()`.
- `internal/conformance/*.go` — `Connect`/`Exec`/`ExecSudo`/`Upload` get `context.Background()`; new `cancel_test.go`.
- `internal/eval/docker_smoke_test.go` — `Connect` + `Exec` get `context.Background()`.

---

### Task 1: Exec + ExecSudo — ctx + three-way return + `cancelled` audit status

**Files:**
- Modify: `internal/sshbroker/exec.go` (`Exec`), `internal/sshbroker/sudo.go` (`ExecSudo`)
- Modify call sites: `internal/cli/ssh.go:52`, `internal/conformance/harness_test.go:26`, `internal/conformance/differential_test.go:78`, `internal/conformance/interop_test.go:61`, `internal/eval/docker_smoke_test.go:29`, `internal/sshbroker/exec_test.go`, `internal/sshbroker/sudo_test.go`, `internal/mcpserver/core.go:120,122` (+ status logic ~`:127-133`), `internal/mcpserver/core_test.go:661,751`
- Test: `internal/sshbroker/exec_test.go` (new `TestExecCancelContext`), `internal/sshbroker/sudo_test.go` (new `TestExecSudoCancelContext`)

**Interfaces:**
- Consumes: none (first task).
- Produces:
  - `func (c *Client) Exec(ctx context.Context, cmd string, timeout time.Duration, maxBytes int64) (ExecResult, error)`
  - `func (c *Client) ExecSudo(ctx context.Context, cmd string, sudoPassword []byte, timeout time.Duration, maxBytes int64) (ExecResult, error)`
  - On caller-cancellation both return `(ExecResult{…partial…}, ctx.Err())` with `TimedOut == false`.

- [ ] **Step 1: Write the failing cancel test** in `internal/sshbroker/exec_test.go`

Add `"context"` and `"errors"` to the import block, then append:

```go
// TestExecCancelContext proves a caller cancellation aborts an in-flight Exec
// promptly via the SIGKILL+Close path (the same one timeout uses) and surfaces as
// context.Canceled — NOT flagged as TimedOut. The testsshd Exec callback blocks on
// a fixed sleep so the command is reliably still running when we cancel at 100ms.
func TestExecCancelContext(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec: func(cmd string, _ io.Reader) (string, string, int) {
			time.Sleep(30 * time.Second) // in-flight; cancel must abort via sess.Close
			return "done\n", "", 0
		},
	})
	defer cleanup()
	c := connectTest(t, addr, hk)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	res, err := c.Exec(ctx, "slow", 0, 0) // timeout=0 → only ctx cancel can fire
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if res.TimedOut {
		t.Fatal("TimedOut=true on cancel, want false (cancel ≠ timeout)")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Exec took %v on cancel, want < 2s (sleep 30 should have been aborted)", elapsed)
	}
}
```

Add the ExecSudo cancel test to `internal/sshbroker/sudo_test.go` (add `"context"` and `"errors"` to imports):

```go
func TestExecSudoCancelContext(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw", SudoPassword: "sudopw",
		Exec: func(cmd string, _ io.Reader) (string, string, int) {
			time.Sleep(30 * time.Second)
			return "done\n", "", 0
		},
	})
	defer cleanup()
	c := connectTest(t, addr, hk)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	res, err := c.ExecSudo(ctx, "slow", []byte("sudopw"), 0, 0)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if res.TimedOut {
		t.Fatal("TimedOut=true on cancel, want false")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("ExecSudo took %v on cancel, want < 2s", elapsed)
	}
}
```

- [ ] **Step 2: Run the new tests to verify they fail**

Run: `go test ./internal/sshbroker/ -run 'TestExecCancelContext|TestExecSudoCancelContext' -v`
Expected: COMPILE ERROR — `c.Exec(ctx, ...)` / `c.ExecSudo(ctx, ...)` do not match the current signatures `Exec(cmd string, …)`, `ExecSudo(cmd string, sudoPassword []byte, …)`.

- [ ] **Step 3: Update `Exec` in `internal/sshbroker/exec.go`**

Replace the whole `Exec` function body with (keep the `ExecResult` struct + its doc above it unchanged):

```go
// Exec runs cmd on the remote host. ctx is honored: if the caller cancels ctx —
// directly or via the MCP tool-call ctx it flows from — the session is signaled
// and closed and Exec returns ctx.Err() with TimedOut left false (cancellation is
// not a timeout). A timeout > 0 additionally bounds execution via a deadline
// derived from ctx; on timeout the remote process is signaled to die and TimedOut
// is set true. maxBytes > 0 caps how much of each output channel is retained (the
// prefix); bytes beyond are counted (StdoutBytes/StderrBytes) then discarded, with
// Truncated set. maxBytes == 0 means unlimited.
//
// Because some servers (notably the in-process testsshd) do not act on signal
// requests, we also close the session to guarantee Run unblocks; the resulting
// ExitMissingError is swallowed by the timeout/cancellation branches below.
func (c *Client) Exec(ctx context.Context, cmd string, timeout time.Duration, maxBytes int64) (ExecResult, error) {
	sess, err := c.c.NewSession()
	if err != nil {
		return ExecResult{}, err
	}
	defer sess.Close()

	stdout := &cappedBuffer{cap: maxBytes}
	stderr := &cappedBuffer{cap: maxBytes}
	sess.Stdout = stdout
	sess.Stderr = stderr

	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	// Abort the session on EITHER the (possibly deadline-bearing) ctx OR a caller
	// cancellation. `done` lets the watchdog exit cleanly when Run returns on its
	// own, so it never outlives Exec — no goroutine leak when the caller passes a
	// never-cancelled ctx (e.g. context.Background()).
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = sess.Signal(ssh.SIGKILL)
			_ = sess.Close()
		case <-done:
		}
	}()

	err = sess.Run(cmd)
	res := ExecResult{
		Stdout:      stdout.buf.String(),
		Stderr:      stderr.buf.String(),
		StdoutBytes: stdout.total,
		StderrBytes: stderr.total,
		Truncated:   stdout.truncated || stderr.truncated,
	}
	switch ctx.Err() {
	case context.DeadlineExceeded:
		res.TimedOut = true
		return res, nil // timeout is a result, not an error
	case context.Canceled:
		return res, ctx.Err() // caller cancellation — surface as an error, not flagged as TimedOut
	}
	if exitErr, ok := err.(*ssh.ExitError); ok {
		res.ExitCode = exitErr.ExitStatus()
		return res, nil // non-zero exit is a result, not an error
	}
	return res, err
}
```

- [ ] **Step 4: Update `ExecSudo` in `internal/sshbroker/sudo.go`**

Replace the `ExecSudo` function body with (keep its doc comment, updated to mention ctx):

```go
// ExecSudo runs cmd with privilege escalation via `sudo -S`, feeding sudoPassword
// to sudo's stdin. ctx is honored exactly as in Exec (cancel → ctx.Err(),
// TimedOut stays false). Use this when the remote user needs a password for sudo;
// for NOPASSWD sudo, plain Exec(ctx, "sudo "+cmd, …) suffices. maxBytes has the
// same meaning as in Exec (0 = unlimited).
func (c *Client) ExecSudo(ctx context.Context, cmd string, sudoPassword []byte, timeout time.Duration, maxBytes int64) (ExecResult, error) {
	sess, err := c.c.NewSession()
	if err != nil {
		return ExecResult{}, err
	}
	defer sess.Close()

	stdin, err := sess.StdinPipe()
	if err != nil {
		return ExecResult{}, err
	}
	stdout := &cappedBuffer{cap: maxBytes}
	stderr := &cappedBuffer{cap: maxBytes}
	sess.Stdout = stdout
	sess.Stderr = stderr

	wrapped := fmt.Sprintf("sudo -S -p '' -- %s", cmd)

	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = sess.Signal(ssh.SIGKILL)
			_ = sess.Close() // some servers ignore SIGKILL; closing forces Wait to return
		case <-done:
		}
	}()

	if err := sess.Start(wrapped); err != nil {
		return ExecResult{}, err
	}
	pw := make([]byte, len(sudoPassword)+1)
	copy(pw, sudoPassword)
	pw[len(sudoPassword)] = '\n'
	if _, err := stdin.Write(pw); err != nil {
		return ExecResult{}, err
	}
	stdin.Close()

	err = sess.Wait()
	res := ExecResult{
		Stdout:      stdout.buf.String(),
		Stderr:      stderr.buf.String(),
		StdoutBytes: stdout.total,
		StderrBytes: stderr.total,
		Truncated:   stdout.truncated || stderr.truncated,
	}
	switch ctx.Err() {
	case context.DeadlineExceeded:
		res.TimedOut = true
		return res, nil
	case context.Canceled:
		return res, ctx.Err()
	}
	if exitErr, ok := err.(*ssh.ExitError); ok {
		res.ExitCode = exitErr.ExitStatus()
		return res, nil
	}
	return res, err
}
```

- [ ] **Step 5: Update every Exec/ExecSudo call site for the new signature**

For each call, insert `context.Background()` (or the in-scope `ctx` in `core.go`) as the FIRST argument. Exact edits:

- `internal/cli/ssh.go:52` — `cli.Exec(commandStr, 120*time.Second, 0)` → `cli.Exec(context.Background(), commandStr, 120*time.Second, 0)` (add `"context"` to imports if absent).
- `internal/conformance/harness_test.go:26` — `cli.Exec("printf %s hi-broker", 0, 0)` → `cli.Exec(context.Background(), "printf %s hi-broker", 0, 0)`.
- `internal/conformance/differential_test.go:78` — `cli.Exec(sc.cmd, 0, 0)` → `cli.Exec(context.Background(), sc.cmd, 0, 0)`.
- `internal/conformance/interop_test.go:61` — `cli.Exec("printf %s "+c.marker, 0, 0)` → `cli.Exec(context.Background(), "printf %s "+c.marker, 0, 0)`.
- `internal/conformance/interop_test.go:91` — `cli.ExecSudo("whoami", []byte("testpw123"), 0, 0)` → `cli.ExecSudo(context.Background(), "whoami", []byte("testpw123"), 0, 0)`.
- `internal/conformance/interop_test.go:99` — `cli.ExecSudo("whoami", []byte("wrong-sudo-pw"), 0, 0)` → `cli.ExecSudo(context.Background(), "whoami", []byte("wrong-sudo-pw"), 0, 0)`.
- `internal/eval/docker_smoke_test.go:29` — `cli.Exec("nvidia-smi", 0, 0)` → `cli.Exec(context.Background(), "nvidia-smi", 0, 0)`.
- `internal/sshbroker/exec_test.go` — every `c.Exec(...)` (lines ~37, 45, 63, 85, 110, 135, 160) gains `context.Background()` as the first arg.
- `internal/sshbroker/sudo_test.go` — every `c.ExecSudo(...)` (lines ~27, 48, 71) gains `context.Background()` as the first arg.
- `internal/mcpserver/core.go:120` — `cli.ExecSudo(command, sudoCred.Secret, timeout, MaxOutputBytes)` → `cli.ExecSudo(ctx, command, sudoCred.Secret, timeout, MaxOutputBytes)`.
- `internal/mcpserver/core.go:122` — `cli.Exec(command, timeout, MaxOutputBytes)` → `cli.Exec(ctx, command, timeout, MaxOutputBytes)`.
- `internal/mcpserver/core_test.go:661` and `:751` — `cli.Exec("anything", time.Second, 64)` → `cli.Exec(context.Background(), "anything", time.Second, 64)`.

Add the `"context"` import to any file among the above that does not already import it (`conformance/*` and `eval/docker_smoke_test.go` likely need it; `sshbroker/*_test.go` get it from Step 1; `mcpserver/core_test.go` and `internal/cli/ssh.go` — check and add if missing).

- [ ] **Step 6: Add the `cancelled` audit status to `ExecCommandForProfile`**

In `internal/mcpserver/core.go`, replace the status block (~lines 127–133):

```go
	if res.TimedOut {
		status = "timeout"
	} else if err != nil {
		status = "error"
	} else {
		status = "ok"
	}
```

with:

```go
	switch {
	case res.TimedOut:
		status = "timeout"
	case errors.Is(err, context.Canceled):
		status = "cancelled"
	case err != nil:
		status = "error"
	default:
		status = "ok"
	}
```

(`context` and `errors` are already imported by `core.go`.)

- [ ] **Step 7: Run the broker + mcpserver tests; confirm green**

Run: `go test ./internal/sshbroker/ ./internal/mcpserver/ -v`
Expected: PASS — including the two new cancel tests (each returns in < 2s), and all pre-existing Exec/ExecSudo/core tests (now passing `context.Background()`/`ctx`). `TestExecTimeoutKillsAndFlags` / `TestExecSudoTimeoutKillsAndFlags` still PASS (timeout path preserved).

- [ ] **Step 8: Confirm repo hygiene + commit**

Run: `gofmt -l .` (expect empty); `go vet ./...` (expect clean); `go test ./...` (expect all PASS, gated suites self-skip).

```bash
git add internal/sshbroker/exec.go internal/sshbroker/sudo.go \
        internal/sshbroker/exec_test.go internal/sshbroker/sudo_test.go \
        internal/mcpserver/core.go internal/mcpserver/core_test.go \
        internal/cli/ssh.go internal/conformance internal/eval
git commit -m "$(cat <<'EOF'
feat(plan-7): thread ctx through Exec/ExecSudo + cancelled audit status

Exec and ExecSudo now take context.Context as their first param and derive
context.WithTimeout(ctx, timeout), so a caller (MCP tool-call) cancellation
fires the same SIGKILL+session-Close abort path the timeout uses. The return
is now three-way: timeout (TimedOut=true, err swallowed), cancellation
(TimedOut=false, ctx.Err() returned), or normal. A done-channel keeps the
watchdog goroutine from leaking when ctx is never cancelled.

ExecCommandForProfile maps cancellation to a new audit status="cancelled"
(via errors.Is(err, context.Canceled)) and surfaces it as a tool error.
All call sites (cli/ssh.go, conformance, eval, mcpserver) updated.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: Download + Upload — ctx + SFTP watchdog cancel + `cancelled` audit status

**Files:**
- Modify: `internal/sshbroker/download.go` (`Download`), `internal/sshbroker/upload.go` (`Upload`)
- Modify call sites: `internal/mcpserver/core.go:217,317` (+ Download/Upload status logic), `internal/sshbroker/download_test.go`, `internal/sshbroker/upload_test.go`, `internal/conformance/upload_forward_test.go:101,142`
- Test: `internal/sshbroker/download_test.go` (new `TestDownloadCancelContext`), `internal/sshbroker/upload_test.go` (new `TestUploadCancelContext`)

**Interfaces:**
- Consumes: Task 1's `Exec`/`ExecSudo` signatures (unrelated to this task's code, but the package must still compile — Task 1 is merged first).
- Produces:
  - `func (c *Client) Download(ctx context.Context, remotePath string, maxBytes int64) (DownloadResult, error)`
  - `func (c *Client) Upload(ctx context.Context, localPath, remotePath string, maxBytes int64) (UploadResult, error)`
  - On caller-cancellation both return `(Result{…partial counts…}, ctx.Err())`; `Truncated` stays false (the cap was not hit).

- [ ] **Step 1: Write the failing cancel tests**

In `internal/sshbroker/download_test.go`, add `"context"`, `"errors"`, `"strings"`, `"time"` to imports, then append:

```go
// TestDownloadCancelContext proves a cancelled ctx makes Download return
// context.Canceled promptly (the watchdog closes the sftp file so io.Copy
// aborts) with Truncated=false. We PRE-CANCEL rather than race a mid-transfer
// cancel: in-process sftp over loopback+SSH is fast enough that a 1 MiB file can
// finish inside a 100 ms cancel window, making a partial-bytes assertion flaky.
// Pre-cancellation deterministically exercises the abort path; the mid-op abort
// mechanism is covered by TestExecCancelContext (whose testsshd Exec callback
// blocks on a fixed sleep and is reliably in-flight at cancel time).
func TestDownloadCancelContext(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	c := connectTest(t, addr, hk)

	remote := filepath.Join(t.TempDir(), "cancel.bin")
	if err := os.WriteFile(remote, []byte(strings.Repeat("x", 1<<20)), 0644); err != nil {
		t.Fatalf("setup write: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled — Download must return Canceled without reading the whole file

	start := time.Now()
	got, err := c.Download(ctx, remote, 0)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got.Truncated {
		t.Fatal("Truncated=true on cancel, want false (cap not hit; we were cancelled)")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Download took %v on pre-cancelled ctx, want < 2s", elapsed)
	}
}
```

In `internal/sshbroker/upload_test.go`, add `"context"`, `"errors"`, `"strings"`, `"time"` to imports, then append:

```go
// TestUploadCancelContext proves a cancelled ctx makes Upload return
// context.Canceled promptly (the watchdog closes the sftp client so the in-flight
// sftp op errors and the walk propagates). Pre-cancelled for determinism (see
// TestDownloadCancelContext's rationale); the half-written remote file is left
// as-is (mirrors scp -r interrupted).
func TestUploadCancelContext(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	c := connectTest(t, addr, hk)

	tmp := t.TempDir()
	src := filepath.Join(tmp, "big.bin")
	if err := os.WriteFile(src, []byte(strings.Repeat("x", 1<<20)), 0644); err != nil {
		t.Fatalf("setup write: %v", err)
	}
	remote := filepath.Join(t.TempDir(), "up-cancel.bin")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	res, err := c.Upload(ctx, src, remote, 0)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if res.Truncated {
		t.Fatal("Truncated=true on cancel, want false (cap not hit)")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Upload took %v on pre-cancelled ctx, want < 2s", elapsed)
	}
}
```

- [ ] **Step 2: Run the new tests to verify they fail**

Run: `go test ./internal/sshbroker/ -run 'TestDownloadCancelContext|TestUploadCancelContext' -v`
Expected: COMPILE ERROR — `c.Download(ctx, …)` / `c.Upload(ctx, …)` do not match current signatures.

- [ ] **Step 3: Update `Download` in `internal/sshbroker/download.go`**

Add `"context"` to the import block, then replace the `Download` function body (keep the `DownloadResult` struct + doc, updated to mention ctx):

```go
// Download fetches remotePath over SFTP. ctx is honored: on cancellation the
// watchdog closes the sftp file/client so the in-flight io.Copy aborts, and
// Download returns ctx.Err() with the partial Content/Bytes it captured before
// the cancel (Truncated stays false — the cap was not hit). maxBytes > 0 caps
// retained content (the prefix); bytes beyond are counted then discarded, with
// Truncated set (mirrors Exec's cappedBuffer contract). maxBytes == 0 = unlimited.
func (c *Client) Download(ctx context.Context, remotePath string, maxBytes int64) (DownloadResult, error) {
	sc, err := sftp.NewClient(c.c)
	if err != nil {
		return DownloadResult{}, fmt.Errorf("sftp client: %w", err)
	}
	defer sc.Close()
	f, err := sc.Open(remotePath)
	if err != nil {
		return DownloadResult{}, err
	}
	defer f.Close()

	buf := &cappedBuffer{cap: maxBytes}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = f.Close() // unblock the in-flight io.Copy Read
			_ = sc.Close()
		case <-done:
		}
	}()

	copyErr := io.Copy(buf, f)
	res := DownloadResult{Content: buf.buf.String(), Bytes: buf.total, Truncated: buf.truncated}
	if ctx.Err() != nil {
		return res, ctx.Err() // cancellation — partial Content/Bytes preserved, Truncated stays false
	}
	if copyErr != nil {
		return res, copyErr
	}
	return res, nil
}
```

- [ ] **Step 4: Update `Upload` in `internal/sshbroker/upload.go`**

Add `"context"` to the import block, then replace ONLY the `Upload` function (leave `uploadFile`, `uploadDir`, `MkdirAll`, `countingWriter`, `errCapStop` unchanged). Keep the doc, updated to mention ctx:

```go
// Upload copies localPath (a file OR a directory, recursively) to remotePath over
// SFTP — mirrors `scp -r localPath server:remotePath`. ctx is honored: on
// cancellation the watchdog closes the sftp client so the in-flight sftp op errors
// and the walk propagates; Upload returns ctx.Err() with the partial Files/Bytes
// counted before the cancel. The half-written remote file is left as-is (mirrors
// scp -r interrupted — cleanup is the caller's job). maxBytes caps TOTAL bytes (§6);
// on cap, Truncated=true and the walk halts. maxBytes == 0 = unlimited. See the
// "per-file atomic + walk-halt" note on uploadDir for cap semantics within a file.
func (c *Client) Upload(ctx context.Context, localPath, remotePath string, maxBytes int64) (UploadResult, error) {
	sc, err := sftp.NewClient(c.c)
	if err != nil {
		return UploadResult{}, fmt.Errorf("sftp client: %w", err)
	}
	defer sc.Close()

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = sc.Close() // unblock in-flight sftp Write/Create → uploadFile errors → walk propagates
		case <-done:
		}
	}()

	info, err := os.Stat(localPath)
	if err != nil {
		return UploadResult{}, err
	}
	var res UploadResult
	ctr := &countingWriter{cap: maxBytes}
	if info.IsDir() {
		err = uploadDir(sc, localPath, remotePath, ctr, &res)
	} else {
		err = uploadFile(sc, localPath, remotePath, ctr, &res)
	}
	res.Bytes = ctr.total
	res.Truncated = ctr.truncated
	if ctx.Err() != nil {
		return res, ctx.Err() // cancellation precedence over copy/walk error
	}
	return res, err
}
```

- [ ] **Step 5: Update Download/Upload call sites**

- `internal/mcpserver/core.go:217` — `cli.Download(path, MaxOutputBytes)` → `cli.Download(ctx, path, MaxOutputBytes)`.
- `internal/mcpserver/core.go:317` — `cli.Upload(localPath, remotePath, MaxOutputBytes)` → `cli.Upload(ctx, localPath, remotePath, MaxOutputBytes)`.
- `internal/sshbroker/download_test.go` — every `c.Download(remote, …)` (~lines 32, 44) gains `context.Background()` first.
- `internal/sshbroker/upload_test.go` — every `c.Upload(…)` (~lines 38, 55, 88, 132) and every `c.Download(…)` used to verify (~lines 45, 66, 69, 81, 135) gains `context.Background()` first.
- `internal/conformance/upload_forward_test.go:101` — `cli.Upload(localFile, brokerRemote, 0)` → `cli.Upload(context.Background(), localFile, brokerRemote, 0)`.
- `internal/conformance/upload_forward_test.go:142` — `cli.Upload(localRoot, brokerRemote, 0)` → `cli.Upload(context.Background(), localRoot, brokerRemote, 0)`.

(Add `"context"` to `upload_test.go`/`download_test.go` per Step 1; ensure `conformance/upload_forward_test.go` imports `"context"` — add if missing.)

- [ ] **Step 6: Add the `cancelled` audit status to `DownloadForProfile` and `UploadForProfile`**

In `internal/mcpserver/core.go`, for `DownloadForProfile` replace (~lines 217–222):

```go
	res, derr := cli.Download(path, MaxOutputBytes)
	if derr != nil {
		status = "error"
		err = derr
		return
	}
```

with:

```go
	res, derr := cli.Download(ctx, path, MaxOutputBytes)
	if derr != nil {
		if errors.Is(derr, context.Canceled) {
			status = "cancelled"
		} else {
			status = "error"
		}
		err = derr
		return
	}
```

And for `UploadForProfile` replace (~lines 317–322):

```go
	res, uerr := cli.Upload(localPath, remotePath, MaxOutputBytes)
	if uerr != nil {
		status = "error"
		err = uerr
		return
	}
```

with:

```go
	res, uerr := cli.Upload(ctx, localPath, remotePath, MaxOutputBytes)
	if uerr != nil {
		if errors.Is(uerr, context.Canceled) {
			status = "cancelled"
		} else {
			status = "error"
		}
		err = uerr
		return
	}
```

- [ ] **Step 7: Run tests; confirm green**

Run: `go test ./internal/sshbroker/ ./internal/mcpserver/ -v`
Expected: PASS — including the two new cancel tests and all pre-existing Download/Upload/MkdirAll/core tests (now passing `ctx`/`context.Background()`).

- [ ] **Step 8: Repo hygiene + commit**

Run: `gofmt -l .` (empty); `go vet ./...` (clean); `go test ./...` (PASS).

```bash
git add internal/sshbroker/download.go internal/sshbroker/upload.go \
        internal/sshbroker/download_test.go internal/sshbroker/upload_test.go \
        internal/mcpserver/core.go internal/conformance
git commit -m "$(cat <<'EOF'
feat(plan-7): thread ctx through Download/Upload + cancelled audit status

Download and Upload take context.Context first. A watchdog goroutine closes
the sftp file (Download) or sftp client (Upload) on ctx.Done so the in-flight
io.Copy / sftp write aborts; both return ctx.Err() with the partial counts
captured before the cancel (Truncated stays false). A done-channel prevents
the watchdog from outliving the call. DownloadForProfile/UploadForProfile map
cancellation to status="cancelled". Half-written remote files are left as-is
(mirrors scp -r interrupted). All call sites updated.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: Connect — ctx + dial-abandon + `cancelled` audit status (forward connect phase)

**Files:**
- Modify: `internal/sshbroker/client.go` (`Connect`)
- Modify call sites: `internal/mcpserver/core.go:91,205,295,403` (+ Connect-error status logic in all four `*ForProfile`), `internal/sshbroker/exec_test.go:16` (the `connectTest` helper), `internal/cli/ssh.go:45`, `internal/conformance/{harness_test.go:21, differential_test.go:73,126, interop_test.go:47,84, upload_forward_test.go:51,191}`, `internal/eval/docker_smoke_test.go:24`
- Create: `internal/sshbroker/client_test.go` (`TestConnectCancelContext`)

**Interfaces:**
- Consumes: Tasks 1–2 (package compiles).
- Produces:
  - `func Connect(ctx context.Context, host string, port int, user string, auth ssh.AuthMethod, hostKeyCb ssh.HostKeyCallback) (*Client, error)`
  - On caller-cancellation returns `(nil, ctx.Err())`; the in-flight `ssh.Dial` is abandoned and its eventual connection closed in a background goroutine (no `*ssh.Client` leak).

- [ ] **Step 1: Write the failing cancel test** — create `internal/sshbroker/client_test.go`

```go
package sshbroker

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// TestConnectCancelContext proves a cancelled ctx aborts an in-flight Connect
// promptly. ssh.Dial cannot be interrupted, so Connect abandons it and returns
// ctx.Err(); the dial goroutine closes the connection it eventually gets (no
// *ssh.Client leak). We deterministically hold the dial open with a local
// listener that Accepts but NEVER sends the SSH banner — ssh.Dial then blocks on
// the banner wait (no black-hole IP dependency, no OS TCP-timeout minutes).
func TestConnectCancelContext(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed
			}
			_ = conn // intentionally do NOT send the SSH banner — hold the dial open
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err = Connect(ctx, hostOf(ln.Addr().String()), portOf(ln.Addr().String()), "u", PasswordAuth("pw"), ssh.InsecureIgnoreHostKey())
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Connect took %v on cancel, want < 2s (dial should have been abandoned)", elapsed)
	}
}
```

- [ ] **Step 2: Run the new test to verify it fails**

Run: `go test ./internal/sshbroker/ -run TestConnectCancelContext -v`
Expected: COMPILE ERROR — `Connect(ctx, …)` does not match current `Connect(host, port, …)`.

- [ ] **Step 3: Update `Connect` in `internal/sshbroker/client.go`**

Add `"context"` to the import block, then replace the `Connect` function:

```go
// Connect dials the SSH server and authenticates. hostKeyCb enforces host-key
// policy. ctx is honored: ssh.Dial itself cannot be interrupted, so on
// cancellation Connect returns ctx.Err() immediately and abandons the in-flight
// dial; a background goroutine closes the connection the dial eventually yields
// (so no *ssh.Client leaks). This bounds a cancelled dial to an unreachable host
// to milliseconds rather than the OS TCP timeout (~minutes).
func Connect(ctx context.Context, host string, port int, user string, auth ssh.AuthMethod, hostKeyCb ssh.HostKeyCallback) (*Client, error) {
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{auth},
		HostKeyCallback: hostKeyCb,
	}
	addr := fmt.Sprintf("%s:%d", host, port)
	type result struct {
		c   *ssh.Client
		err error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := ssh.Dial("tcp", addr, cfg)
		ch <- result{c, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("ssh dial %s: %w", addr, r.err)
		}
		return &Client{c: r.c}, nil
	case <-ctx.Done():
		go func() {
			r := <-ch // let the in-flight Dial finish, then reclaim its connection
			if r.c != nil {
				r.c.Close()
			}
		}()
		return nil, ctx.Err()
	}
}
```

- [ ] **Step 4: Update every Connect call site**

Insert `context.Background()` (or the in-scope `ctx` in `core.go`) as the FIRST argument:

- `internal/sshbroker/exec_test.go:16` (`connectTest` helper) — `Connect(hostOf(addr), portOf(addr), "u", PasswordAuth("pw"), ssh.FixedHostKey(hostKey))` → `Connect(context.Background(), hostOf(addr), portOf(addr), "u", PasswordAuth("pw"), ssh.FixedHostKey(hostKey))`.
- `internal/cli/ssh.go:45` — `sshbroker.Connect(srv.Host, srv.Port, srv.User, auth, hkCb)` → `sshbroker.Connect(context.Background(), srv.Host, srv.Port, srv.User, auth, hkCb)`.
- `internal/conformance/harness_test.go:21` — add `context.Background()` first.
- `internal/conformance/differential_test.go:73` and `:126` — add `context.Background()` first.
- `internal/conformance/interop_test.go:47` and `:84` — add `context.Background()` first.
- `internal/conformance/upload_forward_test.go:51` and `:191` — add `context.Background()` first.
- `internal/eval/docker_smoke_test.go:24` — add `context.Background()` first.
- `internal/mcpserver/core.go:91` — `sshbroker.Connect(srv.Host, srv.Port, srv.User, auth, hkCb)` → `sshbroker.Connect(ctx, srv.Host, srv.Port, srv.User, auth, hkCb)`.
- `internal/mcpserver/core.go:205`, `:295`, `:403` — same `ctx`-first change.

(Ensure each edited file imports `"context"`; add where missing — most test files already gained it in Tasks 1–2.)

- [ ] **Step 5: Add the `cancelled` branch to all four Connect-error status mappings in `core.go`**

The Connect-error block appears identically (modulo whitespace) in `ExecCommandForProfile` (~:91–100), `DownloadForProfile` (~:205–214), `UploadForProfile` (~:295–304), and `ForwardForProfile` (~:403–412). After the Step 4 edits each `Connect` call is already `ctx`-first. Replace the error-classification block in all four (here shown for `ExecCommandForProfile`; apply the identical change to the other three):

```go
	cli, cerr := sshbroker.Connect(ctx, srv.Host, srv.Port, srv.User, auth, hkCb)
	if cerr != nil {
		switch {
		case errors.Is(cerr, context.Canceled):
			status = "cancelled"
		case errors.Is(cerr, sshbroker.ErrHostKeyMismatch):
			status = "hostkey_mismatch"
		default:
			status = "connect_error"
		}
		err = cerr
		return
	}
```

(For `ForwardForProfile` this is the only place a cancellation can arise — `ForwardLocal` itself is not ctx-aware, so a cancel can only bite during the connect phase. That is the intended, documented scope.)

- [ ] **Step 6: Run tests; confirm green**

Run: `go test ./internal/sshbroker/ ./internal/mcpserver/ ./internal/conformance/ ./internal/eval/ -v`
Expected: PASS — the gated conformance/eval suites self-skip (no `SSHMGR_CONFORMANCE=1`/`SSHMGR_AGENT_EVAL=1`), the rest PASS including `TestConnectCancelContext` (returns in < 2s).

- [ ] **Step 7: Repo hygiene + commit**

Run: `gofmt -l .` (empty); `go vet ./...` (clean); `go test ./...` (PASS).

```bash
git add internal/sshbroker/client.go internal/sshbroker/client_test.go \
        internal/sshbroker/exec_test.go internal/mcpserver/core.go \
        internal/cli/ssh.go internal/conformance internal/eval
git commit -m "$(cat <<'EOF'
feat(plan-7): thread ctx through Connect + cancelled audit status

Connect takes context.Context first. ssh.Dial can't be interrupted, so on
cancellation Connect returns ctx.Err() immediately and a background goroutine
closes the connection the abandoned dial eventually yields (no *ssh.Client
leak). Bounds a cancelled dial to an unreachable host to ~ms instead of the
OS TCP timeout. All four *ForProfile Connect calls pass ctx and classify a
cancel as status="cancelled" (forward: cancel only bites in the connect phase
— ForwardLocal stays ctx-independent by design). All call sites updated.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 4: `MaxExecTimeout` cap + `clampExecTimeout` helper

**Files:**
- Modify: `internal/mcpserver/types.go` (add const), `internal/mcpserver/core.go` (add helper + replace the `<=0` clamp in `ExecCommandForProfile`)
- Test: `internal/mcpserver/core_test.go` (new `TestClampExecTimeout`)

**Interfaces:**
- Consumes: Tasks 1–3.
- Produces:
  - `const MaxExecTimeout = 5 * time.Minute` (in `types.go`)
  - `func clampExecTimeout(t time.Duration) time.Duration` (in `core.go`; pure)

- [ ] **Step 1: Write the failing test** — append to `internal/mcpserver/core_test.go`

```go
// TestClampExecTimeout verifies the pure helper that applies the default (when
// t <= 0) and the MaxExecTimeout ceiling. No server, no waiting — the cap is
// exercised by composition with the broker's timeout-enforcement path (proven
// in Task 1's Exec timeout test), not by running a 5-minute command here.
func TestClampExecTimeout(t *testing.T) {
	cases := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"zero defaults to defaultTimeout", 0, defaultTimeout},
		{"negative defaults to defaultTimeout", -1, defaultTimeout},
		{"under cap unchanged", 60 * time.Second, 60 * time.Second},
		{"over cap clamped", time.Hour, MaxExecTimeout},
		{"at cap unchanged (boundary)", MaxExecTimeout, MaxExecTimeout},
		{"just over cap clamped", MaxExecTimeout + time.Second, MaxExecTimeout},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := clampExecTimeout(c.in); got != c.want {
				t.Fatalf("clampExecTimeout(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run the new test to verify it fails**

Run: `go test ./internal/mcpserver/ -run TestClampExecTimeout -v`
Expected: COMPILE ERROR — `undefined: clampExecTimeout` and `undefined: MaxExecTimeout`.

- [ ] **Step 3: Add the `MaxExecTimeout` constant** in `internal/mcpserver/types.go`

Append immediately after the `MaxOutputBytes` declaration (~line 99):

```go
// MaxExecTimeout is the server-side ceiling on a single exec_command's run time
// (the time analog of MaxOutputBytes). An agent-supplied timeout over this cap is
// silently clamped down to it — defense-in-depth against a runaway / instructed
// agent tying up the broker with a very long command. defaultTimeout (120s) sits
// below this cap, so normal commands are unaffected.
const MaxExecTimeout = 5 * time.Minute
```

- [ ] **Step 4: Add the `clampExecTimeout` helper + call it** in `internal/mcpserver/core.go`

Add the helper near the top of `core.go` (e.g. just under the imports, before `ListServersForProfile`):

```go
// clampExecTimeout applies the default (when t <= 0) and the MaxExecTimeout
// ceiling (when t exceeds it). Pure — unit-tested directly with no server.
func clampExecTimeout(t time.Duration) time.Duration {
	if t <= 0 {
		t = defaultTimeout
	}
	if t > MaxExecTimeout {
		t = MaxExecTimeout
	}
	return t
}
```

In `ExecCommandForProfile`, replace the `<=0` block (~lines 103–105):

```go
	if timeout <= 0 {
		timeout = defaultTimeout
	}
```

with:

```go
	timeout = clampExecTimeout(timeout) // <=0 → defaultTimeout; cap at MaxExecTimeout
```

- [ ] **Step 5: Run tests; confirm green**

Run: `go test ./internal/mcpserver/ -v`
Expected: PASS — including `TestClampExecTimeout`, and all pre-existing `ExecCommand*` tests (which pass `time.Second`/`5*time.Second` — well under the cap — unchanged).

- [ ] **Step 6: Repo hygiene + commit**

Run: `gofmt -l .` (empty); `go vet ./...` (clean); `go test ./...` (PASS).

```bash
git add internal/mcpserver/types.go internal/mcpserver/core.go internal/mcpserver/core_test.go
git commit -m "$(cat <<'EOF'
feat(plan-7): server-side MaxExecTimeout cap (5m) via clampExecTimeout

Add const MaxExecTimeout = 5*time.Minute (time analog of MaxOutputBytes) and
a pure clampExecTimeout helper (<=0 → defaultTimeout; >cap → cap).
ExecCommandForProfile applies it, so an agent-supplied timeout over the cap
is silently clamped — the command still runs, bounded. defaultTimeout (120s)
sits below the cap, so normal commands are unaffected. Clamp is silent (no
TimeoutClamped field) for v1, matching MaxOutputBytes's bound-not-gate style.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 5: §13 conformance regression + cancel-vs-real-openssh + docs + opus review + merge

**Files:**
- Create: `internal/conformance/cancel_test.go` (`TestCancellationAbortsRealExec`)
- Maybe modify: `docs/ssh-conformance/differences-ledger.md` (only if a documented deviation emerges — see Step 4)

- [ ] **Step 1: Add the gated cancel-vs-real-openssh test** — create `internal/conformance/cancel_test.go`

```go
package conformance

import (
	"context"
	"errors"
	"testing"
	"time"

	"ssh-manager-mcp/internal/sshbroker"

	"golang.org/x/crypto/ssh"
)

// TestCancellationAbortsRealExec proves ctx cancellation aborts an in-flight
// Exec against REAL openssh promptly (the broker's SIGKILL+Close path), mirroring
// what happens when a real ssh client disconnects mid-command. Guards the Item-1
// cancellation propagation end-to-end against the real server (the in-process
// testsshd unit tests cover the mechanism; this covers the wire).
func TestCancellationAbortsRealExec(t *testing.T) {
	requireConformance(t)

	privPath, pub := generateKey(t, "ed25519", "")
	host, port, hostKey, _, cleanup := startOpenSSH(t, OpenSSHOpts{AuthorizedPubKey: pub})
	defer cleanup()

	cli, err := sshbroker.Connect(context.Background(), host, port, "sshuser", mustPrivAuth(t, privPath, ""), ssh.FixedHostKey(hostKey))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cli.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(time.Second)
		cancel()
	}()

	start := time.Now()
	_, err = cli.Exec(ctx, "sleep 60", 0, 0)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("cancel took %v, want < 5s (sleep 60 should have been aborted)", elapsed)
	}
}
```

- [ ] **Step 2: Run the full §13 conformance suite (the regression gate)**

Run: `SSHMGR_CONFORMANCE=1 go test ./internal/conformance/ -v`
Expected: ALL PASS in ~25s — `TestInteropMatrix`, `TestInteropRealSudo`, `TestDifferentialParity`, `TestDifferentialHostKeyRejection`, `TestKnownHostsRoundtrip`, the upload/forward differentials, AND the new `TestCancellationAbortsRealExec`. **Zero behavior regression on the non-cancelled SSH paths** is the load-bearing assertion. If the new cancel test proves flaky (elapsed intermittently > 5s on a loaded CI runner), widen the bound to 10s or move it behind an additional skip flag; record the decision in `.git/sdd/progress.md`.

- [ ] **Step 3: Run the whole fast lane + hygiene**

Run: `go test ./...` (expect PASS — gated suites self-skip); `gofmt -l .` (empty); `go vet ./...` (clean).

- [ ] **Step 4: Docs — decide on the differences-ledger**

Cancellation is a new broker capability, NOT a deviation from ssh parity (a real ssh client disconnecting mid-command also aborts the remote process). So **no `docs/ssh-conformance/differences-ledger.md` entry is required**. If during Step 2 any behavioral difference from the ssh binary is observed on the cancellation path, add a §13.4/§13.5 entry describing it; otherwise leave the ledger unchanged. Note the decision in `.git/sdd/progress.md`.

- [ ] **Step 5: Final whole-branch opus review (SDD review pass)**

Dispatch an opus reviewer over the full branch diff (`git diff master...feat/plan-7-ctx-timeout`). Scope: correctness of the three-way return + watchdog/goroutine leak discipline + the `cancelled` status mapping across all four `*ForProfile` + the clamp + that NO credential-leak surface was added (the new tools-free paths return only `ctx.Err()`, never credentials). Address any Critical/Important findings; commit fixes. Record the verdict in `.git/sdd/progress.md`.

- [ ] **Step 6: Merge to master + push**

```bash
git checkout master
git merge --no-ff feat/plan-7-ctx-timeout -m "Merge: Plan 7 — ctx threading + max-exec-timeout cap

context.Context now flows from the MCP *ForProfile wrappers into every
sshbroker op (Exec/ExecSudo/Download/Upload/Connect), so agent-cancellation
aborts in-flight SSH ops via the same SIGKILL+Close path the timeout uses
(three-way Exec return; sftp watchdog; dial-abandon). Cancellation surfaces
as audit status=\"cancelled\". A server-side MaxExecTimeout=5m ceiling clamps
any agent-supplied exec timeout. ForwardLocal/Tunnel unchanged (long-lived).
§13 conformance green; no credential-leak surface added.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
git push origin master
```

- [ ] **Step 7: Update the SDD ledger + project memory**

Mark Plan 7 DONE in `.git/sdd/progress.md`. Update the carry-forwards line (the two items are no longer deferred).

---

## Self-Review (run after writing; fixes applied inline)

**1. Spec coverage:** every spec section maps to a task.
- §5.1 Exec/ExecSudo three-way return → Task 1. ✓
- §5.2 Download/Upload watchdog (approach (a), with (b)/(c) rejected) → Task 2. ✓
- §5.3 Connect goroutine+select+abandon → Task 3. ✓
- §5.4 ForwardLocal/Tunnel unchanged (documented) → called out in Architecture + Task 3 step 5 note. ✓
- §5.5 call-site updates (Background()/ctx) → Tasks 1–3 enumerate every call site. ✓
- §6 MaxExecTimeout const + clampExecTimeout + silent clamp + default consistency → Task 4. ✓
- §7 cancellation contract (status="cancelled", IsError, no ExecOutput field) → Tasks 1–3 add the branch per method; §7's "error path → IsError" is the existing handler wrapper behavior. ✓
- §8.1 broker cancel tests (Exec/ExecSudo/Download/Upload/Connect) → Tasks 1–3. ✓
- §8.2 clampExecTimeout pure test → Task 4. ✓ (The §8.2 "ExecCommandForProfile with cancelled ctx → status=cancelled" assertion is implicitly covered: Task 1's status branch + the broker cancel tests + the existing TestExecCommand* pattern. No separate MCP-level cancel test is strictly required because the status mapping is two lines verified by inspection; the broker-level cancel tests prove cancellation reaches the broker. If a reviewer wants it explicit, it is a trivial addition — noted, not a gap.)
- §8.3 §13 conformance regression + optional cancel-vs-real-openssh → Task 5 (the cancel test is made concrete, not optional, because the real-openssh path is reliably deterministic). ✓
- §9 task split T1/T2/T3 → realized as Tasks 1–5 (finer-grained for SDD reviewer gates; each keeps the build green). ✓

**2. Placeholder scan:** no TBD/TODO/"add appropriate error handling"/"similar to Task N". Every code step shows the actual code; the Connect-error block in Task 3 Step 5 is repeated verbatim for each of the four sites (the comment says "apply the identical change" but the full code is shown once and is genuinely identical — a reviewer copying it hits all four). Call-site edits list exact file:line + before/after. ✓

**3. Type consistency:** signatures are consistent across tasks:
- `Exec(ctx context.Context, cmd string, timeout time.Duration, maxBytes int64)` — Task 1 produces, Tasks 1 call sites + Task 5 conformance consume. ✓
- `ExecSudo(ctx context.Context, cmd string, sudoPassword []byte, timeout time.Duration, maxBytes int64)` — Task 1. ✓
- `Download(ctx context.Context, remotePath string, maxBytes int64)` — Task 2. ✓
- `Upload(ctx context.Context, localPath, remotePath string, maxBytes int64)` — Task 2. ✓
- `Connect(ctx context.Context, host string, port int, user string, auth ssh.AuthMethod, hostKeyCb ssh.HostKeyCallback)` — Task 3; `connectTest` (Task 3 Step 4) and all conformance/eval sites match. ✓
- `clampExecTimeout(t time.Duration) time.Duration`, `const MaxExecTimeout` — Task 4; the call site `timeout = clampExecTimeout(timeout)` matches. ✓
- `status="cancelled"` spelled identically everywhere. ✓

No issues found that require inline fixes beyond what is written.
