# Plan 16 — 固定路径 + FileKeyProvider（放弃 DPAPI/keyring）— Design Spec (v2)

> **v2 依据**：2026-08-13 xcheck 异构评审（4 家：codex/kimi/opencode/pi，全 SUGGEST_CHANGES，0 DISAGREE）。核心方向四家认可，但"删干净"执行面低估 blast radius——cache DEK + eval + 接口注入面是事实性遗漏（按 v1 §4 照做 `go build` 会断）。主会话已逐条读代码核实（`cache_dek_*.go` / `eval/broker.go` / grep `store.KeyProvider` 8 处）。v2 采纳全部经核实成立的修订；opencode #2"DPAPI 根因未定位"归第二类（可实验验证、非 blocker、不否决方向）。汇总见 `.xcheck/20260813-153149/SUMMARY.md`。
> **取代**：Plan 15（`2026-08-12-plan-15-machine-scope-dpapi-serve-fix-design.md`）整条。Plan 15 的"machine-scope DPAPI 修 FINDING B"路线**未验过即作废**——根因不是 scope 选错，是**整个用户态密钥模型与"单用户可信机器 + 服务自起"的部署形态不匹配**。Plan 14 已在 Plan 15 时标 Superseded；Plan 15 正文不改写（保留审计轨迹），在其顶部加"Superseded by Plan 16"横幅。
> **依据**：2026-08-13 grilling session（`/grill-me`）13 轮问答，用户逐项拍板。两轮代码探测：部署/数据路径面（`serve_install_*.go` / `store.go` / `masterkey_*.go` / `vault.go` key 解析三tier）、client/MCP 持有拓扑（`mcp.go` / `run.go` / `cache.go` / `ssh.go`）。
> **触发事实**：Plan 15 §7.3 NUC10 验收中，B1 迁移成功（sentinel 写入、master.key 重 protect 成 machine-scope、servers ls = 7）后，前台 serve 仍报 `master key present but unreadable: dpapi: CryptUnprotectData failed: Key not valid for use in specified state`。**machine-scope DPAPI 在 sshd session 同样读不出**——证明"换 scope"不是解药，"砍 DPAPI"才是。

---

## 1. 背景：为什么放弃 DPAPI/keyring

### 1.1 NUC10 §7.3 两次撞墙

| | Plan 14 §7.3（user-scope）| Plan 15 §7.3（machine-scope）|
|---|---|---|
| boot 自起 serve 读 master.key | ❌ `Key not valid for use`（跨 logon session）| ❌ **同样 `Key not valid`**（serve/Service session，B1 迁移成功后仍失败）|
| 根因诊断 | user-scope 绑用户 SID/logon session | **machine-scope 也不可靠**——spike §12 的 roundtrip 测试不覆盖真实 boot/Service session |

两次撞墙说明：**在"服务自起、跨 session、headless"的部署形态下，用户态密钥保护（DPAPI/keyring）的行为不可预测、不可自动化测试**。Plan 14/15 两个大版本在追一个**自找的问题**。

> **F3 实测修正（2026-08-13 §7.3 验收期间）**：本节早期草稿把 Plan 15 §7.3 的失败记为"sshd session 读不出"。但 §7.3 验收实测发现：在 sshd session 跑 `servers ls`（旧 Plan 15 exe + machine-scope DPAPI blob）**成功**（7/7 列出）——sshd 其实能解 machine-scope DPAPI。真正读不出的是 **serve 进程**（Task Scheduler /Run 起的 Password-logon session，或 kardianos Windows Service 的 LocalSystem session）。这两类 session 的 DPAPI 上下文与 sshd 不同。方向不变（DPAPI 在服务自起场景不可预测），但诊断精确化为"serve/Service session"，非"sshd session"。

### 1.2 探测确认的关键事实

- **FileKeyProvider 已存在**（`internal/store/masterkey_file.go:15`）——裸文件 0600、全平台、`resolveMasterKey` 三tier 的最底层 fallback。Q1=2 的"改用文件"不是从零写，是**把它从 fallback 提为唯一生产 provider**。
- **serve 的 key 解析本就是三tier**（`internal/vault/vault.go:63`）：`SSHMGR_MASTERKEY_HEX` env → keychain（Win=DPAPI / Unix=keyring）→ FileKeyProvider。Task Scheduler boot 动作跑 `serve` **不传 key**（`serve_install_windows.go:418`），100% 依赖 keychain tier——这是自找麻烦的根源。
- **所有路径在用户目录**：store.db = `%AppData%/ssh-manager/`（`store.go:56`）、master.key 同、serve.log = `%LocalAppData%`。用户目录路径 + 用户态密钥 = service 注册的权限/session 纠缠。
- **client 侧角色划分**（v2 修正，xcheck 共识 A）：远程在线 client（Claude Code HTTP + bearer）已是零密钥纯转发（`docs/multi-machine.md:56`）；本地 `mcp`/`ssh` 是自包含 broker。**但**离线 cache（`mcp --cache` / `cache pull/push`）的 DEK provider **正是要删的类型**——`cache_dek_windows.go:31` 用 `store.DpapiKeyProvider`、`cache_dek_unix.go:32` 用 `store.KeyringKeyProvider`；eval 框架 `eval/broker.go:143,454` 直接构造 `KeyringKeyProvider`。所以"client 无病灶"只对**远程/本地 broker 路径**成立，**cache DEK + eval 必须一并迁 FileKeyProvider**（见 §2 目标 2/8、§4.2）。v1 "client 零改动"非目标已收回此点。

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
2. **唯一生产密钥后端 = FileKeyProvider**：删 master-key 的生产实现 DPAPI（`dpapi_windows.go` + `masterkey_windows.go` 的 `DpapiKeyProvider`）、删 Unix keyring 生产实现（`masterkey.go:KeyringKeyProvider` + `keychain_unix.go`）。**保留 `KeyProvider` 接口**（xcheck 共识 D：接口是 8 处非测试代码的注入 seam + 测试 fake 依赖，删接口会触发大规模重构，不值）+ **保留 `MemKeyProvider`**（测试 fake）。移除 `zalando/go-keyring` 依赖（仅当 eval 也迁文件后，见目标 8）。
3. **master.key = 裸明文 + 硬 ACL**：Windows ACL 只放 `SYSTEM` + `Administrators` + service 账户，**显式移除 `Users` + `Authenticated Users`**（xcheck 共识 E：ProgramData 默认 DACL 含后者，只删前者不够）；Unix `0600` 属主为 root 或 service 账户。
4. **跨平台 service 注册**：内置 `serve install/uninstall/status` 用 `github.com/kardianos/service`（Win=Windows Service、Linux=systemd unit、macOS=launchd plist），替换现有 Windows-only 的 PowerShell/schtasks 实现。文档附 NSSM/systemd 手动包教程给进阶用户。
5. **内置迁移命令**：`ssh-manager migrate-path`（或 `vault relocate`）读旧路径 vault → 写新路径 → 自检 N/N → 删旧，可脚本化、保数据。
6. **真机验收**：NUC10 重新部署 Plan 16 二进制 → migrate-path → serve install → **reboot 后 serve 自起读 master.key 成功**（纯文件读，无 DPAPI）→ 笔记本 exec。这次"boot 读 master.key"从"赌 DPAPI 跨 session"变成"读个文件"，可预测、可测。
7. **发版 v0.3.0**：架构变更 + 威胁模型降级，0.x 续编号。
8. **cache DEK + eval 一并迁 FileKeyProvider**（xcheck 共识 A+G / 用户选 α）：离线 cache 的 DEK provider（`cache_dek_*.go`）和 eval 框架（`eval/broker*.go`）从 DPAPI/keyring 迁到 FileKeyProvider，彻底移除 `go-keyring` 依赖。顺带修 cache DEK 的跨 session 隐患（`cache_dek_windows.go` 注释自述 served broker / Task Scheduler 调用 `cache pull` 时也读 DPAPI DEK——同一种不可靠 custody）。

**非目标**（明确排除）：
- 远程在线 client（角色 A，Claude Code HTTP + bearer）+ 本地 broker（角色 C，`mcp`/`ssh`）**不动**——前者已零密钥，后者自包含。**仅 cache DEK（角色 B）+ eval 迁文件**（v1 "client 零改动"已据此收回）。
- 不保留 DPAPI/keyring 为 master-key 的可选后端（用户 Q6/Q10 拍板：生产路径全删）。
- 不做 upgrade 子命令（用户先前已 defer）。

---

## 3. 关键决策（grilling 13 轮逐项拍板）

| Q | 决策 | 选项 |
|---|---|---|
| Q1 | 存储与密钥模型 | 2 = 固定路径 + 文件密钥，放弃 DPAPI |
| Q2 | master.key 文件形态 | a = 裸明文 + 硬 ACL |
| Q3 | 三平台路径常量 | Win `C:\ProgramData\ssh-manager\` / Linux `/var/lib/ssh-manager/` / macOS `/var/lib/ssh-manager/`（v2 改，xcheck 共识 F），保留 env override |
| Q4 | service 注册模型 | ii → Q5 修正为 iii（见 Q5）|
| Q5 | Q4 vs E 冲突解法 | β = 回到 (iii)：内置 kardianos install 主路径 + 第三方包文档 |
| Q6 | Plan 15 DPAPI 代码 | 移除 master-key 生产实现（v2 改：保留 KeyProvider 接口 + MemKeyProvider，xcheck 共识 D）|
| Q7 | client 侧 | B → Q11 修正为 a → v2 再修正：cache DEK + eval 收回迁文件（xcheck 共识 A+G，α）|
| Q8 | NUC10 vault 迁移 | a = 内置 migrate 子命令 |
| Q9 | 发版 | a = v0.3.0 |
| Q10 | Unix keyring | 删 master-key 生产实现（v2：cache DEK/eval 一并迁文件，彻底移除 go-keyring）|
| Q11 | client 落地 | a = 远程/本地 broker 不动 → v2：仅 cache DEK + eval 迁文件 |
| Q12 | 威胁模型 docs | b = 最小 docs + L2 升级路径 |
| Q13 | 产出顺序 | a = spec → review → xcheck → writing-plans |
| **v2-xcheck** | cache DEK + eval 依赖被删 provider | α = 一并迁 FileKeyProvider（用户拍板）|
| **v2-xcheck** | KeyProvider 接口 | 保留（seam + 测试 fake 依赖，删接口重构面过大）|

### 3.1 三平台固定路径

| 平台 | 路径 | 理由 |
|---|---|---|
| Windows | `C:\ProgramData\ssh-manager\` | 全机共享，不在用户 profile，Windows Service 账户友好 |
| Linux | `/var/lib/ssh-manager/` | FHS，systemd 服务标准数据目录 |
| macOS | `/var/lib/ssh-manager/`（v2 改） | 与 Linux 一致、不绑 Homebrew（xcheck 共识 F：`/usr/local` 仅 Intel，Apple Silicon 是 `/opt/homebrew`，"与 brew 一致"理由不成立） |

store.db / master.key / serve.log / cache-dek.key 全进这一棵。`SSHMGR_STORE` / `SSHMGR_FILEKEY_PATH` env override（v2 精确：`store.Open` 层已支持但 `DefaultStorePath` 未读，本次统一补上"默认路径读固定路径 + env 可覆盖"，kimi #8）**保留**（测试 + 临时迁移 + 用户自定义用）。

### 3.2 为什么内置 kardianos 而非纯第三方包

用户 Q4 初选 (ii)"纯前台、第三方程序注册"，但 Q5 揭示这与 E「全自动部署测试」正面冲突——第三方包（NSSM/systemd/launchd）的安装配置是平台相关外部副作用，无法被 CI 自动驱动，会重演 NUC10 式返工。kardianos/service 把三平台注册收敛进本程序一条命令，是"可被 CI/脚本自动驱动、且在本程序掌控内的部署路径"。(iii) 同时满足 A（文档附第三方包教程）和 E（内置 install 主路径）。

### 3.3 client 侧处理（v2 修正）

- **远程在线 client（角色 A，Claude Code HTTP + bearer）**：不动，已零密钥。
- **本地 broker（角色 C，`mcp`/`ssh`）**：不动，自包含。
- **离线 cache DEK（角色 B，`cache_dek_*.go`）+ eval**：**一并迁 FileKeyProvider**（xcheck 共识 A+G，用户选 α）。v1 "client 零改动"已据此收回——不是方向变更，是 v1 影响面分析漏了 cache DEK/eval 依赖被删 provider 这个事实。

---

## 4. 文件影响面

### 4.1 删除（master-key 生产实现 + DPAPI 衍生物）

- `internal/store/dpapi_windows.go` — DPAPI syscall 整条
- `internal/store/masterkey_windows.go` — DpapiKeyProvider + sentinel + migrateDpapiScope + postGetMigrator + ACL-for-DPAPI
- `internal/cli/keychain_windows.go` — Windows keychain seam（`var keychain = store.DpapiKeyProvider{}`）
- `internal/cli/keychain_unix.go` — Unix keychain seam（`var keychain = store.envKeyringKeyProvider{}`）
- `internal/cli/migrate_windows.go` — migrateDpapiScope + postGetMigrator 注册（Plan 15 T3）
- `internal/store/masterkey.go` 中 `KeyringKeyProvider`（master-key 的 Unix 生产实现；**保留 `KeyProvider` 接口 + `MemKeyProvider` + `FileKeyProvider` + `DeriveFromPassphrase`**，xcheck 共识 D）
- `internal/cli/serve_install_windows.go` 中 DPAPI 相关：`verifyMachineScopeForBoot`、sentinel precheck
- `internal/cli/unlock.go` 中 Plan 15 migration plumbing：`firstRunMigrator`/`postGetMigrator` hook 变量、`firstRunOutcome`/`migrateOutcome` 枚举、`migrateSource` 结构体（codex P4：删 migrate_windows.go 后变 dead code，须一并清理）

### 4.2 改造（含 cache DEK + eval 迁文件，xcheck 共识 A+G+D）

**密钥/存储核心**：
- `internal/store/masterkey_file.go` — `FileKeyProvider.path()` 从 `os.UserConfigDir()` 改为 §3.1 固定路径；裸文件 0600 + ACL 硬化（见 §5.2）。
- `internal/store/store.go:56` — `DefaultStorePath()` 从 `UserConfigDir` 改为 §3.1 固定路径；**补 `SSHMGR_STORE` env 读取**（kimi #8：现状 `store.Open` 支持 env 但 `DefaultStorePath` 不读，本次统一）。
- `internal/vault/vault.go:63` — `resolveMasterKey` 简化：三tier → 两tier（`SSHMGR_MASTERKEY_HEX` env → FileKeyProvider），删 keychain tier。**`OpenStore(kp store.KeyProvider)` 签名不变**（接口保留）。
- `internal/cli/keychain_windows.go` / `keychain_unix.go` 删后，`cli` 包的 `var keychain store.KeyProvider` seam 改指向 `FileKeyProvider`（或 seam 变量移除，调用点直接 `store.FileKeyProvider{}`）。

**cache DEK（角色 B）迁文件**（共识 A，α）：
- `internal/cli/cache_dek_windows.go` — `var dekProvider` 从 `&store.DpapiKeyProvider{Path: dpapiCacheDekPath()}` 改 `&store.FileKeyProvider{Path: <固定路径>/cache-dek.key}`。
- `internal/cli/cache_dek_unix.go` — `var dekProvider` 从 `&store.KeyringKeyProvider{Service:..., User:"cache-dek"}` 改 `&store.FileKeyProvider{Path: <固定路径>/cache-dek.key}`。
- `SSHMGR_KEYRING_SERVICE` env 在 cache DEK 路径不再使用（eval 隔离改用 §4.3 的 `SSHMGR_FILEKEY_PATH` 覆盖，见 eval）。
- 评估 `cache.bin` 安全性变化：DEK 迁文件后，cache.bin + cache-dek.key 同盘 → 离线拷盘可解 cache。这与 master.key 同等级（L1+ 已接受），cache 是只读快照（非完整凭据），可接受。docs 注明。

**eval 框架迁文件**（共识 G，α）：
- `internal/eval/broker.go:143,454` — `evalKP := store.KeyringKeyProvider{Service: evalKeyringService}` 改 `store.FileKeyProvider{Path: <eval 临时路径>/master.key}`（eval 用临时目录隔离，不再用 keychain service 名）。
- `internal/eval/broker_test.go:63` — 同改。
- `broker.go:440+` spawned broker 子进程经 `SSHMGR_KEYRING_SERVICE` 读 → 改经 `SSHMGR_FILEKEY_PATH` env 指向 eval 临时 master.key（保留隔离契约 Plan 12 CF1，仅换介质）。

**serve / service**：
- `internal/cli/serve_install_windows.go` → 重写为 `internal/cli/serve_install.go`（无 build tag）+ kardianos 实现；`serve_install_other.go` stub 删除（kardianos 跨平台）。
- `internal/cli/serve.go` — boot 动作跑 `serve` 时 master key 经 FileKeyProvider 读固定路径文件（不再依赖 keychain tier）。

**docs**：
- `docs/getting-started.md` — 路径表、密钥形态、service 安装说明全部重写。
- `docs/multi-machine.md` — 角色描述保留（已正确），serve 安装改 kardianos，cache DEK 介质改文件。
- `docs/backup-restore.md` — 若引用 keychain 行为需更新（kimi #5，实现时核查）。

### 4.3 新增

- `internal/cli/migrate_path.go` — `migrate-path`（或 `vault relocate`）子命令：读旧路径 → 写新路径 → 自检 N/N → 删旧。**职责收窄**（xcheck 共识 B+C+kimi#2）：仅搬**文件型** vault（旧路径已是 FileKeyProvider 或可读的 keychain/DPAPI session）；若旧后端在当前 session 不可解（如 NUC10 sshd 下 machine-scope DPAPI），**报错并提示用户在 RDP/交互 session 跑 `export` + `import` 到新固定路径**（migrate-path 不内部调 export/import，不保留 DPAPI/keyring 读代码——与 Q6/Q10"删干净"一致）。
- `internal/cli/serve_service.go` — kardianos service.Config + install/uninstall/status 实现（跨平台）。
- `docs/threat-model.md` — 威胁模型降级记录 + L2 升级路径（见 §6）。
- `docs/getting-started.md` 新增"第三方服务包"小节（NSSM/systemd/launchd 手动包教程）。
- `go.mod` / `go.sum`：**新增** `github.com/kardianos/service`（codex P7：v1 §4 只提移除 go-keyring 漏了新增 kardianos）；移除 `github.com/zalando/go-keyring` 及间接依赖（`danieljoos/wincred`、`godbus/dbus/v5`）——仅在 eval 迁文件完成后。

### 4.4 Plan 15 标记

- `docs/superpowers/specs/2026-08-12-plan-15-machine-scope-dpapi-serve-fix-design.md` 顶部加 `> **Superseded by Plan 16**（2026-08-13）：machine-scope DPAPI 路线未验过即作废，根因是用户态密钥模型与部署形态不匹配。见 Plan 16 §1。`

---

## 5. 关键契约

### 5.1 路径解析契约

`DefaultStorePath()` / `FileKeyProvider.path()` 必须返回 §3.1 平台固定路径，**除非** env override 设置：
- `SSHMGR_STORE` → store.db 路径
- `SSHMGR_FILEKEY_PATH` → master.key 路径

env override 仅供测试 + 迁移 + 用户自定义，文档需明示"生产部署不建议改"。

### 5.2 ACL / 文件权限契约（v2 钉死，xcheck 共识 E）

ACL 是 L1+ 威胁模型（§6）**唯一的保护层**——不能 defer 到实现阶段（codex P5）。本次 spec 钉死：

**Windows**（`C:\ProgramData\ssh-manager\`）：
- 实现方式：**纯 Go `golang.org/x/sys/windows` 安全描述符 API**（不调 `icacls` 外部进程，避免进程依赖 + 失败静默）。
- 目录 + 文件 DACL：`SYSTEM` + `Administrators` 完全控制；service 账户读/写；**显式禁用继承（`SE_DACL_PROTECTED`）+ 移除 `BUILTIN\Users` 和 `Authenticated Users` 和 `Everyone`**（xcheck pi #3 / codex P5：`C:\ProgramData\` 默认 DACL 含 `Authenticated Users` modify，只删 Users/Everyone 仍可被任意同机已登录用户读 → 打穿 §6.1）。
- **`master.key` + `store.db` + `cache-dek.key` 同 ACL**（codex P6：store.db 含加密凭据 blob，继承宽 ACL 则 Users 可读密文；虽需 master.key 才解，但"非特权进程不可读 store.db"是 §6.1 隐含承诺）。
- 非特权首启失败提示：若进程无权设 ACL（非 admin/未提权），`serve install` / 首次写 key 时**显式报错**"需 admin/特权设置 ACL"，不静默降级（kimi #4/pi #5）。

**Linux/macOS**（`/var/lib/ssh-manager/`）：
- 目录 `0700`，文件 `0600`，属主 = service 账户。
- bootstrap 顺序（pi #5）：`serve install`（以 root 跑）负责**建目录 + chown 给 service 账户**；service 账户随后跑 `serve` 只读写不 chown。前台非特权首跑 `serve`/`migrate-path` 建不了 `/var/lib` 路径 → 报错提示先 `serve install`。
- **双访问模式**（kimi #4）：同一台机上"交互用户 CLI + service 进程"都要读 master.key。Linux 不像 Windows 靠"单用户即 admin"。处理：**专用组（如 `ssh-manager`）+ 目录 `0750`/文件 `0640`，service 账户 + 交互用户同属该组**；或文档要求交互 CLI 经 `sudo`/`sg`。**选型在 writing-plans 阶段定，但 spec 明确这是必答题**（不能漏）。

### 5.3 migrate-path 契约（v2 收窄 + session 约束，xcheck 共识 B+C）

- 读旧路径：优先 env `SSHMGR_STORE` 指向旧位置；否则探测 `UserConfigDir/ssh-manager/store.db` 是否存在。
- **职责收窄**：migrate-path 只搬**文件型** vault。**不保留 DPAPI/keyring 读代码**（与 Q6/Q10 删干净一致）。
- 自检：迁移后 `servers ls` 数量 == 迁移前（NUC10 = 7），每条 `AuthForServer` 可解。
- **旧后端不可解的处理**（xcheck kimi#2/codex P8/pi#2）：若旧 master.key 在当前 session 读不出（NUC10 machine-scope DPAPI 在 sshd session 的现状），migrate-path **报错并提示**："旧密钥后端在当前 session 不可读，请在 **RDP/交互登录 session** 跑 `export` 导出 `.sme`，再用 `import --passphrase-file` 导入新固定路径"。**export/import 同样受 session 约束**（pi #2：`export` 走 `vault.OpenStore`→DPAPI，sshd session 也解不开）——文档必须明示 export/import 在旧 key 可解 session 执行。
- 删旧：自检通过后才删旧路径文件（或 `--keep-old` 保留）。
- 幂等：重复跑不损坏数据。

### 5.4 kardianos service 契约

- `serve install`：注册 service（Win=Service、Linux=systemd、macOS=launchd），DisplayName `ssh-manager-serve`，启动 `ssh-manager.exe serve --addr <addr>`。
- `serve uninstall`：停止 + 注销 + 清理（不删 vault 数据）。
- `serve status`：四信号——service 状态（Running/Stopped）、进程存在、HTTP 响应、vault ok（读 master.key 成功）。本地化问题用 kardianos 的 State 枚举规避（不再扫 PS 本地化文本）。
- boot 自起：service 配置 `Automatic`（Win）/ `enable`（systemd）/ `RunAtLoad`（launchd）。
- RestartOnFailure：kardianos 各平台原生支持（Win service recovery、systemd `Restart=on-failure`、launchd `KeepAlive`）。

### 5.5 测试契约（v2，xcheck 共识 E + kimi #8）

- **serve install 集成测试进 CI**：三平台（windows-latest / ubuntu-latest / macos-latest），gated `SSHMGR_SERVE_INSTALL=1`，真跑 install → status → uninstall。这次三平台都用 kardianos，不依赖 hosted runner 的 PowerShell 账户模型（Plan 15 T8 撞过的坑）。**约束标注**（kimi #8）：macOS runner 写 LaunchDaemons 需 sudo、ubuntu runner 容器化 job 可能无 systemd——CI 脚本需处理（`sudo`/systemd 可用性探测）。
- migrate-path 单测：mock 旧路径 vault，验证 N/N 自检 + 删旧 + 幂等 + 旧后端不可解时报错提示正确。
- 路径解析单测：env override 优先、默认落固定路径（含 §4.2 补的 `DefaultStorePath` 读 `SSHMGR_STORE`）。
- **ACL 单测（Windows）**（codex P5/pi #3）：验证 `master.key` + `store.db` + `cache-dek.key` **三者**的文件 DACL **及目录 DACL** 不含 `BUILTIN\Users` / `Authenticated Users` / `Everyone`，且 `SE_DACL_PROTECTED`（继承禁用）。
- **cache DEK 迁文件单测**（共识 A）：`cache_dek_*.go` 的 `dekProvider` 返回 `FileKeyProvider` 且路径落固定路径；`cache pull` → `mcp --cache` roundtrip 在新介质下可解。
- **eval 迁文件单测**（共识 G）：`eval/broker_test.go` 用 FileKeyProvider + `SSHMGR_FILEKEY_PATH` 隔离，spawned 子进程经 env 读到 eval 专用 master.key（Plan 12 CF1 隔离契约保持）。

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

### 7.2 NUC10 真机验收（核心，v2 修正 Phase 2，xcheck 共识 B+C）

```
Phase 1 (SSH)    部署 Plan 16 二进制（备份旧 exe）
Phase 2 (RDP)    ★ 改为 RDP/交互 session（不是 SSH！）
                  NUC10 当前 master.key 是 Plan 15 machine-scope DPAPI blob，
                  sshd session 读不出（§1.1 触发事实）。两条路二选一：
                  (a) RDP 跑 migrate-path —— 若 RDP session 能解 machine-scope DPAPI 则直接迁
                  (b) RDP 跑 export → import 到 C:\ProgramData\ssh-manager\（更稳）
                  自检 7/7
Phase 3 (RDP)    serve install（kardianos），无需 --task-user（单用户机）
Phase 4 (reboot) ★ reboot 后 serve 自起读固定路径 master.key 成功（纯文件读）
Phase 5 (笔记本)  agent exec_command 在 1660Super01 成功
```

**通过标准**：Phase 4 reboot 后 `serve status` = vault ok + serve running + 7878 listening。**这次没有 DPAPI 跨 session 赌博**——读文件就是读文件。

**v1 错误**（已修）：v1 Phase 2 写"SSH migrate-path"，但 NUC10 触发事实就是 sshd session 读不出 DPAPI——migrate-path 经 SSH 必败。v2 改 RDP + export/import 分支。

### 7.3 失败处理（v2 精确，xcheck pi#2）

- 若 Phase 4 仍失败（极不可能，纯文件读）：走 finding，记录根因，**不发 v0.3.0**。
- **数据零丢失的前提**（pi #2）：存在一个旧 key **可解**的 session（RDP/交互）。NUC10 的 machine-scope DPAPI 在 RDP session 预期可解（若 RDP 也不可解，则需从安全绳 `.sme` 重建：RDP 跑 `import` 从 `.sme` 导入新固定路径）。安全绳 `.sme` 双份在手 = 最终兜底。

---

## 8. 开放问题（v2 收窄，xcheck 后多数已钉死）

- ~~O1：Windows ACL 实现选型~~ → **v2 钉死**：纯 Go `x/sys/windows` 安全描述符 API（§5.2）。
- ~~O2：migrate-path 跨后端迁移~~ → **v2 收窄**：不保留 DPAPI/keyring 读代码；旧后端不可解走 export/import（§5.3）。
- O3：kardianos service 账户选型——Windows 用 LocalSystem 还是专用 service 账户（单用户机可 LocalSystem，多用户机需专用账户）。**留实现时定**。
- ~~O4：serve.log 路径~~ → **v2 钉死**：进固定路径 `C:\ProgramData\ssh-manager\serve.log`（kimi #5/opencode #5：service 账户可能无 `%LocalAppData%`）。
- **O5（v2 新）**：Linux 双访问模式选型——专用组 `0750`/`0640` vs `sudo`/`sg` 文档要求（§5.2，writing-plans 阶段必答）。
- **O6（v2 新）**：`SSHMGR_MASTERKEY_HEX` env tier 标注"仅供测试"（opencode #6，防 boot 自起误用 env 落明文进 service 配置）。

---

## 9. 作废清单

- Plan 14（`2026-08-12-plan-14-windows-prod-deploy-design.md`）— 已在 Plan 15 标 Superseded，本 plan 维持。
- Plan 15（`2026-08-12-plan-15-machine-scope-dpapi-serve-fix-design.md`）— **本 plan 标 Superseded**。其 9 个 task 的代码（T1-T9）：
  - T1（DPAPI flag）、T2-T4（machine-scope + sentinel + migrate）、T8 DPAPI precheck：**删**
  - T4-T7（serve install 对象 API 壳、heartbeat、status 四信号）：**留概念，实现换 kardianos**
  - T9（迁移文档）：**重写**为 migrate-path
- NUC10 当前状态（Plan 15 B1 迁移后的 machine-scope master.key）：**v2 修正**——不指望 migrate-path 直接读它（sshd 不可解）。走 §7.2 Phase 2 的 RDP export/import 路径重建到 `C:\ProgramData\ssh-manager\`，旧 machine-scope blob 作废。
