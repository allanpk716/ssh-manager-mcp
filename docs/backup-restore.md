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

- 文件格式：`internal/vaultio`（`SSHMGRV1` magic + Argon2id + AES-256-GCM）封 `store.Snapshot`（version 1 的 JSON）。这套**序列化格式后续会被复用**：群晖定时自动备份、客户端只读缓存（多机路线）都会用同一份 Snapshot + 信封——所以 export 不只是个备份功能，它给后续打了地基。
- 这是 Plan 11（export / import）。多机支持的离线缓存 / vault 复制 / 群晖自动备份 / 迁移+enroll 是后续计划（未做）。

## 相关文档

- [getting-started.md](./getting-started.md)——单机从零到跑通（含 `store.db` 路径 + 基本备份说明）。
- [multi-machine.md](./multi-machine.md)——多机共享 serve 模式（实时同步，**不是**本篇的快照备份）。
- [agent-access.md](./agent-access.md)——project token 生命周期（导入后原 token 仍有效，见本篇"场景②"）。
- 仓库根 [README](../README.md)。
