# Plan 16 — 固定路径 + FileKeyProvider（放弃 DPAPI/keyring）— Design Spec

> **取代**：Plan 15（`2026-08-12-plan-15-machine-scope-dpapi-serve-fix-design.md`）整条。Plan 15 的"machine-scope DPAPI 修 FINDING B"路线**未验过即作废**——根因不是 scope 选错，是**整个用户态密钥模型与"单用户可信机器 + 服务自起"的部署形态不匹配**。Plan 14 已在 Plan 15 时标 Superseded；Plan 15 正文不改写（保留审计轨迹），在其顶部加"Superseded by Plan 16"横幅。
> **依据**：2026-08-13 grilling session（`/grill-me`）13 轮问答，用户逐项拍板。两轮代码探测：部署/数据路径面（`serve_install_*.go` / `store.go` / `masterkey_*.go` / `vault.go` key 解析三tier）、client/MCP 持有拓扑（`mcp.go` / `run.go` / `cache.go` / `ssh.go`）。
> **触发事实**：Plan 15 §7.3 NUC10 验收中，B1 迁移成功（sentinel 写入、master.key 重 protect 成 machine-scope、servers ls = 7）后，前台 serve 仍报 `master key present but unreadable: dpapi: CryptUnprotectData failed: Key not valid for use in specified state`。**machine-scope DPAPI 在 sshd session 同样读不出**——证明"换 scope"不是解药，"砍 DPAPI"才是。

---

## 1. 背景：为什么放弃 DPAPI/keyring

### 1.1 NUC10 §7.3 两次撞墙

| | Plan 14 §7.3（user-scope）| Plan 15 §7.3（machine-scope）|
|---|---|---|
| boot 自起 serve 读 master.key | ❌ `Key not valid for use`（跨 logon session）| ❌ **同样 `Key not valid`**（sshd session，B1 迁移成功后仍失败）|
| 根因诊断 | user-scope 绑用户 SID/logon session | **machine-scope 也不可靠**——spike §12 的 roundtrip 测试不覆盖真实 boot/sshd session |

两次撞墙说明：**在"服务自起、跨 session、headless"的部署形态下，用户态密钥保护（DPAPI/keyring）的行为不可预测、不可自动化测试**。Plan 14/15 两个大版本在追一个**自找的问题**。

### 1.2 探测确认的关键事实

- **FileKeyProvider 已存在**（`internal/store/masterkey_file.go:15`）——裸文件 0600、全平台、`resolveMasterKey` 三tier 的最底层 fallback。Q1=2 的"改用文件"不是从零写，是**把它从 fallback 提为唯一生产 provider**。
- **serve 的 key 解析本就是三tier**（`internal/vault/vault.go:63`）：`SSHMGR_MASTERKEY_HEX` env → keychain（Win=DPAPI / Unix=keyring）→ FileKeyProvider。Task Scheduler boot 动作跑 `serve` **不传 key**（`serve_install_windows.go:418`），100% 依赖 keychain tier——这是自找麻烦的根源。
- **所有路径在用户目录**：store.db = `%AppData%/ssh-manager/`（`store.go:56`）、master.key 同、serve.log = `%LocalAppData%`。用户目录路径 + 用户态密钥 = service 注册的权限/session 纠缠。
- **client 侧无对称 vault 问题**：远程在线 client（Claude Code HTTP + bearer）已是零密钥纯转发（`docs/multi-machine.md:56`）；离线 `mcp --cache` 的 DEK 与 master key 是两把独立钥匙（`cache.go:19`）；本地 `mcp`/`ssh` 是自包含 broker，不是"远程 serve 的 client"。**client 侧无病灶，本次不动。**

### 1.3 威胁模型降级（显式）

| | 旧（Plan 1-15）| 新（Plan 16）|
|---|---|---|
| 等级 | L2（agent 永不碰凭据 + 用户态密钥保护）| **L1+**（固定路径裸文件 + ACL）|
| 防"同机另一用户/非登录态进程读凭据" | ✅ DPAPI/keyring 绑用户态 | ❌ admin/root 可读明文 master.key → 解开 vault 全部 SSH 私钥明文 |
| 防"离线拷盘" | ✅ keychain 不落盘 | ❌ master.key 裸文件，拷盘即得 |
| 适用前提 | 多用户/不可信机器 | **单用户、机器可信**（用户明确接受）|

这是**威胁模型级别的降级**，由用户在 grilling Q1/Q2 显式签字接受。边界与残留风险见 §6。

---

## 2. 目标

1. **三平台统一存储路径**：store.db / master.key / serve.log 全部从用户目录挪到程序指定的固定系统路径（§3.1）。
2. **唯一密钥后端 = FileKeyProvider**：删 DPAPI（`dpapi_windows.go` + `masterkey_windows.go`）、删 Unix keyring（`masterkey.go:KeyringKeyProvider` + `keychain_unix.go`）、删 KeyProvider 接口（只剩一个实现，接口无存在价值）、移除 `zalando/go-keyring` 依赖。
3. **master.key = 裸明文 + 硬 ACL**：Windows ACL 只放 `SYSTEM` + `Administrators` + service 账户；Unix `0600` 属主为 root 或 service 账户。
4. **跨平台 service 注册**：内置 `serve install/uninstall/status` 用 `github.com/kardianos/service`（Win=Windows Service、Linux=systemd unit、macOS=launchd plist），替换现有 Windows-only 的 PowerShell/schtasks 实现。文档附 NSSM/systemd 手动包教程给进阶用户。
5. **内置迁移命令**：`ssh-manager migrate-path`（或 `vault relocate`）读旧路径 vault → 写新路径 → 自检 N/N → 删旧，可脚本化、保数据。
6. **真机验收**：NUC10 重新部署 Plan 16 二进制 → migrate-path → serve install → **reboot 后 serve 自起读 master.key 成功**（纯文件读，无 DPAPI）→ 笔记本 exec。这次"boot 读 master.key"从"赌 DPAPI 跨 session"变成"读个文件"，可预测、可测。
7. **发版 v0.3.0**：架构变更 + 威胁模型降级，0.x 续编号。

**非目标**（明确排除）：
- client 侧零改动（角色 A/B/C 见 §1.2，均无病灶）。
- 不保留 DPAPI/keyring 为可选后端（用户 Q6/Q10 拍板：全删，砍长期维护面）。
- 不做 upgrade 子命令（用户先前已 defer）。

---

## 3. 关键决策（grilling 13 轮逐项拍板）

| Q | 决策 | 选项 |
|---|---|---|
| Q1 | 存储与密钥模型 | 2 = 固定路径 + 文件密钥，放弃 DPAPI |
| Q2 | master.key 文件形态 | a = 裸明文 + 硬 ACL |
| Q3 | 三平台路径常量 | Win `C:\ProgramData\ssh-manager\` / Linux `/var/lib/ssh-manager/` / macOS `/usr/local/var/lib/ssh-manager/`，保留 env override |
| Q4 | service 注册模型 | ii → Q5 修正为 iii（见 Q5）|
| Q5 | Q4 vs E 冲突解法 | β = 回到 (iii)：内置 kardianos install 主路径 + 第三方包文档 |
| Q6 | Plan 15 DPAPI 代码 | 移除（删干净，不留可选）|
| Q7 | client 侧 | B → Q11 修正为 a（见 Q11，client 不动）|
| Q8 | NUC10 vault 迁移 | a = 内置 migrate 子命令 |
| Q9 | 发版 | a = v0.3.0 |
| Q10 | Unix keyring | 全删（三平台统一 FileKeyProvider）|
| Q11 | client 落地 | a = client 完全不动（角色 A 已零密钥）|
| Q12 | 威胁模型 docs | b = 最小 docs + L2 升级路径 |
| Q13 | 产出顺序 | a = spec → review → xcheck → writing-plans |

### 3.1 三平台固定路径

| 平台 | 路径 | 理由 |
|---|---|---|
| Windows | `C:\ProgramData\ssh-manager\` | 全机共享，不在用户 profile，Windows Service 账户友好 |
| Linux | `/var/lib/ssh-manager/` | FHS，systemd 服务标准数据目录 |
| macOS | `/usr/local/var/lib/ssh-manager/` | Homebrew 安装路径一致 |

store.db / master.key / serve.log 全进这一棵。`SSHMGR_STORE` / `SSHMGR_FILEKEY_PATH` env override **保留**（测试 + 临时迁移 + 用户自定义用）。

### 3.2 为什么内置 kardianos 而非纯第三方包

用户 Q4 初选 (ii)"纯前台、第三方程序注册"，但 Q5 揭示这与 E「全自动部署测试」正面冲突——第三方包（NSSM/systemd/launchd）的安装配置是平台相关外部副作用，无法被 CI 自动驱动，会重演 NUC10 式返工。kardianos/service 把三平台注册收敛进本程序一条命令，是"可被 CI/脚本自动驱动、且在本程序掌控内的部署路径"。(iii) 同时满足 A（文档附第三方包教程）和 E（内置 install 主路径）。

### 3.3 为什么 client 不动

探测确认 client 侧三角色（§1.2）均无病灶：远程在线 client 已零密钥、离线 client DEK 独立、本地 broker 自包含。本次病灶在 server 侧（DPAPI 跨 session + 用户目录路径），聚焦打病灶，不扩大战场。

---

## 4. 文件影响面

### 4.1 删除

- `internal/store/dpapi_windows.go` — DPAPI syscall 整条
- `internal/store/masterkey_windows.go` — DpapiKeyProvider + sentinel + migrateDpapiScope + postGetMigrator + ACL-for-DPAPI
- `internal/cli/keychain_windows.go` — Windows keychain seam（`var keychain = store.DpapiKeyProvider{}`）
- `internal/cli/keychain_unix.go` — Unix keychain seam（`var keychain = store.envKeyringKeyProvider{}`）
- `internal/cli/migrate_windows.go` — migrateDpapiScope + postGetMigrator 注册（Plan 15 T3）
- `internal/cli/serve_install_windows.go` 中 DPAPI 相关：`verifyMachineScopeForBoot`、sentinel precheck
- `go.mod` / `go.sum`：移除 `github.com/zalando/go-keyring` 及间接依赖（`danieljoos/wincred`、`godbus/dbus/v5`）

### 4.2 改造

- `internal/store/masterkey.go` — 删 `KeyProvider` 接口、`KeyringKeyProvider`、`MemKeyProvider`；`FileKeyProvider` 成为唯一 provider，相关函数直接顶层化（或保留类型但无接口）。测试改为直接传 `[]byte` master key（不再 mock provider）。`DeriveFromPassphrase` 保留（export/import 仍用）。
- `internal/store/masterkey_file.go` — `FileKeyProvider.path()` 从 `os.UserConfigDir()` 改为 §3.1 固定路径；裸文件 0600 + ACL 硬化（见 §5.2）。
- `internal/store/store.go:56` — `DefaultStorePath()` 从 `UserConfigDir` 改为 §3.1 固定路径。
- `internal/cli/serve_install_windows.go` → 重写为 `internal/cli/serve_install.go`（无 build tag）+ kardianos 实现；`serve_install_other.go` stub 删除（kardianos 跨平台，不再需要 stub）。
- `internal/cli/serve.go` — boot 动作跑 `serve` 时 master key 经 FileKeyProvider 读固定路径文件（不再依赖 keychain tier）。
- `internal/vault/vault.go:63` — `resolveMasterKey` 简化：三tier → 两tier（`SSHMGR_MASTERKEY_HEX` env → FileKeyProvider），删 keychain tier。
- `docs/getting-started.md` — 路径表、密钥形态、service 安装说明全部重写。
- `docs/multi-machine.md` — client/server 角色描述保留（已正确），serve 安装部分改 kardianos。

### 4.3 新增

- `internal/cli/migrate_path.go` — `migrate-path`（或 `vault relocate`）子命令：读旧路径（env 指定或探测用户目录）→ 写新路径 → 自检 N/N → 删旧。
- `internal/cli/serve_service.go` — kardianos service.Config + install/uninstall/status 实现（跨平台）。
- `docs/threat-model.md` — 威胁模型降级记录 + L2 升级路径（见 §6）。
- `docs/getting-started.md` 新增"第三方服务包"小节（NSSM/systemd/launchd 手动包教程）。

### 4.4 Plan 15 标记

- `docs/superpowers/specs/2026-08-12-plan-15-machine-scope-dpapi-serve-fix-design.md` 顶部加 `> **Superseded by Plan 16**（2026-08-13）：machine-scope DPAPI 路线未验过即作废，根因是用户态密钥模型与部署形态不匹配。见 Plan 16 §1。`

---

## 5. 关键契约

### 5.1 路径解析契约

`DefaultStorePath()` / `FileKeyProvider.path()` 必须返回 §3.1 平台固定路径，**除非** env override 设置：
- `SSHMGR_STORE` → store.db 路径
- `SSHMGR_FILEKEY_PATH` → master.key 路径

env override 仅供测试 + 迁移 + 用户自定义，文档需明示"生产部署不建议改"。

### 5.2 ACL / 文件权限契约

**Windows**（`C:\ProgramData\ssh-manager\master.key`）：
- 目录 ACL：`SYSTEM` + `Administrators` 完全控制；service 账户读/写；**移除** `Users` / `Everyone` 继承。
- 文件 ACL 继承目录，或显式设同上。
- 实现：Go 调 PowerShell `icacls` 或纯 Go `golang.org/x/sys/windows` 安全描述符 API（实现时定）。

**Linux/macOS**（`/var/lib/ssh-manager/master.key`）：
- 文件 `0600`，属主 = service 账户（systemd 的 `User=` / launchd 的 `UserName`）。
- 目录 `0700` 同属主。
- 创建时 `os.OpenFile(..., 0600)` + `os.Chown`（如以 root 启动）。

### 5.3 migrate-path 契约

- 读旧路径：优先 env `SSHMGR_STORE` 指向旧位置；否则探测 `UserConfigDir/ssh-manager/store.db` 是否存在。
- 自检：迁移后 `servers ls` 数量 == 迁移前（NUC10 = 7），每条 `AuthForServer` 可解。
- 保数据：旧 master.key 用旧后端（DPAPI/keyring/文件）解出 master key → 用 FileKeyProvider 写新路径。**migrate-path 保持单一职责，不内部调 export/import**：若探测到旧后端已不可解（如 NUC10 当前 machine-scope DPAPI 在 sshd session 读不出），**migrate-path 报错并提示用户手动跑 `export` + `import`**（import 到新固定路径）。
- 删旧：自检通过后才删旧路径文件（或 `--keep-old` 保留）。
- 幂等：重复跑不损坏数据。

### 5.4 kardianos service 契约

- `serve install`：注册 service（Win=Service、Linux=systemd、macOS=launchd），DisplayName `ssh-manager-serve`，启动 `ssh-manager.exe serve --addr <addr>`。
- `serve uninstall`：停止 + 注销 + 清理（不删 vault 数据）。
- `serve status`：四信号——service 状态（Running/Stopped）、进程存在、HTTP 响应、vault ok（读 master.key 成功）。本地化问题用 kardianos 的 State 枚举规避（不再扫 PS 本地化文本）。
- boot 自起：service 配置 `Automatic`（Win）/ `enable`（systemd）/ `RunAtLoad`（launchd）。
- RestartOnFailure：kardianos 各平台原生支持（Win service recovery、systemd `Restart=on-failure`、launchd `KeepAlive`）。

### 5.5 测试契约

- **serve install 集成测试进 CI**：三平台（windows-latest / ubuntu-latest / macos-latest），gated `SSHMGR_SERVE_INSTALL=1`，真跑 install → status → uninstall。这次三平台都用 kardianos，不依赖 hosted runner 的 PowerShell 账户模型（Plan 15 T8 撞过的坑）。
- migrate-path 单测：mock 旧路径 vault，验证 N/N 自检 + 删旧 + 幂等。
- 路径解析单测：env override 优先、默认落固定路径。
- ACL 单测（Windows）：验证 master.key 文件 DACL 不含 `Users`/`Everyone`。

---

## 6. 威胁模型（docs/threat-model.md）

### 6.1 当前等级：L1+

- **保护**：固定路径 + 文件 ACL（防同机非特权进程意外读，不防 admin/root）。
- **不保护**：admin/root 直接读 master.key 明文 → 解开 vault 全部 SSH 私钥；离线拷盘。

### 6.2 适用前提（必须成立）

- 部署机器**单用户**使用（或所有用户互信）。
- 机器**物理 + 管理面可信**（admin/root 不被恶意方获取）。
- 不需要防"同机另一低权用户/服务"读凭据。

### 6.3 残留风险

- R1：admin/root compromise → 全 vault 明文。
- R2：物理/离线盘访问 → master.key + store.db 可被拷走离线解。
- R3：service 账户 compromise → 同 R1（service 账户能读 master.key）。

### 6.4 升级路径（L1+ → L2，未来若需）

若将来部署到多用户/不可信机器，升级选项：
- **U1**：重新引入 keyring（Unix）/ DPAPI（Win）作为可选 KeyProvider——**需先解决跨 session 服务自起问题**（Plan 14/15 的坑），可能需 Windows Service + LoadUserProfile 或 passphrase-at-boot。
- **U2**：passphrase 派生 DEK（`store.DeriveFromPassphrase` 已存在）——serve 启动需输 passphrase（与 boot 自起冲突，除非 passphrase 落盘 → 退化回 L1+）。
- **U3**：外部 KMS / HSM / Vault Transit——架构级改动。

**结论**：L2 在"服务自起 + headless"形态下本质困难（passphrase 要人输、用户态密钥跨 session 不可靠）。L1+ 是该形态下的合理工程选择。

---

## 7. 验收

### 7.1 单测 + CI 集成

- 路径解析、migrate-path、ACL、kardianos install 四块单测全绿。
- `SSHMGR_SERVE_INSTALL=1` CI 三平台集成测试全绿。

### 7.2 NUC10 真机验收（核心）

```
Phase 1 (SSH)    部署 Plan 16 二进制（备份旧 exe）
Phase 2 (SSH)    migrate-path: 用户目录 vault → C:\ProgramData\ssh-manager\，自检 7/7
Phase 3 (RDP)    serve install（kardianos），无需 --task-user（单用户机）
Phase 4 (reboot) ★ reboot 后 serve 自起读固定路径 master.key 成功（纯文件读）
Phase 5 (笔记本)  agent exec_command 在 1660Super01 成功
```

**通过标准**：Phase 4 reboot 后 `serve status` = vault ok + serve running + 7878 listening。**这次没有 DPAPI 跨 session 赌博**——读文件就是读文件。

### 7.3 失败处理

- 若 Phase 4 仍失败（极不可能，纯文件读）：走 finding，记录根因，**不发 v0.3.0**。
- 数据零丢失：migrate-path 保数据 + 自检，安全绳 `.sme` 双份在手。

---

## 8. 开放问题（spec 阶段不阻塞，实现时定）

- O1：Windows ACL 实现选 PowerShell `icacls` vs 纯 Go `x/sys/windows`——实现时权衡（icacls 简单但外部进程，纯 Go 无依赖但代码量大）。
- O2：migrate-path 是否支持跨后端迁移（旧 DPAPI → 新文件）——若旧 DPAPI 已不可解，走 export/import fallback（§5.3）。
- O3：kardianos service 账户选型——Windows 用 LocalSystem 还是专用 service 账户（单用户机可 LocalSystem，多用户机需专用账户）。
- O4：serve.log 路径是否也进固定路径（现状 `%LocalAppData%`，服务账户可能无此目录）——**倾向是**，统一进 `C:\ProgramData\ssh-manager\serve.log`。

---

## 9. 作废清单

- Plan 14（`2026-08-12-plan-14-windows-prod-deploy-design.md`）— 已在 Plan 15 标 Superseded，本 plan 维持。
- Plan 15（`2026-08-12-plan-15-machine-scope-dpapi-serve-fix-design.md`）— **本 plan 标 Superseded**。其 9 个 task 的代码（T1-T9）：
  - T1（DPAPI flag）、T2-T4（machine-scope + sentinel + migrate）、T8 DPAPI precheck：**删**
  - T4-T7（serve install 对象 API 壳、heartbeat、status 四信号）：**留概念，实现换 kardianos**
  - T9（迁移文档）：**重写**为 migrate-path
- NUC10 当前状态（Plan 15 B1 迁移后的 machine-scope master.key）：**migrate-path 会重写为新 FileKeyProvider 格式**，旧 machine-scope blob 作废。
