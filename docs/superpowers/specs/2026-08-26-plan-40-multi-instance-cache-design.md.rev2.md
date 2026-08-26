# Plan 40 设计：多实例离线缓存（同机多 agent 各授权各 profile）+ 锚事实/策略解离 + MAX_OFFLINE 持久化

> 2026-08-26 grilling 定案（两轮 11 问 + bug 插入，owner 全部确认），本文不重议的决策：**client 标准姿态 = cache-first 单入口**（stdio `mcp --cache` 为准；http 直连降为辅助形态——Q1a）、**离线授权时效 = `SSHMGR_CACHE_MAX_OFFLINE` 24h 维持 + 吊销纪律（先 device code 后 project token）文档化**（Q2）、**多 cache 实例做成一等公民**（owner 明确否掉"仅文档化 workaround"——Q3）、**接受每实例凭据落盘（ACL + 时效双纪律）**（Q4）、**实例身份键 = 设备码 name**（Q5A）、**`--instance` flag 为一等形态 + `SSHMGR_CACHE_DIR` env 保留为完全覆盖 escape hatch**（Q6A）、**无 `--instance` = 默认实例，存量零迁移**（Q7A）、**每实例一份 DEK**（Q8A）、**批次：CLI 闭环先行 / TUI·向导二批 / doctor 跟随**（Q9）、**P0 bug 修法 = A（pinned pull 永远记锚）+ C（MAX_OFFLINE 持久化 per-instance），B（meta merge）与 D（load 缺锚降级）否决**（Q10）、**持久化作用域 = per-instance、优先级 env > 文件、内容只搬 MAX_OFFLINE**（Q11）。
>
> **rev1（2026-08-26）**：一轮外部评审确认的 11 条问题全部吸收（3 高 / 5 中 / 3 低，含 6 条经查证/实验实证）。关键修订：① §2.1 name 白名单补 Windows 文件系统语义（大小写折叠唯一性、DOS 保留名、尾点约束——实测依据：保留名 `MkdirAll` 必败、NTFS 大小写不敏感使 `AGENTA` 与 `agentA` 解析同一目录）；② §4 威胁模型措辞收缩（MAX_OFFLINE 约束正常 loader 而非密码学时效；失窃的实际响应 = 轮换凭据）；③ §2.4/§8 批次重排（自动归位推迟至第二批，与向导 `--instance` 接入同批——消除"新 enroll 开箱即起不来"的中间窗口）；④ §2.4/§2.5 默认实例增设备码身份门禁（防异码静默覆盖——该覆盖行为在现状代码即存在，已实验复现）；⑤ §3 config 原子写钉死 + override 目录读取位置；⑥ 测试矩阵补 3 行（P0 拒绝不覆盖断言 / per-instance 401 隔离销毁 / config 并发读）；⑦ §9 残余补登 doctor。
>
> **rev2（2026-08-26，终版候选）**：二轮外部评审确认的 9 条问题全部吸收（对象 = rev1）。关键修订：① **§2.4 身份门禁语义完整化**——生效条件改为"默认目录 `cache.bin` 存在"（材料主体）；meta 不可读但 bin 在 → fail-closed 拒绝（堵损坏绕过）；**重绑定 = 清除默认目录全部 cache 材料（auth.json + cache.bin + cache.meta.json）后重新 enroll**（rev1 的"仅删 auth.json"会 loop 回门禁拒绝——身份记录在 meta 不在 auth）；auth 写序钉死（所有 `cache.auth.json` 写入必须位于 `DoPull` 成功之后——CLI pull 路径现状已如此（实验核实：门禁拒绝即提前 return，auth 不动），TUI 连接编辑表单换码是唯一先写 auth 的路径，其错位后果由门禁拒绝文案兜底，表单预防性警告归第二批 TUI 改造）；§6.8 补 auth 字节断言。② **§2.4/§5 老 serve（响应头缺失）× 无 `--instance` 行为钉死**：门禁跳过（无 name 可比，维持现状兼容，非新增敞口）+ 不归位落默认目录 + 升级提示。③ **§2.1 casefold 完整化**——查重范围扩**全历史（含 revoked，堵 revoke 后重发大小写变体与残留目录/DEK 碰撞）**、查重入事务（进程内 `MaxOpenConns(1)` 已天然串行，事务化是跨进程兜底）、**存量碰撞检测**（serve 启动时发现 active 大小写碰撞 → fail-closed 启动失败 + 引导 owner 改名）、client 端物理碰撞检测（目标实例目录已存在且 meta 身份不同 → 拒绝）；**保留名判定改首段**（casefold 后取首个 `.` 前基名比对——实测 `con.foo`/`COM1.x`/`nul.tar.gz` 过 rev1 全名等值检查但 `MkdirAll` 照样失败）。④ §2.2 `SSHMGR_CACHE_DIR`/`SSHMGR_CACHE_DEK` 与 `--instance` 同在 → **报错**（fail-closed 互斥——静默忽略 flag 即静默错位）。⑤ §3 `pull --max-offline` 写 config 时 env 在场 → WARNING 一行。⑥ §9 登记：跨进程 casefold 双插窗口（极小）、TUI 表单换码预防性警告归二批。
>
> 与 Plan 37（B 时限快照）的关系：本 plan 不改 Plan 37 的闸门机器（meta 三闸、销毁前复查、K=1h 双职），只改**锚的记录条件**与**策略的读取来源**；与 Plan 39（设备码绑 profile）的关系：实例模型直接建立在 Plan 39 的裁剪之上（实例 = 设备码 = profile 三位一体）。

## 0. 现状事实（2026-08-26 于 master 核实；函数名锚定，行号以当时为准）

1. **cache 机器级单份**：`CachePaths()`（clientops）返回 `SSHMGR_CACHE_DIR` env 覆盖或 `UserConfigDir()/ssh-manager/` 下的 `cache.bin` / `cache.meta.json` / `cache-audit.log`；`CacheCredPath()` 同目录下 `cache.auth.json`。一机一份——同机第二个 agent（不同 profile 的 project token）在 `mcp --cache` 的 token 验证处 spawn 即失败：裁剪 snapshot 的 `Projects` 只含设备码所绑 profile 的 projects（`ExportSnapshotForProfile`，store/export.go）。fail-closed 不越权，但第二个 agent 无离线能力。
2. **锚被 pull 进程的策略视图决定存亡（P0 bug 根因）**：`DoPull` 仅当 **pull 进程自己的** env `SSHMGR_CACHE_MAX_OFFLINE > 0` 时解析 pinned 200 的 `Date` 头记锚（`ServerAnchored=true`）；否则 `anchored=false` 且**无条件覆盖写** meta。client TUI `[s]同步` 走同一 `DoPull`，TUI 进程无该 env → 抹锚 → `LoadCacheSnapshot`（mcp 进程有 env）provenance 闸拒载 → ssh MCP 起不来 → agent 会话工具全消失。**循环陷阱**：错误文案让用户 "run cache pull"，用户再从 TUI/裸 CLI pull 又抹一次。这是 v0.10.0 部署记录"env 必须同时在拉取路径与加载路径"坑的第三个实例（前两个：计划任务 wrapper、`.claude.json` env 块）——每堵一个进程上下文就冒出下一个，证明该类问题不能靠"把 env 铺满所有进程"修。
3. **DEK 现状**：`paths.CacheDekPath()` = `SSHMGR_CACHE_DEK` env 覆盖或 `VaultDir()/cache-dek.key`（Windows `C:\ProgramData\ssh-manager\`）；`DekProvider()`（dek_unix/dek_windows 平台分装）包装之；`loadOrCreateDEK`/`loadDEK` 唯一消费。DEK 与 cache 目录**分离**（ProgramData vs UserConfigDir）——有意布局：cache 目录可能被同步/备份工具带走，DEK 不跟。DEK 首次生成后恒复用，**无轮换机制**。
4. **设备码 name 即天然主键——但只在 SQLite 语义下**：`cache_tokens.name` 有 UNIQUE 约束（`AddCacheToken` 事务先回收同名 revoked 行），建表为 `name TEXT NOT NULL UNIQUE` 无 `COLLATE NOCASE`（SQLite 默认 BINARY，**区分大小写**）；`handleSnapshot` 鉴权后 `GetCacheToken` 在手 `ct.Name` 与 `ct.ProfileID`。name 现为自由文本。store 连接池 `SetMaxOpenConns(1)` + WAL + 5s busy_timeout（store.go）——**进程内所有访问天然串行**。
5. **响应头先例**：Plan 39 已加 `X-Sshmgr-Snapshot-Scope: profile`（DoPull 读入 `meta.Scoped`）——实例 name 走同款通道。
6. **`CachePaths` 消费面**：15 处调用（cli/mcp、cli/cache、cli/clear、cli/doctor、clientops 内部、tui/clientpage 等）——实例化改造必须全量过一遍，漏一处即错位。
7. **角色判定读默认目录**：`roles.resolveMode` 的 `cachePresent()` 只查默认 cache 目录——只有命名实例的机器会被误判为 wizard 态。
8. **Plan 37 闸门机器（本 plan 不动）**：load 侧三闸（meta 读、provenance、超龄销毁前复查 + 回拨闸）；K=1h（skew 闸 + 回拨容差同常数）；提交顺序 bin→meta；plaintext pull 在 B-on 时被拒（锚需要 pinned TLS）。
9. **异码静默覆盖在现状即存在（实验实证，2026-08-26）**：`DoPull` 无任何写入前身份比对——双设备码先后 pull 同一默认目录，第二码的 bin/meta **静默替换**第一码材料（零警告零门禁；`cache.auth.json` 的覆盖发生在 CLI 层）。本设计必须同时关掉这条现状敞口。
10. **文件系统事实（实验实证，2026-08-26，Windows 实测）**：`MkdirAll instances/{CON,PRN,AUX,NUL,COM1,LPT9}` 全部报错（保留名死路）；`instances/AGENTA` 解析到与 `agentA` **同一目录**（NTFS 大小写不敏感）；`foo.` 尾点名 `MkdirAll` **成功**但被 Win32 规范化剥除尾点（与显式 `foo` 碰撞）；**`con.foo` / `COM1.x` / `nul.tar.gz` 同样过字符白名单但 `MkdirAll` 必败**（保留设备名判定作用于第一个点之前的基名——对照 `foo.bar`/`agentB` 双 OK）。
11. **`cache.auth.json` 的写序现状（代码核实）**：CLI `cache pull` 的 `WriteCacheCred` 位于 `DoPull` 成功分支**之后**（门禁拒绝即提前 return，auth 不动）；TUI `[s]同步` 用已存 cred 调 `DoPull`、失败不写 auth；**TUI 连接编辑表单保存**独立调 `WriteCacheCred`（换码即写，之后同步才撞门禁——唯一先写 auth 的路径）。

## 1. P0 —— 锚事实/策略解离（止血修复，可独立先行）

**原则：锚是事实（"server 在这次 pull 说几点"），不是策略（"要不要过期检查"）。pull 进程的 env 不配决定记不记事实。锚的安全前提本来就是 pinned TLS（Date 不可注入）——把记录条件从策略开关改回安全前提本身。**

### 1.1 变更点

`DoPull` 的锚分支条件从 `maxOffline > 0` 改为 `pin != ""`：

| pin | env B | 现状 | 新行为 |
|---|---|---|---|
| pinned | on | 锚（Date 解析 + skew 闸） | 锚（不变） |
| pinned | off | **不锚，覆盖写 false** | **锚**（修复点） |
| plaintext | on | 拒 pull（Plan 37 §2.1 明文拒） | 拒 pull（**不变**——保留） |
| plaintext | off | 不锚（`--allow-plaintext`） | 不锚（不变） |

- Date 解析失败 / skew 超限的拒绝分支**随锚分支走 `pin != ""`**：pinned 且 B-off 的 pull 从此也依赖 `Date` 头与 skew 闸。依据：Go `net/http` 服务端逐响应自动携带标准 `Date` 头（Plan 37 §0.3 已核实，含未升级旧 serve）；错锚比无锚更糟（回拨闸会拒载、且未过 skew 闸的锚不可信）——fail-closed 方向。
- **明文拒分支（`maxOffline > 0 && pin == ""` → 拒）保留**：B-on 的 load 侧仍要求锚，明文 pull 产不出锚，放行即制造"拉得动但载不动"半开状态（Plan 37 §1 明确不允许）。
- meta 结构、提交顺序（bin→meta）、`ServerAnchored` 无 omitempty 恒序列化——全部不变（`device_name` 字段见 §2.4，属多实例批次，不进 P0）。

### 1.2 存量自愈与迁移

无锚 meta（`server_anchored: false`，含本 bug 抹掉的）在新版下第一次**带 pin 的** pull 即重建服务器锚——与 Plan 37 §3.3 的从关到开迁移语义完全一致，无需专门迁移步骤。

### 1.3 运维注意（A/C 上线前的过渡期纪律，写入文档）

过渡期内（本修复未部署的机器）：恢复只跑**带 env 的** pull 通道（计划任务脚本）；**禁用 TUI `[s]` 同步与裸 CLI pull**（无 env 的 pull 会再抹一次锚）。

### 1.4 P0 回归测试矩阵

1. pinned pull、pull 进程无 env → `server_anchored=true`（bug 的直接回归）。
2. 端到端 bug 场景：无 env pull → 有 env `LoadCacheSnapshot` 成功（现状此组合失败）。
3. pinned pull、无 env、响应无 Date → 拒绝（新依赖的 fail-closed）。
4. pinned pull、无 env、skew > 1h → 拒绝。
5. plaintext + B-on → 仍拒（分支保留）；plaintext + B-off → `anchored=false`（不变）。
6. Plan 37 既有 B-on 矩阵全量不回归（expiry_pull_test / expiry_load_test 现有用例零改动通过）。
7. **拒绝路径零覆盖断言**：Date 缺失 / skew 超限拒 pull 时，既有 `cache.bin` / `cache.meta.json` 字节不变（sha256 前后比对）且旧 cache 仍按原策略可加载——该行为已在现状实验证实（拒绝发生在任何写盘之前），测试钉住防回归。

## 2. 多实例设计

### 2.1 实例模型

**实例 = 设备码 = profile 授权单元，三位一体。** 一台 client 机上 N 个 agent（各持不同 project token）→ N 个实例 → N 份独立 cache/DEK/设备码/审计/时效策略。

- **实例名 = 设备码 name**（owner 在 `cache-tokens add --name` 时定；运维台账与磁盘目录一一对应）。命名纪律建议 `机器-实例`（如 `laptop-agentA`），写文档不强制。
- **name 白名单（含 Windows 文件系统语义）**：
  - 字符集：`^[A-Za-z0-9]([A-Za-z0-9._-]{0,62}[A-Za-z0-9])?$`（首尾必须字母数字——封死尾点/尾连字符；长度 1–64）。
  - **保留名排除（首段判定）**：casefold 后取**首个 `.` 之前的基名**比对 DOS 保留设备名集合 `{CON, PRN, AUX, NUL, COM1-9, LPT1-9}`——实测 `con.foo`/`COM1.x`/`nul.tar.gz` 过全名等值检查但 `MkdirAll` 必败，判定必须作用于基名而非全名。
  - **大小写折叠唯一性（全历史 + 事务内 + 存量检测）**：server 端 `cache-tokens add` / `bind` 按 casefold（ASCII 小写）查重，**范围 = 全部历史行（含 revoked）**——revoke 后重发大小写变体会与残留实例目录/DEK 碰撞，必须终身占用；查重与 INSERT **同事务**（store 连接 `MaxOpenConns(1)` 已使进程内天然串行，事务化是跨进程双开的兜底——残余窗口见 §9.8）；**存量碰撞检测**：serve 启动时扫描 cache_tokens 发现 active 行之间存在 casefold 碰撞（升级前 BINARY UNIQUE 时代遗留）→ **fail-closed 启动失败** + 引导 owner 手工改名（`cache-tokens revoke` + 新名 add）。
  - client 端 pull 消费 name 前做同款字符白名单 + 首段保留名校验（防御存量非法 name），非法 → 拒写盘，文案引导 owner 改名重发；**物理碰撞检测**：目标实例目录已存在且其 meta 身份 ≠ 本次 name → 拒绝写盘（防手工搬动目录/异源残留）。
- 双端校验动机：name 来自 server 响应用作目录/文件名——白名单封死 `../` 类路径穿越与非法文件名字符；折叠唯一性（全历史）封死"两个授权一个目录"与"重发变体撞残留"；保留名（首段）封死"起名即死路"的全部形态。

### 2.2 目录布局与路径解析

```
UserConfigDir()/ssh-manager/                    ← 默认实例（现状零变化）
├── cache.bin / cache.meta.json / cache.auth.json / cache-audit.log / cache.config.json(§3 新增) / quarantine/
└── instances/<name>/                           ← 命名实例（每实例同构一套）
    ├── cache.bin / cache.meta.json / cache.auth.json / cache-audit.log / cache.config.json / quarantine/

VaultDir()/                                     ← DEK 布局（保持与 cache 分离）
├── cache-dek.key                               ← 默认实例 DEK（现状）
└── cache-dek-<name>.key                        ← 命名实例 DEK
```

- **路径解析优先级**：`SSHMGR_CACHE_DIR`（显式路径，**完全覆盖**——既有 escape hatch 语义不变）> `--instance <name>`（实例目录）> 默认目录。
- **env × flag 互斥（rev2）**：`SSHMGR_CACHE_DIR` 或 `SSHMGR_CACHE_DEK` 显式设置**且** `--instance` 显式给出 → **报错**（fail-closed 互斥，冻结文案指明二者只能其一）——静默忽略 flag 正是本设计反对的"静默错位"（可能路由到错误 cache 或令多实例共享同一 DEK）。要走 env 的测试/迁移场景删掉 flag 即可。
- 实现形态：`CachePaths()` → `CachePathsFor(instance string)`（`CachePaths()` ≡ `CachePathsFor("")`，15 个存量 caller 零改动兼容）；`paths.CacheDekPath()` → `CacheDekPathFor(instance)`；`DekProvider()` 同步实例参数化。**§0.6 的消费面清单全量接线**（`CacheCredPath`、`QuarantineCache`、`CacheReloader`、audit sidecar、`cli/doctor` 等——实现时以 grep `CachePaths(` 逐点核对，漏一处即错位）。
- 实例目录创建：沿用 `DoPull` 的 `MkdirAll(0700)`；`cache.auth.json` 沿用 `HardenACL`；实例内 quarantine/ 子结构不变。

### 2.3 server 侧：实例名下发

`handleSnapshot` 在现有 `X-Sshmgr-Snapshot-Scope` 旁新增响应头：

```
X-Sshmgr-Device-Name: <ct.Name>
```

`ct` 鉴权后已在手，零查询成本。**非安全边界**（name 非秘密；pinned TLS 已防篡改）——仅作路由与一致性校验数据。老 client 忽略之（向前兼容）。

### 2.4 pull 写入规则（`cache pull [--instance <name>]`）——rev2 门禁语义完整化

| 场景 | 第一批 | 第二批 |
|---|---|---|
| `--instance <name>` 显式 | `instances/<name>/`（name 过白名单；**响应头 name ≠ `<name>` → fail-closed 拒写盘**；响应头缺失（旧 serve）→ 拒 + 提示升级 serve） | 同 |
| 无 `--instance`，默认目录已有 cache 材料（见门禁生效条件） | 默认目录（现状）+ **身份门禁（见下）** | 同 |
| 无 `--instance`，默认目录无任何 cache 材料（首次 enroll） | **默认目录（现状行为）+ 提示**（自动归位推迟） | **自动归位** `instances/<响应头 name>/`；**响应头缺失（老 serve）→ 不归位、落默认目录 + 升级提示** |
| 无 `--instance`，**响应头缺失（老 serve）**，默认目录已有材料 | **门禁跳过**（无 name 可比，维持现状兼容——非新增敞口，异码覆盖风险在老 serve 拓扑下本就存在）+ 升级提示（升级后门禁生效） | 同 |

- **自动归位推迟到第二批**（rev1）：与向导 `--instance` 接入卡同批落地——第一批没有向导生成的带 `--instance` 配置，若自动归位先行为，新 enroll 机器的 cache 落 `instances/<name>/` 而 `mcp --cache`（无 flag）读默认目录 → **开箱即起不来**。第一批首次 enroll 保持现状落默认目录。
- **默认实例身份门禁（rev2 完整语义）**：
  - **生效条件：默认目录 `cache.bin` 存在**（材料主体是 bin；`cache.auth.json` 只是拉取凭据——以它为条件会让"删凭据"与"清材料"混淆）。
  - **判定材料**：旧 meta 的 `device_name`（`cacheMeta` 增字段 `DeviceName string`，`json:"device_name"` 无 omitempty；存量 meta 零值空串）。
  - **判定规则**：`DoPull` 成功路径在**任何写盘之前**（含 bin）读旧 meta——旧 `device_name` 非空且 ≠ 本次响应头 name → **fail-closed 拒绝**，冻结文案引导三选一（这是不同设备码：第二个实例请用 `--instance <name>`；**要更换默认实例的设备码：清除默认目录全部 cache 材料（`cache.auth.json` + `cache.bin` + `cache.meta.json` 三件套）后重新 enroll**——身份记录在 meta，只删 auth.json 不清 meta 会 loop 回本拒绝；owner 核对发码是否张冠李戴）。
  - **meta 不可读分支（堵 fail-open）**：`cache.bin` 存在但 meta 缺失/损坏/`device_name` 为空 → **fail-closed 拒绝**（区分"合法真空"（bin 也不存在 = 首次 enroll，正常放行）与"有材料但身份记录不可读"（异常态，拒绝 + 引导清三件套重 enroll）——不以损坏记录为放行依据）。
  - **`cache.auth.json` 写序钉死（防错位）**：所有 auth 写入必须位于 `DoPull` 成功**之后**——CLI pull 路径现状已如此（§0.11 核实：门禁拒绝即提前 return，auth 不动，写序属回归钉住）；TUI `[s]同步` 用已存 cred 不写 auth；**TUI 连接编辑表单换码**是唯一先写 auth 的路径（保存即写、之后同步才撞门禁）——其错位后果由门禁拒绝文案兜底（用户被引导清材料或用 `--instance`），表单内的**预防性警告**（"本机已有 cache 材料，换设备码保存后同步将被身份门禁拒绝"）归第二批 TUI 改造。
  - 门禁在 `DoPull` 内一处接线，CLI/TUI/lazy 全路径覆盖。
- `--instance` 与响应头 name 的强一致校验同前（防 owner 发码张冠李戴——实例目录与授权错位）。

### 2.5 读取规则（`mcp --cache [--instance <name>]` / `cache status [--instance <name>]`）

- 无 `--instance` = 默认目录（现状）。默认目录无 cache 且 `instances/` 下存在实例 → 错误文案**列出实例清单**并指引 `--instance <name>`（**不自动猜**——读到哪个实例必须显式，杜绝静默错位）。
- `--instance <name>`：白名单校验后读 `instances/<name>/`。
- **lazy pull 天然 per-instance**：`MaybeLazyPull` 读实例目录内的 `cache.auth.json`（`CacheCredPath` 实例化后自动成立），拉取写回同实例目录。进程级 quarantine 哨兵语义不变（一个 `mcp --cache` 进程服务一个实例）。
- **quarantine / 到龄销毁 / 审计 sidecar 天然 per-instance**：全部落实例目录内，`QuarantineCache` 走实例化路径即成。

### 2.6 `cache status` 多实例视图

- 无 `--instance`：列**全部**实例——默认实例一行 + `instances/*/` 每行。字段：实例名 / 设备码 name（meta `device_name`）/ profile（仅 `scoped=true` 时显示，Plan 39 溯源纪律）/ servers / creds / age / 锚状态 / quarantine 概要。
- `--instance <name>`：单实例详情（现状格式复用）。

### 2.7 `clear` 与角色判定

- `ssh-manager clear`：在现有清理清单上**追加**——`instances/` 整树 + `VaultDir()/cache-dek-*.key` 全部变体（残留实例目录 = 残留凭据，违反 clear 的存在目的）。默认实例清理清单不变。
- `roles.resolveMode` 的 `cachePresent()`：改为"默认 cache 目录存在 **或** `instances/` 下任一实例存在"——否则只有命名实例的机器误判 wizard 态。

### 2.8 TUI / 向导 / doctor（第二批，边界先行钉死）

- **第一批 TUI client 页保持默认实例视图不动**（读默认目录、`[s]` 同步默认实例）——命名实例用户第一批用 CLI/计划任务同步。第二批：实例列表 + 切换 + 向导 client 接入卡生成带 `--instance` 的 `.mcp.json` + **首次 enroll 自动归位**（§2.4）+ **连接编辑表单换码的预防性警告**（§2.4 门禁配套）。
- doctor：第一批不动（client-cache 检查继续查默认实例，**已知残余见 §9.7**），第二批枚举全部实例诊断（并入 Plan 38-doctor 体系）。

## 3. MAX_OFFLINE 持久化（per-instance 配置）

**P0 治的是"事实被策略抹除"；本节治的是"策略本身跨进程不一致"——env 是进程属性，配置文件是机器（实例）属性。**

- **文件**：实例目录内 `cache.config.json`，v1 内容单一字段：

  ```json
  {"max_offline": "24h"}
  ```

  值为 Go duration 文法字符串（与 env 值同构）。明文——是策略不是凭据。
- **解析优先级**：`SSHMGR_CACHE_MAX_OFFLINE` env > `cache.config.json` > `(0, nil)` 关。env 保留为应急/测试 override（seam 纪律：env 显式给了就是给了）。**`SSHMGR_CACHE_DIR` 覆盖时，config 从 override 目录读**（`resolveMaxOffline(instanceDir)` 接收的是解析后的实例目录，override 时即 override 目录——语义自然统一，钉死防止实现歧义）。
- **校验复用**：config 文件值走与 env 完全相同的规则（≥1h、不可解析/为负/<1h → fail-closed 拒绝，冻结文案沿用 Plan 37 §1 并注明来源 file/env）。现行 `cacheMaxOffline()` 的全部读取点（`DoPull`、`LoadCacheSnapshot` 等）切换到实例感知的 `resolveMaxOffline(instanceDir)`。
- **原子写**：`cache.config.json` 写入复用 `atomicWriteUnique`（唯一 temp + rename，Windows 并发读重试——与 cache.bin/meta 同款纪律）；并发语义测试进 §6。写失败 = pull 成功 + WARNING（config 未更新，沿用 meta 写失败的降级形态）。
- **env 共存提示（rev2）**：`pull --max-offline` 执行时若 `SSHMGR_CACHE_MAX_OFFLINE` env 在场 → 输出一行 WARNING（"env 在场，本次写入的 config 在 env 清除前不生效"）——防止用户以为持久化已生效。
- **写入口**：`cache pull --max-offline <dur>`——pull 成功后顺带写**该实例**的 config（不传 flag 则不动现有 config）。第二批追加独立 `cache config [--instance] --max-offline` 子命令。
- **只搬 MAX_OFFLINE**：`--cache-max-age` 等其余 env/flag 不进 config（YAGNI，Q11 拍板）。

## 4. 安全分析

- **攻击面变化（接受，Q4 拍板）与其实际边界**：N 实例 = N 份（各自 profile 的）凭据集落盘。**ACL 硬化（0700/HardenACL）降低材料被获取的概率；`MAX_OFFLINE` 约束的是正常 loader 的离线加载窗口，不是密码学时效**——cache.bin（AES-256-GCM）的密钥 DEK 同机保存且无轮换，同时获得 cache.bin 与 DEK 的攻击者可**无限期解密任何时点的快照**；多实例使该物理暴露面 ×N。落盘的风险缓解因此是：ACL（获取难度）+ 吊销纪律（切断增量）+ **每实例独立 DEK**（单实例材料泄露不连坐他实例的解密）。
- **失窃的实际响应**：吊销设备码 = 切断未来 pull + 销毁**本机**材料（quarantine 全为本机文件操作，无作用于已离盘副本的密钥毁损语义）；**已可能外泄的凭据必须轮换**（server 端 re-credential，受影响 profile 的全部凭据）——文档写死，不得暗示吊销即消除外泄。
- **实例间隔离**：① DEK per-instance——A 实例目录连同其 DEK 单独泄露，解不开 B 实例 cache.bin（分离布局下"目录被同步工具带走"不含 DEK）；② token×cache 交叉 fail-closed——B 的 project token 在 A 实例 spawn 即验证失败（既有机制：裁剪 snapshot 只含 A profile 的 projects 行，MCP 层授权过滤本来就对）；③ name 大小写折叠唯一性（全历史，§2.1）——封死两个授权映射同一目录/DEK 的错配（含 revoke 后重发变体与存量遗留，前者终身占用、后者启动即败）。
- **路径穿越**：name 白名单双端校验；`instances/<name>` 的 name 恒过白名单才进 `filepath.Join`。
- **错配防护**：`--instance` 与响应头 name 强一致校验；默认实例 `device_name` 门禁（§2.4，含 meta 不可读 fail-closed 分支）——异码覆盖在写盘前被拒；client 端物理碰撞检测（§2.1）。
- **残余（非新增）**：同机恶意进程可读所有实例材料（文件 ACL/DPAPI 不挡同机）——与单实例现状同级。

## 5. 兼容性

- **存量单实例机器**：零迁移零行为变化（默认实例 = 现路径；无 flag 的 pull/mcp/status 全走默认目录）。
- **新 serve + 老 client**：多一个响应头，老 client 忽略——零影响。
- **老 serve + 新 client**：`--instance` 拒（提示升 serve，§2.4）；无 flag 路径 = **现状行为 + 门禁跳过（无 name 可比）+ 不归位落默认目录 + 升级提示**（§2.4 首行表格与末行钉死）。升级顺序铁律（client 先 serve 后）继续成立并在 compat-matrix 强调。
- **存量大小写碰撞（升级前 BINARY 时代遗留）**：serve 启动 fail-closed（§2.1 存量检测）——owner 改名后恢复。
- **compat-matrix**：登记 v0.11.0×v0.10.0 组合（老 serve 受限面：`--instance` 不可用、门禁不生效、无自动归位）与 v0.11.0×v0.11.0 全功能行。

## 6. 测试计划（摘要）

P0 矩阵见 §1.4（含零覆盖断言）。多实例：

1. 双实例 e2e：A/B 各 pull 各实例 → 各自 `mcp --cache --instance` 起得来、各自裁剪视图正确。
2. 交叉 fail-closed：B token 在 A 实例 spawn 失败（错误文案可辨认）。
3. `--instance` × 响应头 name 错配 → 拒写盘；头缺失（旧 serve fixture）→ 拒 + 升级提示；**无 flag + 头缺失 + 默认目录有材料 → 门禁跳过、写默认目录、输出升级提示**。
4. name 白名单：server `add` 拒非法（含**大小写折叠碰撞**：对 active 与 **revoked** 各一例、`agentA` vs 已有 `AGENTA`、**首段保留名** `con.foo`/`COM1.x`、全名保留名、尾点）；client 端非法 name 头拒写（含 `../` 形态）；**serve 启动对存量 active 折叠碰撞 fail-closed**；**client 物理碰撞**（目录在而身份不同 → 拒）。
5. DEK 隔离：A 实例 DEK 解不开 B 实例 cache.bin（解密失败路径）。
6. 首批 enroll：默认目录无材料 → **仍写默认目录**（自动归位二批前不生效）；默认目录有材料 → 同。
7. 存量兼容：无 flag 读写默认目录行为与现版逐字节等价（现有测试零改动通过）。
8. **默认实例身份门禁矩阵**：已有 `device_name=X` 的默认目录，用 Y 码 pull → 拒绝（**bin/meta/auth 三者字节均不变**——含 rev2 补的 auth 断言）；空 `device_name` 且无 bin（真空首次）→ 放行补记；**bin 在而 meta 缺失/损坏 → 拒绝**（fail-closed 分支）；**清除三件套（auth+bin+meta）后重 enroll → 门禁不触发**（重绑定语义）；仅删 auth.json（bin/meta 在）→ 门禁仍拒（文案引导清三件套）。
9. config 优先级：env > file > 关；file 非法值 fail-closed；`pull --max-offline` 写入后 env 清空仍生效；**env 在场写 config → WARNING 输出**；**并发**：pull 写 config 与 `LoadCacheSnapshot` 读并发 → 无半截 JSON（atomicWriteUnique 语义）。
10. **env × flag 互斥**：`SSHMGR_CACHE_DIR`（或 `SSHMGR_CACHE_DEK`）+ `--instance` 同在 → 报错（互斥文案）。
11. `clear` 清全部实例 + DEK 变体；`roles` 判定：仅命名实例的机器判为 client。
12. lazy pull / quarantine / 到龄销毁在命名实例上的 per-instance 行为（复用 Plan 34/37 测试形态，路径换实例目录）；**重点钉住：双实例 + 吊销 A 设备码 → A 实例 pull 401 → A 实例目录 quarantine、B 实例完好**（失窃响应粒度，不得只当既有机制假设）。

## 7. 文档联动

- `docs/multi-machine.md`：多实例章节（enroll 双 agent 流程、`--instance` 用法、失窃响应 = **吊销设备码切断增量 + 轮换受影响凭据**、吊销纪律：快速断 agent = 先吊 device code（下次 pull 销毁该实例 cache）再吊 project token（等 pull/过期）、**默认实例换码 runbook（清三件套重 enroll）**、过渡期应急纪律 §1.3）。
- `docs/threat-model.md` §1.1：多凭据集落盘登记（Q4 取舍 + §4 的实际边界措辞——ACL 降低获取概率、时效约束 loader、失窃须轮换）。
- `docs/agent-access.md`：离线多 agent 形态一句话指向 multi-machine。
- `README.md`：`--instance` / `--max-offline` flag 说明 + cache-first 标准姿态措辞（在线 http 直连降为辅助）。
- `docs/compat-matrix.md`：§5 两行 + 存量大小写碰撞的启动失败行。
- `docs/backlog.md`：销项（多实例）/登记（TUI 实例列表二批、doctor 二批、cacheMaxAge 不进 config 的 YAGNI 决策）。
- 二批：`tui-multi-machine.md` 实例列表、向导 client 接入卡 `--instance` 形态、表单换码预防性警告。

## 8. 交付物与批次

- **P0（§1）**：独立小 PR，可单独合并并发 hotfix 版（几行改动 + §1.4 矩阵；不依赖本 spec 其余任何部分）。
- **第一批**：多实例 CLI 闭环——`CachePathsFor`/`CacheDekPathFor`/`DekProvider` 参数化 + 全消费面接线（§2.2 含 env×flag 互斥）、name 白名单双端含首段保留名/全历史折叠/存量启动检测/物理碰撞检测（§2.1）、server name 头（§2.3）、pull 写入规则含**默认实例身份门禁完整语义**（§2.4 一批列）、mcp/status 读取规则（§2.5-2.6）、clear + roles（§2.7）、MAX_OFFLINE 持久化（§3）、文档联动（§7 首批部分）。**首批 enroll 保持现状落默认目录（自动归位不在此批）。**
- **第二批**：TUI client 页实例列表、向导 `--instance` 接入卡、**首次 enroll 自动归位**（§2.4 二批列）、连接编辑表单换码预防性警告（§2.4）、`cache config` 子命令。
- **跟随**：doctor 多实例诊断（并入 Plan 38-doctor 体系）。

## 9. 残余与登记

1. **存量机器永不自动迁移到实例目录**：自动归位仅第二批起、且只作用于首次 enroll；要进实例形态需显式 `--instance` 重新 enroll（零迁移承诺的有意代价）。
2. **第一批 TUI 同步仅限默认实例**：命名实例用户第一批用 CLI/计划任务（`schtasks` wrapper 内 `--instance` flag 即可）。
3. `cacheMaxAge`（`--cache-max-age`）不进 config——只搬 MAX_OFFLINE（YAGNI）。
4. 同机恶意进程可读全部实例材料——与现状同级，非新增。
5. `SSHMGR_CACHE_DEK` 显式路径不按实例派生——单 escape hatch 语义，多实例测试需每实例独立设值（测试注意项）。
6. Plan 37 的 B-off 半开禁令在 P0 后只剩一个触发面：plaintext + B-on（env 或 config 任一开）——分支保留即为此。
7. **doctor 一批不感知命名实例**：仅命名实例的机器，第一批 doctor 的 client-cache 检查会报"cache 缺失"（roles 已修，doctor 二批跟进——不静默）。
8. **跨进程 casefold 双插窗口（rev2 登记）**：查重已入事务 + 进程内 `MaxOpenConns(1)` 天然串行；两个独立 ssh-manager 进程**恰好同时** add 互为大小写变体的理论窗口仍存在——owner 单人操作面下接受为残余（发生时 client 端物理碰撞检测兜底拦截）。
9. **TUI 表单换码预防性警告归二批**（§2.8）：第一批该路径的错位由门禁拒绝文案兜底（用户被引导清三件套或用 `--instance`），体验欠佳但非静默。
