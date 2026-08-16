# Plan 22 设计：部署后修正包（spec v1）

> 日期：2026-08-16。来源：v0.7.0 部署现场发现 + Plan 20/21 终审 deferred 残余。用户拍板「可以」的分层清单原样立项。

## T1 serve status 探针修复（部署现场发现的存量 bug）

`probeServeHTTP`（serve_service.go:554）用明文 `http://` 探测——自 auto-TLS 起 serve 是 TLS-only，握手必败 → http 信号永远误报 "not responding"。修复：

- 探测改 `https://` + `InsecureSkipVerify`（本机自签 liveness 探测，可接受），仍接受 200/401 为活。
- 探测地址不再硬编码：`serve status` 增可选 `--addr` flag（默认 `127.0.0.1:7878`），探针用该值（注册服务的真实 addr 跨平台回读 brittle，注释已自认；flag 是最小可测修正）。
- 测试：httptest.NewTLSServer 返回 401 → 探针 true；返回 500 → false；明文 http server（模拟旧 serve）→ false（确认我们不再接受明文活信号）。

## T2 `servers edit --password ""` 空密凭据拒绝（既有 bug）

现状：`Changed("password")` 为真时空串直接铸 `CredPassword{Secret: []byte("")}`。修复：pwSet 且 `strings.TrimSpace(password)==""` → 报错 `--password 不能为空（清除凭据请用 --clear-credential）`；`--key` 空路径同款（`--key ""` 现在会 readKeyFile("") 报 OS 错，改成清晰报错）。sudo-password 同判。测试三 flag 各一条。

## T3 卫生打包（六件，全小）

1. readonly_test 补 `ClearServerCredential` 枚举行。
2. walk-halt 紧钉：upload_test 两文件用例（小文件+超限文件 → `Files==1`、第二文件远端缺席）。
3. emoji 文件名 fixture 一行（差分套餐 `writeDifferentialSuite` 加一个 🚀 文件）。
4. `--clear-credential` 用户文档段（managing-servers.md 的 edit/无凭据节 + README TUI 表）。
5. README:278 死 hash `8526ad9`→`c188b0d`；README/eval :388 单文件超限措辞（「partial tree」→ 单文件完整落盘+Truncated 如实）。
6. rename+clear 打印：clear 分支打印**现库名**（GetServerByName 后未改的 `srv.Name` 在字段应用前先存）；importflow 结果页 FAILED 行计入「失败 N」第四计数。

## T4 serve 日志落盘

现状：`program.run` 错误写 stderr → kardianos/Windows 落 EventLog（可用但难查）；正常启动/请求无日志。修复：

- `program.run` 的两处 Fprintln 与 RunServe 启动行改走 `log.New(io.MultiWriter(os.Stderr, serveLogFile))`。
- `serveLogFile`：`paths.ServeLogPath()`（已存在，无 env seam——**顺手补 `SSHMGR_SERVE_LOG` seam**，守「新生产路径必须有 env seam」铁律）；打开失败降级 stderr-only（serve 不因日志失败拒启）。
- 轮转：简单尺寸轮转（>5MB 时 rename serve.log→serve.log.1 覆盖旧的，保留 1 代）——不引依赖。
- 前台 `ssh-manager serve`（非服务）不变（stderr 即可）。
- 测试：env seam 指 tmp 路径，起 in-process serve 短连一次，断言日志文件出现请求/启动行；轮转测试（预置 6MB 文件 → 首写后出现 .1）。

## 边界与不做

- 不做 kardianos Logger 全量接管（stderr 路径保留，EventLog 兼容）。
- 不做 `serve status` 读注册 addr 回读（--addr flag 是本卡方案）。
- 不做超限上传拒绝语义（OWNER 设计题另行拍板）。
- 不做 CI 基线（OWNER 动作）。

## 验证策略

每任务 TDD + 全量 `go build ./... && go vet ./... && go test ./...`；T3 的差分项在 `SSHMGR_CONFORMANCE=1` gate 内跑；T4 的 serve 冒烟用 in-process httptest 模式（既有 serve_test 基建）。
