# Plan 22 实施计划：部署后修正包

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 从 spec `docs/superpowers/specs/2026-08-16-plan-22-post-deploy-fixes-design.md` 落地 4 个任务：serve status 探针修复、edit 空密凭据拒绝、六件卫生打包、serve 日志落盘。

**Architecture:** 全部是既有代码的小修正与补强；T1/T4 同碰 `internal/cli/serve_service.go` **必须串行**（T1 先）；T2/T3 独立。

**Tech Stack:** Go 1.25，既有依赖（kardianos 仅用现有接口）。

## Global Constraints

- 仓库 PUBLIC：无真实 secret/主机。
- TDD：失败测试先行；`go build ./... && go vet ./...` 干净；既有测试不弱化。
- 测试 env seam（`SSHMGR_STORE`/`SSHMGR_SERVE_LOG` 等）绝不触碰生产固定路径。
- 提交前缀 `fix:`/`feat:`/`test:`/`docs:`；行号以符号名定位为准（可能漂移）。

---

### Task 1: serve status 探针修复

**Files:**
- Modify: `internal/cli/serve_service.go`（probeServeHTTP :554 附近 + runServeStatus :393 附近）
- Test: `internal/cli/serve_service_test.go`（若无对应用例文件则加到既有 serve 测试文件）

**Interfaces:** Produces: `probeServeHTTP(addr string) bool` 语义变更（https 探测）；`serve status [--addr <host:port>]` 新 flag。

- [ ] **Step 1: 失败测试**

```go
func TestProbeServeHTTPOverTLS(t *testing.T) {
	// httptest.NewTLSServer 401 → probeServeHTTP(host) == true
	// 同 server 500 路径 → false
	// httptest.NewServer(明文) → false（不再接受明文活信号）
}
```

- [ ] **Step 2: 实现**

```go
func probeServeHTTP(addr string) bool {
	client := &http.Client{
		Timeout: time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, // localhost liveness: self-signed cert
	}
	resp, err := client.Get("https://" + addr + "/")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusUnauthorized
}
```

`runServeStatus` 加 `--addr` flag（默认 `127.0.0.1:7878`），两处 `probeServeHTTP(...)` 调用点（服务分支 + 无服务分支）改用 flag 值；flag help 注明「注册时 --addr 改过的话这里也要给」。

- [ ] **Step 3: 全量 + 提交** `fix: serve status probes https (auto-TLS) + --addr flag`

### Task 2: edit 空密凭据拒绝

**Files:**
- Modify: `internal/cli/servers.go`（serversEditCmd re-credential 段 :225 附近）
- Test: `internal/cli/servers_test.go`

- [ ] **Step 1: 失败测试**：三条——`edit x --password ""` / `--key ""` / `--sudo-password ""` 各报错（含「清除凭据请用 --clear-credential」文案于前两条），库内凭据行数不变。
- [ ] **Step 2: 实现**：`pwSet && strings.TrimSpace(password)==""` → error；keySet 同判（路径空）；sudo Changed 且空 → error。文案：`--password 不能为空（更换凭据请给新值；清除凭据请用 --clear-credential）`。
- [ ] **Step 3: 全量 + 提交** `fix: reject empty --password/--key/--sudo-password on servers edit`

### Task 3: 卫生打包（六件）

**Files:**
- Modify: `internal/store/readonly_test.go`（+1 行枚举）
- Modify: `internal/sshbroker/upload_test.go`（两文件紧钉用例）
- Modify: `internal/conformance/upload_forward_test.go`（emoji fixture）
- Modify: `docs/managing-servers.md`、`README.md`、`internal/eval/README.md`
- Modify: `internal/cli/servers.go`（clear 分支打印现库名）、`internal/tui/importflow.go`（FAILED 第四计数）

- [ ] **Step 1: 测试先行**（readonly 枚举行；两文件 upload 用例断言 `Files==1` + 第二文件远端缺席——用 in-process testsshd 断言远端路径不存在；importflow `failN` 计数断言结果页含「失败 1」）。
- [ ] **Step 2: 实现六件**：
  1. readonly 枚举：`require ErrReadOnly from ClearServerCredential`。
  2. 两文件用例：small(1KB)+over-cap(MaxOutputBytes+1) → Upload 返回 `Files==1`、`Truncated==true`；远端 stat 第二文件 ENOENT。
  3. emoji：`writeDifferentialSuite` 加 `pkg/🚀rocket.txt`（含内容），Files/Bytes 期望值同步 +1。
  4. 文档：managing-servers 的 edit 节加 `--clear-credential` 段（独占动作语义+互斥+TUI 勾选对应）；README TUI/CLI 表加一行。
  5. README:278 `8526ad9`→`c188b0d`；internal/eval/README.md :388 一句改「单文件超限时完整落盘 + Truncated=true（多文件时后续文件不再上传）」。
  6. clear 打印现库名（字段应用前 `origName := srv.Name`，打印用 origName）；importflow 结果页加 `失败 %d` 计数（结果行不变，只补计数）。
- [ ] **Step 3: 验证**：`SSHMGR_CONFORMANCE=1 go test ./internal/conformance/ -run TestUploadDifferential -count=1` + 全量。提交 `test/docs/fix: post-deploy hygiene pack (6 items)`（或按性质拆两 commit）。

### Task 4: serve 日志落盘

**Files:**
- Modify: `internal/paths/paths.go`（ServeLogPath 加 `SSHMGR_SERVE_LOG` seam）+ `internal/paths/paths_test.go`
- Modify: `internal/cli/serve_service.go`（program.run 日志 MultiWriter + 轮转）
- Test: `internal/cli/serve_log_test.go`（新）

**Interfaces:** Produces: env `SSHMGR_SERVE_LOG`（serve 日志路径 seam，测试/迁移用）；文件 `serve.log` + `serve.log.1`（>5MB 轮转，保 1 代）。

- [ ] **Step 1: 失败测试**（env seam 指 tmp）：起 in-process serve（既有 serve_test 模式）短连一次 → 日志文件含启动行/请求行；预置 6MB serve.log → 首写后 `.log.1` 出现且 `.log` 重生。
- [ ] **Step 2: 实现**：

```go
// openServeLog returns a rotating file sink (nil on failure → stderr-only).
// >5MB → serve.log renamed to serve.log.1 (one generation, overwrite old .1).
func openServeLog() *os.File {
	p, err := paths.ServeLogPath()
	if err != nil { return nil }
	if fi, err := os.Stat(p); err == nil && fi.Size() > 5<<20 {
		_ = os.Rename(p, p+".1") // best-effort rotation
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil { return nil } // serve must not fail because logging failed
	return f
}
```

`program.run`：`var w io.Writer = os.Stderr; if f := openServeLog(); f != nil { w = io.MultiWriter(os.Stderr, f); defer f.Close() }`，两处 `Fprintln(os.Stderr, ...)` 改走该 writer；RunServe 若有启动日志行同样接 w（读 RunServe 现状决定最小接线——若 RunServe 无日志输出，本任务只覆盖 program.run 两处 + 在 serve 监听成功后补一行「listening on %s (tls=auto)」）。

- [ ] **Step 3: 全量 + 提交** `feat: serve file logging with size rotation (SSHMGR_SERVE_LOG seam)`

---

## 任务依赖图

```
T1 → T4 (同文件串行)     T2、T3 独立
串行序建议: T1→T2→T3→T4
```
