# Plan 40 第二批设计：TUI/向导多实例接入 + 首次 enroll 自动归位 + `cache config` 子命令

> 2026-08-27 grilling 两轮定案（owner 全部确认）。**基 spec = `2026-08-26-plan-40-multi-instance-cache-design.md.rev3.md`**（下称 rev3）——本文件只做第二批五项的实现级设计，rev3 已钉死的边界（批次划分 §8、门禁语义 §2.4、目录布局 §2.2、老 serve 行为 §5）不重议。范围 = backlog「第二批（已裁决待开工）」五项：① TUI client 页实例列表+切换；② 向导 `--instance` 接入卡；③ 首次 enroll 自动归位；④ 连接编辑表单换码预防性警告；⑤ 独立 `cache config [--instance] --max-offline` 子命令。
>
> **rev1（2026-08-27）**：初稿一轮评审 9 处吸收：归位条件扩为两个 override env；TUI 单槽模式；归位时序重排 + DEK 创建后置；面板 `[c]` 保存切换 `m.instance`；面板真空默认强制显式字段；picker 自动打开收紧；残留物登记；auth 写失败用例。
> **rev2（2026-08-27）**：二轮 10 处（7 证实）吸收：面板新实例 MkdirAll+即时写；cap 校验统一最终目标槽；`cache config` 仅已存在实例；两 override env 语义分离；picker 默认行区分；换码警告静态化；恢复路径文案。
> **rev3（2026-08-27，owner 特批第 3 轮）**：三轮 9 处吸收：真空 v3（config 入定义）；`[c]` 守卫无条件放行；表单前置校验三连；MkdirAll 失败清理；§9 零写盘限定；非法头名分派钉死；归位幂等用例。
> **rev4（2026-08-27，owner 特批第 4 轮）**：四轮 10 处吸收，两处结构性修正：真空 v4（meta 入定义——"曾有材料"天然痕迹；runbook v2 清三件套保留 meta+config）；删除"跳默认槽 precheck"机制（批1 时序恢复）；auto-picker 判定统一；auth-only 目标放行；MkdirAll 后置 + 零目录断言；空码条件规则；casefold 基准。
> **rev5（2026-08-27，owner 特批第 5 轮——书面收口轮，先例=批1 末轮免复审）**：五轮 10 处（全部文案/登记/测试级 + 一处一行语义收紧，零架构项）吸收：① **pull 时目标槽校验语义收紧**——`cache.config.json` 的**有效性校验独立于 env**（存在即校验，env 只覆盖生效值）——杜绝"env 有效拉得动、无 env 载不动"半开态的写入面（该暴露为批1 既有 `resolveMaxOffline` env 短路语义，rev4 文本曾过度宣称"非法必拒"）；② **规范名（canonical）原则**——表单 casefold 命中既有实例 ⇒ 路由一律用既有目录的规范名（= 服务器下发名 = 目录名），pull 响应头强一致与门禁身份比对均 **exact**（对规范名；casefold 比对反而制造跨槽覆盖）；③ CLI 归位提示改指真正破点（`mcp --cache` 需 `--instance`，裸 pull 幂等再归位）；④ retarget 写死"整套 `CachePathsFor` 重解析"（audit sidecar/quarantine 随行）；⑤ env 单槽模式的面板真空拒绝文案分模式；⑥ 跨槽已存在实例 = 表单层硬拒（删除不可达的"门禁兜底"表述，换码须先 `[i]` 切槽）；⑦⑧ 测试矩阵补两行（env 非法 HTTP 前拒 / env 有效+目标 config 非法拒 / casefold 变体碰撞）；⑨ §12 补 CLI-first 接入指引；⑩ TOCTOU 残余登记。
>
> 本轮（2026-08-27）拍板、不重议的决策：
> **R1-Q1** TUI 切换 = `[i]` picker overlay（默认实例 + `ListInstances()` 各一行），**会话内有效**；默认槽**真真空（归位真空同款四文件判定）**而 instances 有货 → 启动自动开 picker；picker 只列**已存在**实例。
> **R1-Q2** 连接表单增**可选"实例名"字段**：空 = 默认（wizard 首拉自动归位）；非空 = 显式路由（即时校验 + 拉取时响应头**exact** 强一致对规范名 + 保存前置校验三连）。
> **R1-Q3** auth 写序 = **"目标槽未知则后移、已知则即时"**：wizard 保存不写盘、pull 成功后按实际槽写；面板保存即写**表单路由槽**（新实例目录先 `MkdirAll(0700)`，写失败清理本次新建空目录）并切换 `m.instance`；CLI 按 `DoPull` 返回实际槽写。
> **R1-Q4** 自动归位触发条件 = **真空 v4 定义**：默认槽 `cache.bin`、`cache.auth.json`、`cache.meta.json`、`cache.config.json` **四者均不存在**——meta/config 任一在场 = 默认槽意图标记。**任一单槽 override env 在场 → 不归位**；响应头 name 过不了 `instname.Valid` → 拒写盘。
> **R1-Q5** `DoPull` 签名改为返回 `(PullResult, error)`——编译器驱动迁移全部调用点。
> **R1-Q6** 换码警告 = 纯 UX 层（构建时静态按选中槽）。
> **R1-Q7** `cache config` 子命令：显示/写入（仅已存在实例）；不做 off 开关；默认槽 meta/config 即意图标记勿删。
> **R1-Q8** doctor 多实例不进本批；版本 v0.12.0；流程 = spec → 评审收敛 → plan → SDD。
> **R2-Q1** 归位触发面 = {CLI 裸 `cache pull`（真空机器）+ TUI wizard 首拉} 两处；CLI 补一行显式提示。
> **R2-Q2** 首拉成功后 TUI 自动选中实际生效实例；`[c]` 守卫无条件放行。
> **rev1-R** env-override 与多实例 UI 互斥（单槽模式禁用）；面板真空默认强制显式字段。
> **rev2-R** 面板新实例 = MkdirAll + 即时写；config 不预配置未 enroll 实例。
> **rev3-R** 表单跨槽路由必须显式重输设备码；casefold 碰撞拒。
> **rev4-R** 换码 runbook 清三件套（auth+bin+quarantine）、保留 meta+config；cap precheck 时序回归批1。
> **rev5-R** pull 时文件有效性校验独立于 env（env 只覆盖生效值）；规范名原则（表单 casefold 归一到目录规范名，pull/门禁 exact）。

## 0. 现状事实（2026-08-27 于批1 合并后代码核实；函数名锚定）

1. **TUI client 页是纯默认实例视图**：`clientModel`（tui/clientpage.go:32）无实例概念；`refreshDataCmd` 读默认槽 cred + `LoadCacheSnapshot()`（:94-118）；`[s]` → `syncCmdMode` → `DoPull(..., PullOpts{Timeout})` 不带 Instance（:126-143）；`[c]` → `editConnForm` → `WriteCacheCred`（默认槽，:381）——rev3 §0.11 钉死的唯一"auth 先于 pull"路径。
2. **wizard client 流**：`enterClient`（tui/wizard.go:184）→ 连接表单 → `connSavedMsg` → `syncCmdMode(cred, wizard=true)` → `pullSucceededMsg` → `clientFinishScreen(serveURL)` 生成 `.mcp.json` 接入卡，offline 形态 `["mcp", "--cache"]`（clientpage.go:439-465）。
3. **DoPull 结构**（clientops.go:442-637）：顶部按 `o.Instance` 解析 `CachePathsFor` → cap 预检 `resolveMaxOffline(dir)`（**HTTP 前**）→ HTTP → 身份门禁 → 写盘（首动作 `MkdirAll` + bin→meta）。**归位的目标名（响应头 `X-Sshmgr-Device-Name`）在 HTTP 响应到手后才可知**。
4. **两个 override env 语义分离**：`CachePathsFor`（clientops.go:137-141）只认 `SSHMGR_CACHE_DIR`（cache 目录路由）；`paths.CacheDekPathFor`（paths.go:83-86）只认 `SSHMGR_CACHE_DEK`（盖过 per-instance DEK 派生）。`SSHMGR_CACHE_DEK_DIR` 只搬迁 DEK 根目录（连贯 seam）。
5. **DEK 加载两形态**（dek.go）：`loadOrCreateDEK`（DoPull 路径，缺失则生成+落盘）；`loadDEK`（LoadCacheSnapshotFor 路径，不自动创建——缺失即拒载）。
6. **`resolveMaxOffline` env 短路**（config.go:20-38）：`SSHMGR_CACHE_MAX_OFFLINE` 命中即返回 env 值，**config 文件从不被读/校验**（批1 既有语义）——env 有效 + 文件非法的组合在现状下"拉得动"而"无 env 载不动"（rev5 §1.2-5 收紧写入面）。
7. **CLI pull 的 auth 写序**：`DoPull` 成功后 `WriteCacheCredFor(instance="")`（cli/cache.go:119）+ config 同槽（:122-128）——归位必须连动 auth 落槽（§5）。
8. **DoPull 调用面**：生产 5 处 + 测试 ~40 处（机械 `err :=` → `_, err :=`）。
9. **config 积木全在**（clientops/config.go）：`resolveMaxOffline(dir)`、`WriteCacheConfig(dir, v)`、`ValidateMaxOffline(v)`。**config 是可选文件**——仅由 `pull --max-offline` / `cache config` 写出。
10. **多实例积木全在**（批1）：`ListInstances()`/`InstancesRoot()`、For-variant 家族、`checkInstanceFlag`（cli/common.go:18）、`cacheStatusList`（cli/cache.go:233）。
11. **Plan 30 消息门**（clientpage.go:145-168）：新增 client-owned 消息类型必须登记 owned allowlist。
12. **面板 `[s]` 归位不可达**：面板同步前提 = 默认槽 auth 在 = 非真空。归位触发面 = {CLI 裸 pull（真空机）、TUI wizard 首拉}。
13. **`[c]` 现守卫**：面板模式 `m.cred == nil` → 拒绝开表单（clientpage.go:248-254）——rev3 修正为无条件放行。
14. **`WriteCacheCredFor` 不建父目录**（实验实证 2026-08-27）：面板路径写新实例槽 auth 前必须自建目录（§5）；wizard/CLI 后移写天然满足。
15. **实例名 `default` 合法**（instname.go 核实）：与"默认槽"显示语面潜在同名（§3.1 显示区分）。
16. **NTFS 大小写不敏感**（rev3 §0.10 实验实证）：`instances/AGENTA` 与 `instances/agentA` 解析同一目录——casefold 只用于**归一到规范名**（表单层），比对一律 exact（§4/§1.2-4）。
17. **meta 随每次成功 pull 覆写**（clientops.go:621，既有行为）：默认槽只要有過任何一次成功 pull 就有 meta 在盘——"默认槽曾有材料"的天然痕迹文件（rev4 真空 v4 依据）。

## 1. 首次 enroll 自动归位（rev3 §2.4 二批列落地）

### 1.1 触发条件（真空 v4 定义，全部满足才归位）

| # | 条件 | 依据 |
|---|---|---|
| 1 | `o.Instance == ""`（无显式路由） | 显式路由已是命名实例 |
| 2 | `pin != ""` 且响应 200 且头 `X-Sshmgr-Device-Name` 非空 | plaintext 无头永不归位；老 serve 无头不归位（不归位、落默认目录 + 既有升级 WARNING） |
| 3 | 头 name 过 `instname.Valid` | 非法 → **拒写盘**（owner 改名重发，既有文案）——不属"不归位"分支 |
| 4 | **默认槽 `cache.bin`、`cache.auth.json`、`cache.meta.json`、`cache.config.json` 四者均不存在**（真空 v4） | **meta/config 任一在场 = 默认槽意图标记**：meta 随每次成功 pull 覆写（§0.17）——"曾有材料"的天然痕迹；config = "曾配置"。二者共同覆盖全部存量形态。换码 runbook v2 = 清三件套（auth+bin+quarantine）、**保留 meta 与 config**（§12）。auth 在而 bin 不在（§9.10 边缘）→ 不归位、默认目录写入 + 门禁补记（现状行为） |
| 5 | **`SSHMGR_CACHE_DIR` 与 `SSHMGR_CACHE_DEK` 均未设** | 任一在场 = 单槽完全覆盖语义——归位会把"无 flag pull"变成事实上的命名实例而材料却挂在单槽语义下，env 清除后按派生 DEK 拒载（端到端实证）。`SSHMGR_CACHE_DEK_DIR` 不在此列（连贯 seam） |

### 1.2 归位机制（DoPull 内部单一时序；时序 = 批1 + 目标槽校验一处新增）

1. **cap precheck 恒在 HTTP 前**（批1 时序）：DoPull 顶部对**本次解析槽**（默认/显式实例）执行 `resolveMaxOffline`——真空候选按定义四文件全缺，默认槽 precheck 恒为 off 或 env-only（env 非法 → HTTP 前拒）；非真空路径非法 config → HTTP 前拒（现状）。env 短路文件校验的既有语义见 §1.2-5 收紧。
2. HTTP 拿响应 + 头。**分派：条件 2 不满足（头缺失/plaintext）→ §1.3 不归位分支；条件 3 不满足（非法头名）→ 拒写盘（终局错误，不进 §1.3）。**
3. 真空候选成立 → **retarget：以头 name 重跑 `CachePathsFor` 整套解析**（rev5）——`dir/bin/meta/audit` 四元组与 `quarantine/` 子路径全部随行重解析到 `instances/<name>/`（audit sidecar 与隔离材料绝不落回默认槽——基 spec §2.2"全消费面接线"纪律）。
4. **目标槽门禁**（`gateNamedInstance` 形态；**身份比对一律 exact**——rev5：casefold 比对会让头 `agenta` "命中"存量 `instances/AGENTA` 的目录而身份实异，制造跨槽覆盖；NTFS 同目录语义下 casefold 命中即同一物理槽，不存在需要 casefold 判异的合法态）：目录在且 meta 身份（exact）异于头 name → 拒；**bin 在而 meta 缺失/不可读 → 拒**（半写态 + 清理文案）；meta 身份相同 → **放行**（幂等再归位）；**目录在而 bin 缺（auth-only / 空目录）→ 放行**（fresh-slot 语义，auth 顺位覆盖——面板新实例 enroll 的闭合通路）。
5. **目标槽 cap 校验（post-HTTP、pre-write；rev5 语义收紧）**：对目标实例目录执行——① **文件有效性校验独立于 env**：`cache.config.json` 存在即校验（合法性与否不受 `SSHMGR_CACHE_MAX_OFFLINE` 在场影响——env 只覆盖**生效值**，不豁免校验）；非法 → **拒绝本次 pull**（响应丢弃、零写盘）——杜绝"env 有效拉得动、无 env 的 loader 载不动"半开态的写入面（批1 既有 env 短路语义（§0.6）在 pull 写入面收紧；**load 路径保持 env 优先现状**——手动改坏文件属可接受残余）。② 生效值按 env > file > off 取，用于锚/明文拒判定。
6. **DEK 创建后置**：`loadOrCreateDEK(头name)`——位于全部门禁/cap 校验之后，归位路径任何拒绝分支不创建任何 DEK 文件（§9）。
7. **写盘**：首动作 `MkdirAll(0700, instances/<name>/)`（位于全部门禁/cap/DEK 之后——拒绝分支不新增目录）→ bin→meta（既有提交序）+ meta 记 `device_name`。

### 1.3 不归位分支（行为与批1一致；唯一时序差异见注）

- 头缺失（老 serve）：默认目录 + 既有升级 WARNING。
- §9.10 auth-only（auth 在 bin 无）：默认目录 + 门禁补记。
- **默认槽 meta 或 config 任一在场**（意图标记）：默认目录写盘（policy 就地生效）。
- **任一单槽 override env 在场**：写 override/默认目录（单槽语义一致）。
- plaintext（`--allow-plaintext`）：无头 → 默认目录。
- **时序注**：全部不归位路径与批1 时序逐字节一致（cap precheck 恒 HTTP 前）；唯一时序差异只存在于归位路径本身（目标槽校验在 HTTP 后、写盘前）。

### 1.4 401/失败路径

401 发生在头到手前 → 归位无从谈起；`QuarantineCacheFor("")` 打默认槽（真空 = 无材料 = no-op）——既有行为不变。

### 1.5 CLI 提示（rev5 改指真正破点）

归位发生时 `StatusOut` 追加一行：`first enroll located to instance <name> — mcp --cache needs --instance <name> in .mcp.json (bare cache pull re-locates idempotently; only the agent's cache-mode launch is affected)`。（裸 pull 幂等再归位不需要 flag——§11.7；真正受影响的是无 flag 的 `mcp --cache` 启动。）

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

启动 = 默认实例起步；若默认槽**真真空（归位真空同款四文件判定）**而 `ListInstances()` 非空 → 首个 `dataReadyMsg`（errMsg 形态）后自动打开 picker + 提示。**非真空的"部分缺件"形态（bin 在 auth 缺 / meta 在 / config 在）一律不弹**——默认槽有意图或材料，不把用户从默认槽恢复路径引开。

### 3.3 per-instance 动作

- `[s]`：`ReadCacheCredFor(m.instance)` → `PullOpts{Instance: m.instance}`（无 cred → 既有错误行）。
- `[c]`：prefill 选中实例 cred + 实例字段预填选中名（默认 → 空）；**保存后 `m.instance` = 表单路由结果并 refresh 该槽**。
- `[t]`：不变。

### 3.4 消息门登记（Plan 30 checklist）

新增 client-owned 消息类型必须登记 owned allowlist——配套路由测试钉住。

### 3.5 env-override 单槽模式

**任一单槽 override env 在场 → 单槽模式**：`SSHMGR_CACHE_DIR` = 目录路由恒 override；`SSHMGR_CACHE_DEK` = 路由不变但全部实例共享同一 DEK。统一处置：`[i]` 禁用、auto-picker 不触发、表单实例字段禁用、页顶横幅。**禁用而非适配**。`SSHMGR_CACHE_DEK_DIR` 不触发。

## 4. 连接表单实例字段（保存前置校验三连 + 规范名原则）

- huh 可选输入"实例名"：空 = 默认（wizard 首拉自动归位）；非空 = 显式路由。即时校验 `instname.Valid`。
- **规范名原则（rev5）**：表单层 casefold 比对仅用于**归一到既有目录的规范名**（= 服务器下发名 = 目录名）——字段 `agenta` casefold 命中既有 `instances/AGENTA/` ⇒ 路由值取 **AGENTA**（规范名）；pull 层响应头强一致对规范名 **exact** 比较（两层不冲突：表单归一在前，pull exact 在后）。
- **保存前置校验三连**（提交时判定；比较一律 casefold 归一后）：
  1. **字段（归一后）≠ 选中槽（归一后）→ 设备码必填**（"留空=保持"的保持对象是选中槽的既有 token；该规则前提 = 选中槽确有 auth——选中槽无 auth 时空码一律必填，无"保持"可言）。
  2. **字段 casefold 命中既有实例且 ≠ 选中槽 → 校验拒**（NTFS 碰撞保护；文案指明碰撞对象）。**跨槽路由到已存在实例 = 表单层硬拒（rev5 登记）——对该实例换码须先 `[i]` 切到该实例再 `[c]`（同槽换码走第 3 条）**。
  3. **命中且 == 选中槽 → 允许**（同槽显式换码；路由用规范名；不可逆由重输恢复，§13）。
- **env 互斥即时提示**：单槽 override env 在场 → 字段禁用 + 横幅（§3.5）。
- prefill：选中命名实例 → 预填其名（规范名）；默认实例 → 空。
- **面板模式 × 默认槽真空（四文件判定）× 字段空 → 表单校验拒**，文案分模式（rev5）：常规——`默认实例无材料——首次 enroll 请走向导流程（自动归位），或填实例名显式路由`；**env 单槽模式——`override env（SSHMGR_CACHE_DIR/SSHMGR_CACHE_DEK）覆盖中：单槽语义下无多实例路由，请清除 env 或按单槽使用`**（该模式下"填实例名/向导归位"两条出路均不可用，不得误导）。
- wizard 与面板共用此表单。

## 5. auth 写序（"目标槽未知后移、已知即时"）

| 路径 | 写序 | 落槽 |
|---|---|---|
| CLI `cache pull` | DoPull 成功后（现状） | `WriteCacheCredFor(res.Instance)`（归位连动）；config 同槽 |
| TUI wizard 首拉 | 表单保存不写盘；pull 成功后写 | `WriteCacheCredFor(res.Instance)`（目标目录已由 DoPull 建）；写失败 = WARNING 行含恢复路径（`auth 未落盘——本 TUI 的 [s] 同步不可用；恢复 = CLI cache pull 或重跑向导表单（输入已保留）`） |
| TUI 面板 `[c]` | 保存即写（现状时序） | 新实例槽先 `MkdirAll(0700)` 再写；写失败 best-effort 清理本次新建的空目录（仅空才删）；保存后 `m.instance` 切槽 + refresh |

- wizard 改善登记：后移后失败 auth 零写入。
- **auth-only 新实例边缘**：面板路由新实例保存后、首拉前——§9.10 同族，`[s]` 首拉即闭合。
- §9.10 收缩登记："先写 auth"路径收缩至"面板改连接（非真空槽）+ 面板新实例 enroll"两条（均登记）。

## 6. 换码预防性警告

- **构建时静态判定**（按表单打开时的选中槽）：
  - **默认槽 + bin 在**：`默认实例已绑定设备 <meta.device_name 或 "(旧 cache 未登记)">——更换设备码前须清三件套（cache.auth.json + cache.bin + quarantine/，保留 cache.meta.json 与 cache.config.json——它们是默认槽意图标记，删了重 enroll 会被归位到实例槽）重 enroll，否则下次同步将被门禁拒绝；若是本机第二个 agent，请在"实例名"字段填新实例名。`
  - **命名实例槽 + bin 在**：`实例 <name> 已绑定设备 <device_name>——换码须删除该实例目录重 enroll，否则同步将被拒。`
  - 选中槽无 bin：不显。
- **跨槽路由到已存在实例的换码：表单层硬拒（§4 rule 2，rev5）**——不存在"拉取时门禁兜底"层次；对既有实例换码的唯一 TUI 路径 = `[i]` 切槽后 `[c]`（同槽换码，警告已显）。
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
- **不做 off 开关**：删 cap = 手动删该实例 config 文件。**默认槽警示**：删默认槽的 config 或 meta 都会削弱意图标记——换码 runbook 二者皆勿删（§12）。
- `--instance` 走 `checkInstanceFlag` 互斥。不 pull——无 plaintext 语义、不触发归位。

## 9. 安全分析

- **归位不新增暴露**：头来自 pinned TLS；非法名拒；物理碰撞/半写态检查在归位路径同样生效（身份比对 exact，§1.2-4）；**归位路径任何拒绝分支 = 零写盘、零新增目录、零新增 DEK**。非真空路径保留批1 现状（HTTP 前预建默认 DEK——门禁拒绝时可能已存在，既有行为）。
- **半开状态防线（rev5 收紧）**：非真空路径 cap precheck 恒 HTTP 前；归位路径目标槽校验 pre-write 且**文件有效性独立于 env**——Plan 37"拉得动必载得动"在 pull 写入面全路径成立（load 侧 env 优先为批1 现状，手动改坏文件属可接受残余）。
- **单槽 override env × 归位互斥**；**表单跨槽错置防线**（跨槽码必填 + casefold 拒 + 规范名归一）；**警告是体验层**；**auth 后移不降保护**；**picker 不解密**。

## 10. 兼容性

- **v0.12.0 client × v0.11.0 serve**：全功能。**× 老 serve**：无归位；字段非空 → pull 拒；接入卡无 `--instance`。
- **存量零迁移（全形态成立）**：无 flag 行为不变；**换码 runbook 回默认槽对全部存量形态成立**——有 config 靠 config、无 config 靠 meta。
- **DoPull 签名变更**：internal 包。compat-matrix 登记 v0.12.0 行。

## 11. 测试计划

1. **归位触发**：真空机（四文件均缺）裸 `cache pull`（两 override env 均未设）→ 材料落 `instances/<name>/` + auth 同槽 + CLI 提示行 + meta `device_name` 记录。
2. **归位不触发七态**：auth 在 bin 无；头缺失；`SSHMGR_CACHE_DIR` 在场（→override 目录）；`SSHMGR_CACHE_DEK` 在场（→默认、材料用 env DEK）；plaintext；config 在场（→默认，policy 生效）；meta 在场（无 config 存量形态 → 默认+补记）。`SSHMGR_CACHE_DEK_DIR` 在场 → 归位照常（实例 DEK 落 env dir）。
3. **换码 runbook 回默认槽**：有材料默认槽 → 清三件套（保留 meta+config）→ 裸 pull → 默认槽 + 门禁补记（meta 覆写）；对照四件全清 → 归位（机器重置语义）。
4. **归位目标门禁四分支 + 大小写变体（rev5 补）**：meta 身份异 → 拒；bin 在 meta 不可读 → 拒；meta 身份同 → 放行（幂等）；目录在 bin 缺（auth-only/空）→ 放行；**头 name 与存量 meta 身份仅大小写不同（`agenta` vs `AGENTA`）→ 拒**（exact 比对钉子——casefold 实现即回归）。
5. **归位拒绝零写盘/零目录**：目标实例目录预置非法 config → 拒 + bin/meta/DEK 全零新增 + `instances/` 目录树零新增。
6. **cap 校验时序（rev5 扩两行）**：① 非真空默认槽非法 config → HTTP 前拒（现状回归）；② 归位目标实例目录非法 config → 写盘前拒（唯一新增校验）；③ plaintext + B-on(env) → HTTP 前拒（现状回归）；④ 显式 `--instance` 路径非法 config → HTTP 前拒（现状回归）；⑤ **真空候选 + env 非法 → HTTP 前拒**（rev5 补 §1.2-1 声明行）；⑥ **env 有效 + 目标实例目录 config 非法 → 写盘前拒**（rev5 §1.2-5 收紧钉子——env 只覆盖生效值，不豁免校验）。
7. **归位幂等**：归位后默认槽仍真空 → 再裸 pull 同码 → 同实例目录放行、DEK 不重复生成、auth 覆写同槽。
8. **签名迁移零语义变化**：既有 DoPull 测试机械迁移后绿；`PullResult.Instance` 无 flag 默认路径 == ""。
9. **TUI picker**：开/选/切换 per-instance 读写；auto-picker 触发 = 四文件真空 + instances 非空；bin 在 auth 缺 / meta 在 / config 在 → 不弹；默认行「（默认实例）」区分；`[s]` 带选中实例；`[c]` prefill/写槽/切槽+refresh；消息门登记路由测试。
10. **TUI 单槽模式**：两 override env 各自 → `[i]` 禁用/auto-picker 不触发/字段禁用/横幅；**面板真空拒绝文案 = env 模式版**（rev5）；`SSHMGR_CACHE_DEK_DIR` → 照常。
11. **表单前置校验三连（casefold 归一）**：非法名拒；空=默认；面板真空默认×空 field 拒（常规文案）；字段≠选中槽（归一后）+空码 → 拒；选中槽无 auth + 空码 → 拒；字段 casefold 撞他实例（≠选中槽）→ 拒（**含纯大小写变体：选中 agentB 字段 AGENTB → 命中他实例 agentB → 拒**）；字段 casefold == 选中槽（`AGENTA`/`agenta`）→ 同槽换码允许且**路由值 = 规范名 `AGENTA`**（rev5 钉子）。
12. **wizard 流**：真空首拉 → 归位 + 自动选中 + 接入卡 `--instance <name>`；首拉失败 → 两槽 auth 零写入；显式字段 enroll → 卡片同（字段归一规范名）；pull 成功 auth 写失败（目录占位）→ WARNING 含恢复路径 + 卡片仍出 + 下次成功 pull 恢复。
13. **换码警告形态**：默认槽有材料（文案含三件套+保留 meta/config）；命名实例；无 bin 不显。
14. **config 子命令**：三源显示；写入读回；env WARNING；互斥；不存在目录报错含指引。
15. **面板新实例写失败清理**：目录占位 → 本次新建空目录被清理；既有目录写失败 → 保留。
16. **e2e 双实例 enroll 全程 TUI 化**：真空机 A 首拉归位 → 默认槽占用机 B 表单字段 enroll（MkdirAll + auth 落 B + 切槽）→ `[s]` 首拉 → 两实例 `mcp --cache --instance` 各自起、裁剪正确。

## 12. 文档联动

- `docs/tui-multi-machine.md`：picker、表单字段与前置校验三连（含规范名原则）、换码警告、单槽横幅。
- `docs/multi-machine.md`：自动归位语义（真空 v4 + 不归位分支）；**换码 runbook v2：清三件套、保留 meta 与 config（意图标记勿删）**；双 agent enroll 全程 TUI 流程；`cache config`（仅已存在实例；默认槽 meta/config 勿删）；**CLI-first 指引（rev5 补）：真空机用 CLI 裸 pull 归位后，手工 `.mcp.json` 必须加 `--instance <name>`（CLI 路径没有向导接入卡——README 同步写明）**。
- `docs/agent-access.md` 一句话指向；`README.md` 子命令 + `[i]` + CLI 归位 `--instance` 指引；`compat-matrix.md` v0.12.0 行；`backlog.md` 销项 + doctor 登记。
- **产品内文案联动**：`gateDefaultInstance` 拒绝文案的三选一引导更新为三件套表述（保留 meta/config）。

## 13. 残余与登记

1. **doctor 多实例不在本批**（Plan 38 体系内解决）。
2. **picker 行不解密**（§3.1 取舍）。
3. **config 无 off 开关**：手动删文件；默认槽删 config 或 meta 都削弱意图标记。
4. **面板 `[c]` 换码仍先写 auth**（收缩至两条已登记路径）：警告 + 前置校验三连 + 门禁兜底。
5. **wizard 首拉成功后 auth 写失败** = WARNING 不阻断（含恢复路径文案）。
6. **会话内 picker 不记忆**（零新增状态文件）。
7. **换码三件套保留的 meta 携带旧 device_name**：bin 已删 → 门禁不生效；下次成功 pull 覆写——无害痕迹（文档写明"保留是特性"）。
8. **auth-only 新实例**：面板路由新实例保存后、首拉前——§9.10 同族，`[s]` 首拉即闭合。
9. **实例名 `default` 同名歧义**：显示已区分；命名纪律建议避开（不改服务端白名单）。
10. **跨槽路由到已存在实例 = 表单层硬拒（rev5 钉死）**：对该实例换码的唯一 TUI 路径 = `[i]` 切槽后 `[c]`；CLI 路径（`pull --instance`）不受影响。
11. **同槽显式换码 auth 即时覆盖不可逆**：旧码若未另行保存即丢失，恢复 = owner 重发码。
12. **rm -rf 级全清 = 机器重置语义**：裸 pull 归位（文档写明——有意代价）。
13. **表单 casefold 检查与面板写入间 TOCTOU（rev5 登记）**：`ListInstances()` 检查后、`MkdirAll`+写 auth 前的理论并发窗口（两个进程同时以大小写变体创建同实例）——owner 单人交互面 + 服务端全历史 casefold 查重双前置，实际暴露极窄；与基 spec §9.8 跨进程 casefold 双插窗口同族接受，发生时目标目录物理碰撞检测（§1.2-4）在 pull 时兜底。
14. **load 侧 env 优先短路文件校验（批1 既有语义，rev5 保留）**：env 携带进程可加载而 env-less 进程拒载的组合只能由**写入后手动改坏文件**造成（pull 写入面已收紧为文件校验独立于 env，§1.2-5）——可接受残余。
