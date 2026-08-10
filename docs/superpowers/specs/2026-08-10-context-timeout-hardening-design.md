# Plan 7 Design: `context.Context` threading + server-side max-exec-timeout cap

**Date:** 2026-08-10
**Status:** Design (brainstormed; pending implementation plan)
**Scope:** Two carry-forward sshbroker/MCP hardening items, shipped together as Plan 7.
**Nature:** NO-LLM, deterministic broker + MCP-server changes. Verified by unit tests + the §13 conformance suite (real openssh in Docker, gated, no \$). No §12 / no Fable-5 needed.

---

## 1. Background

`ssh-manager-mcp` is an L2 SSH-credential-broker MCP: the agent never touches credentials. Plans 1–6 are merged (vault → in-process broker → MCP + iron-rule profile enforcement → §13 SSH conformance → §12 agent-usability eval → SFTP download/upload → local port forward). The broker exposes six MCP tools: `list_servers`, `exec_command`, `download_file`, `upload_file`, `forward_port`, `close_port`.

Two real robustness gaps remain in the broker (identified in `docs/handoff-context-timeout-hardening.md`):

- **Cancellation does not propagate.** The MCP SDK handlers receive `ctx context.Context` (cancelled when the agent cancels the tool call), and the `*ForProfile` wrappers in `internal/mcpserver/core.go` already receive `ctx` — but they do not pass it to the broker. The broker methods take no `ctx`; `Exec`/`ExecSudo` derive `context.WithTimeout(context.Background(), timeout)` internally. So **timeout works, but agent-cancellation does not**: a long `exec_command` the agent abandons keeps running server-side until the timeout fires.
- **No server-side time ceiling.** `exec_command`'s `timeout_seconds` (agent-supplied) is used directly as the `timeout` (clamped to `defaultTimeout` only when `<= 0`). There is no server-side ceiling — an agent can pass `timeout_seconds: 3600` and the broker runs the command for an hour. Output is already capped (`MaxOutputBytes`); time is not.

## 2. Goals

1. **G1 — Cancellation propagates end-to-end.** Cancelling the MCP handler's `ctx` aborts the in-flight broker operation (Exec/ExecSudo/Download/Upload/Connect) promptly — same abort path the timeout already uses. The agent abandoning a long SSH op no longer ties up the broker + SSH session until the timeout.
2. **G2 — Server-side max-exec-timeout cap.** A `MaxExecTimeout` ceiling (time analog of `MaxOutputBytes`) bounds any single `exec_command` regardless of what the agent requests — defense-in-depth against a runaway / instructed agent.

## 3. Non-goals (deferred)

- `ctx` for `ForwardLocal`/`Tunnel` — long-lived, owned by `TunnelManager` (lifecycle is the manager's job, not a single tool-call's `ctx`).
- Signal-aware `ctx` for the owner CLI (`internal/cli/ssh.go`, Ctrl-C handling) — v1 passes `context.Background()`.
- `MaxExecTimeout` env-configurability — v1 is a `const`; env override can come later if a use case needs longer.
- T7 strong-model hallucination, eval-safety local-command residual, interactive shell / `-R`/`-D` forward, the accumulated polish Minors — all orthogonal, not this work.

## 4. Confirmed current state (verified against source)

Broker signatures take no `ctx`:
- `client.go:15` `Connect(host, port, user, auth, hostKeyCb) (*Client, error)`
- `exec.go:31` `Exec(cmd, timeout, maxBytes) (ExecResult, error)`
- `sudo.go:14` `ExecSudo(cmd, sudoPassword, timeout, maxBytes) (ExecResult, error)`
- `download.go:22` `Download(remotePath, maxBytes) (DownloadResult, error)`
- `upload.go:38` `Upload(localPath, remotePath, maxBytes) (UploadResult, error)`
- `tunnel.go:67` `ForwardLocal(localPort, remoteHost, remotePort) (*Tunnel, error)` — **unchanged by this plan**

`Exec`/`ExecSudo` derive `context.WithTimeout(context.Background(), timeout)` + a goroutine that `sess.Signal(SIGKILL) + sess.Close()` on `ctx.Done()`. `exec.go:63` sets `TimedOut` via `ctx.Err() == context.DeadlineExceeded`, and `exec.go:70-72` swallows the error on timeout (`return res, nil`).

`*ForProfile` functions already receive `ctx context.Context` but call the broker without it (`core.go:120` ExecSudo, `:122` Exec, `:217` Download, `:317` Upload, `:415` ForwardLocal, plus four `sshbroker.Connect` calls at `:91/:205/:295/:403`).

Constants in `internal/mcpserver/types.go`: `const defaultTimeout = 120 * time.Second` (`:91`), `const MaxOutputBytes int64 = 1 << 20` (`:99`). There is no `MaxExecTimeout` today.

Broker call sites (repo-wide): production in `internal/mcpserver/core.go` (above) and `internal/cli/ssh.go:45/52`; tests in `internal/sshbroker/*_test.go`, `internal/conformance/*`, `internal/eval/docker_smoke_test.go`. **There is no broker interface / fake** — `*ForProfile` calls the concrete `sshbroker.Connect(...)` + concrete methods; tests reach a real `*sshbroker.Client` (e.g. `core_test.go:661/751` grabs `mgr.tunnels[id].client`). Item 2's test strategy is designed around this (no fake is introduced).

## 5. Design — Item 1: `ctx` threading

### 5.1 Exec / ExecSudo

Add `ctx context.Context` as the first parameter; **keep `timeout`**. Internally derive from the caller's ctx:
```go
ctx, cancel = context.WithTimeout(ctx, timeout)   // was: context.WithTimeout(context.Background(), timeout)
```
The existing `<-ctx.Done() → SIGKILL + sess.Close()` goroutine is unchanged — it now fires on **either** timeout **or** caller-cancellation. The change is the **three-way return** (fixes the cancellation-vs-timeout conflation):

| `ctx.Err()`          | `TimedOut` | return error                                  | meaning            |
|----------------------|------------|-----------------------------------------------|--------------------|
| `DeadlineExceeded`   | `true`     | swallowed — `return res, nil` (unchanged)     | timeout            |
| `Canceled`           | `false`    | **returned** — `ctx.Err()` (or wrapped)        | caller cancellation |
| `nil`                | `false`    | per existing logic (`*ssh.ExitError`→nil; else err) | normal            |

The decision moves from `== DeadlineExceeded` to a three-branch switch so cancellation is neither swallowed nor mis-flagged as `TimedOut`.

### 5.2 Download / Upload (SFTP) — approach selection

`io.Copy` takes no `ctx`; SFTP reads/writes block. Three approaches were considered:

- **(a) Watchdog goroutine — CHOSEN.** A goroutine `select { <-ctx.Done(): close the sftp file/client; <-done: }`. Closing the in-copy file unblocks `io.Copy`'s Read immediately; `close(done)` after `io.Copy` returns, then the accumulated totals (`cappedBuffer`/`countingWriter`) are read safely.
- **(b) Goroutine + result channel** — run `io.Copy` in a goroutine, `select { done; ctx.Done() }`. On `ctx.Done` the main path returns but **the `io.Copy` goroutine leaks** until it finishes naturally (still holding the file handle). Strictly worse than (a).
- **(c) ctx-aware Reader/Writer wrapper** (check `ctx.Done()` inside `Read`/`Write`) — a slow-network SFTP `Read` blocks inside a single call; `ctx` is only honored *between* reads, a weaker guarantee than (a).

**(a) is chosen**: it is the only option that *actively unblocks* an in-flight `io.Copy`, and it does so cleanly. Semantics:
- **Download, on cancel:** if `ctx.Err() != nil`, return `(DownloadResult{Content: prefix-so-far, Bytes: bytes-seen-before-cancel, Truncated: false}, ctx.Err())` regardless of the copy error — the cap was not hit, so `Truncated` stays false.
- **Upload, on cancel:** the watchdog closes `sc` → the in-flight `sftp.Create`/`Write` errors → `uploadFile` returns → the walk propagates. When `ctx.Err() != nil`, return the cancellation error (taking precedence over `errCapStop`/copy errors); `Files`/`Bytes` carry the partial counts; **the half-written remote file is left as-is** (mirrors `scp -r` interrupted). A `sync.Once`/`done`-channel guards against close races with the deferred `sc.Close()`/`f.Close()`.

### 5.3 Connect

`ssh.Dial` blocks and cannot be interrupted mid-flight. Wrap it:
```go
func Connect(ctx context.Context, host string, port int, user string, auth ssh.AuthMethod, hostKeyCb ssh.HostKeyCallback) (*Client, error) {
    cfg := &ssh.ClientConfig{User: user, Auth: []ssh.AuthMethod{auth}, HostKeyCallback: hostKeyCb}
    addr := fmt.Sprintf("%s:%d", host, port)
    type result struct{ c *ssh.Client; err error }
    ch := make(chan result, 1)
    go func() { c, err := ssh.Dial("tcp", addr, cfg); ch <- result{c, err} }()
    select {
    case r := <-ch:
        if r.err != nil { return nil, fmt.Errorf("ssh dial %s: %w", addr, r.err) }
        return &Client{c: r.c}, nil
    case <-ctx.Done():
        go func() { r := <-ch; if r.c != nil { r.c.Close() } }() // let the in-flight Dial finish, then discard+close
        return nil, ctx.Err()
    }
}
```
Cancellation returns immediately; the in-flight Dial is abandoned and its eventual connection closed (no `ssh.Client` leak). Real win: a cancelled dial to an unreachable host returns in ~ms instead of waiting for the OS TCP timeout (~minutes).

### 5.4 ForwardLocal / Tunnel — unchanged

Long-lived, `TunnelManager`-owned (closed via `close_port` / idle-sweeper / MCP-shutdown). Not scoped to a single tool-call's `ctx`. Called out in the plan as an intentional non-change.

### 5.5 Call-site updates

`internal/cli/ssh.go` and all test call sites (`internal/sshbroker/*_test.go`, `internal/conformance/*`, `internal/eval/docker_smoke_test.go`) pass `context.Background()`. The `*ForProfile` functions pass the `ctx` already in scope. Mechanical, repo-wide signature update.

## 6. Design — Item 2: `MaxExecTimeout` cap

- Add `const MaxExecTimeout = 5 * time.Minute` to `internal/mcpserver/types.go` (mirrors the exported `const MaxOutputBytes`). `defaultTimeout (120s) < MaxExecTimeout (300s)`, so the default is never clamped, but the code is correct for any values.
- Pure helper:
  ```go
  func clampExecTimeout(t time.Duration) time.Duration {
      if t <= 0 { t = defaultTimeout }
      if t > MaxExecTimeout { t = MaxExecTimeout }
      return t
  }
  ```
- In `ExecCommandForProfile`, after the profile gate and before `cli.Exec`/`cli.ExecSudo`: `timeout = clampExecTimeout(timeout)`. The `<=0 → defaultTimeout` substitution and the cap are unified in one call. **Clamp is silent** — the command still runs, bounded; no `TimeoutClamped` field is added for v1 (YAGNI; the cap bounds each retry, so an agent re-requesting an over-cap timeout is bounded per-iteration).
- Composition with Item 1: the clamped `timeout` flows into `cli.Exec(ctx, clampedTimeout, …)` → broker derives `context.WithTimeout(ctx, clampedTimeout)` → the existing SIGKILL+Close path bounds execution. Item 1 + Item 2 compose cleanly.

## 7. Cross-cutting: the cancellation contract (audit + surfacing)

The broker returns `context.Canceled` (not a swallowed/flagged result). `ExecCommandForProfile` gains a branch ordered after `TimedOut` and before the generic error:
```go
switch {
case res.TimedOut:                          status = "timeout"
case errors.Is(err, context.Canceled):      status = "cancelled"   // NEW
case err != nil:                            status = "error"
default:                                    status = "ok"
}
```
Cancellation travels the **error path** → the handler surfaces it as `IsError: true` (consistent with how `connect_error` etc. are surfaced). **No `Cancelled` field is added to `ExecOutput`** — cancellation is an abort, not a partial-output result (unlike timeout, which carries useful truncated output). The audit row's `status="cancelled"` is the durable record. `DownloadForProfile`/`UploadForProfile`/`ForwardForProfile` map cancellation to `status="cancelled"` by the same `errors.Is(err, context.Canceled)` check.

## 8. Test coverage (measurable)

### 8.1 Broker unit tests (in-process `testsshd`)
- **Exec cancel:** start `sleep 30`, cancel `ctx` after ~100 ms → `Exec` returns **within ~1 s**, `errors.Is(err, context.Canceled)`, `TimedOut == false`. (Existing timeout test retained: `sleep 10` + 200 ms timeout → ~200 ms, `TimedOut == true`.)
- **ExecSudo cancel:** same shape through the sudo path.
- **Download cancel:** place a ~10 MiB file in `testsshd`, cancel mid-download → returns **within ~1 s**, Canceled, `0 < Bytes < 10 MiB`, `Truncated == false`.
- **Upload cancel:** multi-file directory, cancel after the first file starts → Canceled, partial `Files`/`Bytes`, `Truncated == false`.
- **Connect cancel (headline proof for §5.3):** start a local `net.Listen` that `Accept`s but **never sends the SSH banner** → `ssh.Dial` blocks on the banner wait; cancel `ctx` after ~100 ms → `Connect` returns **within ~1 s**, Canceled. Deterministic, local, no black-hole IP dependency.

### 8.2 MCP-layer unit tests (`internal/mcpserver/core_test.go`)
- `ExecCommandForProfile` with an **already-cancelled `ctx`** + `testsshd` `sleep` → broker aborts; `ExecCommandForProfile` returns an error satisfying `errors.Is(err, context.Canceled)`; audit row `status == "cancelled"`. The handler wrapper in `server.go` surfaces any non-nil error as `IsError: true` (unconditional — covered by the existing error-surfacing pattern, not re-asserted here).
- `clampExecTimeout` pure-function tests: `(0) → 120 s`, `(time.Hour) → 300 s`, `(60 s) → 60 s`, `(300 s) → 300 s` boundary. Zero wait.
- Cap enforcement is covered by composition: the broker-level timeout-enforcement test (small timeout + `sleep` proves the bound) + the clamp-helper test (input is bounded) together prove "agent's huge timeout → clamped → enforced." **No fake broker is introduced** (avoids refactoring every `*ForProfile` signature), and **no 5-minute wait** is needed.

### 8.3 §13 conformance regression
Run `SSHMGR_CONFORMANCE=1 go test ./internal/conformance/ -v` (~25 s, no LLM): confirm zero SSH-behavior regression on the non-cancelled paths. If feasible and non-flaky, add a cancel-vs-real-openssh case (long exec + cancel → prompt return, mirroring a real ssh client disconnect); otherwise record it as optional. If the cancellation behavior constitutes a documented deviation, update `docs/ssh-conformance/differences-ledger.md`.

## 9. Implementation task split (high level — the plan doc details)

- **T1 — broker `ctx` threading:** add `ctx` to `Exec`/`ExecSudo`/`Download`/`Upload`/`Connect` (three-way Exec return, watchdog for SFTP, goroutine+select for Connect); write the §8.1 cancel tests; update all broker call sites (`cli/ssh.go`, `conformance/*`, `eval/docker_smoke_test`, `sshbroker/*_test`) to pass `context.Background()`.
- **T2 — MCP layer:** `*ForProfile` pass `ctx` to the broker (core.go); add `MaxExecTimeout` const + `clampExecTimeout` + the clamp call; the `status="cancelled"` branch; the §8.2 unit tests.
- **T3 — regression + merge:** §13 conformance run (§8.3); docs (`differences-ledger.md` if applicable); final whole-branch opus review; `--no-ff` merge to `master`; `git push origin master`. Maintain `.git/sdd/progress.md` as the SDD ledger.

## 10. Pointers

- Spec (parent): `docs/superpowers/specs/2026-08-08-ssh-key-manager-mcp-design.md` (§6 = output truncation, the `MaxOutputBytes` analog; §12/§13 = the eval layers).
- Broker: `internal/sshbroker/` (`exec.go`, `sudo.go`, `download.go`, `upload.go`, `tunnel.go`, `client.go`, `output.go`).
- MCP: `internal/mcpserver/` (`server.go` [handlers + `BrokerTools`], `core.go` [`*ForProfile`], `tunnels.go` [`TunnelManager`], `types.go` [consts]).
- §6 cap (`MaxOutputBytes`) is the precedent for the `MaxExecTimeout` cap.
- §13 conformance: `internal/conformance/` (gated `SSHMGR_CONFORMANCE=1`).
- Plan-6 cross-platform-path lesson: use `path` (POSIX) for remote paths, never `filepath`, on remote-target construction.
- SDD ledger (resume source after compaction): `.git/sdd/progress.md`.
