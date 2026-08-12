# Plan 13 — NAS 定时备份（明文快照）— Design Spec

**Date:** 2026-08-12
**Status:** Design v3 — 两轮 xcheck + 用户拍板 + 第三轮评审收敛（8 条必改/应改全部落地）；pending implementation plan
**Worktree/branch:** `plan-13-nas-backup`

> 服务器的**可用性兜底**：把整个 vault 定时备份到挂载的群晖目录。明文 JSON 快照、
> 无变化不备份、按份数轮转、防挂载掉了静默写本地。灾难恢复 = 从 NAS 拷文件 +
> `ssh-manager import`。

## 1. 问题

服务器是 vault 的单点权威（serve 模式常驻）。目前**没有任何自动备份**——vault 丢了就丢了（除非手动 `export`）。需要一个自动化、保数据、可校验、可轮转的备份机制，灾难发生时能从 NAS 恢复。

## 2. 目标

服务器定时把整个 vault 以**明文 JSON 快照**形式写到挂载的群晖目录；vault 无变化时不产新文件；按份数轮转；防"挂载掉了静默写本地"；灾难时从 NAS 拷文件 + 现有 `import` 恢复。

## 3. 关键决策（三轮 xcheck 评审 + 用户拍板）

### 3.1 砍掉加密，存明文 JSON（本 plan 与最初设想的最大分歧，经评审验证成立）

**不加密备份文件，存 `store.Snapshot` 的明文 JSON。** 理由（用户论证，两轮三家异构评审认可）：

1. **职责分离**：SSH 凭据本来就不只在 vault 里——用户在 1Password/其他密码管理器早有副本（那是专业加密凭据库）。**备份的职责是可用性（防丢），机密性由密码管理器负责。** 不在备份里重复加密职责。
2. **避免复杂度根源**：加密 + skip-相同内容 两个需求打架（`vaultio.Encrypt` 随机 salt+nonce → 相同明文每次密文不同 → skip 必须解密比对或存明文 hash → 孵出"口令轮换→零可恢复备份"灾难 bug、明文 hash 指纹、口令离线存活等一堆问题）。砍加密让这些**全部消失**。
3. **NAS 受信**：群晖在 VLAN 内、外网不可达、永不开 Cloud Sync/快照复制/公网（用户确认，作为**部署硬约束**）。
4. **避免单点赌注**：不把所有信任压在 vault 一个程序上。

> **不是 regression**——之前根本没有备份功能。明文备份是"修不动的加密备份 < 一个正确的明文备份"的取舍（"备份你不能恢复的，等于没有备份"）。

### 3.2 明文备份的威胁模型（诚实，必写进文档）

明文文件 = **凭据明文 + 审计日志（可能含 agent 执行过的命令行，历史里或带过 API key/临时密码）+ 服务器拓扑 + profiles + host keys**。这些**不全在 1Password 里**（尤其审计日志）。所以明文备份暴露的范围比"1Password 冗余副本"更广。

**安全性的成立条件（三者同时）**：NAS 受信 ∧ server 受信 ∧ 挂载路径受信。

**适用**：受信 VLAN、无公网同步、目录权限锁死、物理介质单独保管。
**不适用**：开了 Cloud Sync / Drive / Universal Search / Snapshot Replication / 公网共享、多人共用 NAS、备份会拷到不受信介质。**（部署硬约束，违反则必须重新启用加密——见 §10 未来工作。）**

### 3.3 复用 Plan 11，零新加密逻辑

- `ExportSnapshot()`（Plan 11）——整个 vault 的 DTO。
- `ImportSnapshot()`（Plan 11）——恢复路径。
- 不碰 `vaultio`（加密包）。

### 3.4 Windows 一等公民 + 加固降级（用户 Q3，部署目标 = Windows）

核心备份功能全平台（写、skip、轮转、锁文件基础、marker、ORDER BY）。**加固项按平台分级**：

| 加固项 | Linux | Windows |
|---|---|---|
| temp 文件 0600 | ✅（有意义） | `0600` 位被忽略；靠 NTFS ACL（不程序校验，文档说明）|
| `--dir` 0700 校验 | **不校验，只 warn 建议**（与 Windows 一致；统一靠 ACL） | 同 Linux：跳过程序校验，文档强调"靠文件夹 ACL 隔离" |
| 文件 fsync（`f.Sync()`） | ✅ | ✅（`FlushFileBuffers`） |
| **目录 fsync（`dir.Sync()`）** | ✅ | **不支持**（Windows 无 sync 目录语义；跳过，不报错）|
| 锁回收 | **纯时间戳超时回收**（全平台，不分流；见 §3.8） | 同（无 build-tag 分流） |
| 定时器文档 | systemd timer | **任务计划程序（Task Scheduler）** |
| 挂载 NAS | fstab SMB mount | **UNC 路径 `\\host\share\backups`**（优先；`net use` 映射盘有 per-session 陷阱，见 §6.4） |
| `.git` 路径检测 | 只查 `--dir` 自身是否含 `.git`（不向上遍历；见 §5.2.2） | 同（无问题） |

> **v3 评审瘦身（codex+opencode 共识）**：原 v2 表的"锁回收 pid 判活"行（Linux signal 0 / Windows OpenProcess + build-tag 分流）**整行删除**——单机日级触发 + KB 级备份场景下，pidAlive 收益不抵跨平台复杂度，改纯时间戳超时回收（§3.8）。`--dir` 0700 校验行改为"Linux 也不程序校验、只 warn"，消除 v2 表与 §6.1 正文的矛盾。

### 3.5 无变化不备份（skip-if-unchanged）

算当前明文 JSON 的 SHA256，比最新一份备份边车的 `file_sha256`。相同则 skip（不产新文件）。

> **⚠️ 已知 gap（v3 评审 codex+pi 共识，诚实降级）**：`ExportSnapshot()` 的 JSON 含整个 `audit_log` 表，serve 模式每执行一条 SSH 命令就 append 一行 audit。**只要备份间隔内有 agent 活动，SHA256 就变 → skip 在活跃服务器上几乎永不触发。** "无变化不备份"**实际主要只在夜间空闲窗口 / 闲置期生效**；活跃时段每次产新备份，靠 §3.7 轮转兜底。
>
> **v3 选定方向（方向一：诚实降级，零代码）**：不拆 hash、不引入"skip 判据 ≠ 备份内容"的额外复杂度。skip 视为"空闲优化 + 长期静态 vault 的去重"，不是核心功能；rotation 才是兜底。文档（§7）明确此语义，避免用户/实现者对 skip 效果有错误预期。
>
> （被否决的方向二：skip 比对只 hash credentials+servers+profiles+grants+projects+host_keys 不含 audit，备份文件仍含完整 audit。代价是"skip 了但 audit 更新"时最新 audit 不在最新备份里，且备份文件内容 ≠ skip 判据，复杂度不抵收益。audit 是取证数据非恢复数据，但方向一已足够。）

### 3.6 依赖 marker 文件防"挂载掉了静默写本地"

`--dir/.ssh-manager-backup-marker` 必须存在（只查存在性），否则 fail-closed。文档钉死"**先挂载，后建 marker，marker 必须在挂载的 NAS 上**"（防先建 marker 再 mount 导致 marker 落 shadow、挂载掉时 shadow marker 露出 → fail-open）。

### 3.7 轮转 + UTC 时间戳

文件名 `vault-<UTC时间戳>.json`（`YYYYMMDD-HHMMSS` UTC，避 DST 撞名）；轮转按**文件名字典序**（= 时间序，免疫 NAS/rsync/同步重写 mtime）。留前 `--keep` 份，删其余（边车跟着删，忽略 not-found）。`--keep 0` = 禁用轮转（help 明确）。marker/锁永不动。

### 3.8 锁文件 + 陈旧锁回收（pi 的 blocker 修复，纯超时回收）

`O_EXCL` 建 `.ssh-manager-backup.lock`。**`O_EXCL` 锁不会进程退出时自动释放**——任何崩溃（SIGKILL/OOM/panic/Ctrl-C）留孤儿锁 → 后续撞锁 → 静默停摆。锁文件写 `<start-ts>`（**v3 瘦身：只留 start-ts**——单机单挂载点无跨机器竞争，`host`/`pid` 字段删；见下方"v3 评审瘦身"）。撞锁时判：**锁文件 `start-ts` 距今 > 超时窗口（默认 5 min）→ 判定孤儿/僵死 → 窃取陈旧锁重建**；否则视为真并发，exit 0 + "another backup in progress; skipping"。**不用 flock**（SMB 上不可靠）。

> **v3 评审瘦身（codex+opencode+pi 共识）**：
> - **砍 pidAlive 跨平台判活**：原 v2 计划 `lock_unix.go`（signal 0）+ `lock_windows.go`（OpenProcess）+ build-tag 分流。单机 Windows + 日级触发 + KB 级备份（毫秒级完成）场景下，pidAlive 的价值是"不用等超时、立即窃取"——但日级定时里"立即"和"等下次触发"无实际差别。O_EXCL + 时间戳超时回收已覆盖所有崩溃场景（进程崩溃→孤儿锁→下次触发时已超时→窃取）。**纯超时回收，不分平台，不写 build-tag 文件**。
> - **超时窗口 30 min → 5 min**：KB 级 vault + LAN NAS 备份是秒级，30 min 前提（"备份可能跑 15 min"）不成立。5 min 对 KB 级绰绰有余；跑 5 min 没完 = NAS 挂了，不值得等。
> - **锁文件字段 `<pid> <host> <start-ts>` → `<start-ts>`**：单机部署无跨机器竞争，`host` 是死字段（增加"窃取要不要检查 host / 跨 host 算不算 stale"的概念复杂度无场景）；pidAlive 已砍，`pid` 也无用。只留 `start-ts` 供超时判定。

### 3.9 边车保留（codex+pi 共识：明文丢了 GCM 免费 bit-rot 防线）

`.sha256` 边车是**唯一**抓"合法 JSON 但字节已坏"（base64/字符串单字节翻转仍合法 JSON → 静默还原错误凭据）的机制。`json.Unmarshal` 抓不到这类。边车字段：**`file_sha256=<hex>`（只此一个字段）**。砍掉原计划的 `plaintext_sha256`（明文了不需要）；**v3 也砍掉 `size=<n>`**（pi：同 sha256 必同 size，size 仅对大文件预筛有意义，KB 级 vault 无收益）。

## 4. 非目标（v1 不做）

- **不挂载/不传输**（运维层 SMB/NFS/net use）。
- **不加密备份文件**（§3.1，核心决策）。
- **不做 `backup restore`**（= 手动拷文件 + 现有 `import`）。
- **不增量**（全量快照；vault 通常 KB 级）。
- **不事件触发**（纯定时，写路径零改动）。
- **CLI 改动后不提醒备份**。
- **cache_tokens 不纳入 Snapshot**（见 §11）。
- **不自动校验历史备份**（`verify` 按需；不塞进 `create` 怕拖慢定时）。

## 5. 设计

### 5.1 命令

- `ssh-manager backup create --dir <挂载点> [--keep 7] [--prefix vault]`
- `ssh-manager backup verify <file>`
- 不做 `backup restore`。

### 5.2 `backup create` 流程

1. **marker 检测**：`--dir/.ssh-manager-backup-marker` 必须存在（只查存在性），否则 fail-closed 报错退出。
2. **`.git` 路径护栏**（用户 Q3 同意；v3 简化）：**只查 `--dir` 自身是否含 `.git`**（不做向上祖先遍历——NAS 挂载点落在某 git repo 子目录的概率极低，向上遍历收益不抵复杂度）。命中 → fail-closed（防明文误提交 git）。跨平台 `filepath` 实现。
3. **文件锁 + 陈旧锁回收**（§3.8，纯超时回收）：
   - `O_EXCL` 建 `--dir/.ssh-manager-backup.lock`（内容只有 `<start-ts>`）。
   - `O_EXCL` 失败（`os.ErrExist`）→ 读锁文件 `start-ts` → **判超时（5 min）**：超时 → 孤儿/僵死 → 窃取重建；未超时 → 真并发 → exit 0 + "another backup in progress; skipping"。**不调 pidAlive，不分平台**（v3 瘦身，见 §3.8）。
4. **解锁 vault**（master key，和 serve 同路径：`SSHMGR_MASTERKEY_HEX` env 或 keychain）→ `ExportSnapshot()` → `json.MarshalIndent` → 算明文 SHA256。**复用同一 `[]byte` 算 hash 和写盘，不 re-marshal**。
5. **skip 判定**：找最新一份 `<prefix>-*.json`（按文件名字典序最大，glob 严格 `*.json` 不匹配 `.sha256`），读其 `.sha256` 边车的 `file_sha256` → 相同 → "vault unchanged; skipping"，删锁退出 0。
   - **边车缺失/读不了 → 当作"无可比对"出 new 备份**（fail-open，不报错停摆）。
   - **（v3 删）原"可选自愈：无边车时直接 hash .json"已删**——与 §3.9"边车是唯一完整性防线"矛盾（边车缺失 = 该 .json 无完整性保证，hash 它做 skip = 基于可能已坏的数据决策）。fail-open 已安全简单。
   - **注意 skip 的现实语义**（§3.5）：活跃服务器因 audit_log 持续增长，skip 主要只在空闲窗口触发；这是已知降级，不是 bug。
6. **原子写新备份**：`vault-<UTC时间戳>.json`（temp + `os.Rename`，0600；**Windows 上 0600 位被忽略，靠 ACL**）。同秒撞名 → 加 `-2`/`-3` 后缀（既保持原子写又避免覆盖）。**失败路径（磁盘满 / rename 失败）须 `defer os.Remove(tempFile)` 清理**（v3 补）。
7. **fsync 耐久性**（codex）：`f.Sync()` temp 文件 → rename → `os.Open(dir).Sync()`（**Linux**；Windows 跳过 dir fsync，不报错）。
8. **写边车** `<file>.sha256`（0600，key=value 一行）：**`file_sha256=<hex>`（只此字段，v3 删 size）**。
9. **写后验证**（codex/pi）：`json.Unmarshal` 重读落盘 `.json` 回 `store.Snapshot`（查结构损坏 / 写半截——这是主要价值）。另含一次 SHA256 重算断言 == 边车（窗口窄、抓 bit-rot 概率低，保留作防御纵深但非核心加固）。**不做字段级校验**（数据源是 live vault，已强不变量；重做会漂移）。
10. **轮转**：列 `<prefix>-*.json`（glob 严格 `*.json`）按文件名降序留前 `--keep` 份，删其余（边车 `jsonpath + ".sha256"` 跟着删，`os.Remove` 忽略 not-found）。`--keep 0` = 不删。marker/锁不匹配 glob，永不动。**孤儿边车清理**（v3 补）：轮转末尾 best-effort 扫一遍无对应 `.json` 的 `.sha256` 残留（上次中断留下），删之，避免缓慢堆积。
11. 删锁，退出 0。

### 5.3 `backup verify <file>` 流程

- 读 `<file>.sha256` 的 `file_sha256` → 重算落盘文件 SHA256 → 比对（检测 bit-rot / 文件坏）。
- `json.Unmarshal` 回 `store.Snapshot`（查结构损坏）。
- 不一致 → 报错退出非零。
- 不用口令（明文）。

### 5.4 `import` 加格式嗅探（支持明文 .json）

**流程顺序（v3 钉死，修 UX 倒退）**：`import <file>` 必须**先读文件 → sniff → 仅加密分支才 prompt passphrase**。现状 `import.go:30` 无条件先 `passphrasePrompt()` 再解密；嗅探支持明文后若不改顺序，明文路径会无谓弹口令（UX 倒退）。

嗅探规则（读文件前 8 字节，**sniff 前先 lstrip 空白 / 去 UTF-8 BOM**——v3 补，防用户手改 JSON 带 BOM/前导空白导致首字节≠`{` 误判为加密）：
- `SSHMGRV1` → Plan 11 加密文件 → **此时才** `passphrasePrompt()` → 走原口令解密路径（`vaultio.Decrypt`）。
- 否则 → 当明文 JSON 直接 `json.Unmarshal`（不 prompt）。
- JSON 首字节必是 `{`（0x7B），`SSHMGRV1` 首字节 `S`（0x53），正向零误报（lstrip 后）。
- **歧义注释**（pi）：Plan 12 cache 文件也用 `SSHMGRV1` magic（`EncryptWithKey`），误把 cache 文件喂 import → 当口令文件解 → GCM auth 失败（安全，但不是用户意图）。`import.go` sniff 处加注释说明此假设。
- 嗅探逻辑放 `vaultio` 导出 helper（如 `vaultio.IsEncrypted(data []byte) bool`，helper 内部含 lstrip/BOM 处理），不内联 magic 到 `cli/import.go`。

### 5.5 ExportSnapshot ORDER BY 修复（共识，独立必修）

`internal/store/export.go` 的 credentials/profile_servers(grants)/projects 三查询加 `ORDER BY id`（grants 是 `ORDER BY profile_id, server_id`）。**v3 加固（opencode 验证）**：现有 `servers`/`profiles` 的 `ORDER BY name`（export.go:118,171）**也改为 `ORDER BY id`**——name 不保证唯一，SQLite 对等值 key 的行序不稳定 → 仍会破坏确定性 → skip 失效。**统一用主键 `id`（必唯一）**，根治。这是 ExportSnapshot 自身的**确定性 bug**——不修则相同 vault 每次序列化字节序可能不同 → skip 永远失效 → NAS 堆满相同备份。也修了 export/import/cache 的确定性。

### 5.6 数据模型

无新表。复用 `store.Snapshot`（Plan 11）。**cache_tokens 不进 Snapshot**（§11）。

### 5.7 文件布局

- `<--dir>/vault-<UTC>.json` — 明文快照（0600/ACL）
- `<--dir>/vault-<UTC>.json.sha256` — 边车（`file_sha256=`）
- `<--dir>/.ssh-manager-backup-marker` — marker（运维建，一次性）
- `<--dir>/.ssh-manager-backup.lock` — 锁（运行时，内容只有 `<start-ts>`，供 5 min 超时判定；v3 砍了 pid/host）

## 6. Windows 适配细节（§3.4 展开）

### 6.1 权限位降级
- temp 文件 `os.OpenFile(..., 0600)`：Linux 有意义；Windows 位被忽略（`FileMode` 在 Windows 只区分 readonly vs rw）。**Windows 不程序校验目录权限**，文档强调靠 NTFS ACL（文件夹属性 + 共享权限）。
- 不做"校验 `--dir` 是 0700"——Linux 上改为"建议但 warn"，Windows 跳过。

### 6.2 fsync 分平台
- 文件 `f.Sync()`：全平台（Windows = `FlushFileBuffers`）。
- 目录 `os.Open(dir).Sync()`：Linux 有效；**Windows 不支持（目录句柄不能 FlushFileBuffers），跳过 + 不报错**。代码用 `runtime.GOOS == "windows"` 分流，或 `dir.Sync()` 忽略特定错误。

### 6.3 锁回收（v3 瘦身：不分平台，纯超时）

**v3 删除原 pidAlive 分平台方案**（`lock_unix.go` signal 0 + `lock_windows.go` OpenProcess + build-tag 分流）。锁回收统一为：读锁文件 `start-ts` → 超过 5 min 即窃取。无 build-tag 文件、无 `golang.org/x/sys/windows` 依赖、无跨平台判活测试。理由见 §3.8（单机日级触发 + KB 级毫秒级备份，pidAlive 收益不抵跨平台复杂度）。

### 6.4 定时器文档（任务计划程序）

**`--dir` 用 UNC 路径，不用映射盘号**（v3 必改，pi 指出的 per-session 陷阱）：映射盘号（`Z:`）是 per-user/per-session 的，任务计划程序以别的 user 或 SYSTEM 跑时看不到 `Z:` → marker fail-closed 表现为"备份永远不跑"（无人值守典型静默失败）。**UNC 路径 `\\synology\backups` 绕开此问题**，任何 session 都可达。

Windows 用 `schtasks` 注册（UNC 路径示例）：
```cmd
schtasks /Create /SC DAILY /ST 03:30 /TN ssh-manager-backup ^
  /TR "ssh-manager.exe backup create --dir \\synology\backups --keep 7" ^
  /RU <user> /RP <password>
```
- `/RU` 用户的 master key 要么在 keychain 要么用 `setx SSHMGR_MASTERKEY_HEX`。
- **勾"超过 X 分钟停止任务"**（v3 补，pi/codex）：SMB 写挂起无应用层超时，NAS 卡住会无限挂进程；任务计划层的硬超时是兜底（陈旧锁超时只救下次运行，救不了当前卡死）。建议 X = 10 min。

Linux 仍给 systemd timer 模板（`Type=oneshot` 防 timer 自身重叠；`TimeoutStartSec=` 同理兜底 SMB 挂起）。

## 7. 安全考虑（写进文档，诚实）

- **明文 = 凭据明文**：适用/不适用见 §3.2。
- **NAS 受信是部署硬约束**：违反（开 Cloud Sync/公网）→ 必须重新启用加密（见 §10 未来工作）。
- **运维 footgun**：禁 `cat`/`grep -r password`（用 `backup verify` 或恢复到测试 vault 后 `servers ls`）；`--dir` 绝对路径非 git；仓库 `.gitignore` 发模板 `vault-*.json` + `*.sha256`。
- **master key 来源**（部署文档）：backup unit（systemd 或 schtasks）要能解锁 vault——要么和 serve 同 user 会话（keychain 可达），要么 `SSHMGR_MASTERKEY_HEX` 注入（systemd `Environment=`/`EnvironmentFile=`，或 Windows `setx`）。
- **长期静态 vault**：skip 让你只握 1 份不刷新文件 → 依赖定期 `backup verify` + NAS 快照做底层兜底（文档点明）。
- **审计日志明文 = 新暴露（不是单纯扩张）**（v3 措辞修正，opencode）：`audit_log.command` 原样导出（export.go:87,231），含历史命令行——可能携带**一次性 token / 临时密码 / 无副本 secret**，这些**不在 1Password 里**，对密码管理器而言就是新暴露，不是 §3.2 "blast radius 扩张" 能轻描淡写的。部署文档必须把"审计日志含历史命令 secret"列为**独立风险项**（与 §3.2 措辞统一）。不改设计（明文决策已定），但风险定性与文档措辞必须诚实。

## 8. 测试

- **`internal/store/export_test.go`**（修 ORDER BY）：相同 vault 连续 `ExportSnapshot` → JSON 字节完全一致（确定性）；删除+重插一个 credential 后 → 仍一致（顺序由 ORDER BY 保证，非 rowid）。**v3 加场景**：插两个 `name` 相同的 server/profile → 连续导出仍字节一致（验证统一 `ORDER BY id` 而非 `name` 的必要性）。
- **`internal/cli/backup_test.go`**（新）：
  - `create` → 产 `.json` + `.sha256` 边车（边车只含 `file_sha256=` 一行，无 size）；marker 缺失 → fail-closed；`--dir` 自身含 `.git` → fail-closed（不测向上祖先）。
  - **skip**：第二次 `create`（vault 未变）→ 不产新文件（exit 0）；改 vault 后 → 产新文件。**注**：skip 在 audit_log 增长时不触发是已知语义（§3.5），测试用关闭 audit 写入或清空 audit 的 vault 构造"未变"。
  - **轮转**：`--keep 2` + 3 次变化 → 留最新 2 份（边车跟着删）。
  - **孤儿边车清理**（v3）：预置一个无对应 `.json` 的 `.sha256` 残留 → 轮转后它被 best-effort 删除。
  - **锁**：两个 `create` 并发（goroutine）→ 一个 exit 0 skip、一个正常完成。
  - **陈旧锁回收**（v3 改）：手动写一个 `<start-ts>` 为"5+ min 前"的锁文件 → 下次 `create` 窃取并正常完成（不 stuck）；写一个"刚建"的锁 → 下次撞锁 exit 0 skip（不窃取）。**不测 pid 判活**（已砍）。
  - **同秒撞名**：模拟 → `-2` 后缀。
  - **写后验证**：落盘文件被篡改（flip byte）→ 边车 hash 比对失败（不静默）。
  - **temp 清理**（v3）：rename 失败/磁盘满路径 → temp 文件被 `defer os.Remove` 清掉，不留残。
  - **Windows 跳过**：`runtime.GOOS == "windows"` 时 dir fsync 跳过（不报错）——用单元测试 + build-tag 隔离。
- **`internal/cli/import_test.go`**（加嗅探）：明文 `.json` → 走 unmarshal（**不弹 passphrase**，v3 验证 prompt 重排）；`SSHMGRV1` 加密文件 → 弹 passphrase 走原口令路径。**v3 加**：明文 JSON 带 UTF-8 BOM / 前导空白 → 仍识别为明文（sniff lstrip 生效）。
- **`internal/cli/backup_verify_test.go`**（新）：`verify` 检测 bit-rot（flip byte）→ 非零退出。
- **（v3 删）原"跨平台锁回收 lock_unix_test / lock_windows_test"** —— pidAlive 已砍，无此测试。
- **No-regression**：`go test ./...` green；`gofmt -l .` 干净；`go vet ./...` clean。

## 9. 实现触点（file-by-file）

| 文件 | 改动 |
|---|---|
| `internal/store/export.go` | credentials/profile_servers/projects 三查询加 `ORDER BY id`（grants 用 profile_id,server_id）；**现有 servers/profiles 的 `ORDER BY name` 也改 `ORDER BY id`**（§5.5 v3 统一主键）|
| `internal/cli/backup.go`（新）| `newBackupCmd()`（create + verify）；marker 检测；`.git` 自身护栏；纯超时锁回收；skip；原子写（temp defer remove）；轮转（含孤儿边车扫）；写后验证 |
| `internal/cli/backup_test.go`（新）| 见 §8 |
| `internal/cli/lock.go`（新）| **唯一锁文件**：O_EXCL 建/读、内容 `<start-ts>`、5 min 超时回收。**v3：无 lock_unix.go / lock_windows.go，无 pidAlive，无 build-tag 分流** |
| `internal/cli/import.go` | 格式嗅探（`SSHMGRV1` vs 明文 JSON）；**prompt 重排**（读→sniff→仅加密分支才 passphrasePrompt，§5.4）|
| `internal/vaultio/vaultio.go` | `IsEncrypted(data []byte) bool`（导出 magic 检测 helper，含 lstrip/BOM）|
| `internal/cli/root.go` | 注册 `newBackupCmd()` |
| docs（新）| 部署硬约束、marker 顺序、systemd/任务计划（**UNC 路径 + 超时停止**，§6.4）、恢复、Windows 适配、skip 语义降级（§3.5）、审计日志新暴露风险（§7）、限制 |

## 10. 未来工作（显式 deferred）

- **重新启用加密**：若违反部署硬约束（需 Cloud Sync/公网），回加密版（codex 第 1 轮的 decrypt-and-compare skip 方案）。
- **cache_tokens 纳入 Snapshot**（§11）：若要"恢复到出事前一模一样"。
- **fsync 根治 Windows**：探索 Windows 替代（如 `FILE_FLAG_WRITE_THROUGH`）。
- **字段级写后校验**：若 vault 不变量演化。
- **backup restore 一条龙**：若恢复高频（目前 = 拷 + import）。

## 11. cache_tokens 范围（v1 决策）

**cache_tokens 不纳入 Snapshot（v1 文档化为显式非目标）。** 恢复后需 `cache-tokens add` 重发各工作机设备授权码。

理由：cache_tokens 是**设备身份**（语义上不属于"保险柜内容"）；纳入需动 Plan 11 的 Snapshot 结构（version bump）；重发成本极低（owner 一条命令 + 每台机 `cache pull` 一次）。

文档（recovery 章节）明确："DR 恢复 vault + project token（agent 的 .mcp.json 不用动）；cache-tokens 需 `cache-tokens add` 重发，各工作机 `cache pull` 重拉。"

## 12. 落地前 checklist（评审必修项映射）

- [x] O_EXCL 锁 + 陈旧锁回收（§3.8 / §5.2.3 / §6.3）—— pi blocker；**v3 瘦身：纯超时回收（5 min），无 pidAlive / 无 build-tag 分流 / 锁文件只留 start-ts**
- [x] ExportSnapshot ORDER BY（§5.5）—— 共识；**v3 加固：servers/profiles 的 `ORDER BY name` 也统一为 `ORDER BY id`**
- [x] `.sha256` 边车保留（§3.9）—— codex+pi；**v3：删 size 字段，只留 file_sha256**
- [ ] fsync 文件（§5.2.7）—— codex
- [ ] 写后校验（§5.2.9）—— codex/pi；**v3：以 json.Unmarshal 抓结构损坏为主，hash 往返保留但非核心**
- [ ] temp 文件 0600（§5.2.6，Linux）/ ACL（Windows，§6.1）—— codex；**v3：补 defer os.Remove 清理**
- [ ] skip + rotation 统一文件名排序（§3.7 / §5.2.5）—— 共识
- [ ] UTC 时间戳（§3.7）—— 三家
- [ ] marker 文档钉死顺序（§3.6）—— 三家
- [ ] `.git` 路径护栏（§5.2.2）—— claude Q3；**v3 简化：只查 `--dir` 自身含 `.git`，不向上遍历**
- [x] Windows 分平台（§6）—— 用户 Q3；**v3：fsync 仍分平台（dir fsync Windows 跳过），但锁回收不再分平台；schtasks 用 UNC 路径 + 超时停止**
- [x] 安全模型文档（§7）—— 三家；**v3：审计日志定性统一为"新暴露"（非"扩张"），列为独立风险项**
- [x] **v3 新增必改**：
  - [ ] skip 语义诚实降级（§3.5，audit_log 让 skip 名不副实）—— codex+pi 共识
  - [ ] import sniff 重排 passphrasePrompt + lstrip/BOM（§5.4）—— opencode+pi
  - [ ] 孤儿边车 best-effort 清理（§5.2.10）—— opencode
  - [ ] §3.4 表格 vs §6.1 矛盾消除（0700 统一 warn 不校验）—— codex

## 13. 参考

- 三轮 xcheck 评审综合：`.xcheck/20260812-071531/SUMMARY.md`（第 1 轮，加密版）、`.xcheck/20260812-074658/SUMMARY.md`（第 2 轮，明文版）、`.xcheck/20260812-105930/SUMMARY.md`（第 3 轮，明文版瘦身，codex/opencode/pi 三家 SUGGEST_CHANGES，8 条必改/应改已落地到本 v3）
- Plan 11（export/import）：`internal/store/export.go`、`internal/vaultio/vaultio.go`
- Plan 12（cache）：`internal/store/cachetoken.go`（Snapshot 不含 cache_tokens）
