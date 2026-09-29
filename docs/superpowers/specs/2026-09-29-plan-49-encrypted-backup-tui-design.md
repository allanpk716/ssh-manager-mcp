# Plan 49 — 定时口令加密备份 + TUI 备份页（design spec rev0）

> 2026-09-29 grilling 定案。状态：**待 owner 终审**（流程从简，一轮终审，跳过盲评——owner 2026-09-29 拍板）。

## 0. 背景与动机

本会话实测发现：**生产 vault（NUC10 权威端）一份备份都没有在跑**。

- 代码侧能力齐全：`sshmgr export` / `import`（口令加密便携文件，Plan 11）与 `sshmgr backup create` / `verify`（NAS 明文定时备份，Plan 13）都已落地。
- 部署侧空白：NUC10 上 `schtasks` 全表查无 sshmgr 备份任务；`sshmgr-serve` 是 Windows 服务在跑（与备份无关）；常见位置无 `.sme` 导出文件。

同时，owner 给定的备份目标经实测与 Plan 13 明文备份的部署硬约束相撞（详见 §1），必须走加密路线。本 plan 做三件事：

1. 给 `backup create` 补上**加密模式**（复用 export 的口令加密信封），补齐轮转；
2. 新增**备份配置文件**作为程序内唯一事实源，计划任务 / TUI / 手工命令行三个入口同走一条路；
3. broker TUI 加第 6 页「备份」（状态展示 + 立即备份 + 校验 + 改配置）。

## 1. 实测事实（2026-09-29，NUC10 实机探查）

这些事实是本设计的直接依据，写在这里防未来读者疑惑"为什么不用现成的明文备份"：

| # | 事实 | 探查方式 |
|---|---|---|
| F1 | NUC10 有映射盘 `Z:` → `\\100.121.254.22\Comic`，但在 SSH 非交互会话中状态"不可用"，`dir` 该共享报凭据错误 | `net use`、`dir` 实测 |
| F2 | `100.121.254.22` 是 Tailscale 地址（100.x 段），ping 延迟 74ms——群晖不在 NUC10 同一局域网，是跨站点远端 | `ping` 实测 |
| F3 | 目标共享名 `Comic` 是媒体共享；owner 计划"自己用群晖映射"把本地目录复制上去（同步机制由 owner 自管，形态未知，可能是 Synology Drive Client 一类同步工具） | owner 口述 + 目录实查 |
| F4 | owner 指定的备份目录 `C:\WorkSpace\backup` 是 NUC10 本地普通目录（非挂载点/链接），已存在且**混放个人文档**（排查笔记等），不在任何 git 工作树内，磁盘余量 245 GB | `dir /A` 实查 |
| F5 | 生产 vault 位于 `C:\ProgramData\ssh-manager\`（固定路径 + ACL 硬化，Plan 16 路线），`master.key.plain` 为裸文件、ACL 保护 | 既有部署 + 目录实查 |

结论对照 `docs/backup-restore.md` 明文备份部署硬约束（"NAS 在受信 VLAN 内、外网不可达；永不开 Cloud Sync / Drive / 同步服务"）：

- F2 远端 tailnet 可达 → 违反"受信 VLAN"；
- F3 owner 自管同步恰恰是"永不开 Drive/Cloud Sync"禁止的那类机制；
- F4 明文凭据混进个人文档目录、再被同步出去 → 暴露面失控。

**三条全破 → 按文档自身纪律"违反则必须回加密版"，本部署走加密路线。**

## 2. 定案决策记录（grilling 结论）

| # | 决策 | 定案 |
|---|---|---|
| D1 | 备份格式 | **加密 `.sme`**（口令加密信封 + `store.Snapshot` 全量快照 JSON），不用明文 JSON |
| D2 | 备份目标 | `C:\WorkSpace\backup\sshmgr\`（专用子目录，与 owner 个人文档不混放）；到 NAS 的复制由 owner 用群晖映射自管，程序只管写到本地目录 |
| D3 | 调度 | Windows 任务计划程序，每天 03:30，任务层"超过 10 分钟强制停止"兜底；**部署期一次性安装**，程序与 TUI 均不提供装/卸 |
| D4 | 保留份数 | 30 份（约一个月），轮转程序内建 |
| D5 | 口令 | 生成强随机口令写入口令文件（NUC10 `C:\ProgramData\ssh-manager\backup.pass`，与 `master.key.plain` 同目录同 ACL 保护）；部署时明示 owner 一次，**owner 必须存进 1Password 等离机位置**（整机损毁时口令文件随机器消失，NAS 上的密文备份全部作废——无后门） |
| D6 | TUI | broker TUI 第 6 页「备份」：状态区 + `[b]` 立即备份 + `[v]` 校验最新 + `[e]` 编辑配置；客户端角色的 TUI 不显示此页；不做调度器装/卸（只读探测任务存在性） |
| D7 | 底座统一 | 新配置文件为唯一事实源；`backup create` 扩展加密模式；三个入口（计划任务 / TUI / 手工）同走 `backup create`；并发锁复用现有 `acquireBackupLock` |
| D8 | 恢复 | 复用既有 `sshmgr import`（空 vault）原路；设备码（cache_tokens）不随备份走，恢复后逐设备重新发码入网（既有语义，预期行为非事故）；本 plan 验收含一次真实恢复演练 |
| D9 | 流程 | spec（本文档）→ owner 终审 → 实现 → 测试 → 发版 → 双端部署 → 首备份 → 恢复演练 → 验收册回写。盲评从简（跳过） |

## 3. 设计

### 3.1 备份配置文件 `backup.json`

- 路径：vault 目录下（Windows `C:\ProgramData\ssh-manager\backup.json`；Unix `/var/lib/ssh-manager/backup.json`；随 `SSHMGR_STORE`/固定路径逻辑走，代码用 `paths.VaultDir()`）。
- 内容（JSON，UTF-8）：

```json
{
  "dir": "C:\\WorkSpace\\backup\\sshmgr",
  "keep": 30,
  "passphrase_file": "C:\\ProgramData\\ssh-manager\\backup.pass"
}
```

- 三字段全部必填（文件缺失 = 未配置；`backup create --config` 报"先配置"或 TUI 引导，不猜测默认目录——备份写错目录比不备份更糟）。
- 路径必须是绝对路径。
- **唯一事实源**：计划任务、TUI、手工 CLI 全部经它取值。显式命令行旗标（`--dir` / `--keep` / `--passphrase-file`）仍可覆盖，优先级 = **旗标 > 配置文件 > 内置默认**（既有一键无配置的明文用法完全不受影响）。
- 调度时刻**不在**配置文件里——时序归外部任务计划程序管，两边不越界（TUI 探测任务存在性只读展示）。

### 3.2 CLI：`backup create` 扩展加密模式

```bash
sshmgr backup create --config C:\ProgramData\ssh-manager\backup.json   # 读配置：加密模式
sshmgr backup create --dir X --passphrase-file P --keep 30            # 旗标直给：加密模式
sshmgr backup create --dir X                                          # 无口令：既有明文模式，行为不变
```

判定规则：**`--passphrase-file`（含经 `--config` 解析出的）在场 → 加密模式**；缺席 → 现有明文 NAS 模式逐字节不变。

加密模式行为（与明文模式的差异点）：

| 环节 | 明文模式（现状，不动） | 加密模式（新增） |
|---|---|---|
| 产物 | `vault-<UTC时间戳>.json` 明文 JSON | `vault-<UTC时间戳>.sme`（同一 `prefix` 语义） |
| 封装 | 直接写 JSON | `store.ExportSnapshot()` → JSON → `vaultio.Encrypt(口令, JSON)` |
| skip（无变化跳过） | 最新备份 SHA256 相同则跳过 | **不适用**——密文含随机盐与随机数，每次必不同，比对无意义；每次都写新文件 |
| 写后验证 | 重读 + 重哈希 + 重解析 JSON | **重读 + `vaultio.Decrypt` 试解密 + 重解析 JSON**（GCM 认证标签保证：口令对且文件未损坏才解得开） |
| 轮转 | `--keep` 管 `*.json` | `--keep` 管 `*.sme`（按扩展名各自独立轮转；同目录混放两种格式互不误删） |
| marker 门槛 | 需要（防 NAS 未挂载） | 同样需要（防误指目录；本地目录部署时一次性建好 marker 即可） |
| .git 防线 / 并发锁 / 原子写 | 照旧 | 照旧（复用，不重写） |

新增 `--config <path>` 旗标：读 `backup.json`，取 `dir` / `keep` / `passphrase_file` 作为对应旗标的缺省值；文件不存在 / 字段缺失 / 非绝对路径 / JSON 不合法 → 明确报错退出（fail-closed，不落半套行为）。

`backup verify <file>` 同步扩展：`.sme` 文件加 `--passphrase-file` 即可做"解密级校验"（解密 + JSON 重解析，两关都过才报 ok）；无口令时对 `.sme` 明确报"加密文件需要 --passphrase-file"。明文 `.json` 分支行为不变。

### 3.3 口令文件 `backup.pass`

- 内容：单行口令文本（尾随换行容忍，与 `readPassphraseFile` 既有语义一致）。
- 生成：部署期由程序外一次生成（强随机，≥32 字符）；写入后靠 vault 目录 ACL 保护（该目录已按 Plan 16 硬化：仅 SYSTEM + Administrators + 当前用户，禁继承——新文件继承同款保护）。
- **离机副本是硬性运营前提**（D5）：口令只活在这一台机器上 = 整机损毁时全部备份作废。部署时明示 owner 存入 1Password；验收册登记该动作。
- `--passphrase-file` 复用 `internal/cli/export.go` 的 `readPassphraseFile`（同一实现，不复制）。

### 3.4 调度（部署期一次性，非运行时功能）

任务计划程序任务 `sshmgr-backup`：

```
动作:  sshmgr.exe backup create --config C:\ProgramData\ssh-manager\backup.json
触发:  每天 03:30
账户:  当前用户（vault 目录 ACL 已授当前用户读权限，无需提权）
超时:  "超过 10 分钟停止任务"（SMB/磁盘挂起兜底——本部署是本地目录，此条是通用卫生）
```

安装动作用部署 runbook 里的一条 `schtasks /Create` 完成（本文档 §8），**不**做 `backup install-schedule` 之类的命令。理由：调度器装/卸是 Windows 任务计划的维护面（凭据、触发器语义、跨平台差异），一次性动作不值得背上长期维护成本；TUI 只读探测任务存在性（`schtasks /Query /TN sshmgr-backup` 是否命中），显示"已装 / 未装"，不提供任何写操作。

### 3.5 TUI 第 6 页「备份」

- 页枚举 `pageBackup` 追加到 broker App（`internal/tui/app.go` 页数组从 5 页变 6 页，Tab 循环自然覆盖）。**客户端角色的 TUI 入口不构造此页**（备份是权威端的事务；clientpage 路径不动）。
- 状态区（只读，进页/刷新时加载）：
  - 配置摘要：目标目录、保留份数、口令文件路径（来自 `backup.json`；文件缺失 → 整页显示"未配置"引导态，动作键仅 `[e]` 可用）；
  - 备份文件列表：`*.sme` 按时间倒序（时间、大小），最多列最近 10 份 + 总数；
  - 新鲜度：最新一份距今 >25 小时 → 警示色（调度没跑一眼可见）；
  - 调度探测：`sshmgr-backup` 任务已装 / 未装（只读）。
- 动作：
  - `[b]` 立即备份：后台 `tea.Cmd` 跑与 CLI 同一条 `backup create` 逻辑（同锁、同轮转、同验证），完成后刷新列表 + 状态行报结果（含失败原因，如 marker 缺失 / 口令文件读不出）；
  - `[v]` 校验最新：`vaultio.Decrypt` 试解密 + JSON 重解析（与写后验证同函数），结果报"完好 / 损坏 / 口令不符"；
  - `[e]` 编辑配置：huh 表单三字段（目录 / 保留份数 / 口令文件路径），保存即写 `backup.json`（原子写）。
- 该页所有远程副作用只有"写备份目录 + 写 backup.json"，不碰 vault 数据、不碰调度。

### 3.6 恢复（思路与演练）

恢复走既有原路，零新增代码：

1. 灾备机（或修复后的 NUC10）装 sshmgr → `sshmgr unlock`（建新 master key）；
2. 从 `C:\WorkSpace\backup\sshmgr\`（或群晖副本）取最新 `.sme` → `sshmgr import <file>`（交互输口令，或 `--passphrase-file` 指向离机副本）；
3. 设备码不随备份走：各工作机下次回连拿 unknown 401 被批量切断（Plan 34 语义，预期行为），逐台 `sshmgr cache-tokens add --name <设备>` 重发 + `cache pull` 重新入网；agent 的 `.mcp.json` 不用动（project token 在备份里）。

**恢复演练**（D8，验收必做）：在 NUC10 上用 `SSHMGR_STORE` / `SSHMGR_FILEKEY_PATH` 指向临时目录建一次性 vault → import 最新 `.sme` → 核对服务器/凭据/profile/项目条数与生产一致 → 抽凭据解密验证 → 清理临时目录。全程不碰生产库。

## 4. 格式与版本兼容（owner 2026-09-29 提问的正面回答）

**不造任何新格式。** 加密备份文件与 `sshmgr export` 产物**逐字节同构**：

- 信封层：`vaultio` 的 `SSHMGRV1` magic + 盐 + 随机数 + AES-256-GCM 密文（`internal/vaultio/vaultio.go`）。magic 本身就是信封版本号——未来若升信封（如 `SSHMGRV2`），解密端按 magic 分发，旧信封必须继续可解。
- 载荷层：`store.Snapshot`（`internal/store/export.go`，`version` 字段当前为 1）。既有纪律：导出端写当前版本；导入端（`import`）接受 `version ≤` 当前值，未来升快照版本时必须继续收旧版（向后兼容是承诺，不是巧合）。

由此：**任何能 import 一份 `export` 文件的 sshmgr 版本，就能恢复任何 `backup create` 加密模式产出的备份**——因为它们是同一种文件。版本兼容的测试责任落在既有的 export/import 测试面上，本 plan 只需加一条"backup 加密产物可被 import 恢复"的往返测试钉死这个等价性。

升级路径：旧版 sshmgr（无加密模式）遇到 `.sme` → `import` 的 `IsEncrypted` 嗅探已存在（Plan 11 起），行为不变；新版对旧明文 `.json` 备份 → `backup verify` 与 `import` 均不变。**无破坏性变更。**

## 5. 安全模型

| 资产 | 位置 | 保护 | 剩余风险（如实） |
|---|---|---|---|
| `.sme` 备份文件 | `C:\WorkSpace\backup\sshmgr\`（会被 owner 同步到远端群晖） | 口令加密（Argon2id 64 MiB + AES-256-GCM）。同步/泄露路径上流动的是密文 | 弱口令可离线爆破——所以口令由程序生成（强随机），非人选 |
| 口令 `backup.pass` | `C:\ProgramData\ssh-manager\`（不同步） | 目录 ACL 硬化（Plan 16：SYSTEM+Administrators+当前用户，禁继承） | 与 `master.key.plain` 同级：能读该目录的本地攻陷者本来就拥有 vault——**新增边际暴露 ≈ 0** |
| 明文快照 | 仅内存（export → 加密 → 落盘全程密文） | 同 export 既有纪律 | 无新增 |
| 审计日志 | 随快照进 `.sme`（密文） | 同上 | 对比明文 NAS 方案（审计明文落盘是独立暴露面）：加密路线天然消除 |

威胁不变式：备份目录与同步链路（owner 的群晖映射）**全程不需要被信任**——这是本设计对 F2/F3/F4 三个事实的结构性回答。

## 6. 测试计划

单元/集成（Go，既有风格）：

1. `backup create` 加密模式：产物以 `SSHMGRV1` 开头；marker/`.git`/锁/原子写路径全覆盖；
2. 轮转：`.sme` 保留 N 份、同目录 `.json`/`.sme` 混放互不误删、孤儿边车语义（加密模式无 `.sha256` 边车——轮转只管 `.sme` 本体）；
3. 写后验证：篡改一个字节 → 解密失败报错；口令错 → 失败；
4. `--config`：正常读入、字段缺失/相对路径/坏 JSON 各自 fail-closed；旗标覆盖优先级；
5. **往返等价**：`backup create` 加密产物 → `sshmgr import` 恢复 → 条数与字段全等（钉死 §4 的格式等价承诺）；
6. `backup verify` 对 `.sme` 带/不带 `--passphrase-file` 两分支；
7. TUI：备份页契约测试（未配置引导态、列表刷新、`[b]`/`[v]`/`[e]` 键位分发、角色门控——client 路径不显示第 6 页），沿用既有 TUI 测试基建；
8. 既有明文模式回归：无 `--passphrase-file` 时全部现有测试不改一行照绿。

真机（验收册登记，§7）。

## 7. 验收（验收册条目，含恢复演练）

前置：新版部署到 NUC10、`backup.json` / marker / `backup.pass` / 计划任务就绪。

| # | 步骤 | 通过判据 |
|---|---|---|
| A1 | 手工 `sshmgr backup create --config ...` | 写出 `vault-*.sme`；stdout 报 wrote；退出码 0 |
| A2 | `sshmgr backup verify <最新.sme> --passphrase-file ...` | ok |
| A3 | 连跑 keep+2 次 | 目录中 `.sme` 恒 ≤ keep 份，最旧被删 |
| A4 | TUI：备份页目验 | 状态区/列表/新鲜度/调度探测如实显示（**owner 人工保留面**：真终端观感） |
| A5 | TUI `[b]` 立即备份 + `[v]` 校验 | 新文件出现在列表；校验报完好 |
| A6 | **恢复演练**（§3.6） | 临时 vault import 成功，条数与生产一致，抽凭据解密通过，生产库零接触 |
| A7 | 口令离机保管 | owner 确认口令已存入 1Password（验收册记一行） |
| A8 | 次日 03:30 定时任务实跑 | 群晖侧（owner 自管）出现当日新 `.sme`；任务历史退出码 0 |

## 8. 部署顺序（发版后 runbook）

1. 发版（版本号顺延，`v0.18.0`）→ CI 双平台绿；
2. NUC10 自更新部署（`sshmgr update`，既有链路）→ `doctor` 全绿；
3. 建 `C:\WorkSpace\backup\sshmgr\` + 写入 marker 文件 `.ssh-manager-backup-marker`；
4. 生成强随机口令 → 写 `backup.pass`（落 vault 目录，随 ACL）→ **屏幕明示 owner 一次，存 1Password（A7）**；
5. 写 `backup.json`（§3.1 内容）；
6. `schtasks /Create` 装 `sshmgr-backup`（每天 03:30，超时强停）；
7. 手工触发 A1–A6；
8. 验收册回写 + compat-matrix 登记；次日 A8。

## 9. 非目标（明确不做）

- 调度器装/卸/管理（D3——部署期一次性，非运行时功能）；
- 明文 NAS 模式的任何行为变更（保留给满足其硬约束的其他部署用）；
- 增量备份 / 事件触发备份（全量定时，文件 MB 级）；
- 多 owner / 备份加密多人分持；
- 程序内管到群晖（复制是 owner 自管边界，程序只写本地目录）；
- 客户端角色 TUI 的备份界面（权威端事务）。
