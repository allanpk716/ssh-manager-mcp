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

---

# Plan 26 终审修复报告（final-review fix round）

HEAD 基线 `b96de92`（worktree `plan-26-arrears`）。三项修复落地，三个 commit（每 finding-group 一个）。

## 逐项改动

### 1（Critical）`internal/sshbroker/upload.go` — EvalSymlinks 硬失败回归

终审判定：T1 在 uploadDir 入口加了 `resolved, rerr := filepath.EvalSymlinks(localRoot); if rerr != nil { return rerr }`。go1.25.8/Windows 上，路径**只是途经** junction 祖先（root 本身是真实目录，如 `<junction>\app`）时 EvalSymlinks 也会报 "The system cannot find the path specified"，无条件 return 把 Plan 26 之前能正常上传的 root 打挂（终审用真实 `mklink /J` 实证）。

**修复**（upload.go:146-160，TDD：先写测试见 RED，再修，复测见 GREEN）：

```go
// 修复前
resolved, rerr := filepath.EvalSymlinks(localRoot)
if rerr != nil {
    return rerr
}

// 修复后（best-effort 解析，Walk 仍是坏 root 的报错权威）
if resolved, rerr := filepath.EvalSymlinks(localRoot); rerr == nil {
    localRoot = resolved
}
```

配套：junction 跟链循环原以 `resolved` 为工作变量，`resolved` 收进 if 作用域后循环改为直接在 `localRoot` 上迭代（upload.go:161-180），循环后多余的 `localRoot = resolved` 删除。外层函数 doc（upload.go:141-145）保留 Plan 26 rationale 不动；内层注释补一句 Windows 途经 junction 祖先会令 EvalSymlinks 失败、解析是 best-effort、坏 root 由 Walk 报错。

**RED→GREEN 证据**：

```
$ go test ./internal/sshbroker/ -run TestUploadDirUnderJunctionAncestor -count=1 -v   # 修复前
=== RUN   TestUploadDirUnderJunctionAncestor
    upload_test.go:626: upload under link ancestor: The system cannot find the path specified.
--- FAIL: TestUploadDirUnderJunctionAncestor (0.09s)

$ go test ./internal/sshbroker/ -run TestUploadDirUnderJunctionAncestor -count=1 -v   # 修复后
=== RUN   TestUploadDirUnderJunctionAncestor
--- PASS: TestUploadDirUnderJunctionAncestor (0.08s)
```

新测试 `TestUploadDirUnderJunctionAncestor`（upload_test.go:599-640）覆盖：真实目录 `real/app`，junction `anc-link → real`，上传 root 取 `anc-link\app`（路径途经 junction 但 root 本身真实）。Windows（junction）车道 RED→GREEN；unix（symlink）车道 EvalSymlinks 能处理途经 symlink，两侧都 GREEN — 跨车道回归哨兵依然有效。

### 2（Important）验收标准 3 idle 措辞漏网 — 三处注释

T5 已把这些文件里其余措辞改完，这三处是漏网（按 grep 定位，不信行号）：

- `internal/mcpserver/server.go:55`：`// background idle-reaper (closes tunnels idle > forwardIdleTimeout)` → `// background tunnel sweeper (creation-based reclaim, see forwardIdleTimeout)`
- `internal/mcpserver/tunnels.go:81`：`// sweepLoop is the idle-reaper: ...` → `// sweepLoop is the tunnel sweeper: ...`（行内其余 byte 原样）
- `internal/mcpserver/core_test.go:961`：`... verifies the idle-sweeper's` → `... verifies the tunnel sweeper's`（行内其余 byte 原样）

仅注释，标识符（`forwardSweepInterval` / `SweepIdle` / `StartSweeper`）未动。

**grep 证明**（改后，internal/ 全域）：

```
$ git grep -n "idle-reaper" -- internal/
（零匹配）
$ git grep -n -e "idle sweeper" -e "idle-sweeper" -- internal/
（零匹配）
```

docs/ 下命中（handoff / 冻结 plan 归档）按验收标准豁免。

### 3（Important）`README.md:137` — 与 concepts.md 矛盾

`ssh-manager clear` 描述 `——按角色枚举删除本机 vault` 与 concepts.md 新的 role-blind scan 段落矛盾。替换为 `——**按实际存在枚举**删除（与 role.json 声明的角色无关）本机 vault`，行内其余 byte 原样。

**byte 级验证**（非 ASCII 编辑，按仓库历史回归纪律）：

```
$ sed -n '137p' README.md   # 行内容（渲染正确，见 git diff 对称）
空机器第一次运行 `tui` 会进入**角色向导**（单机 / server / client 三选，可中断续配）；`ssh-manager clear` 角色清理——**按实际存在枚举**删除（与 role.json 声明的角色无关）本机 vault / serve / 缓存残留（vault 角色先自动 export 备份 + 输入 `DELETE` 确认），机器回到首次向导状态。

$ sed -n '137p' README.md | od -An -tx1 | head -8   # 抽样：全部合法 UTF-8 lead/continuation 序列
$ grep -cF "**按实际存在枚举**删除（与 role.json 声明的角色无关）" README.md
1
$ grep -cF "按角色枚举" README.md
0
$ iconv -f UTF-8 -t UTF-8 README.md | grep -cF "按实际存在枚举"
1
```

- 新短语 fixed-string 恰好 1 处（grep 字节匹配 = 精确字节序列存在）；
- 旧短语全域 0 处；
- 全文件 UTF-8→UTF-8 iconv 往返无错（坏字节会报错退出），往返后短语仍匹配 — 全文件编码完好。

## 验证输出（全跑）

```
$ gofmt -l internal/sshbroker/ internal/mcpserver/
（空输出）
$ go test ./internal/sshbroker/ -count=1
ok  	ssh-manager-mcp/internal/sshbroker	4.306s
$ go test ./internal/mcpserver/ -count=1
ok  	ssh-manager-mcp/internal/mcpserver	4.826s
```

## Commits

```
c79ef11 fix(upload): root resolution survives a traversed junction ancestor — EvalSymlinks failure no longer aborts (final-review Critical)
2fe768e docs(cosmetic): last idle-reaper/sweeper stragglers — acceptance #3 clean (final-review)
dcd8b75 docs: README clear 按-role wording aligned with role-blind scan (final-review)
```

每个 commit 仅含本组文件（1: upload.go + upload_test.go；2: server.go + tunnels.go + core_test.go；3: README.md）。工作区既有未提交项（sdd/task-1..5-report.md 删除、本报告）不属于本轮三 commit，留给 plan 收口处理。

