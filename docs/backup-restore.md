# 备份与迁移（export / import）

> 把整个 vault（服务器 + 凭据 + profile + project + host key + 审计）导出成一个**口令加密的便携文件**，用来备份、迁移、灾难恢复。文件**与机器的 master key 无关**——只要有口令，任何机器都能恢复。

## 它解决什么

你往保险柜里录了很多服务器。万一 `store.db` 损坏 / 丢失，或换机器，需要一份**可移植**的备份。`store.db` 自身虽加密，但绑死在原机的 keychain master key 上（**不可移植**——恢复时需要原机的 keychain）；`export` 解决"可移植"：文件用**你自己的口令**加密，跨机可恢复。

## 命令

```bash
ssh-manager export --out vault.sme       # 提示输口令（输两次确认）；vault.sme 是加密文件
ssh-manager import vault.sme             # 在目标机（空 vault + 已 unlock）恢复
```

- `export --out -` 或省略 `--out` → 输出到 stdout（管道 / 重定向场景）。
- 文件后缀随意（`.sme` 只是约定）。内容是 `SSHMGRV1` magic + Argon2id 派生 key + AES-256-GCM 密文。

## 安全模型（必读）

- 文件是 **KeePass 式**：`Argon2id(你的口令, 随机盐) → AES-256-GCM` 封住整个 vault 的 JSON（其中含**明文凭据**——密码 / 私钥都在里头，靠口令加密保护）。
- **文件 + 口令 = 全部凭据。** 文件泄露 + 弱口令 → 可被离线爆破（和 KeePass 数据库一个道理）。**必须用强口令**（长随机串，存进密码管理器）。
- 口令丢了 = 文件**无法恢复**（没有后门，找不回）。
- 明文凭据只在内存里短暂存在（export：解密 → 加密；import：解密 → 重封）；**落盘的始终是密文**。
- **与「直接复制 `store.db`」对比**：`store.db` 的凭据按**本机 master key** 加密，恢复需原机 keychain（不可移植）；`export` 文件按**你的口令**加密，跨机只要口令即可。

## 使用场景

### 场景 ① 定期备份

每周 / 每月 `ssh-manager export --out vault-YYYYMM.sme`，文件收进密码管理器 / 离线介质。vault 损坏时从最近一份恢复。

### 场景 ② 迁移到新机器

- 旧机：`ssh-manager export --out vault.sme`。
- 新机：装好 `ssh-manager` → `ssh-manager unlock`（建新的 master key）→ `ssh-manager import vault.sme`。
- **原 project token 导入后仍有效**（token 的 hash 保留）——已经配进 Claude Code 的 agent 不用改 `.mcp.json`。

### 场景 ③ 灾难恢复

vault 损坏 / 丢失：删掉坏的 `store.db`（或把 `SSHMGR_STORE` 指向一个新路径）→ `ssh-manager unlock` → `ssh-manager import vault.sme` → 恢复到出事前的状态。

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

`backup create` 把整个 vault 以**明文 JSON 快照**定时写到挂载的群晖目录，无变化不备份，按份数轮转，带 `.sha256` 边车抓 bit-rot。`backup verify` 按需校验。灾难恢复 = 从 NAS 拷文件 + `ssh-manager import`。

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
schtasks /Create /SC DAILY /ST 03:30 /TN ssh-manager-backup ^
  /TR "ssh-manager.exe backup create --dir \\synology\backups --keep 7" ^
  /RU <user> /RP <password>
```

- master key：`setx SSHMGR_MASTERKEY_HEX <hex>`（或任务以 serve 同 user 跑、keychain 可达）。
- **勾"超过 10 分钟停止任务"**：SMB 写挂起无应用层超时，NAS 卡住会无限挂进程；任务计划层硬超时是兜底（陈旧锁 5 min 超时只救下次运行）。

### Linux：systemd timer

```ini
# /etc/systemd/system/ssh-manager-backup.service
[Service]
Type=oneshot
Environment=SSHMGR_MASTERKEY_HEX=<hex>
ExecStart=/usr/local/bin/ssh-manager backup create --dir /mnt/nas/backups --keep 7
TimeoutStartSec=600

# /etc/systemd/system/ssh-manager-backup.timer
[Timer]
OnCalendar=*-*-* 03:30:00
Persistent=true
```

`Type=oneshot` 防 timer 自身重叠；`TimeoutStartSec=600` 兜底 SMB 挂起。

### 恢复

1. 从 NAS 拷最新的 `vault-*.json`（和它的 `.sha256`）到本机。
2. （可选）`ssh-manager backup verify <file>` 确认没坏。
3. `ssh-manager import <file>` —— 嗅探自动识别明文，**不弹口令**；导入到**空的** vault（`store.db` 不存在或空）。
4. **cache_tokens 不在备份里**（设备身份，非 vault 内容）：恢复后需 `ssh-manager cache-tokens add` 重发各工作机授权码，各工作机 `ssh-manager cache pull` 重拉。agent 的 `.mcp.json` 不用动（project token 在备份里）。

### skip 语义（诚实）

活跃服务器上 `backup create` 的"无变化不备份"**几乎不触发**——`audit_log` 每执行一条 SSH 命令就增长，SHA256 必变。skip 主要服务**空闲窗口 / 长期静态 vault**。**rotation 才是兜底**。长期静态 vault（skip 让你只握 1 份不刷新文件）需定期 `backup verify` + 依赖 NAS 自身快照做底层兜底。

### 运维 footgun

- 禁 `cat`/`grep -r password` 查备份；用 `backup verify` 或恢复到测试 vault 后 `ssh-manager servers ls`。
- `--dir` 必须是绝对路径且不在任何 git 工作树里（`backup create` 会检测 `--dir` 自身含 `.git` 并拒绝）。
- `.gitignore` 模板：`vault-*.json` + `*.sha256`。

### 限制 / 未来工作

- 不加密（见上"部署硬约束"）、不增量（全量快照）、不事件触发（纯定时）。
- 无 `backup restore` 一条龙（= 手动拷 + `import`）。
- 未来若需 Cloud Sync / 公网，回加密版（decrypt-and-compare skip）。

## Plan 14 — Windows 生产部署（DPAPI master key + serve 常驻）

> 设计 spec：`docs/superpowers/specs/2026-08-12-plan-14-windows-prod-deploy-design.md`（v2）。

把 Windows 上的 master key 存储从 **Windows Credential Manager（keychain）** 换成 **DPAPI 加密的本地文件**（`%AppData%\ssh-manager\master.key`），并新增 `ssh-manager serve install` 把 serve 注册成 Task Scheduler 常驻任务。原因：实测发现 Windows Credential Manager 在 sshd / Service / Task-Scheduler 的非交互 session 里报 `ERROR_NO_SUCH_LOGON_SESSION (1312)`——master key 存不进 / 读不出，serve 在这些 session 里拿不到 master key 起不来；DPAPI 不受此限制（spec §12 spike 实证三 session 全通）。

### 升级 Runbook（v0.2.0 → 新版，Windows）

已有 v0.2.0 vault 的机器（master key 存在 keychain）升级到新版（master key 存 DPAPI 文件）：

1. **停掉所有正在跑的 ssh-manager 进程**（v0.2.0 的 mcp / serve 等）。原因：旧进程持有 `store.db` 句柄（E2E FINDING 5），不停干净会在迁移 + 重启时撞锁；更重要的是新旧进程不能同时持有不同位置的 master key。
2. **在交互式 session（本地终端 / RDP，不是 ssh）跑** `ssh-manager unlock`：
   - 程序检测到 `master.key` 不存在但 v0.2.0 keychain slot 可读 → 提示"迁移到 DPAPI 文件？[y/N]" → 确认后写 `master.key` + 删旧 keychain slot。
   - **同时迁移 cache DEK**（Plan 12 的 cache-dek keychain slot → `cache-dek.key` 文件，和 master.key 同目录不同文件）。
   - 若在 sshd / 非交互 session 跑这一步：旧 slot 读不出（1312）→ 程序会明确提示"请在交互式 session 重跑"，**不会自动生成新 master key**（避免 orphans 旧 vault）。
3. **`ssh-manager serve install`**（如果这台机要常驻 serve）：注册 Task Scheduler 任务，boot + logon 自起，崩溃自重启。

⚠️ **跳过步骤 2 直接 `serve install`** → serve 读不到 master key（旧 slot 在非交互 session 读不出，新文件还没生成）→ 任务启动失败循环。`serve install` 自身会在 master.key 缺失时拒绝注册（报"run 'ssh-manager unlock' in an interactive session first"），但别依赖这道闸——正确顺序是先 unlock 迁移。

### master.key ≠ 备份（不可移植）

**`master.key` 不是备份，是本机日常解锁缓存**：

- 它是 **user-scope DPAPI** 加密的——绑死本机 profile + 当前用户 SID。
- 换机 / 重装系统 / 换用户账户 → `master.key` 成废物（新环境解不开）。
- 即便拷到同机另一用户账户，也解不开（user-scope DPAPI 对其他用户保密，见下"威胁模型"）。

**唯一可移植的灾备手段**是：

- **Plan 11 export 信封**（口令加密）：在新机 `ssh-manager import vault.sme`（[场景②](#场景-②-迁移到新机器)）。
- **Plan 13 NAS 明文备份**：从 NAS 拷最新 `vault-*.json` + `.sha256` → 新机 `ssh-manager import <file>`（明文嗅探自动识别，不弹口令，见 [Plan 13 恢复](#恢复)）。

两条都支持 `--passphrase-file`（export / import 加密分支）做**无人值守**恢复：

```bash
ssh-manager export --out vault.sme --passphrase-file /secure/vault.pass   # 脚本里直接出
ssh-manager import vault.sme --passphrase-file /secure/vault.pass          # 脚本里直接进
# Plan 13 明文备份走 import 时 --passphrase-file 被忽略（明文分支不需要口令）
```

passphrase 文件自身要 0600、不进 git、恢复后删掉。**口令丢了 = export 文件无法恢复**（无后门）。

### 密码变更（事实，别写错）

| 情形 | master.key 还能解吗？ | 要做什么 |
|---|---|---|
| **用户自行改密码**（Ctrl+Alt+Del → Change password，知道旧密码） | ✅ 还能解 | 无影响——Windows 用旧密码自动 re-wrap DPAPI Master Key，已有密文仍可解 |
| **管理员强制重置密码**（admin reset，不知旧密码） | ❌ 解不开 | 重置前先 [export 一份](#场景-①-定期备份)；重置后在新密码环境里 `unlock` 会报"master key present but unreadable"，按错误提示从 export 备份恢复或重设 vault |

⚠️ 旧版本文档/spec 写过"改密码就断"——**那是事实错误，已修正**。日常改密码无影响，**只有 admin 强制重置**才会断。

### Windows：`serve install` / `uninstall` / `status`

```powershell
ssh-manager serve install [--addr 0.0.0.0:7878] [--tls-cert cert.pem] [--tls-key key.pem]
ssh-manager serve status
ssh-manager serve uninstall
```

**`serve install`**：把前台 `serve` 包成 Task Scheduler 任务 `ssh-manager-serve`：

- **触发器**：boot + 用户 logon（任务以 `LogonType=Password` 跑，boot 时无需等人登录就能起）。
- **崩溃恢复**：`RestartOnFailure` PT1M × 3（1 分钟间隔，最多 3 次）。
- **以当前用户身份 + filtered token（非 RunLevel Highest）**——足够读用户 profile + 监听端口，不需提权。
- **stdout/stderr 重定向**到 `%LocalAppData%\ssh-manager\serve.log`——headless 启动失败（如 master key 解不开）也能事后翻日志。
- **密码处理**：Task Scheduler 要存 Windows 密码才能 boot 时起任务。程序**不**用 `schtasks /Create /RP <密码>`（密码会进命令行 + 4688 审计日志），而是 shell 进 PowerShell 调 `Register-ScheduledTask`，由 PowerShell 的 `Get-Credential` 交互弹窗读密码。**密码只活在 PowerShell 进程内存里**，不进 ssh-manager.exe argv，不进 4688 日志。Task Scheduler 把它存在自己的 LSA secret store（标准路径）。
- **装完立即 `schtasks /Run`** 跑一次验证 + 生成 serve.log。

**`serve status`**：四路独立检查（每路单独打一行，部分失败也看得清）：

```
task:      registered (Running, last result 0)
process:   running
http:      responding (401/200 = auth working)
vault:     ok
overall:   HEALTHY
```

`vault: LOCKED` 那行特别关键：它扫 `serve.log` 末尾找硬失败标记（`unreadable` / `undecryptable` / `vault locked` / `run \`ssh-manager unlock\``），**进程在跑 ≠ vault 已解锁**——比如 admin 重置密码后 master.key 解不开，进程会崩溃自重启循环，HTTP 可能短暂 200/401 但实际不可用，这一行会标 `LOCKED`。

**`serve uninstall`**：删 Task Scheduler 任务 + best-effort `taskkill` 残留 serve 进程。

#### 账户密码过期 footgun

Task Scheduler 存的是你**装任务当时**的 Windows 密码。**密码过期后任务起不来**（凭据失效）——这是 `LogonType=Password` 的固有代价（换来 boot 时无需人登录就能起）。单用户本地账户（NUC10 / 家用服务器）建议直接禁用密码过期：

```powershell
wmic UserAccount where Name='<你的用户名>' set PasswordExpires=False
```

域账户通常有强制密码策略，不能这么搞——定期重装任务（`serve uninstall` → 改密码 → `serve install`）是唯一的运维路径。

### 威胁模型（诚实，非 regression）

- ⚠ **master.key 对同用户（`allan716`）跑的任意进程不保密**——任何 `allan716` 启动的进程都能 `CryptUnprotectData` 解开 `master.key`。**这与 v0.2.0 的 keychain 等级完全相同**（keychain 对同用户进程也不设防），**不是 regression**。
- ✓ **master.key 对同机其他用户保密**（user-scope DPAPI 绑用户 SID）。
- ✓ **master.key 对 agent 保密**（agent 进程无 master key，走 broker；L2 模型不变）。
- **新增信任根**：`%AppData%\ssh-manager\master.key`（+ `cache-dek.key`）——物理 / 同机进程访问控制是它的防线（文件夹 ACL `icacls /inheritance:r /grant "<user>:(OI)(CI)F"`，不靠 Go 的 0600 位——Windows 忽略 mode 位）。

### 限制

- **Windows only**：DPAPI master key + `serve install`（Task Scheduler）只在 Windows 实现。Linux/macOS 继续用 keychain（`KeyringKeyProvider`）+ `FileKeyProvider` 兜底（无 keychain 的 headless 环境），**`serve install` 在 Linux/macOS 报 `not yet supported`**（spec §3.4 / §9 defer 到专门 plan，见 [multi-machine.md 的 Linux/macOS 章节](./multi-machine.md#linuxmacos-尚未支持)）。
- **迁移必须交互式 session**：v0.2.0 → DPAPI 的迁移（`unlock` 触发）依赖读旧 keychain slot，只在本地终端 / RDP 通；sshd / 非交互 session 读不出（1312），程序会提示重跑（不自动生成新 key）。
- **admin 重置密码 = master.key 失效**：见上"密码变更"——重置前必须 export，否则只能重设 vault。
- **serve install 需交互式 session 装**（PowerShell `Get-Credential` 要弹窗）——不能在纯 sshd session 装。
- **`serve install` 不是跨平台的**：Linux systemd / macOS launchd 各有平台陷阱（linger 权限、D-Bus session、LaunchAgent 仅 GUI login 后启动），未实现。
