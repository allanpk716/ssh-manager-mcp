# Task 8 报告 — 概念模型文档（docs/concepts.md）+ 交叉链接

**状态**：✅ 完成　|　**Commit**：`ecdd607`（feat/plan19-role-wizard）　|　**构建**：`go build ./...` BUILD_OK（仅文档文本改动，未触碰代码）

## 交付物

### 1. `docs/concepts.md`（新建，79 行 — 符合 ~100 行以内的要求）

按 spec §5 与任务 ground truth 逐项落实：

- **数据流图（ASCII）**：server 机四页签（服务器=货架 / Profiles=装箱单 / Projects=钥匙 / 设备码=水管）↔ project token（阀门，在线远程 MCP 路径）+ 设备码（水管钥匙，GET /snapshot 整仓快照路径）↔ client 机 `cache.bin`（加密整仓只读快照）→ `.mcp.json --token` → agent 只见自己 project 绑定 profile 内的服务器（一把钥匙只开一个箱子）。
- **类比表**：仓库 / 货架 / 装箱单 / 钥匙 / **设备码=水管钥匙** / **token=阀门** / **服务器指纹（pin）=防伪封条（首次连接必须核对）** / cache.bin=加密整箱货物。含 pin、水管钥匙两个必补行。
- **两种输入形态**：分开填（裸码 + 指纹栏 / `--pin` / `SSHMGR_SERVE_PIN`）vs `<码>:<指纹>` 合并串，等价；三处皆无 pin → 默认 hard-fail 不明文（与 shipped 行为一致）。
- **第二台 client 完整接入链**（4 步）：设备码页 [a] 签发（一台一枚、吊销粒度=单机、**不推荐复用**——失窃只能全吊）→ Projects 页 [a] 新建 project 绑 profile → token 一次性展示（可重发）→ client 机向导 或 `cache pull --token '<码>:sha256:<指纹>'` → `.mcp.json --cache --token`。project/token 按项目粒度自由建、设备码按机器粒度发。
- **角色与向导一段话**：role.json 三角色（standalone/server/client）、后果导向首屏、选定瞬间落盘 role.json（Esc=安全暂停、可续配）、standalone→server 经 `[u]` 无损升级、vault→client 必经 clear。
- **clear 一段话**：按实存枚举清单（vault/serve/缓存残留/Windows 遗留计划任务 `ssh-manager-cache-refresh`/role.json）→ 输入 `DELETE`（输错零改动）→ vault 角色先 export 安全绳（回读校验+抄录口令确认）→ 幂等执行 → exe 保留。

### 2. README.md（2 处）

- TUI 章节开头加一行指向 `docs/concepts.md`（「概念模型图解」），并加首跑角色向导 + `ssh-manager clear` 一句说明。
- 命令清单（"Other commands"）补 `clear`（role teardown — wipes the machine back to first-run）。

### 3. docs/multi-machine.md（2 处）

- 「enroll 一台新机（3 步）」标题下加 🧭 指针行 → concepts.md（并提示空机直接跑 `tui` 走向导）。
- 「##### 可选：系统定时器（给非 Claude 的消费方）」标题加 **—— legacy** 后缀 + 醒目 banner：v0.5.0+ 进程内自动保鲜取代（MCP 消费方无需 OS 定时器）；模板仅服务直接读 cache.bin 的非 MCP 消费方；Windows 遗留计划任务 `ssh-manager-cache-refresh` 由 `ssh-manager clear`（client 角色）顺带删除，Unix 自建 unit 需自行清理（与 `internal/cli/clear_timer_*.go` 的 `legacyTimerName` 实现一致）。

### 4. internal/tui/wizard.go 首屏提示（核对，无需改动）

wizard.go:745 现文 `概念图解：docs/concepts.md（或 --help）` —— 与实际文件名 `docs/concepts.md` 一致，零改动（本次 commit 因此不含 wizard.go）。

## 验证

- `go build ./...` → BUILD_OK。
- `git show --stat ecdd607`：3 files changed（concepts.md 新建 79 行 + README.md + multi-machine.md），无代码文件混入。
- 交叉引用核对：concepts.md 中 `getting-started.md` / `quickstart-multi-machine.md` 链接的文件均存在；曾写的「护栏矩阵见 multi-machine.md」在核对后发现该内容实际在 spec 而非 multi-machine.md，已删（避免死链/错链）。

## 备注 / 边界

- spec §5 提到「三份上手文档前置引用此节」；任务 Do 清单明确圈定为 README（TUI 章）+ multi-machine.md + 向导首屏三处交叉链接，未包含 quickstart×2 / getting-started——按任务范围执行，未扩大。
- sdd/task-4-report.md、task-7-report.md 的工作区改动与 task-5-report.md（未跟踪）是前序任务的遗留，未纳入本次 commit。

## Pre-merge cleanup

Final review pickups applied in one commit (branch `feat/plan19-role-wizard`):

1. **clear 安全网改判 VaultExists（加固）** — `internal/cli/clear.go` `runClear`：安全网判定从「resolved role 是 vault 角色」改为 `roles.VaultExists()`（store.db 实际存在于枚举目标中）。被篡改/手改的 role.json=client + 真实 vault 共存时，导出安全网不再被绕过。`本机角色：%s` 显示行保持 role 驱动。
2. **q/Ctrl+C = 退出（非密钥静态屏）** — `internal/tui/wizardsteps.go` `wizStaticView.Update`：q/Ctrl+C 现返回 `tea.Quit`（经 wizard.go / app.go / clientpage.go 三处 overlay 委托路径的 cmd 传播，覆盖 serveAdminNotice / serveResult / accessCard / mcpConfigScreen 及 upgrade 流复用与 client 缓存变体）。一次性密钥屏（`wizSecretView`，wizTokenScreen）刻意保留任意键推进——数据已持久化，密钥必须显式确认而非被退出反射误关。Esc 在静态屏仍前进（按要求只做 q/Ctrl+C carve-out）。
3. **saveErr 可见性** — `internal/tui/wizard.go` `View()` default 分支（角色流程表单步）现也渲染 `saveErr`（首屏 roles.Save 失败），不再只在防御性占位页可见。
4. **`--mode broker` 下线公告** — README TUI 启动代码块移除 `--mode broker` 行，补一行：v0.7.0 起 `tui --mode broker` 移除（自动判定覆盖该场景；`--mode client` 保留）。与 `roles.ResolveMode` 现状一致（force 仅接受 ""/"client"）。

验证：`gofmt -l` 无输出；`go build ./...`、`go vet ./...`、`go test ./... -count=1` 全绿（15 包 ok / 2 无测试文件）。
