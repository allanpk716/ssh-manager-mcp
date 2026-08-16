# Task 5 Report: 隧道语义文案对齐（工具描述 + 注释 + scenarios「无活动」）

## 状态: DONE

## Commit Hash: `8a6df3e`

## 改动后的完整行（逐字节自查通过）

### 1. internal/mcpserver/server.go:133 (forward_port Description)
```go
Description: "Open a local port that forwards to a remote service through a server (the `ssh -L` semantic). Use this to reach a service running ON the server (or reachable from it) from your own machine — e.g. a database, web UI, or metrics endpoint. Pass the server's id (from list_servers), remote_host + remote_port (the host:port to forward to FROM THE SERVER'S PERSPECTIVE — usually 127.0.0.1 + the service's port on the server's own loopback), and an optional local_port (omit / 0 = the broker picks a free port). Returns tunnel_id + local_port: the forward listens on 127.0.0.1:<local_port> ON THE MACHINE THE BROKER RUNS ON — with a stdio MCP that is your machine; with a remote serve broker it is the serve host, so reach it from there (e.g. curl on that host) — it is NOT reachable from a different machine. Out-of-profile server ids are rejected. This holds an SSH connection open in the broker for the tunnel's life — call close_port with tunnel_id when done (tunnels auto-close ~10 minutes after creation, not based on activity).",
```

**关键变更**:
- "on YOUR machine" → "ON THE MACHINE THE BROKER RUNS ON — with a stdio MCP that is your machine; with a remote serve broker it is the serve host, so reach it from there (e.g. curl on that host) — it is NOT reachable from a different machine"
- "(tunnels also auto-close after ~10 min idle)" → "(tunnels auto-close ~10 minutes after creation, not based on activity)"

### 2. internal/mcpserver/server.go:151 (close_port Description)
```go
Description: "Close a tunnel opened by forward_port. Pass the tunnel_id forward_port returned. Tears down the local listener AND the SSH connection that backed it (frees the resource — the broker was holding it open). Returns ok on success; an error if the tunnel_id is unknown (already closed, or never opened). No server_id / profile needed: the tunnel_id is an opaque handle bound to the broker process that opened it. You SHOULD call this when you are done with a forward rather than waiting for the ~10-minutes-after-creation auto-close.",
```

**关键变更**:
- "waiting for the ~10 min idle timeout" → "waiting for the ~10-minutes-after-creation auto-close"

### 3. internal/mcpserver/types.go:78 (ForwardOutput 注释)
```go
// close_port when done (tunnels auto-close ~10 min after creation — creation-based, not activity-based).
```

**关键变更**:
- "(tunnels also auto-close after ~10 min idle)" → "(tunnels auto-close ~10 min after creation — creation-based, not activity-based)"

### 4. internal/mcpserver/tunnels.go:10-16 (forwardIdleTimeout 注释)
```go
// forwardIdleTimeout is how long a tunnel lives before the sweeper reaps it.
// NOTE: the signal is CREATION time (lastActivity = time.Now() in Open) —
// Touch(id) exists to refresh it but has NO production caller today, so a
// tunnel dies ~10 min after creation even under continuous traffic. Making
// this activity-aware (wiring Touch) is a tracked backlog item. Default 10
// min per Plan 6 §T4.
```

**关键变更**:
- 整段重写，明确说明信号是创建时间，Touch 无生产调用方，隧道在持续流量下也会 ~10 分钟后回收

### 5. internal/eval/README.md:423-426
```markdown
- A background sweeper auto-closes tunnels **~10 min after creation**
  (`forwardIdleTimeout`; creation-based — `Touch` exists but has no
  production caller) — defense-in-depth so a forgetful agent can't leak
  tunnels indefinitely.
```

**关键变更**:
- "A background **idle-sweeper** auto-closes tunnels idle > ~10 min" → "A background sweeper auto-closes tunnels **~10 min after creation**"
- 增加 "(creation-based — `Touch` exists but has no production caller)"

### 6. docs/scenarios.md:96
```markdown
- 隧道会占住一条 SSH 连接，**创建约 10 分钟后被后台自动回收**（按创建时间计，**不看活动量**——持续有流量也会回收）；你或 agent 也可以随时 `close_port` 主动关。
```

**关键变更**:
- "约 10 分钟无活动会被后台自动回收" → "创建约 10 分钟后被后台自动回收（按创建时间计，不看活动量——持续有流量也会回收）"

### 7. README.md:43 (工具表 close_port 行)
```markdown
| `close_port` | — | Close a forward when done (tunnels auto-close ~10 min after creation). |
```

**关键变更**:
- "(tunnels also auto-close after idle / on exit)" → "(tunnels auto-close ~10 min after creation)"

## Grep 验证（Step 4）

```bash
$ git grep -n -e "10 min idle" -e "分钟无活动" -e "~10 min idle" -e "idle > ~10" -- internal/ docs/ README.md
docs/eval/phase3.md:189:background idle-sweeper auto-closes tunnels idle > ~10 min
internal/eval/summary_test.go:137:	t.Log("forward_port + close_port (ssh -L, stateful TunnelManager, ~10 min idle-timeout + close_port +")
```

**结果**:
- 目标文件（server.go, types.go, tunnels.go, README.md, scenarios.md, eval/README.md）无匹配 ✅
- 发现的匹配在：
  - `docs/eval/phase3.md:189` — 计划工件（非本次修改范围）
  - `internal/eval/summary_test.go:137` — 测试文件日志输出（非本次修改范围）
- `grep -rn "10 min idle" internal/ --include="*_test.go"` 也仅在 summary_test.go:137 命中（日志字符串，非断言），与 brief 预期一致

## 测试验证（Step 4）

```bash
$ go build ./...
# (no output - success)

$ go test ./internal/mcpserver/ -count=1
ok  	ssh-manager-mcp/internal/mcpserver	4.211s
```

**结果**:
- 编译通过 ✅
- 所有 mcpserver 测试通过 ✅
- 描述是字面量字符串，无行为变更，测试全部保持绿色

## Commit（Step 5）

```bash
$ git add internal/mcpserver/server.go internal/mcpserver/types.go internal/mcpserver/tunnels.go internal/eval/README.md docs/scenarios.md README.md
$ git commit -m "docs+schema: tunnel auto-close is creation-based (~10 min after open), and forward_port address is broker-host-local in serve mode"
[worktree-plan-25-ci-gate 8a6df3e] docs+schema: tunnel auto-close is creation-based (~10 min after open), and forward_port address is broker-host-local in serve mode
 6 files changed, 14 insertions(+), 11 deletions(-)
```

## 对齐验证

### 中文引号/标点核对
- scenarios.md 的中文行正确使用中文引号 `「…」` 和 `（…）` ✅
- 标点符号方向一致（`——` 为破折号，`，` 为逗号）✅

### 字节级验证
- 所有替换后的完整行已用 `git diff` 逐行核对
- 中文编码正确（UTF-8）
- 反引号、连字符等特殊字符保持原样 ✅

## Concerns
无
