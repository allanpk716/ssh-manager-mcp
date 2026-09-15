# 备份与迁移（export / import）

> 把整个 vault（服务器 + 凭据 + profile + project + host key + 审计）导出成一个**口令加密的便携文件**，用来备份、迁移、灾难恢复。文件**与机器的 master key 无关**——只要有口令，任何机器都能恢复。

## 它解决什么

你往保险柜里录了很多服务器。万一 `store.db` 损坏 / 丢失，或换机器，需要一份**可移植**的备份。`store.db` 自身虽加密，但绑死在原机的 master key 上（**不可移植**——恢复时需要原机的 `master.key.plain`，或同盘拷贝 master.key + store.db，见 [threat-model.md](./threat-model.md)）；`export` 解决"可移植"：文件用**你自己的口令**加密，跨机可恢复。

## 命令

```bash
sshmgr export --out vault.sme       # 提示输口令（输两次确认）；vault.sme 是加密文件
sshmgr import vault.sme             # 在目标机（空 vault + 已 unlock）恢复
```

- `export --out -` 或省略 `--out` → 输出到 stdout（管道 / 重定向场景）。
- 文件后缀随意（`.sme` 只是约定）。内容是 `SSHMGRV1` magic + Argon2id 派生 key + AES-256-GCM 密文。

## 安全模型（必读）

- 文件是 **KeePass 式**：`Argon2id(你的口令, 随机盐) → AES-256-GCM` 封住整个 vault 的 JSON（其中含**明文凭据**——密码 / 私钥都在里头，靠口令加密保护）。
- **文件 + 口令 = 全部凭据。** 文件泄露 + 弱口令 → 可被离线爆破（和 KeePass 数据库一个道理）。**必须用强口令**（长随机串，存进密码管理器）。
- 口令丢了 = 文件**无法恢复**（没有后门，找不回）。
- 明文凭据只在内存里短暂存在（export：解密 → 加密；import：解密 → 重封）；**落盘的始终是密文**。
- **与「直接复制 `store.db`」对比**：`store.db` 的凭据按**本机 master key** 加密，恢复需原机 `master.key.plain`（不可移植——且 master.key + store.db 同盘，L1+ 下离线拷盘可解，见 [threat-model.md](./threat-model.md)）；`export` 文件按**你的口令**加密，跨机只要口令即可。

## 使用场景

### 场景 ① 定期备份

每周 / 每月 `sshmgr export --out vault-YYYYMM.sme`，文件收进密码管理器 / 离线介质。vault 损坏时从最近一份恢复。

### 场景 ② 迁移到新机器

- 旧机：`sshmgr export --out vault.sme`。
- 新机：装好 `sshmgr` → `sshmgr unlock`（建新的 master key）→ `sshmgr import vault.sme`。
- **原 project token 导入后仍有效**（token 的 hash 保留）——已经配进 Claude Code 的 agent 不用改 `.mcp.json`。

### 场景 ③ 灾难恢复

vault 损坏 / 丢失：删掉坏的 `store.db`（或把 `SSHMGR_STORE` 指向一个新路径）→ `sshmgr unlock` → `sshmgr import vault.sme` → 恢复到出事前的状态。

## 限制（如实）

- **import 只入空 vault**：不覆盖既有数据（防误删）。要恢复到一个非空 vault，先删 / 移走 `store.db` 得到一个空 vault 再 import。
- **不增量同步**：export / import 是**全量快照**，不是多机实时同步（实时共享见 [multi-machine.md](./multi-machine.md) 的 serve 模式）。
- **审计自增 id 不保留**：audit 行的时间戳和内容保留，但 id 是目标库重新分配的（id 不被其他表引用，无影响）。
- **原 project token 仍有效**：导入保留 token 的 `hash/salt/prefix`，所以导出时拿到的明文 token 在导入后照样验证——agent 配置不用动。
- **单 owner**：和多机方案一样，这是"一个人"的备份 / 迁移工具，不解决多人共享访问控制。

## 格式与后续路线

- 文件格式：`internal/vaultio`（`SSHMGRV1` magic + Argon2id + AES-256-GCM）封 `store.Snapshot`（version 1 的 JSON）。这套 **`Snapshot` DTO 被多处复用**：客户端只读缓存（[Plan 12，已落地](./multi-machine.md#离线只读缓存plan-12)）和群晖定时自动备份（[Plan 13，已落地](#plan-13--nas-定时明文备份backup-create--verify)）都复用同一份 Snapshot。注意：Plan 13 是**明文**备份（只复用 DTO，不复用加密信封）；Plan 12 缓存用本机 keychain DEK 做 raw-key AES-GCM（复用 magic 但不口令派生）。所以 export 不只是个备份功能，它给后续打了地基。
- 这是 Plan 11（export / import）。**Plan 12（离线只读缓存）、Plan 13（NAS 明文备份）、Plan 14（Windows 生产部署）都复用了本篇的 `Snapshot` DTO**。多机的 迁移+enroll 仍是后续计划（未做）。

## 相关文档

- [getting-started.md](./getting-started.md)——单机从零到跑通（含 `store.db` 路径 + 基本备份说明）。
- [multi-machine.md](./multi-machine.md)——多机共享 serve 模式（实时同步，**不是**本篇的快照备份）。
- [agent-access.md](./agent-access.md)——project token 生命周期（导入后原 token 仍有效，见本篇"场景②"）。
- 仓库根 [README](../README.md)。

## Plan 13 — NAS 定时明文备份（backup create / verify）

> 设计 spec：`docs/superpowers/specs/2026-08-12-plan-13-nas-backup-design.md`（v3）。

`backup create` 把整个 vault 以**明文 JSON 快照**定时写到挂载的群晖目录，无变化不备份，按份数轮转，带 `.sha256` 边车抓 bit-rot。`backup verify` 按需校验。灾难恢复 = 从 NAS 拷文件 + `sshmgr import`。

### 部署硬约束（违反则必须回加密版）

明文备份**只在以下条件全部满足时安全**：

- NAS 在受信 VLAN 内，外网不可达；
- **永不开** Cloud Sync / Drive / Universal Search / Snapshot Replication / 公网共享；
- 目录权限锁死，物理介质单独保管。

**独立风险项 — 审计日志明文 = 新暴露**：备份里的 `audit_log.command` 原样导出，含历史命令行——可能携带**一次性 token / 临时密码 / 无副本 secret**，这些**不在 1Password 里**。明文备份暴露的范围比"1Password 冗余副本"更广。若开了任何 Cloud Sync / 公网，必须停止明文备份，回加密版（见 spec §10 未来工作）。

### marker 文件（挂载在场的硬保证）

`--dir/.ssh-manager-backup-marker` 必须存在（只查存在性）。**顺序**：先挂载 NAS → 在挂载的 NAS 上建 marker → 之后 `backup create` 才会写。这防"先建 marker 再 mount"导致 marker 落 shadow、挂载掉时 shadow marker 露出 → 静默写本地（fail-open）。

### Windows：任务计划程序 + UNC 路径

**用 UNC 路径，不要用 `net use` 映射盘号**：映射盘号 per-user/per-session，任务计划程序以别的 user 或 SYSTEM 跑时看不到 `Z:` → marker fail-closed 表现为"备份永远不跑"（无人值守典型静默失败）。UNC 路径 `\\synology\backups` 任何 session 都可达。

```cmd
schtasks /Create /SC DAILY /ST 03:30 /TN sshmgr-backup ^
  /TR "sshmgr.exe backup create --dir \\synology\backups --keep 7" ^
  /RU <user> /RP <password>
```

- master key：固定路径裸文件（Win `C:\ProgramData\ssh-manager\master.key.plain` / Unix `/var/lib/ssh-manager/master.key.plain`，见 [getting-started.md](./getting-started.md)）——任务只需以 admin/root / service 账户跑即可读（无需 keychain）。
- **勾"超过 10 分钟停止任务"**：SMB 写挂起无应用层超时，NAS 卡住会无限挂进程；任务计划层硬超时是兜底（陈旧锁 5 min 超时只救下次运行）。

### Linux：systemd timer

```ini
# /etc/systemd/system/sshmgr-backup.service
[Service]
Type=oneshot
# master key 走固定路径裸文件 /var/lib/ssh-manager/master.key.plain（见上 Windows 段）
# —— 服务以 root / service 账户跑即可读，不要用 Environment=/EnvironmentFile= 塞 hex
# （明文落 service config、枚举可见、无 ACL 粒度，比 0600+ACL 裸文件更差，见 threat-model.md §5）
ExecStart=/usr/local/bin/sshmgr backup create --dir /mnt/nas/backups --keep 7
TimeoutStartSec=600

# /etc/systemd/system/sshmgr-backup.timer
[Timer]
OnCalendar=*-*-* 03:30:00
Persistent=true
```

`Type=oneshot` 防 timer 自身重叠；`TimeoutStartSec=600` 兜底 SMB 挂起。

### 恢复

1. 从 NAS 拷最新的 `vault-*.json`（和它的 `.sha256`）到本机。
2. （可选）`sshmgr backup verify <file>` 确认没坏。
3. `sshmgr import <file>` —— 嗅探自动识别明文，**不弹口令**；导入到**空的** vault（`store.db` 不存在或空）。
4. **cache_tokens 不在备份里**（设备身份，非 vault 内容——`ExportSnapshot` 零处读该表，export/import 与 NAS 两路恢复同理）：恢复后该表**为空** → 所有工作机下次回连拿到 **unknown 401** → 按 Plan 34 语义**批量切断**（各机本地 cache 四件销毁 + `quarantine/` 痕迹 + 明确归因报文）——这是**预期行为、非事故**（设备码历史本就不随 vault 走）。恢复流程 = **逐设备重新发码 + enroll**：每台 `sshmgr cache-tokens add --name <device>` 重发授权码，工作机用新码 `cache pull` 重新拉取（全量重建）。agent 的 `.mcp.json` 不用动（project token 在备份里）。
   > ⚠️ **带外警示——raw-DB 直拷不走这条**：直接拷贝 `store.db` 文件恢复会使 cache_tokens 连**历史状态一起回滚**——**已 revoke 的码可能复活**（被吊销设备重新拉到新快照）。此类恢复后必须**逐行审计**（`cache-tokens ls` 核对每行 status），把该死的行重新 revoke、该换的码逐台重发。

### skip 语义（诚实）

活跃服务器上 `backup create` 的"无变化不备份"**几乎不触发**——`audit_log` 每执行一条 SSH 命令就增长，SHA256 必变。skip 主要服务**空闲窗口 / 长期静态 vault**。**rotation 才是兜底**。长期静态 vault（skip 让你只握 1 份不刷新文件）需定期 `backup verify` + 依赖 NAS 自身快照做底层兜底。

### 运维 footgun

- 禁 `cat`/`grep -r password` 查备份；用 `backup verify` 或恢复到测试 vault 后 `sshmgr servers ls`。
- `--dir` 必须是绝对路径且不在任何 git 工作树里（`backup create` 会检测 `--dir` 自身含 `.git` 并拒绝）。
- `.gitignore` 模板：`vault-*.json` + `*.sha256`。

### 限制 / 未来工作

- 不加密（见上"部署硬约束"）、不增量（全量快照）、不事件触发（纯定时）。
- 无 `backup restore` 一条龙（= 手动拷 + `import`）。
- 未来若需 Cloud Sync / 公网，回加密版（decrypt-and-compare skip）。

## Plan 14 — Windows 生产部署（DPAPI master key + serve 常驻）

> 设计 spec：`docs/superpowers/specs/2026-08-12-plan-14-windows-prod-deploy-design.md`（v2）。**⚠️ SUPERSEDED by Plan 16**（`2026-08-13-plan-16-fixed-path-filekey-design.md`，固定路径 + FileKeyProvider，废弃 DPAPI/keyring 路线）。Plan 14 先被 Plan 15（machine-scope DPAPI）取代，Plan 15 又被 Plan 16 取代——两次撞墙证明"用户态密钥模型 + 服务自起"部署形态下 DPAPI 跨 session 不可靠。**当前生产路径**见本篇末 [Plan 16 迁移 Runbook](#plan-16--固定路径--filekeyprovider迁移-runbook)。Plan 14/15 正文保留作审计轨迹。

把 Windows 上的 master key 存储从 **Windows Credential Manager（keychain）** 换成 **DPAPI 加密的本地文件**（`%AppData%\ssh-manager\master.key`），并新增 `sshmgr serve install` 把 serve 注册成 Task Scheduler 常驻任务。原因：实测发现 Windows Credential Manager 在 sshd / Service / Task-Scheduler 的非交互 session 里报 `ERROR_NO_SUCH_LOGON_SESSION (1312)`——master key 存不进 / 读不出，serve 在这些 session 里拿不到 master key 起不来；DPAPI 不受此限制（spec §12 spike 实证三 session 全通）。**Plan 15 修正为 machine-scope DPAPI**（见下方「升级 Runbook (Plan 15 修正)」）。

### 升级 Runbook（v0.2.0 → 新版，Windows）

已有 v0.2.0 vault 的机器（master key 存在 keychain）升级到新版（master key 存 DPAPI 文件）：

1. **停掉所有正在跑的 sshmgr 进程**（v0.2.0 的 mcp / serve 等）。原因：旧进程持有 `store.db` 句柄（E2E FINDING 5），不停干净会在迁移 + 重启时撞锁；更重要的是新旧进程不能同时持有不同位置的 master key。
2. **在交互式 session（本地终端 / RDP，不是 ssh）跑** `sshmgr unlock`：
   - 程序检测到 `master.key` 不存在但 v0.2.0 keychain slot 可读 → 提示"迁移到 DPAPI 文件？[y/N]" → 确认后写 `master.key` + 删旧 keychain slot。
   - **同时迁移 cache DEK**（Plan 12 的 cache-dek keychain slot → `cache-dek.key` 文件，和 master.key 同目录不同文件）。
   - 若在 sshd / 非交互 session 跑这一步：旧 slot 读不出（1312）→ 程序会明确提示"请在交互式 session 重跑"，**不会自动生成新 master key**（避免 orphans 旧 vault）。
3. **`sshmgr serve install`**（如果这台机要常驻 serve）：注册 Task Scheduler 任务，boot + logon 自起，崩溃自重启。

⚠️ **跳过步骤 2 直接 `serve install`** → serve 读不到 master key（旧 slot 在非交互 session 读不出，新文件还没生成）→ 任务启动失败循环。`serve install` 自身会在 master.key 缺失时拒绝注册（报"run 'sshmgr unlock' in an interactive session first"），但别依赖这道闸——正确顺序是先 unlock 迁移。

### 升级 Runbook（Plan 15 修正：user-scope → machine-scope 迁移）

Plan 14 的 user-scope DPAPI 在 NUC10 §7.3 真机验收中暴露了**跨 logon session 失败**（FINDING B）。Plan 15 修正为 **machine-scope DPAPI**（`CRYPTPROTECT_LOCAL_MACHINE`）——master.key 绑**机器**，不绑用户 SID / logon session。

**已有 Plan 14 user-scope vault 的机器升级到 Plan 15 machine-scope**：

1. 部署 Plan 15 新版 `sshmgr.exe`（覆盖旧版）。
2. **在交互式 session（本地终端 / RDP）跑** `sshmgr unlock`：
   - 程序检测到 `master.key` 是 **user-scope**（可解）→ 自动触发 **`migrateDpapiScope`**（T3 实现）→ 用 **machine-scope** 重新 protect → 覆盖 `master.key`。
   - **无需手动重设 vault**——master key 内容不变，只换 DPAPI scope。
   - 若在 sshd / 非交互 session 跑这一步：user-scope `master.key` 读不出 → 程序会明确提示"请在交互式 session 重跑"（不会破坏现有 vault）。
3. **`sshmgr serve install`**（如果这台机要常驻 serve）：
   - Plan 15 修正了 serve install 的 **Go 密码读**（codex #2：`Get-Credential` → PowerShell `secureString` 读密码，不再把密码写进 4688 审计日志）。
   - Plan 15 修正了 **precheck machine-scope**（安装前检查 master.key 是 machine-scope，避免 user-scope 的跨 session 失败）。
   - Plan 15 新增 **TLS 支持**（codex #5：`--tls-cert` / `--tls-key` 选项）。
   - Plan 15 新增 **MultipleInstances 支持**（pi #2：允许多个 serve 实例同时跑，用不同端口）。

⚠️ **跳过步骤 2 直接 `serve install`** → Plan 15 的 `serve install` 会报 **precheck 失败**（"master.key is user-scope, run 'sshmgr unlock' in an interactive session first to migrate to machine-scope"），不会注册错误配置的任务。

### master.key ≠ 备份（不可移植）

**`master.key` 不是备份，是本机日常解锁缓存**：

- 它是 **machine-scope DPAPI** 加密的（Plan 15 修正）——绑死本机 **机器**，不绑用户 SID / logon session。
- 换机 / 重装系统 / 换用户账户 → `master.key` 成废物（新环境解不开）。
- **machine-scope 对同机其他用户不保密**（见下"威胁模型"）。

**唯一可移植的灾备手段**是：

- **Plan 11 export 信封**（口令加密）：在新机 `sshmgr import vault.sme`（[场景②](#场景-②-迁移到新机器)）。
- **Plan 13 NAS 明文备份**：从 NAS 拷最新 `vault-*.json` + `.sha256` → 新机 `sshmgr import <file>`（明文嗅探自动识别，不弹口令，见 [Plan 13 恢复](#恢复)）。

两条都支持 `--passphrase-file`（export / import 加密分支）做**无人值守**恢复：

```bash
sshmgr export --out vault.sme --passphrase-file /secure/vault.pass   # 脚本里直接出
sshmgr import vault.sme --passphrase-file /secure/vault.pass          # 脚本里直接进
# Plan 13 明文备份走 import 时 --passphrase-file 被忽略（明文分支不需要口令）
```

passphrase 文件自身要 0600、不进 git、恢复后删掉。**口令丢了 = export 文件无法恢复**（无后门）。

### 密码变更（事实，别写错）

| 情形 | master.key 还能解吗？ | 要做什么 |
|---|---|---|
| **用户自行改密码**（Ctrl+Alt+Del → Change password，知道旧密码） | ✅ 还能解 | 无影响——Windows 用旧密码自动 re-wrap DPAPI Master Key，已有密文仍可解 |
| **管理员强制重置密码**（admin reset，不知旧密码） | ✅ **还能解**（machine-scope DPAPI 免疫） | **machine-scope 对 admin 强制重置密码免疫**（用 DPAPI_SYSTEM LSA secret，不依赖用户 Master Key）—— 与 user-scope 相反。代价：同机其他用户进程能解（靠文件夹 ACL 兜底）。 |

⚠️ **Plan 14 文档写的"user-scope admin 重置密码会断"是事实正确**，但 Plan 15 修正为 machine-scope 后，**这个威胁消失了**。machine-scope 的威胁模型是"同机其他用户能解"（见下），不是"admin 重置密码会断"。

### Windows：`serve install` / `uninstall` / `status`

```powershell
sshmgr serve install [--addr 0.0.0.0:7878] [--tls-cert cert.pem] [--tls-key key.pem]
sshmgr serve status
sshmgr serve uninstall
```

**`serve install`**：把前台 `serve` 包成 Task Scheduler 任务 `sshmgr-serve`：

- **触发器**：boot + 用户 logon（任务以 `LogonType=Password` 跑，boot 时无需等人登录就能起）。
- **崩溃恢复**：`RestartOnFailure` PT1M × 3（1 分钟间隔，最多 3 次）。
- **以当前用户身份 + filtered token（非 RunLevel Highest）**——足够读用户 profile + 监听端口，不需提权。
- **stdout/stderr 重定向**到 `%LocalAppData%\ssh-manager\serve.log`——headless 启动失败（如 master key 解不开）也能事后翻日志。
- **密码处理**：Task Scheduler 要存 Windows 密码才能 boot 时起任务。程序**不**用 `schtasks /Create /RP <密码>`（密码会进命令行 + 4688 审计日志），而是 shell 进 PowerShell 调 `Register-ScheduledTask`，由 PowerShell 的 `Get-Credential` 交互弹窗读密码。**密码只活在 PowerShell 进程内存里**，不进 sshmgr.exe argv，不进 4688 日志。Task Scheduler 把它存在自己的 LSA secret store（标准路径）。
- **装完立即 `schtasks /Run`** 跑一次验证 + 生成 serve.log。

**`serve status`**：四路独立检查（每路单独打一行，部分失败也看得清）：

```
task:      registered (Running, last result 0)
process:   running
http:      responding (401/200 = auth working)
vault:     ok
overall:   HEALTHY
```

`vault: LOCKED` 那行特别关键：它扫 `serve.log` 末尾找硬失败标记（`unreadable` / `undecryptable` / `vault locked` / `run \`sshmgr unlock\``），**进程在跑 ≠ vault 已解锁**——比如 admin 重置密码后 master.key 解不开，进程会崩溃自重启循环，HTTP 可能短暂 200/401 但实际不可用，这一行会标 `LOCKED`。

**`serve uninstall`**：删 Task Scheduler 任务 + best-effort `taskkill` 残留 serve 进程。

#### 账户密码过期 footgun

Task Scheduler 存的是你**装任务当时**的 Windows 密码。**密码过期后任务起不来**（凭据失效）——这是 `LogonType=Password` 的固有代价（换来 boot 时无需人登录就能起）。单用户本地账户（NUC10 / 家用服务器）建议直接禁用密码过期：

```powershell
wmic UserAccount where Name='<你的用户名>' set PasswordExpires=False
```

> **Win11 22H2+ 注意**：`wmic` 在新版 Windows 默认不装（弃用）。改用 PowerShell cmdlet：
> ```powershell
> Set-LocalUser -Name '<你的用户名>' -PasswordNeverExpires $true
> ```
> （NUC10 当前是 Win10 19045，`wmic` 可用；未来升级到 24H2 需切到 `Set-LocalUser`。）

域账户通常有强制密码策略，不能这么搞——定期重装任务（`serve uninstall` → 改密码 → `serve install`）是唯一的运维路径。

### 威胁模型（诚实，Plan 15 修正为 machine-scope）

- ⚠ **master.key 对同机其他用户进程不保密**（machine-scope DPAPI）——任何用户启动的进程都能 `CryptUnprotectData` 解开 `master.key`。**这是 machine-scope 的代价**（换取跨 logon session 可用 + admin 重置密码免疫）。
- ⚠ **master.key 对同用户（`allan716`）跑的任意进程不保密**——任何 `allan716` 启动的进程都能 `CryptUnprotectData` 解开 `master.key`。**这与 v0.2.0 的 keychain 等级相同**（keychain 对同用户进程也不设防），**不是 regression**。
- ✓ **master.key 对 agent 保密**（agent 进程无 master key，走 broker；L2 模型不变）。
- ✓ **master.key 对 admin 强制重置密码免疫**（machine-scope DPAPI 用 DPAPI_SYSTEM LSA secret，不依赖用户 Master Key；与 user-scope 相反）。
- **新增信任根 + 防线**：`%AppData%\ssh-manager\master.key`（+ `cache-dek.key`）——物理 / 同机进程访问控制是它的防线（文件夹 ACL `icacls /inheritance:r /grant "<user>:(OI)(CI)F"`，不靠 Go 的 0600 位——Windows 忽略 mode 位）。**同机其他用户能解 machine-scope DPAPI，但读不到文件（ACL 阻止）——这是defense-in-depth**。

**威胁模型对比（user-scope vs machine-scope）**：

| 威胁 | user-scope（Plan 14） | machine-scope（Plan 15） |
|---|---|---|
| 同机其他用户进程 | ✅ 保密（DPAPI 绑用户 SID） | ⚠️ 不保密（但 ACL 阻止读文件） |
| admin 强制重置密码 | ❌ 会断（user Master Key 失效） | ✅ 免疫（DPAPI_SYSTEM LSA secret） |
| 跨 logon session（sshd → Task Scheduler） | ❌ 失败（FINDING B） | ✅ 可用（machine-scope 绑机器） |
| 同用户进程 | ⚠️ 不保密（与 keychain 同级） | ⚠️ 不保密（与 keychain 同级） |

### 限制

- **Windows only**：DPAPI master key + `serve install`（Task Scheduler）只在 Windows 实现。Linux/macOS 继续用 keychain（`KeyringKeyProvider`）+ `FileKeyProvider` 兜底（无 keychain 的 headless 环境），**`serve install` 在 Linux/macOS 报 `not yet supported`**（spec §3.4 / §9 defer 到专门 plan，见 [multi-machine.md 的 Linux/macOS 章节](./multi-machine.md#linuxmacos-尚未支持)）。
- **迁移必须交互式 session**：v0.2.0 → DPAPI 的迁移（`unlock` 触发）依赖读旧 keychain slot，只在本地终端 / RDP 通；sshd / 非交互 session 读不出（1312），程序会提示重跑（不自动生成新 key）。
- **user→machine 迁移必须交互式 session**：user-scope → machine-scope 的迁移（Plan 15 的 `migrateDpapiScope`）依赖读现有 user-scope master.key，只在本地终端 / RDP 通；sshd / 非交互 session 读不出（user-scope 绑用户 SID），程序会提示重跑（不自动生成新 key）。
- **machine-scope 威胁模型**：同机其他用户进程能解 DPAPI（machine-scope 不绑用户 SID），但文件夹 ACL 阻止读文件（`icacls /inheritance:r /grant "<user>:(OI)(CI)F"`）。这是 defense-in-depth。
- **serve install 需交互式 session 装**（PowerShell `Get-Credential` 要弹窗）——不能在纯 sshd session 装。
- **`serve install` 不是跨平台的**：Linux systemd / macOS launchd 各有平台陷阱（linger 权限、D-Bus session、LaunchAgent 仅 GUI login 后启动），未实现。

### Plan 15 §7.3 NUC10 reboot 验证（release 前 checklist）

CI 不能 reboot，boot 自起（BootTrigger）+ 跨重启 DPAPI（machine-scope）的闭环验证在这里：

1. NUC10 部署新版 sshmgr.exe。
2. NUC10 交互式（RDP）跑 `sshmgr unlock` → 触发 user→machine 迁移（重 protect C）。
3. `sshmgr serve install`（输 allan716 密码）→ 对象 API 注册。
4. **reboot NUC10** → BootTrigger 自起 serve。
5. NUC10 起来后 `sshmgr serve status` → `vault: ok`（machine-scope 跨重启可解）+ `overall: HEALTHY`。
6. 笔记本 MCP 连 `http://192.168.100.235:7878` → `exec_command` 在 1660Super01 跑 `hostname` → 返回 `DESKTOP-UP1MHGT`。
7. 清理：`serve uninstall`。

**关键验证点**：

- **跨重启 DPAPI**：machine-scope master.key 在 reboot 后仍可解（`vault: ok`）。这是 CI 不能验证的（CI 无 reboot）。
- **BootTrigger 自起**：Task Scheduler 的 boot trigger 让 serve 在无人登录时自动启动（`overall: HEALTHY`）。
- **跨机 exec_command**：笔记本 MCP 通过 serve 在目标机（1660Super01）执行命令成功 → end-to-end 多机架构验证。

⚠️ **这个 runbook 是 FINDING B 的 closure**（user-scope 跨 logon session 失败在真机 reboot 验证中被发现；machine-scope 修正后必须通过真机 reboot 验证）。

---

## Plan 16 — 固定路径 + FileKeyProvider（迁移 Runbook）

> 设计 spec：`docs/superpowers/specs/2026-08-13-plan-16-fixed-path-filekey-design.md`（v2，**当前生产路径**）。取代 Plan 14/15 的 DPAPI 路线。威胁模型见 [threat-model.md](./threat-model.md)。

Plan 16 把 master key 从 **DPAPI 加密文件**（Plan 14/15）换成**固定路径的裸明文文件 + ACL**（`master.key.plain`），并把 vault 目录从用户目录（`%AppData%\ssh-manager\`）挪到程序固定路径（Win `C:\ProgramData\ssh-manager\` / Unix `/var/lib/ssh-manager/`）。原因见 [Plan 16 §1](./superpowers/specs/2026-08-13-plan-16-fixed-path-filekey-design.md)：machine-scope DPAPI 在 NUC10 §7.3 真机验收中**同样**跨 session 失败（sshd session 读不出），证明"换 scope"不是解药，"砍 DPAPI"才是。

### 新的存储路径

| 平台 | store.db / master.key.plain / cache-dek.key / serve.log |
|---|---|
| Windows | `C:\ProgramData\ssh-manager\` |
| Linux | `/var/lib/ssh-manager/` |
| macOS | `/var/lib/ssh-manager/` |

环境变量 `SSHMGR_STORE` / `SSHMGR_FILEKEY_PATH` 可覆盖（仅供测试 / 迁移 / 自定义，**生产不建议改**）。

### master.key 形态（L1+）

- **裸明文文件**（不是 DPAPI blob）——service 进程直接 `os.ReadFile` 读，无跨 session / 跨账户的密钥库依赖。
- **Windows ACL 硬化**：`SYSTEM` + `Administrators` + 当前用户，**移除 `Users` / `Authenticated Users` / `Everyone`，禁用继承**（`SE_DACL_PROTECTED`）。这是 L1+ 唯一的保护层——`store.HardenACL` 用纯 Go `golang.org/x/sys/windows` advapi32 实现，不调 `icacls`。
- **Unix**：`0600` 文件 / `0700` 目录，属主为 service 账户。
- **不再依赖**：Windows DPAPI、Windows Credential Manager、zalando/go-keyring、Unix Secret Service / Keychain（Plan 16 全删）。

### 从 Plan 14/15 升级（两条路二选一）

旧 master.key 是 **DPAPI blob**（Plan 14 user-scope 或 Plan 15 machine-scope），新版本（Plan 16）读不了 DPAPI——必须迁移。`migrate-path` 子命令只搬**文件型** vault（不读 DPAPI/keyring，spec §5.3 / Q6/Q10 删干净）；旧 blob 不可解时走 export + import。

**路 A：`migrate-path`（旧 master.key 是文件型，或当前 session 能解 DPAPI blob——少数情况）**

```bash
# 默认从 UserConfigDir/ssh-manager/（旧默认）搬到 paths.VaultDir()（新固定路径）
sshmgr migrate-path
# 或指定旧目录：
sshmgr migrate-path --from /old/path
# --keep-old 保留旧文件（默认删，N/N 自检通过后才删）
```

`migrate-path` 会：checkpoint 旧 vault（WAL truncate）→ 复制 `store.db` + `master.key.plain` 到新路径 → N/N 自检（每条凭据 + sudo 都能解）→ 删旧（除非 `--keep-old`）→ 幂等。

**路 B：export + import（NUC10 现状——sshd session 读不出 machine-scope DPAPI blob）**

这是 NUC10 §7.3 触发事实的处置路径。`export` 和 `migrate-path` 一样经旧后端读——sshd session 解不开时，**必须在 RDP / 交互 session 跑**。

```bash
# 1. 在 RDP / 交互 session（旧 master.key 可解的 session）：
sshmgr export --out vault.sme --passphrase-file /secure/vault.pass

# 2. 部署 Plan 16 新版 sshmgr（覆盖旧二进制）

# 3. admin/root 跑 unlock（建固定路径目录 + 新 master.key.plain + 空 store.db）：
sshmgr unlock

# 4. 导入到新固定路径 vault（re-seal 到新 master.key）：
sshmgr import --passphrase-file /secure/vault.pass vault.sme

# 5. 手动删旧 DPAPI blob + 旧 store.db（Plan 14/15 留在 UserConfigDir 的旧文件）：
#    Windows: del "%APPDATA%\ssh-manager\master.key" "%APPDATA%\ssh-manager\store.db"
#    Unix: rm ~/.config/ssh-manager/master.key ~/.config/ssh-manager/store.db
```

> ⚠️ **`migrate-path` / `export` / `import` 都受 session 约束**：三者都经旧 master key 后端读。NUC10 sshd session 读不出 machine-scope DPAPI → 三者在 sshd 都会失败并提示去 RDP。**没有"headless 一键迁移"的路径**——这是 Plan 16 §5.3 显式接受的代价（不保留 DPAPI/keyring 读代码，Q6/Q10 删干净）。

### 安全绳（最终兜底）

无论走哪条路，**`.sme` 双份在手 = 最终兜底**：从 `.sme` 总能 `import` 到一个全新的 Plan 16 vault（固定路径 + 新 master.key）。这是"vault 不可解"场景的唯一恢复手段。

### NUC10 §7.2 真机验收（Plan 16，Phase 2 改 RDP）

```
Phase 1 (SSH)    部署 Plan 16 二进制（备份旧 exe）
Phase 2 (RDP)    ★ 不是 SSH——sshd 读不出旧 machine-scope DPAPI blob
                  路 B：RDP 跑 export → unlock → import 到 C:\ProgramData\ssh-manager\
                  自检 7/7
Phase 3 (RDP)    serve install（kardianos），admin 跑
Phase 4 (reboot) ★ reboot 后 serve 自起读固定路径 master.key.plain 成功（纯文件读，无 DPAPI）
Phase 5 (笔记本)  agent exec_command 在 1660Super01 成功
```

**通过标准**：Phase 4 reboot 后 `serve status` = `vault: ok` + `service: Running` + `http: responding` + `overall: HEALTHY`。**这次没有 DPAPI 跨 session 赌博**——读文件就是读文件。

详见 [Plan 16 §7.2](./superpowers/specs/2026-08-13-plan-16-fixed-path-filekey-design.md)。
