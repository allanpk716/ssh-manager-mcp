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
- 这是 Plan 11（export / import）。**Plan 12（离线只读缓存）和 Plan 13（NAS 明文备份）都复用了本篇的 `Snapshot` DTO**。多机支持的 迁移+enroll 仍是后续计划（未做）。

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
