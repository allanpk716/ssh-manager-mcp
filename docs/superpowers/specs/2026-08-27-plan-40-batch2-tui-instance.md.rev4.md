# Plan 40 第二批设计：TUI/向导多实例接入 + 首次 enroll 自动归位 + `cache config` 子命令

> 2026-08-27 grilling 两轮定案（owner 全部确认）。**基 spec = `2026-08-26-plan-40-multi-instance-cache-design.md.rev3.md`**（下称 rev3）——本文件只做第二批五项的实现级设计，rev3 已钉死的边界（批次划分 §8、门禁语义 §2.4、目录布局 §2.2、老 serve 行为 §5）不重议。范围 = backlog「第二批（已裁决待开工）」五项：① TUI client 页实例列表+切换；② 向导 `--instance` 接入卡；③ 首次 enroll 自动归位；④ 连接编辑表单换码预防性警告；⑤ 独立 `cache config [--instance] --max-offline` 子命令。
>
> **rev1（2026-08-27）**：初稿经一轮外部评审发现 9 处问题吸收：归位条件扩为两个 override env；TUI env-override 单槽模式；归位时序重排（真空候选判定前置 + DEK 创建后置）；面板 `[c]` 保存切换 `m.instance`；面板真空默认强制显式字段；picker 自动打开条件收紧；残留物登记；auth 写失败用例。
> **rev2（2026-08-27）**：二轮 10 处（7 证实）吸收：面板新实例 MkdirAll+即时写；cap 校验统一最终目标槽；`cache config` 仅已存在实例；两 override env 语义分离；picker 默认行区分；换码警告静态化；恢复路径文案。
> **rev3（2026-08-27，owner 特批第 3 轮）**：三轮 9 处吸收：真空 v3（config 入真空定义 = 默认槽意图标记）；`[c]` 守卫无条件放行；表单保存前置校验三连；MkdirAll 失败清理；§9 零写盘限定；非法头名分派钉死；归位幂等用例。
> **rev4（2026-08-27，owner 特批第 4 轮 + 最终验证轮）**：四轮 10 处（含 2 高）吸收，**两处结构性修正**：① **真空 v4**——`cache.bin`、`cache.auth.json`、**`cache.meta.json`**、`cache.config.json` **四者均缺**才归位：config 是可选文件（仅 `pull --max-offline`/`cache config` 写出，从未配 cap 的存量默认实例没有它），"保留 config"对这类机器失效（四评审 codex#1 高severity）——meta 随每次成功 pull 覆写、是"默认槽曾有材料"的天然痕迹文件（零新增机制），换码 runbook 改为**清三件套（auth+bin+quarantine）、保留 meta 与 config**，存量零迁移承诺对全部形态成立；② **删除"真空候选跳过默认槽 precheck"机制**（四评审 kimi 死路径分析：真空候选按定义无 config → 默认槽 precheck 恒为 off/env-only，跳与不跳等价）——cap precheck 恒在 HTTP 前跑（**批1 时序完全恢复**），唯一新增校验 = retarget 后对目标实例目录的 config 校验（post-HTTP、pre-write），§11.6 两个不可能状态用例随之删除。其余：auto-picker 判定与真空 v4 统一（共识）；归位目标 auth-only 目录放行分支补定义；实例目录 MkdirAll 后置明示 + 拒绝路径目录零新增断言；"空码=保持"条件规则成文；前置校验比较基准钉 casefold。
>
> 本轮（2026-08-27）拍板、不重议的决策：
> **R1-Q1** TUI 切换 = `[i]` picker overlay（默认实例 + `ListInstances()` 各一行），**会话内有效**；默认槽**真真空（归位真空同款四文件判定，rev4 统一）**而 instances 有货 → 启动自动开 picker + 提示；picker 只列**已存在**实例（新实例名入口 = 表单字段）。
> **R1-Q2** 连接表单增**可选"实例名"字段**：空 = 默认（首拉自动归位）；非空 = 显式路由该实例（即时校验 + 拉取时响应头强一致 + 保存前置校验三连）。
> **R1-Q3** auth 写序 = **"目标槽未知则后移、已知则即时"**：wizard 表单保存不写盘、pull 成功后按实际槽写；面板保存即写**表单路由槽**（新实例目录先 `MkdirAll(0700)`，写失败清理本次新建空目录）并切换 `m.instance`；CLI 按 `DoPull` 返回实际槽写。
> **R1-Q4** 自动归位触发条件 = **真空 v4 定义**：默认槽 `cache.bin`、`cache.auth.json`、`cache.meta.json`、`cache.config.json` **四者均不存在**——meta/config 任一在场 = 默认槽意图标记（曾有材料/曾配置的痕迹）。**任一单槽 override env（`SSHMGR_CACHE_DIR`/`SSHMGR_CACHE_DEK`）在场 → 不归位**；响应头 name 过不了 `instname.Valid` → 拒写盘。
> **R1-Q5** `DoPull` 签名改为返回 `(PullResult, error)`——编译器驱动迁移全部调用点。
> **R1-Q6** 换码警告 = 纯 UX 层（构建时静态按选中槽）。
> **R1-Q7** `cache config` 子命令：显示/写入（仅已存在实例）；不做 off 开关。默认槽 config/meta 即意图标记——删之则重 enroll 会被归位。
> **R1-Q8** doctor 多实例不进本批；版本 v0.12.0；流程 = spec → 评审收敛 → plan → SDD。
> **R2-Q1** 归位触发面 = {CLI 裸 `cache pull`（真空机器）+ TUI wizard 首拉} 两处；CLI 补一行显式提示。
> **R2-Q2** 首拉成功后 TUI 自动选中实际生效实例；`[c]` 守卫无条件放行（提交校验 + §4 兜底）。
> **rev1-R** env-override 与多实例 UI 互斥（单槽模式禁用）；面板真空默认强制显式字段。
> **rev2-R** 面板新实例 = MkdirAll + 即时写；config 不预配置未 enroll 实例。
> **rev3-R** 表单跨槽路由必须显式重输设备码；casefold 碰撞拒。
> **rev4-R** 换码 runbook 清三件套（auth+bin+quarantine）、保留 meta+config（意图标记勿删）；cap precheck 时序回归批1（删跳过机制）。

## 0. 现状事实（2026-08-27 于批1 合并后代码核实；函数名锚定）

1. **TUI client 页是纯默认实例视图**：`clientModel`（tui/clientpage.go:32）无实例概念；`refreshDataCmd` 读默认槽 cred + `LoadCacheSnapshot()`（:94-118）；`[s]` → `syncCmdMode` → `DoPull(..., PullOpts{Timeout})` 不带 Instance（:126-143）；`[c]` → `editConnForm` → `WriteCacheCred`（默认槽，:381）——rev3 §0.11 钉死的唯一"auth 先于 pull"路径。
2. **wizard client 流**：`enterClient`（tui/wizard.go:184）→ 连接表单 → `connSavedMsg` → `syncCmdMode(cred, wizard=true)` → `pullSucceededMsg` → `clientFinishScreen(serveURL)` 生成 `.mcp.json` 接入卡，offline 形态 `["mcp", "--cache"]`（clientpage.go:439-465）。
3. **DoPull 结构**（clientops.go:442-637）：顶部按 `o.Instance` 解析 `CachePathsFor` → cap 预检 `resolveMaxOffline(dir)`（**HTTP 前**）→ HTTP → 身份门禁 → 写盘（写盘阶段首动作 `MkdirAll` + bin→meta）。**归位的目标名（响应头 `X-Sshmgr-Device-Name`）在 HTTP 响应到手后才可知**。
4. **两个 override env 语义分离**：`CachePathsFor`（clientops.go:137-141）只认 `SSHMGR_CACHE_DIR`（cache 目录路由，命中即忽略 instance 参数）；`paths.CacheDekPathFor`（paths.go:83-86）只认 `SSHMGR_CACHE_DEK`（盖过一切 per-instance DEK 派生）。`SSHMGR_CACHE_DEK_DIR` 只搬迁 DEK 根目录（连贯 seam）。
5. **DEK 加载两形态**（dek.go）：`loadOrCreateDEK`（DoPull 路径，缺失则生成+落盘）；`loadDEK`（LoadCacheSnapshotFor 路径，不自动创建——缺失即拒载）。
6. **CLI pull 的 auth 写序**：`DoPull` 成功后 `WriteCacheCredFor(instance="")`（cli/cache.go:119）+ config 同槽（:122-128）——归位必须连动 auth 落槽（§5）。
7. **DoPull 调用面**：生产 5 处 + 测试 ~40 处（机械 `err :=` → `_, err :=`）。
8. **config 积木全在**（clientops/config.go）：`resolveMaxOffline(dir)`（env > file > off）、`WriteCacheConfig(dir, v)`、`ValidateMaxOffline(v)`。**config 是可选文件**——仅由 `pull --max-offline` / `cache config` 写出；从未配置 cap 的机器（含全部存量默认实例的多数）无此文件。
9. **多实例积木全在**（批1）：`ListInstances()`/`InstancesRoot()`、For-variant 家族、`checkInstanceFlag`（cli/common.go:18）、`cacheStatusList`（cli/cache.go:233）。
10. **Plan 30 消息门**（clientpage.go:145-168）：新增 client-owned 消息类型必须登记 owned allowlist。
11. **面板 `[s]` 归位不可达**：面板同步前提 = 默认槽 auth 在 = 非真空。归位触发面 = {CLI 裸 pull（真空机）、TUI wizard 首拉}。
12. **`[c]` 现守卫**：面板模式 `m.cred == nil` → 拒绝开表单（clientpage.go:248-254）——rev3 修正为无条件放行。
13. **`WriteCacheCredFor` 不建父目录**（实验实证 2026-08-27）：`atomicWriteUnique` 在目标目录不存在时直接报错——面板路径写新实例槽 auth 前必须自建目录（§5）；wizard/CLI 后移写天然满足。
14. **实例名 `default` 合法**（instname.go 核实）：与"默认槽"显示语面潜在同名（§3.1 显示区分）。
15. **NTFS 大小写不敏感**（rev3 §0.10 实验实证）：`instances/AGENTA` 与 `instances/agentA` 解析同一目录——表单实例字段写入前须 casefold 碰撞比对（§4）。
16. **meta 随每次成功 pull 覆写**（clientops.go:621，既有行为）：默认槽只要有过任何一次成功 pull 就有 `cache.meta.json` 在盘；meta 只会被 runbook 清理/`clear`/手动删除——是"默认槽曾有材料"的**天然痕迹文件**（rev4 真空 v4 的依据，零新增机制）。

## 1. 首次 enroll 自动归位（rev3 §2.4 二批列落地）

### 1.1 触发条件（真空 v4 定义，全部满足才归位）

| # | 条件 | 依据 |
|---|---|---|
| 1 | `o.Instance == ""`（无显式路由） | 显式路由已是命名实例 |
| 2 | `pin != ""` 且响应 200 且头 `X-Sshmgr-Device-Name` 非空 | plaintext 无头永不归位；老 serve 无头不归位（不归位、落默认目录 + 既有升级 WARNING） |
| 3 | 头 name 过 `instname.Valid` | 非法 → **拒写盘**（owner 改名重发，既有文案）——不属"不归位"分支 |
| 4 | **默认槽 `cache.bin`、`cache.auth.json`、`cache.meta.json`、`cache.config.json` 四者均不存在**（真空 v4，rev4） | **meta/config 任一在场 = 默认槽意图标记**：meta 随每次成功 pull 覆写（§0.16）——"曾有材料"的天然痕迹；config = "曾配置"。二者共同覆盖全部存量形态（有 config 靠 config、无 config 靠 meta——rev4 修四评审 codex#1：config 可选，"保留 config"对无 config 机器失效）。换码 runbook v2 = 清三件套（auth+bin+quarantine）、**保留 meta 与 config**（§12）。auth 在而 bin 不在（§9.10 边缘）→ 不归位、默认目录写入 + 门禁补记（现状行为） |
| 5 | **`SSHMGR_CACHE_DIR` 与 `SSHMGR_CACHE_DEK` 均未设** | 任一在场 = 单槽完全覆盖语义——归位会把"无 flag pull"变成事实上的命名实例而材料却挂在单槽语义下，env 清除后按派生 DEK 拒载（端到端实证）。`SSHMGR_CACHE_DEK_DIR` 不在此列（连贯 seam，不拦归位） |

### 1.2 归位机制（DoPull 内部单一时序；rev4 删跳过机制、时序回归批1）

1. **cap precheck 恒在 HTTP 前**（批1 时序，rev4 恢复）：DoPull 顶部对**本次解析槽**（默认/显式实例）执行 `resolveMaxOffline`——真空候选按定义四文件全缺，默认槽 precheck 恒为 off 或 env-only（env 非法 → HTTP 前拒，本就该拒）；非真空路径（含默认槽 config 在场的任何机器）非法 config → HTTP 前拒（现状行为不变）。**rev1 曾引入的"真空候选跳过默认槽 precheck"机制删除**（四评审 kimi 证实为死路径：真空候选无 config，跳与不跳等价）。
2. HTTP 拿响应 + 头。**分派：条件 2 不满足（头缺失/plaintext）→ §1.3 不归位分支；条件 3 不满足（非法头名）→ 拒写盘（终局错误，不进 §1.3）。**
3. 真空候选成立 → **retarget**：`dir/bin/metaPath` 重解析为 `instances/<头name>/`。
4. **目标槽门禁**（`gateNamedInstance` 形态）：物理碰撞——目录在且 meta 身份**异**于本次头 name → 拒；**bin 在而 meta 缺失/不可读 → 拒**（半写态，+清理路径文案）；**meta 身份相同 → 放行**（幂等再归位）；**目录在而 bin 缺（auth-only / 仅空目录）→ 放行**（fresh-slot 语义，auth 顺位覆盖——rev4 补定义：§5 面板新实例 enroll 故意制造 auth-only 目录，该分支是 `[s]` 闭合的通路，不得解释为身份不匹配）。
5. **目标槽 cap 校验（rev4 后唯一新增校验，post-HTTP、pre-write）**：retarget 后对**目标实例目录**执行 `resolveMaxOffline`——目标目录已存非法 `cache.config.json` → **拒绝本次 pull**（响应丢弃、零写盘）。对齐显式 `--instance` 路径的既有语义（其 precheck 在顶部即跑实例目录）。
6. **DEK 创建后置**：`loadOrCreateDEK(头name)`——位于全部门禁/cap 校验之后，归位路径任何拒绝分支不创建任何 DEK 文件（§9）。
7. **写盘**：首动作 `MkdirAll(0700, instances/<name>/)`（**位于全部门禁/cap/DEK 之后——拒绝分支不新增目录**，rev4 明示）→ bin→meta（既有提交序）+ meta 记 `device_name`。

### 1.3 不归位分支（行为与批1一致；唯一时序差异见注）

- 头缺失（老 serve）：默认目录 + 既有升级 WARNING。
- §9.10 auth-only（auth 在 bin 无）：默认目录 + 门禁补记。
- **默认槽 meta 或 config 任一在场**（意图标记，rev4）：默认目录写盘（policy/config 就地生效）。
- **任一单槽 override env 在场**：写 override/默认目录（单槽语义一致）。
- plaintext（`--allow-plaintext`）：无头 → 默认目录。
- **时序注（rev4 简化）**：全部不归位路径与批1 **时序逐字节一致**（cap precheck 恒 HTTP 前）；唯一时序差异只存在于归位路径本身（目标槽校验在 HTTP 后、写盘前——§1.2-5）。

### 1.4 401/失败路径

401 发生在头到手前（鉴权失败无 `ct`，serve 不下发头）→ 归位无从谈起；`QuarantineCacheFor("")` 打默认槽（真空 = 无材料 = no-op）——既有行为不变。

### 1.5 CLI 提示

归位发生时 `StatusOut` 追加一行：`first enroll located to instance <name> — future pulls need --instance <name>`。

## 2. DoPull 签名变更

```go
type PullResult struct {
    Instance string // effective slot: "" = default, else instances/<name>/
}
func DoPull(url, token, pin string, o PullOpts) (PullResult, error)
```

- 消费方：CLI（auth + config 按实际槽写）、TUI wizard（接入卡 + auth 写槽）。迁移：编译器驱动（生产 5 处 + 测试 ~40 处机械改）。弃回调方案的登记：可选接入可被静默遗忘；返回值让编译器强制完备。

## 3. TUI client 页多实例（picker）

### 3.1 `[i]` 实例 picker overlay

- 行 = 默认实例 + `ListInstances()` 每实例；**默认行显示「（默认实例）」、实例行显示实例名**（实例名 `default` 合法，行文案区分）；行数据轻量（name + bin mtime age + meta 的 profile，scoped 时）——**不解密**。
- 选中 → `clientModel.instance` 置值 + `refreshDataCmdFor(instance)` 重读。**会话内有效**（不落盘、不跨进程）。
- footer：`[s]同步 [i]实例 [c]编辑连接 [t]TTL  q 退出`；header 追加当前实例名（命名实例显示 `· 实例 <name>`）。`busy` 中禁 `[i]`。

### 3.2 启动落点与空默认自动开 picker

启动 = 默认实例起步；若默认槽**真真空（与归位真空 v4 同款四文件判定，rev4 统一）**而 `ListInstances()` 非空 → 首个 `dataReadyMsg`（errMsg 形态）后自动打开 picker + 提示。**非真空的"部分缺件"形态（bin 在 auth 缺 / meta 在 / config 在）一律不弹**——默认槽有意图或材料，不把用户从默认槽恢复路径上引开（换码清理中的机器正是此形态）。

### 3.3 per-instance 动作

- `[s]`：`ReadCacheCredFor(m.instance)` → `PullOpts{Instance: m.instance}`（无 cred → 既有错误行）。
- `[c]`：prefill 选中实例 cred + 实例字段预填选中名（默认 → 空）；**保存后 `m.instance` = 表单路由结果并 refresh 该槽**。
- `[t]`：不变。

### 3.4 消息门登记（Plan 30 checklist）

新增 client-owned 消息类型必须登记 owned allowlist——配套路由测试钉住。

### 3.5 env-override 单槽模式

**任一单槽 override env 在场 → 单槽模式**：`SSHMGR_CACHE_DIR` = 目录路由恒 override；`SSHMGR_CACHE_DEK` = 路由不变但全部实例共享同一 DEK。统一处置：`[i]` 禁用、auto-picker 不触发、表单实例字段禁用、页顶横幅。**禁用而非适配**（env = 测试/迁移 escape hatch）。`SSHMGR_CACHE_DEK_DIR` 不触发。

## 4. 连接表单实例字段（保存前置校验三连；rev4 补条件规则与比较基准）

- huh 可选输入"实例名"：空 = 默认（wizard 首拉自动归位）；非空 = 显式路由。即时校验 `instname.Valid`。
- **保存前置校验三连**（提交时判定；**比较基准一律 casefold**——rev4 钉死，与 NTFS 同目录语义一致）：
  1. **字段（casefold）≠ 选中槽（casefold）→ 设备码必填**："留空=保持不变"的保持对象是**选中槽**的既有 token；且**该规则本身有前提——选中槽确有 auth**（rev4 成文）：选中槽无 auth（picker 选中的无 cred 实例目录）时空码一律必填，不存在"保持"。
  2. **字段与 `ListInstances()` casefold 比对，命中且 ≠ 选中槽 → 校验拒**（NTFS 碰撞——防静默覆盖既有实例 auth → 401 → quarantine；文案指明碰撞对象）。
  3. **命中且 == 选中槽（casefold 等）→ 允许**（同槽显式换码；不可逆由重输恢复，§13）。
- **env 互斥即时提示**：单槽 override env 在场 → 字段禁用 + 横幅（§3.5）。
- prefill：选中命名实例 → 预填其名；默认实例 → 空。
- **面板模式 × 默认槽真空（四文件判定）× 字段空 → 表单校验拒**：提示"默认实例无材料——首次 enroll 请走向导流程（自动归位），或填实例名显式路由"。
- wizard 与面板共用此表单。

## 5. auth 写序（"目标槽未知后移、已知即时"）

| 路径 | 写序 | 落槽 |
|---|---|---|
| CLI `cache pull` | DoPull 成功后（现状） | `WriteCacheCredFor(res.Instance)`（归位连动）；config 同槽 |
| TUI wizard 首拉 | 表单保存不写盘；pull 成功后写 | `WriteCacheCredFor(res.Instance)`（目标目录已由 DoPull 建）；写失败 = WARNING 行含恢复路径（`auth 未落盘——本 TUI 的 [s] 同步不可用；恢复 = CLI cache pull 或重跑向导表单（输入已保留）`） |
| TUI 面板 `[c]` | 保存即写（现状时序） | 新实例槽先 `MkdirAll(0700)` 再写；**写失败 best-effort 清理本次新建的空目录（仅空才删——防空目录进 `ListInstances()`/picker）**；保存后 `m.instance` 切槽 + refresh |

- wizard 改善登记：现状"保存即写、pull 失败 auth 已落盘"→ 后移后失败 auth 零写入。
- **auth-only 新实例边缘**：面板路由新实例保存后、首拉前——§9.10 同族，`[s]` 首拉即闭合。
- §9.10 收缩登记："先写 auth"路径收缩至"面板改连接（非真空槽）+ 面板新实例 enroll"两条（均登记）。

## 6. 换码预防性警告

- **构建时静态判定**（按表单打开时的选中槽）：
  - **默认槽 + bin 在**：`默认实例已绑定设备 <meta.device_name 或 "(旧 cache 未登记)">——更换设备码前须清三件套（cache.auth.json + cache.bin + quarantine/，**保留 cache.meta.json 与 cache.config.json——它们是默认槽意图标记，删了重 enroll 会被归位到实例槽**）重 enroll，否则下次同步将被门禁拒绝；若是本机第二个 agent，请在"实例名"字段填新实例名。`
  - **命名实例槽 + bin 在**：`实例 <name> 已绑定设备 <device_name>——换码须删除该实例目录重 enroll，否则同步将被拒。`
  - 选中槽无 bin：不显。
- 字段路由已存在实例的换码：无静态警告——由 §4 前置校验三连（casefold 命中即显式化）+ 拉取时门禁兜底（§13）。
- 纯 UX 层：实际拦截仍由门禁 fail-closed。

## 7. 向导接入卡

- `clientFinishScreen(serveURL, instance string)`；`instance != ""` → offline args = `["mcp", "--cache", "--instance", "<name>"]` + 注释行；online http 形态不变。instance 来源 = `PullResult.Instance`。首拉成功后自动选中生效实例。

## 8. `cache config` 子命令

```
ssh-manager cache config [--instance <name>] [--max-offline <dur>]
```

- **仅对已存在实例目录可写/可显**：不存在 → 报错含 enroll 指引（不预配置、不 MkdirAll）。
- 无 `--max-offline`：只读显示目录 + 生效 cap + 来源（env/file/off）。
- `--max-offline`：`ValidateMaxOffline` → `WriteCacheConfig`；env 在场 → 既有 WARNING。
- **不做 off 开关**：删 cap = 手动删该实例 config 文件。**默认槽警示（rev4 措辞扩展）**：删默认槽的 config **或 meta** 都会削弱意图标记——换码 runbook 二者皆勿删（§12）。
- `--instance` 走 `checkInstanceFlag` 互斥。不 pull——无 plaintext 语义、不触发归位。

## 9. 安全分析

- **归位不新增暴露**：头来自 pinned TLS；非法名拒；物理碰撞/半写态检查在归位路径同样生效；**归位路径任何拒绝分支 = 零写盘、零新增目录、零新增 DEK**（§1.2-6/7）。非真空路径保留批1 现状（HTTP 前预建默认 DEK——门禁拒绝时该 DEK 可能已存在，既有行为非本批引入）。
- **半开状态防线**：非真空路径 cap precheck 恒 HTTP 前（现状）；归位路径目标槽校验 pre-write（§1.2-5）——Plan 37"拉得动必载得动"全路径成立。
- **单槽 override env × 归位互斥**；**表单跨槽错置防线**（跨槽码必填 + casefold 拒）；**警告是体验层**；**auth 后移不降保护**；**picker 不解密**——均同 rev3。

## 10. 兼容性

- **v0.12.0 client × v0.11.0 serve**：全功能。**× 老 serve**：无归位；字段非空 → pull 拒；接入卡无 `--instance`。
- **存量零迁移（rev4 全形态成立）**：无 flag 行为不变；**换码 runbook 回默认槽对全部存量形态成立**——有 config 靠 config（rev3）、无 config 靠 meta（rev4）——"曾有材料"的默认槽清三件套后必然非真空。
- **DoPull 签名变更**：internal 包。compat-matrix 登记 v0.12.0 行。

## 11. 测试计划

1. **归位触发**：真空机（**四文件均缺**）裸 `cache pull`（两 override env 均未设）→ 材料落 `instances/<name>/` + auth 同槽 + CLI 提示行 + meta `device_name` 记录。
2. **归位不触发七态**：auth 在 bin 无（→默认+补记）；头缺失（→默认+WARNING）；`SSHMGR_CACHE_DIR` 在场（→override 目录）；`SSHMGR_CACHE_DEK` 在场（→默认、材料用 env DEK，断言不落 `instances/`）；plaintext（→默认）；**config 在场（→默认，policy 生效）**；**meta 在场（无 config 的存量形态，rev4：→默认+门禁补记）**。`SSHMGR_CACHE_DEK_DIR` 在场 → 归位照常（实例 DEK 落 env dir）。
3. **换码 runbook 回默认槽**（rev4）：有材料默认槽 → 清三件套（**保留 meta+config**）→ 裸 pull → **默认槽** + 门禁补记新身份（meta 覆写）；对照：四件全清（rm 级）→ 归位（机器重置语义，文档写明）。
4. **归位目标门禁四分支**：meta 身份异 → 拒；bin 在 meta 不可读 → 拒（清理文案）；meta 身份同 → **放行**（幂等）；**目录在而 bin 缺（auth-only/空）→ 放行**（rev4——面板新实例 `[s]` 闭合通路）。
5. **归位拒绝零写盘/零目录**：目标实例目录预置非法 config → 拒 + 默认槽与目标槽 bin/meta/DEK 全零新增 + **`instances/` 目录树零新增**（rev4 扩）。
6. **cap 校验时序**（rev4 重构，删不可能状态用例）：① 非真空机器默认槽非法 config → **HTTP 前拒**（现状回归——含 config 在场的任何机器）；② 归位目标实例目录非法 config → **写盘前拒、零写盘**（唯一新增校验）；③ plaintext + B-on(env) → HTTP 前拒（现状回归）；④ 显式 `--instance` 路径非法 config → HTTP 前拒（现状回归）。
7. **归位幂等**：归位后默认槽仍真空 → 再裸 pull 同码 → 同实例目录放行、DEK 不重复生成、auth 覆写同槽。
8. **签名迁移零语义变化**：既有 DoPull 测试机械迁移后绿；`PullResult.Instance` 无 flag 默认路径 == ""。
9. **TUI picker**：开/选/切换 per-instance 读写；**auto-picker 触发 = 四文件真空 + instances 非空**；bin 在 auth 缺 / **meta 在 / config 在（换码清理中形态）→ 不弹**（rev4）；默认行「（默认实例）」区分；`[s]` 带选中实例；`[c]` prefill/写槽/切槽+refresh；消息门登记路由测试。
10. **TUI 单槽模式**：两 override env 各自 → `[i]` 禁用/auto-picker 不触发/字段禁用/横幅；`SSHMGR_CACHE_DEK_DIR` → 照常。
11. **表单前置校验三连（casefold 基准）**：非法名拒；空=默认；面板真空默认×空 field 拒；字段≠选中槽（casefold）+空码 → 拒；**选中槽无 auth + 空码 → 拒（无"保持"可言，rev4）**；字段 casefold 撞他实例（≠选中槽）→ 拒；**字段 casefold == 选中槽（如选中 `AGENTA` 填 `agenta`）→ 同槽换码允许（rev4 钉死）**。
12. **wizard 流**：真空首拉 → 归位 + 自动选中 + 接入卡 `--instance <name>`；首拉失败 → 两槽 auth 零写入；显式字段 enroll → 卡片同；pull 成功 auth 写失败（目录占位）→ WARNING 含恢复路径 + 卡片仍出 + 下次成功 pull 恢复。
13. **换码警告形态**：默认槽有材料（文案含三件套+保留 meta/config）；命名实例；无 bin 不显。
14. **config 子命令**：三源显示；写入读回；env WARNING；互斥；不存在目录报错含指引。
15. **面板新实例写失败清理**：目录占位 → 本次新建空目录被清理（`ListInstances()` 不含）；既有目录写失败 → 保留。
16. **e2e 双实例 enroll 全程 TUI 化**：真空机 A 首拉归位 → 默认槽占用机 B 表单字段 enroll（MkdirAll + auth 落 B + 切槽）→ `[s]` 首拉 → 两实例 `mcp --cache --instance` 各自起、裁剪正确。

## 12. 文档联动

- `docs/tui-multi-machine.md`：picker、表单字段与前置校验三连、换码警告、单槽横幅。
- `docs/multi-machine.md`：自动归位语义（真空 v4 四文件定义 + 不归位分支）；**换码 runbook v2：清三件套（auth+bin+quarantine），保留 meta 与 config（默认槽意图标记——删则重 enroll 被归位到实例槽）**；双 agent enroll 全程 TUI 流程；`cache config`（仅已存在实例；默认槽 meta/config 勿删）。
- `docs/agent-access.md` 一句话指向；`README.md` 子命令 + `[i]`；`compat-matrix.md` v0.12.0 行；`backlog.md` 销项 + doctor 登记。
- **产品内文案联动（rev4）**：`gateDefaultInstance` 拒绝文案的三选一引导中"删除 cache.auth.json + cache.bin + cache.meta.json + quarantine/" 更新为三件套表述（保留 meta/config）。

## 13. 残余与登记

1. **doctor 多实例不在本批**（Plan 38 体系内解决）。
2. **picker 行不解密**（§3.1 取舍）。
3. **config 无 off 开关**：手动删文件；**默认槽删 config 或 meta 都削弱意图标记**（§8 警示）。
4. **面板 `[c]` 换码仍先写 auth**（§0.11 例外收缩至两条，均登记）：警告 + 前置校验三连 + 门禁兜底。
5. **wizard 首拉成功后 auth 写失败** = WARNING 不阻断（含恢复路径文案）。
6. **会话内 picker 不记忆**（零新增状态文件）。
7. **换码三件套保留的 meta 携带旧 device_name**（rev4 取舍）：bin 已删 → 门禁不生效（生效条件是 bin 在）；下次成功 pull 覆写为新身份——无害痕迹，文档写明"保留是特性"。归位机器默认槽本就无 meta 可孤留（真空前提），原 rev2 残余项随 v4 失效。
8. **auth-only 新实例**：面板 `[c]` 路由新实例保存后、首拉前——§9.10 同族，`[s]` 首拉即闭合。
9. **实例名 `default` 同名歧义**：显示已区分；命名纪律建议避开（不改服务端白名单）。
10. **字段路由已存在实例的换码无静态警告**：前置校验三连显式化 + 门禁兜底。
11. **同槽显式换码 auth 即时覆盖不可逆**：旧码若未另行保存即丢失，恢复 = owner 重发码。
12. **rm -rf 级全清（含 meta/config）= 机器重置语义**：裸 pull 归位（文档写明——有意代价：全清即声明放弃默认槽历史）。
