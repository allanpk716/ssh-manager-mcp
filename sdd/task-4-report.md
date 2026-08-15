# Task 4 Report: server 向导（双密钥 + serve install v2 + 接入卡）

**Commit:** `8416d00` on `feat/plan19-role-wizard`
**Verification:** `go build ./... && go vet ./... && go test ./... -count=1` 全绿；`gofmt -l` 干净。

## What was built

### 1. cli 侧抽核（serve_service.go）
- `installServeService(addr, tlsCert, tlsKey string, out io.Writer) error` — 原 runServeInstall 全部逻辑（master.key 预检 → os.Executable 解析 → Config 构建 → vault 目录 ACL 加固 → 幂等 Uninstall+Install+Start），所有输出（含 best-effort warning）走 `out`。
- `uninstallServeService(out io.Writer) error` — 同法抽出（Task 7 clear 复用）。
- runServeInstall/runServeUninstall 变薄壳（`cmd.OutOrStdout()` 直传）。`--addr` cobra 默认值保持 `127.0.0.1:7878` 不变（向后兼容）；**向导路径恒传 `0.0.0.0:7878`**。

### 2. ⚠ 偏离说明：import cycle → 注入钩子（binding 未覆盖，需 reviewer 知晓）
brief 要求 tui 的 `installServeStep` 直接调 cli 的 `installServeService`，但 **cli imports tui**（cli/tui.go 的 `tui` 命令），tui 反向 import cli = 编译期循环。两个选项：(a) 新建 shared 包搬家；(b) 注入。选了 **(b)**，理由：计划文件清单零偏离、迁移面最小：
- tui/wizardserve.go: `var serveInstall func(...)` + `SetServeInstaller(fn)`。
- cli/tui.go: `RunE` 里 `tui.SetServeInstaller(installServeService)` 后再 `tui.Run(mode)`。
- nil hook（理论上仅非 CLI 入口可达）→ installServeStep 返回明确错误「未接线」，不静默跳过。测试直接 stub 该 var。

### 3. mcpserver：LocalNonLoopbackIPs 导出
cert.go `localNonLoopbackIPs` → `LocalNonLoopbackIPs`（generateServeCert 改调）。注释说明双用途：cert SAN + 向导地址选择器（选中的 IP 正是自签 cert 覆盖的 SAN）。

### 4. server 向导串联（wizard.go + wizardserve.go）
步骤（spec §2.4）：
1. **客户端名**（stepClientName，默认 os.Hostname()）→ 进共享 ③（server loop，可跳过）→ ④ profile+grant（**profile 默认名 = 客户端名**，enterProfileGrant 按 role 区分）→ project →
2. **密钥 1/2：project token**（wizTokenScreen；用途「贴到 client 机 .mcp.json 的 --token 参数」，丢失→Projects 页 [a]）→
3. **密钥 2/2：设备码**（issueDeviceCode：**先 LoadOrCreateServeCert 后 AddCacheToken**——顺序保证半失败重试幂等，避免 active-name 撞名；用途行内嵌现成合并串 `cache pull --token '<码>:<指纹>'`，丢失→设备码页 [a]）→
4. **⑥ serve 段**：wizAddrForm（非环网 IPv4 Select，value=显示串=`https://<ip>:7878`，默认第一项；空列表退化手输框，校验 https:// 前缀）→ admin 前置提示屏 → installServeStep（绑 0.0.0.0:7878，addr 仅展示）→ serveInstalledMsg.err 时**不阻断**继续 → probeServe（TLS GET /snapshot，InsecureSkipVerify，任意 HTTP 状态即 ok，3s 超时）→
5. **结果屏**（stepServeResult overlay）：安装失败=红横幅+**原文提权命令**（Windows「管理员终端」/POSIX sudo）；探活=绿「已就绪」/黄「未验证，client 可能连不上」+排查提示（防火墙/serve status/稍候重试）→
6. **⑦ 接入卡** accessCard(addr, fp)：实值地址+指纹+两密钥去向表+命令式备选（指纹拼进 token）+「密钥不重显」说明+丢失重发指引 → wizFinish(RoleServer) → broker 控制台（mode.go 既有链路）。

### 5. Resume 启发式（镜像 T3 并扩展一档）
| vault 状态 | resume 落点 |
|---|---|
| 0 profile | 全新流程：客户端名 → … |
| ≥1 profile, 0 project | 复用既有 profile，从 project 步继续（同 T3） |
| ≥1 profile+project+**≥1 设备码** | 直跳 serve 段（addr form）；指纹经幂等 LoadOrCreateServeCert 恢复（不可读时降级为提示文本，不 trap resume） |
| ≥1 profile+project, 0 设备码 | 重问客户端名（仅用于命名设备码），提交后**只签发设备码**，不重建任何实体（profileID 预载用于路由） |

设备码签发失败：stepDeviceIssue 等待屏 + `r` 重试（幂等）；q/Esc 安全暂停。stepVaultErr 的 `r` 重试按 role 分流（enterServer/enterStandalone）。

## Tests（wizardserve_test.go，TDD：先失败后实现）
brief 两条原样落地（TestAccessCard_Copy / TestProbeServe）+ 新增：
- lanAddrOptions 值形态（value=key=`https://<ip>:7878`、IPv6/环回过滤、默认第一项）、wizAddrForm 预置/手输退化
- installServeStep 恒绑 0.0.0.0:7878、err 传递、nil-hook 报错
- serveResultScreen 横幅要素（失败=原文命令+不阻断；探活=已就绪/未验证+排查）
- 全链路状态机：fresh 起点客户端名、profile 默认名=客户端名、token 屏 client 用途、设备码签发+屏文案+落库、resume 两档、serve 段失败不阻断直达接入卡+wizFinish(RoleServer)
- 回归：既有 wizard/wizardsteps/cli serve 全部通过；cli `serve install` 冒烟（serve_smoke_test 等）绿

## 既有测试的必要更新（行为合法变更）
- `TestWizard_FirstScreenSavesRole`：加 `w.closeStore()`（server 流现在真的开 store；Windows TempDir 清理需要）
- `TestWizard_ResumeSkipsFirstScreen`：server resume 不再是 placeholder——断言改为落在 stepClientName

## Concerns / 留给后续任务
1. **注入钩子 vs 共享包**（见 §2）：若 reviewer 倾向 `internal/servesvc` 搬家方案，Task 7（clear 复用 uninstall）时一并重构成本最低。
2. 设备码 active-name 撞名：若操作者在向导前手工建过同名设备码，AddCacheToken 报错→ r 重试仍撞名；错误文案足够指路（主控台吊销或改名），未做 dedupe（与 AddProject 路径行为一致）。
3. probeServe 探活的 `InsecureSkipVerify` 带 `//nolint:gosec` 注释（probe-only client，pin 是 client 侧关切——brief 明确）。
4. sdd/task-2-report.md 有一段**他人遗留的未提交修改**（记录 29412d9 的 fix），与本任务无关，未纳入本 commit。
