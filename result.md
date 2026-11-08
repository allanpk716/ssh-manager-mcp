# 规格分支合入 master 的结果报告

## 合并状态

- 已在 `spec-143` 分支执行 `git merge --no-ff --no-commit master`。
- 合并双方：`spec-143` 为 `e588f86`，`master` 为 `25249c6`，共同祖先为 `a7a38407`。
- 六个冲突文件都已按下方规则解决，并且已执行 `git add`。
- 没有执行 `git commit`、`git merge --continue` 或 `git push`；合并保持在“已解决、已暂存、未提交”状态。

## 冲突文件的处理

### `internal/cli/ui.go`

保留了规格分支的服务器页写入口 `ServerWriter`，同时保留 master 的运维来源 `Ops` 和客户端启动实例 `ClientInstance`。`webui.Config` 现在同时携带服务器页、访问区域和运维区域的全部来源。

### `internal/cli/ui_sources.go`

- 合并双方 import：保留 `servermanage`、`webuiservers`，也保留 `ops`、`webuiops`。
- 单机来源实现 `Servers(instance)`、`ServerList(instance, attentionOnly)`、`ServerDetail(instance, identifier)`；单机只有一个本机加密存储，因此后两个方法忽略实例名。
- 客户端来源的 `ServerList` 与 `ServerDetail` 都改为按第一个参数调用 `clientops.LoadCacheSnapshotFor(instance)`，与 master 中 `Servers(instance)` 的客户端行为一致。

### `internal/mcpserver/serve_webui.go`

- 合并双方 import，权威端同时装配服务器页与运维区域。
- `serveWebUIServerSource` 的 `ServerList` 与 `ServerDetail` 增加 `instance` 参数；权威端直接读取权威端加密存储，因此忽略该参数。
- `webui.NewAuthority` 的配置同时保留 `ServerWriter`、`Ops`、`ServiceRestart`。

### `internal/webui/authority.go`

- `NewAuthority` 同时注册 `registerServerRoutes()` 与 `registerOpsRoutes()`。
- `handleAuthorityRoot` 只负责识别根路径并调用 `h.handleServerList(w, r)`。
- 删除 master 侧在处理函数内重复的权威端会话判断和服务器列表渲染；会话检查由外层 `h.requireSession(...)` 完成。

### `internal/webui/static/app.css`

保留双方全部样式：服务器页的链接、详情、长表单和窄屏样式，以及 master 的审计、设置、升级、客户端、备份页样式。共享祖先里的样式只保留一份；合并结果等价于“两个分支样式集合去掉共同祖先重复部分后的并集”。

### `internal/webui/webui.go`

- `ServerSource` 统一为：
  - `Servers(instance string) ([]ServerRow, error)`
  - `ServerList(instance string, attentionOnly bool) ([]ServerListEntry, error)`
  - `ServerDetail(instance, identifier string) (*ServerDetailView, error)`
- `Config` 同时保留 `ServerWriter`、`Ops`、`ServiceRestart`、`ClientInstance` 等字段。
- 回环模式 `New` 同时注册服务器页与运维区域路由。
- `handleUIRoot` 保留规格分支的 `h.handleServerList(w, r)`，删除 master 侧重复的会话检查与旧列表渲染。

## 运维路由的会话检查

以下 master 新增的、需要会话的运维路由全部改为外层 `h.requireSession(...)` 包装：

- `/ui/ops/`
- `/ui/ops/update`
- `/ui/ops/update/restart`
- `/ui/ops/client`
- `/ui/ops/client/sync`
- `/ui/ops/client/instance`
- `/ui/ops/client-audit`
- `/ui/ops/backup`
- `/ui/ops/backup/config`
- `/ui/ops/backup/create`
- `/ui/ops/backup/verify`
- `/ui/ops/settings`
- `/ui/ops/audit`

处理函数里的 `requireRecentReverify`、防跨站请求伪造检查和方法检查保留；它们分别负责敏感操作前的近期重新验证、写请求防护和方法约束，不是重复的登录会话检查。启动入口、静态资源、登录和注销仍不使用 `requireSession` 包装。

新增 `internal/webui/ops_test.go` 中的 `TestOpsPagesRequireSession`，覆盖单机、客户端与权威端运维路由未登录时的行为：单机和客户端返回 401，权威端跳转登录页。

## `ServerList` / `ServerDetail` 签名涉及的文件

接口与调用：

- `internal/webui/webui.go`
- `internal/webui/servers.go`

实现：

- `internal/cli/ui_sources.go`
- `internal/mcpserver/serve_webui.go`
- `internal/webuiservers/store.go`

测试与测试桩：

- `internal/cli/ui_sources_test.go`
- `internal/webui/handler_test.go`
- `internal/webui/servers_test.go`
- `internal/webui/ops_client_test.go`
- `internal/vault/webui_auth_test.go`
- `internal/webuiservers/store_test.go`

`internal/webui/servers.go` 中所有服务器页列表和详情调用都传入 `h.requestClientInstance(r)`。客户端实例切换后，列表、详情、编辑表单回显、补全队列和补全结果都会读取当前浏览器会话选中的实例。

## 验证输出

### `go build ./...`

```text
命令退出码 0，无输出。
```

### `go vet ./...`

```text
命令退出码 0，无输出。
```

附加检查 `gofmt -l .` 也无输出。

### `go test ./...`

已先按原命令执行，后又用 `-count=1` 关闭缓存复跑；两次结果相同。除任务说明中已声明的 `internal/probe` 四个既有失败外，其余包全部通过。关键输出：

```text
ok  ssh-manager-mcp/internal/cli
ok  ssh-manager-mcp/internal/clientops
ok  ssh-manager-mcp/internal/mcpserver
ok  ssh-manager-mcp/internal/ops
ok  ssh-manager-mcp/internal/servermanage
ok  ssh-manager-mcp/internal/tui
ok  ssh-manager-mcp/internal/vault
ok  ssh-manager-mcp/internal/webui
ok  ssh-manager-mcp/internal/webuiaccess
ok  ssh-manager-mcp/internal/webuiops
ok  ssh-manager-mcp/internal/webuiservers

--- FAIL: TestServiceSelfProbeGroupsResultsByText
--- FAIL: TestServiceTargetParsingDefaultsAndLimits
--- FAIL: TestServiceRemoteProbeUsesFixedCommandAndParsesOutput
--- FAIL: TestServiceRemoteCommandTimeoutKeepsPartialOutput
FAIL    ssh-manager-mcp/internal/probe
```

这四个失败与任务说明一致，未修改。

### `go test -race ./internal/webui/... ./internal/webuiservers/... ./internal/webuiops/... ./internal/cli/...`

命令已运行。前三个包通过，`internal/cli` 有一个与本次合并无关、master 上可复现的既有耗时敏感测试失败：

```text
ok      ssh-manager-mcp/internal/webui
ok      ssh-manager-mcp/internal/webuiservers
ok      ssh-manager-mcp/internal/webuiops
--- FAIL: TestSnapshotPollE2E_TwoBridgesSharedCache
    mcp_cache_poll_test.go:347: 两个桥共存时全量快照请求 = 3,应为 2(只有一个桥下载)
FAIL
FAIL    ssh-manager-mcp/internal/cli
```

输出中没有 `WARNING: DATA RACE`。为确认不是合并引入，我用 `git archive master` 导出的 `25249c6` 干净内容执行：

```text
go test -race ./internal/cli \
  -run '^TestSnapshotPollE2E_TwoBridgesSharedCache$' -count=10
```

结果同样出现该失败；十次中有八次失败。这说明失败来自 master 既有的测试时序问题。非并发检测模式下 `internal/cli` 全部测试通过。

### Linux 构建

```text
GOOS=linux go build ./...：通过
```

### Windows 构建

```text
GOOS=windows go build ./...：通过
```

### Playwright 浏览器测试

依赖已存在，未重新安装。按 `e2e/README.md` 与 `.github/workflows/e2e.yml` 的方式，在 `e2e` 目录执行 `npm test`：

```text
Running 16 tests using 1 worker
  ✓ [desktop-chromium] 访问区域、运维备份、运维设置、审计、服务器过滤编辑、删除预览、导入补全：8 项通过
  ✓ [mobile-chromium]  访问区域、运维备份、运维设置、审计、服务器过滤编辑、删除预览、导入补全：8 项通过

16 passed (9.7s)
```
