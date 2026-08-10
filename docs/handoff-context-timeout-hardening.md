# Handoff: context.Context threading + server-side max-exec-timeout cap

**Purpose:** a self-contained briefing for a NEW session to pick up + drive the two carry-forward sshbroker/MCP hardening items (the user chose these as the next work). Read this, then use the **superpowers:writing-plans** skill to write the implementation plan (≈ "Plan 7"), then **superpowers:subagent-driven-development** to execute — the same flow as Plans 5c/5d/5e/6.

**These are NO-LLM changes** (broker + MCP server code). Test via unit tests + the §13 conformance suite (real openssh in Docker, gated, no $). No Fable-5 / no `ANTHROPIC_API_KEY` needed.

---

## Project context (so a fresh session is grounded)

`ssh-manager-mcp` (at `C:\WorkSpace\agent\ssh-manager-mcp`, on `master`, pushed to https://github.com/allanpk716/ssh-manager-mcp — private): an MCP server that brokers SSH for AI agents at security level **L2** — the agent never touches credentials. Single Go binary, in-process `golang.org/x/crypto/ssh`. Plans 1–6 are DONE + merged (vault → in-process broker → MCP + iron-rule profile enforcement → §13 SSH conformance → §12 agent-usability eval → SFTP download/upload → local port forward). The broker exposes 6 MCP tools: `list_servers`, `exec_command`, `download_file`, `upload_file`, `forward_port`, `close_port`. The iron rule (no credential leak) is proven against a top-tier model (claude-fable-5). Read the root `README.md` for the user-facing picture; `docs/superpowers/specs/2026-08-08-ssh-key-manager-mcp-design.md` for the design spec.

**Architecture (the layers this work touches):**
- `internal/sshbroker/` — the handcrafted SSH client. `Client` wraps `*ssh.Client`. `Connect`, `Exec`, `ExecSudo`, `Download`, `Upload`, `ForwardLocal`/`Tunnel`.
- `internal/mcpserver/` — the MCP server. `server.go` (`NewServer` registers the tools; the `mcp.AddTool` handlers receive `ctx context.Context` from the SDK). `core.go` (`*ForProfile` — profile-gated + audited wrappers that call the broker). `tunnels.go` (`TunnelManager` — the stateful forward lifecycle).

**Established workflow conventions (match these):**
- Plan docs: `docs/superpowers/plans/YYYY-MM-DD-<name>.md`. Per-task SDD ledger: `.git/sdd/progress.md` (controller-maintained; resume source after compaction).
- Branch per plan (`feat/plan-7-...`), one logical commit per task, `--no-ff` merge to `master` at the end, then `git push origin master`. Commit messages end with `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.
- `.gitattributes` enforces LF; `gofmt -l .` must be empty; `go vet ./...` clean. Go 1.24. Windows/Git Bash dev host (so mind cross-platform paths — the broker may run on Windows against Linux servers; use `path` not `filepath` for remote POSIX paths — a Plan-6 bug bit us on exactly this).
- Tests: fast-lane `go test ./...` always green (gated tests self-skip ungated). The §13 conformance suite is gated by `SSHMGR_CONFORMANCE=1` (real openssh in Docker; ~25s; no LLM) — run it for broker changes.

---

## Item 1: thread `context.Context` through sshbroker (MCP cancellation)

### The gap (confirmed)

The MCP SDK handlers receive `ctx context.Context` (cancelled when the agent cancels the tool call). The `*ForProfile` functions in `core.go` **already receive `ctx`** in their signatures — but they **do not pass it to the broker**:

```
core.go:120   res, err = cli.ExecSudo(command, sudoCred.Secret, timeout, MaxOutputBytes)
core.go:122   res, err = cli.Exec(command, timeout, MaxOutputBytes)
core.go:217   res, derr := cli.Download(path, MaxOutputBytes)
core.go:317   res, uerr := cli.Upload(localPath, remotePath, MaxOutputBytes)
core.go:415   tun, ferr2 := cli.ForwardLocal(localPort, remoteHost, remotePort)
```

And the broker methods **take no `ctx`**:
```
client.go:15  func Connect(host, port, user, auth, hostKeyCb) (*Client, error)
exec.go:31    func (c *Client) Exec(cmd string, timeout time.Duration, maxBytes int64) (ExecResult, error)
sudo.go:14    func (c *Client) ExecSudo(cmd string, sudoPassword []byte, timeout time.Duration, maxBytes int64) (ExecResult, error)
download.go:22 func (c *Client) Download(remotePath string, maxBytes int64) (DownloadResult, error)
upload.go:38  func (c *Client) Upload(localPath, remotePath string, maxBytes int64) (UploadResult, error)
tunnel.go:67  func (c *Client) ForwardLocal(localPort int, remoteHost string, remotePort int) (*Tunnel, error)
```

`Exec`/`ExecSudo` currently create their own `context.WithTimeout(context.Background(), timeout)` internally (exec.go) + a goroutine that `sess.Signal(SIGKILL) + sess.Close()` on ctx.Done. **So timeout works, but agent-cancellation does not propagate** (the broker's ctx is `Background()`, untouched by the handler's ctx). A long `exec_command` the agent abandons keeps running server-side until the timeout fires.

### What to do

Thread `ctx context.Context` from the `*ForProfile` functions into the broker methods, and have the broker respect `ctx.Done()` (abort the SSH session on cancellation — the same SIGKILL+Close path the timeout already uses).

- `Exec(ctx, cmd, timeout, maxBytes)` / `ExecSudo(ctx, sudoPassword, cmd, timeout, maxBytes)`: the cleanest is to **derive the broker's ctx from the caller's ctx** (`context.WithTimeout(ctx, timeout)` instead of `context.WithTimeout(context.Background(), timeout)`). Then the existing `<-ctx.Done() → SIGKILL+Close` goroutine fires on **either** timeout **or** caller-cancellation. One change, both paths unified.
- `Download(ctx, remotePath, maxBytes)` / `Upload(ctx, localPath, remotePath, maxBytes)`: these use `sftp.NewClient(c.c)` + `io.Copy`. Wrap the `io.Copy` to respect ctx (e.g., `io.Copy` doesn't take ctx — use a select-on-ctx.Done() watchdog that closes the sftp client/file on cancel, OR copy in a goroutine + select). The sftp read/write blocks; on ctx cancel, close the underlying file/client to unblock.
- `Connect(ctx, host, port, user, auth, hostKeyCb)`: `ssh.Dial` blocks; wrap with a deadline from ctx (cancel the dial on ctx.Done). Optional but consistent.
- `ForwardLocal` + `Tunnel`: the tunnel is **long-lived** (managed by `TunnelManager`, closed via `close_port`/idle-sweeper/MCP-shutdown) — it is NOT scoped to a single tool-call's ctx. Leave it ctx-independent (the lifecycle is `TunnelManager`'s job). Just note it in the plan.

The `*ForProfile` functions then pass their `ctx` through: `cli.Exec(ctx, ...)`, etc. The MCP handlers already pass `ctx` to `*ForProfile`. So the only call-site edits are in `core.go` (pass ctx to the broker calls) + the broker signatures (exec.go/sudo.go/download.go/upload.go/client.go).

### Design questions to resolve in the plan

1. **`Exec` signature**: take `ctx` AND keep `timeout` (derive `context.WithTimeout(ctx, timeout)` internally), OR take only `ctx` and have the caller apply the timeout to ctx? Recommend: keep both (`ctx` + `timeout`) — the caller (MCP layer) knows the timeout; the broker applies it to the ctx. Least churn.
2. **Cancellation semantics for sftp (Download/Upload)**: closing the sftp client mid-`io.Copy` aborts the transfer with an error — surface that as a cancellation error (not a silent partial). The §6 `cappedBuffer`/`countingWriter` accounting should still report what was transferred before the cancel.
3. **Connect under ctx**: `ssh.Dial` doesn't take ctx; wrap it (dial in a goroutine, select on ctx.Done → can't cancel an in-flight Dial cleanly, but you can abandon it + return ctx.Err()). Worth doing for consistency; low risk.
4. **Backward compat**: the broker methods are also called by `internal/eval/` (the eval harness drives the broker for some tests) + `internal/cli/ssh.go` (the owner `ssh-manager ssh` command). Updating the signatures means updating those callers too (pass `context.Background()` or a real ctx). Grep `cli.Exec(`/`cli.Download(`/etc. repo-wide for all call sites.

### Test coverage

- Unit (`internal/sshbroker/*_test.go`, in-process via `testsshd`): a test that starts a long `Exec` (e.g., `sleep 30`), cancels the ctx, + asserts the `Exec` returns promptly (within ~1s) with a cancellation error (not after 30s). Same for Download/Upload mid-transfer.
- §13 conformance: confirm the cancellation works against real openssh (the existing `TestInteropMatrix` etc. shouldn't regress).
- `internal/mcpserver/*_test.go`: confirm the handler ctx flows to the broker (a cancelled handler ctx aborts the underlying exec).

---

## Item 2: server-side max-exec-timeout cap

### The gap (confirmed)

`exec_command`'s input has `timeout_seconds` (client/agent-supplied). `ExecCommandForProfile` uses it as the `timeout` (clamped to `defaultTimeout` only when `<= 0`). **There is no server-side ceiling** — an agent can pass `timeout_seconds: 3600` and the broker runs the command for an hour. A misbehaving / prompt-injected agent could tie up the broker (and the SSH session) with a long command. `grep MaxExecTimeout internal/mcpserver/` → nothing; it doesn't exist.

(`MaxOutputBytes` exists — the §6 output cap. `MaxExecTimeout` would be its time analog.)

### What to do

Add a server-side `MaxExecTimeout` constant in `internal/mcpserver/` (alongside `MaxOutputBytes`) + clamp the agent's `timeout` in `ExecCommandForProfile`:

```go
// in ExecCommandForProfile, after the defaultTimeout clamp:
if timeout <= 0 || timeout > MaxExecTimeout {
    timeout = MaxExecTimeout   // server-enforced ceiling; the agent can't exceed it
}
```

(Decide: **clamp silently** to the cap, OR **reject** with an error if the agent requests over the cap. Recommend clamp — it's defense-in-depth, not a usability gate; the command still runs, just bounded.)

### Design questions

1. **Cap value**: 5 min? 10? Make it a `const` (simple) or configurable via env (`SSHMGR_MAX_EXEC_TIMEOUT`)? Recommend: a `const` (e.g., 5 min) for v1; env-config can come later if a use case needs longer.
2. **Where to enforce**: in `ExecCommandForProfile` (core.go), after the profile gate, before `cli.Exec`. (The broker's own `timeout` then can't exceed the cap regardless of what the agent sends.) Also consider: should `defaultTimeout` (the `<=0` fallback) be ≤ `MaxExecTimeout`? (It should — keep them consistent.)
3. **Does this interact with the ctx-threading (Item 1)?** Yes, lightly: with Item 1, the broker derives `context.WithTimeout(ctx, timeout)` — the clamped `timeout` (≤ MaxExecTimeout) is what bounds it. So enforce the cap in `ExecCommandForProfile` (clamp), then pass the bounded `timeout` down. Item 1 + Item 2 compose cleanly.

### Test coverage

- Unit (`internal/mcpserver/core_test.go`): an `ExecCommandForProfile` call with `timeout` way over the cap → assert the actual `timeout` passed to the broker is ≤ `MaxExecTimeout` (capture via a fake broker, or assert the observed run time is bounded). A test with `timeout <= 0` → defaults to `defaultTimeout` (≤ cap). 
- This is small enough to fold into the same task as Item 1's ExecCommandForProfile edit, OR a tiny standalone task.

---

## How to proceed (the new session's flow)

1. **Read** this file + the root `README.md` + skim `internal/sshbroker/exec.go` + `internal/mcpserver/core.go` (the `*ForProfile` functions) to confirm the current state.
2. **Use superpowers:writing-plans** to write Plan 7 (`docs/superpowers/plans/YYYY-MM-DD-context-timeout-hardening.md`). Suggested task split:
   - **T1**: thread `ctx` through `sshbroker` (`Exec`/`ExecSudo` derive from caller ctx; `Download`/`Upload`/`Connect` respect ctx) + the unit tests (cancel-mid-flight). Update all broker call sites (eval, cli/ssh.go, mcpserver).
   - **T2**: `*ForProfile` pass ctx to the broker (core.go) + the `MaxExecTimeout` cap (clamp in `ExecCommandForProfile`) + the cap unit test.
   - **T3**: §13 conformance regression (run the gated suite; confirm no SSH-behavior regression) + a §13 cancellation-vs-real-openssh test if feasible + docs (`docs/ssh-conformance/differences-ledger.md` if the cancellation behavior is a documented deviation) + final whole-branch opus review + merge + push.
   - (No gated §12 / no Fable-5 needed — these are deterministic broker/MCP changes.)
3. **Use superpowers:subagent-driven-development** to execute: implementer sonnet per task → task reviewer sonnet → final opus whole-branch review → merge `--no-ff` → `git push origin master`. Maintain `.git/sdd/progress.md` as the ledger.

## Pointers

- Spec: `docs/superpowers/specs/2026-08-08-ssh-key-manager-mcp-design.md` (§6 = output truncation — the `MaxOutputBytes` analog; §12/§13 = the eval layers).
- The broker: `internal/sshbroker/` (`exec.go`, `sudo.go`, `download.go`, `upload.go`, `tunnel.go`, `client.go`, `output.go` [the `cappedBuffer`]).
- The MCP: `internal/mcpserver/` (`server.go` [handlers + `BrokerTools`], `core.go` [`*ForProfile`], `tunnels.go` [`TunnelManager`]).
- The §6 cap (`MaxOutputBytes`) is the precedent for the `MaxExecTimeout` cap — mirror its pattern.
- §13 conformance: `internal/conformance/` (gated `SSHMGR_CONFORMANCE=1`; the differential pattern).
- The Plan-6 cross-platform-path lesson: use `path` (POSIX) for remote paths, never `filepath`, on remote-target construction.
- SDD ledger (resume source): `.git/sdd/progress.md`.

## Why these two (not other things)

The project is functionally complete + proven. These two are the **real robustness gaps** in the broker:
- **ctx threading**: a long SSH op the agent abandons currently runs to its timeout — cancellation doesn't propagate. Real operational annoyance (stuck commands) + correctness (no clean abort).
- **max-timeout cap**: defense-in-depth against a runaway / instructed agent command tying up the broker. The output is already capped (`MaxOutputBytes`); time should be too.

Deferred (NOT this work): T7 strong-model hallucination (model behavior, not code-fixable); the eval-safety local-command residual (accepted §4 boundary); scope expansion (interactive shell / `-R`/`-D` forward — only if a use case needs them); the accumulated polish Minors (unchecked `out.Close()`, `close_port` audit richness, etc.).
