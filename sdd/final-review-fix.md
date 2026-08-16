# Plan 25 终审修复报告（final-review fix round）

HEAD 基线 `a553693`（worktree `plan-25-ci-gate`）。四项修复全部落地，一个 commit 收口。

## 逐项改动

### 1（Important）`internal/mcpserver/types.go:81` — LocalPort jsonschema 口径

原文（T5 只改了 server.go:133 工具 Description，漏了 schema tag）：

```go
LocalPort int    `json:"local_port" jsonschema:"the local port now forwarding to remote_host:remote_port — reach it via 127.0.0.1:local_port on your machine"`
```

改为：

```go
LocalPort int    `json:"local_port" jsonschema:"the local port now forwarding to remote_host:remote_port — reach it via 127.0.0.1:local_port on the machine the broker runs on"`
```

### 2（同类）`internal/mcpserver/types.go:75-78` — ForwardOutput 注释首句

原文（首句 `ITS OWN machine (the broker host's loopback)` 与 server.go:133 新 Description 矛盾）：

```go
// ForwardOutput is the forward_port tool output. The agent reaches the remote
// service at 127.0.0.1:local_port on ITS OWN machine (the broker host's
// loopback) — e.g. curl http://127.0.0.1:<local_port>. Pass tunnel_id to
// close_port when done (tunnels auto-close ~10 min after creation — creation-based, not activity-based).
```

改为（等价替换，语义对齐 server.go:133 的 "ON THE MACHINE THE BROKER RUNS ON — stdio: your machine; remote serve: the serve host"）：

```go
// ForwardOutput is the forward_port tool output. The agent reaches the remote
// service at 127.0.0.1:local_port on the machine the broker runs on (stdio:
// the agent's own machine; remote serve: the serve host) — e.g. curl
// http://127.0.0.1:<local_port> or point your client at it, from that host.
// Pass tunnel_id to close_port when done (tunnels auto-close ~10 min after
// creation — creation-based, not activity-based).
```

### 3（Minor）`internal/cli/ssh.go:12` — 去掉多余 import 别名

```diff
 	"ssh-manager-mcp/internal/sshbroker"
-	sshmanstore "ssh-manager-mcp/internal/store"
+	"ssh-manager-mcp/internal/store"
 	"ssh-manager-mcp/internal/vault"
```

文件内 `sshmanstore.AuditRow` 两处（WriteAudit error 分支 line 63、正常分支 line 74）同步改回 `store.AuditRow`。

### 4（Minor）`internal/cli/ssh_test.go:82,88` — 丢弃式接收

`TestOwnerSSHPropagatesRemoteExitCode` 内：

```diff
-	srvID, _ := st.AddServer(&models.Server{
+	_, _ = st.AddServer(&models.Server{
 		Name: "t", Host: host, Port: portOfAddr(addr), User: "u",
 		AuthMethod: models.AuthPassword, CredentialID: cid,
 	})
 	_ = st.SaveHostKey(host, portOfAddr(addr), hostKey.Marshal())
 	st.Close()
-	_ = srvID
```

grep 全文件确认 `srvID` 仅此一处；`TestOwnerSSHConnectDeadlineBounded`（line 126）本来就是 `_, _ = st.AddServer(...)`，无需改动。

## 验证输出（全跑）

### build / vet / gofmt

```
$ go build ./... && go vet ./... && gofmt -l . && echo "BUILD_VET_FMT_OK"
BUILD_VET_FMT_OK
```

`gofmt -l .` 空输出（三者全过）。

### 测试

```
$ go test ./internal/cli/ ./internal/mcpserver/ -count=1
ok  	ssh-manager-mcp/internal/cli	9.075s
ok  	ssh-manager-mcp/internal/mcpserver	5.429s
```

### 口径残留 grep

```
$ git grep -n "on your machine" internal/mcpserver/types.go
internal/mcpserver/types.go:52:	LocalPath  string `json:"local_path" jsonschema:"absolute local path (on your machine) of the file or directory to push to the server; ..."`

$ git grep -n "ITS OWN machine" internal/mcpserver/
（零匹配，exit 1）
```

- `"ITS OWN machine"`：internal/mcpserver/ 全域零匹配 — 达标。
- `"on your machine"`：types.go 剩 1 处命中 — line 52 `UploadInput.LocalPath`。**不在终审 4 项范围**：upload 工具在 server.go:115 的 Description 同样写 "LocalPath is on your machine"，且 T5/终审均未把 upload 列入 re-scope（只 re-scope 了 forward_port）。upload 口径与 forward 是同类理论问题（remote serve 下 LocalPath 实际在 broker host），但按 scope 纪律未动，留作 concern 上报，由 orchestrator 决定是否另开 fix。

## Commit

```
git commit -m "fix(schema): LocalPort description is broker-host-local (match forward_port tool description); drop needless import alias"
```

仅含三个代码文件（ssh.go / ssh_test.go / types.go）；sdd 下 9 份 task report + 本报告为终审前既有未提交 docs 物料，保持未提交状态留给 plan 收口。
