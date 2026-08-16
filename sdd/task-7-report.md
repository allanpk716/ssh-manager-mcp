# Task 7 Report: Revoke 语义回归测试

**Status:** ✅ COMPLETE
**Commit:** `ad451cc`
**Date:** 2026-08-16

## 任务概述

本任务是 tests-only 任务，将 Plan 25 Task 6 文档中验证过的两个断连语义事实钉成永久回归测试：

1. **Layer 1+3**: revoke 后 token 门立即拒绝（VerifyToken 返回 nil），但已建立的隧道继续转发
2. **Layer 2**: serve HTTP 层对 revoke 后的请求（close_port 和 initialize）返回 401，请求未到达工具层

## 实施过程

### Step 1: 创建测试文件

文件：`internal/mcpserver/revoke_semantics_test.go`
- 测试 1: `TestRevokedProjectKeepsOpenTunnelForwarding` — 验证 revoke 后已建立的隧道仍能转发
- 测试 2: `TestServeHTTPRejectsRevokedTokenPerRequest` — 验证 serve HTTP 层对 revoke token 的 401 拒绝

**Brief 代码修正：**
- 移除了未使用的导入：`io`、`strconv`
- 移除了未使用的变量：`host`、`port`、`portStr`
- 这些是 brief 代码的笔误，不影响测试逻辑和语义

### Step 2: 运行两个测试（预期 PASS）

```bash
go test ./internal/mcpserver/ -run 'TestRevokedProjectKeepsOpenTunnelForwarding|TestServeHTTPRejectsRevokedTokenPerRequest' -v
```

**输出：**

```
=== RUN   TestRevokedProjectKeepsOpenTunnelForwarding
    revoke_semantics_test.go:70: before-revoke: tunnel forwarded "ping-before-revoke\n"
    revoke_semantics_test.go:85: after-revoke: tunnel forwarded "ping-after-revoke\n"
--- PASS: TestRevokedProjectKeepsOpenTunnelForwarding (0.11s)
=== RUN   TestServeHTTPRejectsRevokedTokenPerRequest
--- PASS: TestServeHTTPRejectsRevokedTokenPerRequest (0.05s)
PASS
ok  	ssh-manager-mcp/internal/mcpserver	0.947s
```

**结果：** ✅ 两个测试均 PASS，符合预期（行为已存在，测试仅钉住语义）

### Step 3: 全包回归测试

```bash
go test ./internal/mcpserver/ -count=1
```

**输出：**

```
ok  	ssh-manager-mcp/internal/mcpserver	3.503s
```

**结果：** ✅ 全包回归通过，无回归

### Step 4: Commit

```bash
git add internal/mcpserver/revoke_semantics_test.go
git commit -m "test: pin disconnect semantics — token gate rejects immediately, open tunnels survive revocation, serve HTTP 401s pre-tool"
```

**Commit Hash:** `ad451cc`

## 代码质量检查

### gofmt 检查
```bash
gofmt -l internal/mcpserver/revoke_semantics_test.go
```
**结果：** ✅ 无输出（格式正确）

## 测试验证的语义事实

### TestRevokedProjectKeepsOpenTunnelForwarding 验证：

1. **Revoke 前**：隧道正常转发（`before-revoke` 探针成功）
2. **Token 验证**：revoke 前 `VerifyToken` 返回有效 project，revoke 后返回 `nil`（Layer 1 验证）
3. **Revoke 后**：已建立的隧道继续转发（`after-revoke` 探针成功）（Layer 3 验证）

### TestServeHTTPRejectsRevokedTokenPerRequest 验证：

1. **Revoke 前**：`initialize` 请求返回 200
2. **Revoke 后**：
   - `close_port` 请求返回 401（在到达工具层前被拒绝）
   - `initialize` 请求返回 401（在到达工具层前被拒绝）

## 包依赖使用的 Helper

本测试使用了包内现有的 helper 函数：

- `newStore(t)` — core_test.go:23
- `seedRealServer(t, st, name, addr, hk, sudoPw)` — core_test.go:248
- `startEchoListener(t)` — core_test.go:687
- `NewServer(st, profileID, projectID)` — server.go
- `ForwardForProfile(ctx, st, projectID, profileID, serverID, remoteHost, remotePort, localPort, mgr)` — core.go:427
- `NewServeRunner(st)` / `r.HTTPHandler()` — serve.go

## 结论

Task 7 完成度 100%：
- ✅ 两个语义事实已钉成回归测试
- ✅ 测试通过（行为符合文档）
- ✅ 全包回归无问题
- ✅ 代码格式正确
- ✅ 已提交到 worktree 分支

这两个测试将成为 CI 基线的一部分，永久监控断连语义的正确性。
