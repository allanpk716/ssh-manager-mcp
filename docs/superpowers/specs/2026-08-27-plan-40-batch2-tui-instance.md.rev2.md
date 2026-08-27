# Plan 40 第二批设计：TUI/向导多实例接入 + 首次 enroll 自动归位 + `cache config` 子命令

> 2026-08-27 grilling 两轮定案（owner 全部确认）。**基 spec = `2026-08-26-plan-40-multi-instance-cache-design.md.rev3.md`**（下称 rev3）——本文件只做第二批五项的实现级设计，rev3 已钉死的边界（批次划分 §8、门禁语义 §2.4、目录布局 §2.2、老 serve 行为 §5）不重议。范围 = backlog「第二批（已裁决待开工）」五项：① TUI client 页实例列表+切换；② 向导 `--instance` 接入卡；③ 首次 enroll 自动归位；④ 连接编辑表单换码预防性警告；⑤ 独立 `cache config [--instance] --max-offline` 子命令。
>
> **rev1（2026-08-27）**：初稿经一轮外部评审发现 9 处问题（全部经代码核对/临时实验证实）吸收：① 归位条件 5 扩为**两个** override env（`SSHMGR_CACHE_DIR`/`SSHMGR_CACHE_DEK` 任一在场即不归位——DEK env 下归位材料用 env DEK 加密、后续无 env 加载按派生 DEK 拒载，端到端实证）；② TUI env-override 行为钉死为**单槽模式**（禁 `[i]`/auto-picker/表单实例字段 + 横幅）；③ 归位时序重排：真空候选判定前置（跳过默认槽 cap precheck）→ 拿头 → 门禁 → cap 校验 → **DEK 创建后置**（拒绝分支真·零写盘）；④ 面板 `[c]` 保存后 `m.instance` 切到表单路由槽并 refresh；⑤ 面板模式真空默认 × 空 field = 表单校验拒（自动归位触发面维持 {CLI 裸 pull, wizard 首拉} 两处）；⑥ picker 自动打开条件收紧（cred 缺**且 bin 缺**）；⑦ §13 登记默认槽残留 meta/quarantine 孤留；⑧ §11.8 补 auth 写失败 WARNING 用例（目录占位形态触发，实证无需代码 seam）。
>
> **rev2（2026-08-27）**：二轮评审 10 处问题（7 条证实，含一组高severity 共识）吸收：① **面板 `[c]` 路由新实例的写序**——保存前 `MkdirAll(0700)` 目标实例目录再即时写 auth（否则 `WriteCacheCredFor` 因父目录不存在必败——临时实验直接复现该失败形态）；auth-only 新实例登记为 §9.10 族已接受边缘（首拉自愈）；② **cap 校验对象统一为最终目标槽（无论归位与否）**——真空候选的头缺失/plaintext 不归位分支在写盘前对默认槽补 cap/明文拒检查（堵半开态复活）；③ `cache config` 仅对**已存在**实例目录可写（不预配置）；④ §3.5 措辞修正——两个 override env 语义分离（目录路由 override vs DEK 共享）；⑤ picker 默认行显示"（默认实例）"与实例行区分（实例名 `default` 合法可撞行）；⑥ §6 换码警告简化为构建时静态（按选中槽）——删除依赖输入后字段值的分支（静态表单不可达）；⑦ wizard auth 写失败 WARNING 文案写明恢复路径（auth 未落盘时 TUI `[s]` 不可用）。
>
> 本轮（2026-08-27）拍板、不重议的决策：
> **R1-Q1** TUI 切换 = `[i]` picker overlay（默认实例 + `ListInstances()` 各一行），**会话内有效**（不落盘，下次启动回默认实例）；默认槽真真空（cred 与 bin 均无）而 instances 有货 → 启动自动开 picker + 提示；picker 只列**已存在**实例（新实例名入口 = 表单字段）。
> **R1-Q2** 连接表单增**可选"实例名"字段**：空 = 默认（首拉自动归位）；非空 = 显式路由该实例（`instname.Valid` 即时校验 + 拉取时响应头强一致）。
> **R1-Q3** auth 写序 = **"目标槽未知则后移、已知则即时"**：TUI wizard 模式表单保存不写盘，pull 成功后按实际槽 `WriteCacheCredFor`；TUI 面板模式保存即写**表单路由槽**（新实例目录不存在时先 `MkdirAll(0700)`——rev2）并切换 `m.instance`；CLI 按 `DoPull` 返回的实际槽写。
> **R1-Q4** 自动归位触发条件 = **真空定义**：默认槽 `cache.bin` 与 `cache.auth.json` **均**不存在。auth 在而 bin 不在（§9.10 边缘）→ 不归位、默认目录写入 + 门禁补记（现状行为）。**任一单槽 override env（`SSHMGR_CACHE_DIR`/`SSHMGR_CACHE_DEK`）在场 → 不归位**（单槽语义优先；`SSHMGR_CACHE_DEK_DIR` 为目录级连贯 seam、不拦归位）；响应头 name 过不了 `instname.Valid` → 拒写盘。
> **R1-Q5** `DoPull` 签名改为返回 `(PullResult, error)`（`PullResult{Instance string}`，""=默认槽）——编译器驱动迁移全部调用点（弃回调方案：可选接入可被静默遗忘）。
> **R1-Q6** 换码警告 = 纯 UX 层（**构建时静态**按选中槽，表单顶部插行，不阻断；实际拦截仍由门禁 fail-closed 兜底）。
> **R1-Q7** `cache config` 子命令：无 flag 只读显示生效 cap + 来源（env/file/off）；`--max-offline` 写入（**仅已存在实例**——rev2）；**不做 off 开关**（删 cap = 手动删该实例 config 文件，文档写明）。
> **R1-Q8** doctor 多实例**不进本批**（跟随 Plan 38-doctor 体系独立立项）；版本 **v0.12.0**；流程 = 独立批2 spec（本文件）→ 评审收敛 → plan → SDD。
> **R2-Q1** 撤回"面板 `[s]` 归位提示"（R1 草案附加点）——真空定义下面板 `[s]` 前提是 cred 已加载 = 默认槽 auth 在 = **非真空**，归位逻辑上不可达。归位触发面收敛为：**CLI 裸 `cache pull`（真空机器）+ TUI wizard 首拉** 两处；CLI 补一行显式提示（§1.5）。
> **R2-Q2** 首拉成功后 TUI **自动选中实际生效实例**（面板直接落在新实例视图）；`[c]` 守卫放宽（选中槽无 cred 但机器存在实例 → 放行，空 code 提交校验兜底）。
> **rev1-R** env-override 与 TUI 多实例 UI 互斥（单槽模式禁用，禁用而非适配）；面板真空默认强制显式实例字段。
> **rev2-R** 面板新实例 enroll = MkdirAll + 即时写（不隐式发起网络拉取——离线可保存）；`cache config` 不预配置未 enroll 实例。

## 0. 现状事实（2026-08-27 于批1 合并后代码核实；函数名锚定）

1. **TUI client 页是纯默认实例视图**：`clientModel`（tui/clientpage.go:32）无实例概念；`refreshDataCmd` 读默认槽 cred + `LoadCacheSnapshot()`（:94-118）；`[s]` → `syncCmdMode` → `DoPull(..., PullOpts{Timeout})` **不带 Instance**（:126-143）；`[c]` → `editConnForm` → `WriteCacheCred`（默认槽，:381）——rev3 §0.11 钉死的唯一"auth 先于 pull"路径。
2. **wizard client 流**：`enterClient`（tui/wizard.go:184）→ 连接表单 → `connSavedMsg` → `syncCmdMode(cred, wizard=true)` → `pullSucceededMsg` → `clientFinishScreen(serveURL)` 生成 `.mcp.json` 接入卡，offline 形态 `["mcp", "--cache"]`（clientpage.go:439-465）；online http 形态无实例概念。
3. **DoPull 结构**（clientops.go:442-637）：顶部按 `o.Instance` 解析 `CachePathsFor` → cap 预检 `resolveMaxOffline(dir)` → HTTP → 身份门禁 → 写盘。**归位的目标名（响应头 `X-Sshmgr-Device-Name`）在 HTTP 响应到手后才可知**——归位意味着路径/cap/DEK/门禁在拿到头之后对**最终目标目录**二次解析。
4. **两个 override env 语义分离**（paths 两函数各管一边）：`CachePathsFor`（clientops.go:137-141）只认 `SSHMGR_CACHE_DIR`——命中即返回 override 目录、**忽略 instance 参数**（cache 目录路由）；`paths.CacheDekPathFor`（paths.go:83-86）只认 `SSHMGR_CACHE_DEK`——命中即返回该文件、**盖过一切 per-instance DEK 派生**。`SSHMGR_CACHE_DEK_DIR` 则只搬迁 DEK 根目录、per-instance 文件名派生保持（连贯 seam）。
5. **DEK 加载两形态**（dek.go）：`loadOrCreateDEK`（DoPull 路径，缺失则生成+落盘）；`loadDEK`（LoadCacheSnapshotFor 路径，clientops.go:406，**不自动创建**——缺失即 `cache DEK not found in keychain` 拒载）。
6. **CLI pull 的 auth 写序**：`DoPull` 成功后 `WriteCacheCredFor(instance="")`（cli/cache.go:119）+ `--max-offline` 时 config 写同槽（:122-128）。若 pull 归位到 `instances/<name>/` 而 auth 仍写默认槽 → lazy pull 读不到凭据（刷新链断）——归位必须连动 auth 落槽（§2、§5）。
7. **DoPull 调用面**：生产 5 处（cli/cache.go ×2[plain/pinned] + clientops.go:108 lazy + tui/clientpage.go:134 TUI 同步）+ 测试 ~40 处（`err := DoPull(...)` 机械改 `_, err :=`）。
8. **config 积木全在**（clientops/config.go）：`resolveMaxOffline(dir)`（env > file > off，file 非法 fail-closed）、`WriteCacheConfig(dir, v)`、`ValidateMaxOffline(v)`——`cache config` 子命令是纯接线。
9. **多实例积木全在**（批1）：`ListInstances()`/`InstancesRoot()`、`ReadCacheCredFor`/`WriteCacheCredFor`/`LoadCacheSnapshotFor`、`checkInstanceFlag` 互斥（cli/common.go:18）、`cacheStatusList` 列表视图（cli/cache.go:233）。
10. **Plan 30 消息门**（clientpage.go:145-168）：`clientModel` 的 owned allowlist——**新增 client-owned 消息类型必须登记**（checklist 项，漏登记 = 消息被 overlay 吞）。
11. **面板 `[s]` 归位不可达**（R2-Q1 推论）：面板同步前提 `m.cred != nil` = 默认槽 `cache.auth.json` 在 = 非真空；lazy pull 同理（读默认槽 auth）。归位触发面 = {CLI 裸 pull（真空机）、TUI wizard 首拉（R1-Q3 后移后拉取时默认槽真空）}。
12. **`[c]` 现守卫**：面板模式 `m.cred == nil` → 拒绝开表单（clientpage.go:248-254）——真空默认 + instances 有货的机器上该守卫堵死 TUI enroll 入口（R2-Q2 放宽）。
13. **`WriteCacheCredFor` 不建父目录**（实验实证 2026-08-27）：`atomicWriteUnique` 在目标目录不存在时直接报错（`open …\.cache.auth.json.tmp-…: cannot find the path specified`）。真机上默认槽配置目录由 wizard（role.json）预建；**命名实例目录只由 DoPull 的 `MkdirAll` 建立**——面板路径写新实例槽 auth 前必须自建目录（§5，rev2）；wizard/CLI 后移写天然满足（DoPull 已建目录）。
14. **实例名 `default` 合法**（instname.go 核实）：`dosReserved` 仅 CON/PRN/AUX/NUL/COM1-9/LPT1-9，`default` 过 pattern——服务端可合法发码 `default`，与任何"默认槽"显示语面潜在同名（§3.1 显示区分、§13 登记）。

## 1. 首次 enroll 自动归位（rev3 §2.4 二批列落地）

### 1.1 触发条件（真空定义，全部满足才归位）

| # | 条件 | 依据 |
|---|---|---|
| 1 | `o.Instance == ""`（无显式路由） | 显式路由已是命名实例 |
| 2 | `pin != ""` 且响应 200 且头 `X-Sshmgr-Device-Name` 非空 | plaintext 无头永不归位；老 serve 无头不归位（rev3 §2.4 二批列：不归位、落默认目录 + 升级提示——既有 WARNING 文案沿用） |
| 3 | 头 name 过 `instname.Valid` | 非法 → 拒写盘（owner 改名重发，既有文案） |
| 4 | **默认槽 `cache.bin` 与 `cache.auth.json` 均不存在**（真空） | R1-Q4；auth 在 bin 无（§9.10 边缘）→ 不归位、默认目录写入 + 门禁补记（现状行为，补记后窗口闭合） |
| 5 | **`SSHMGR_CACHE_DIR` 与 `SSHMGR_CACHE_DEK` 均未设**（rev1：单文件 DEK env 补入） | 任一在场 = 单槽完全覆盖语义（两 env 各覆盖一边，§0.4）——归位会把"无 flag pull"变成事实上的命名实例而材料却挂在单槽语义下，env 清除后按派生 DEK 拒载（§0.5，端到端实证）。**`SSHMGR_CACHE_DEK_DIR` 不在此列**——目录级 seam 把整棵 DEK 树（默认+per-instance 变体）连贯搬到新根，per-instance 语义保持，不拦归位 |

### 1.2 归位机制（DoPull 内部单一时序，rev1 重排 / rev2 补未归位分支）

1. **真空候选判定前置**：DoPull 顶部、默认槽 cap precheck **之前**，stat 默认槽 `cache.bin`/`cache.auth.json`（零 HTTP 依赖）+ 查条件 5 的 env。**真空候选 → 跳过默认槽 cap precheck**（cap 校验延至最终目标槽，见步 5——真空机默认槽残留非法 `cache.config.json` 不再拦截归位）；**非真空 → 时序与现状逐字节一致**（默认槽 precheck 照旧在 HTTP 前）。
2. HTTP 拿响应 + 头（条件 2/3 不满足 → 走 §1.3 不归位分支）。
3. 真空候选成立 → **retarget**：`dir/bin/metaPath` 重解析为 `instances/<头name>/`。
4. **目标槽门禁**（`gateNamedInstance` 形态：头==name 平凡成立；**物理碰撞 + 半写态检查对归位目标目录生效**——目录在而 meta 身份异 → 拒；bin 在而 meta 不可读 → 拒 + 清理路径文案。归位不是绕过物理检查的后门）。
5. **最终目标槽 cap 校验（rev2：无论归位与否）**：对**最终写盘目标**执行 `resolveMaxOffline`——归位 = 新实例目录；**未归位（头缺失/plaintext/...）= 默认槽**（真空候选跳过的 precheck 在此补做，含 plaintext+B-on 的明文拒分支）。非法/超限 → **拒绝本次 pull**（响应丢弃、零写盘；杜绝"拉得动载不动"半开状态在任何路径复活）。
6. **DEK 创建后置**：`loadOrCreateDEK(头name)`（归位路径；per-instance DEK，批1 §2.2 布局）。**位于全部门禁/cap 校验之后**——任何拒绝分支不创建任何 DEK 文件（拒绝分支真·零写盘，§9）。非真空候选的默认槽 DEK 预建（现状行为，HTTP 前创建）不在归位路径上、不随本条改动。
7. 写盘（bin→meta，既有提交序）+ meta 记 `device_name`（既有）。

### 1.3 不归位分支（网络与写入行为与批1一致；校验时序见注）

- 头缺失（老 serve）：默认目录 + 既有升级 WARNING。
- §9.10 auth-only（auth 在 bin 无）：默认目录 + 门禁补记。
- **任一单槽 override env 在场**（`SSHMGR_CACHE_DIR`/`SSHMGR_CACHE_DEK`）：写 override/默认目录（单槽语义一致；实验实证 DEK env 下归位产物 env 清除后拒载）。
- plaintext（`--allow-plaintext`）：无头 → 默认目录。
- **时序注（rev2）**：真空候选路径上，这些分支的 cap/明文拒校验从批1 的"HTTP 前"移至"HTTP 后、写盘前"（§1.2-5）——**语义等价**（同 fail-closed、同错误面、拒绝即零写盘），唯发起时机不同；非真空路径时序逐字节不变。

### 1.4 401/失败路径

401 发生在头到手前（鉴权失败无 `ct`，serve 不下发头）→ 归位无从谈起；`QuarantineCacheFor("")` 打默认槽（真空 = 无材料 = no-op）——既有行为不变。

### 1.5 CLI 提示（R2-Q1 补偿）

归位发生时 `StatusOut` 追加一行：`first enroll located to instance <name> — future pulls need --instance <name>`。

## 2. DoPull 签名变更（R1-Q5）

```go
type PullResult struct {
    Instance string // effective slot: "" = default, else instances/<name>/
}
func DoPull(url, token, pin string, o PullOpts) (PullResult, error)
```

- **消费方**：CLI（auth + config 按实际槽写，§5）、TUI wizard（接入卡 `--instance` 段 + auth 写槽，§5/§7）。
- **迁移**：编译器驱动——生产 5 处 + 测试 ~40 处机械 `err :=` → `_, err :=`；`MaybeLazyPullFor` 忽略 result（其 Instance 本就是显式传入）。
- 弃回调方案（`PullOpts.Report`）的登记：可选接入可被静默遗忘 = 本仓最恨的静默错位类；返回值让编译器强制完备。

## 3. TUI client 页多实例（picker）

### 3.1 `[i]` 实例 picker overlay

- 行 = 默认实例 + `ListInstances()` 每实例；**默认行显示「（默认实例）」、实例行显示实例名**（rev2——实例名 `default` 合法（§0.14），行文案区分避免同名歧义）；**行数据轻量**：name + bin mtime age + meta 的 profile（scoped 时）——**不解密**（解密需 per-instance DEK，列表行不值得；选中后 refresh 才解密；DEK 故障不阻断列表）。
- 选中 → `clientModel.instance` 置值 + `refreshDataCmdFor(instance)` 重读该实例 cred/snap/age（`ReadCacheCredFor`/`LoadCacheSnapshotFor`/`CachePathsFor`）。
- **会话内有效**：不落盘、不跨进程；下次启动回默认实例。
- footer 追加：`[s]同步 [i]实例 [c]编辑连接 [t]TTL  q 退出`；header 追加当前实例名（命名实例显示 `· 实例 <name>`，默认不显）。
- `busy` 中禁 `[i]`。

### 3.2 启动落点与空默认自动开 picker

启动 = 默认实例起步；若默认槽 **cred 缺且 bin 缺（真真空，rev1 收紧）** 而 `ListInstances()` 非空 → 首个 `dataReadyMsg`（errMsg 形态）后**自动打开 picker** + 提示"默认实例无材料，选择一个实例"。bin 在而 auth 缺的半残形态**不弹**（默认槽有材料，不把用户引开——`[c]` 可补 auth）。

### 3.3 per-instance 动作

- `[s]`：`ReadCacheCredFor(m.instance)` 的 cred → `PullOpts{Instance: m.instance}`（选中实例无 cred → 既有"连接配置未加载"错误行）。
- `[c]`：表单 prefill 选中实例的 cred + **实例字段预填选中实例名**（默认实例 → 空）；**保存后 `m.instance` = 表单路由结果（字段值或空=默认）并 refresh 该槽**（rev1：UI、auth 落槽、后续 `[s]` 三者一致——字段填 B 时 UI 切到 B、auth 写 B、`[s]` 同步 B）。
- `[t]`：不变。

### 3.4 消息门登记（Plan 30 checklist）

新增 client-owned 消息类型（picker 选择结果、per-instance 数据就绪等）**必须**登记 `clientModel.Update` 的 owned allowlist——配套路由测试钉住。

### 3.5 env-override 单槽模式（rev1 新增 / rev2 措辞修正）

**任一单槽 override env 在场 → TUI client 页进入单槽模式**，两个 env 的语义各不相同（§0.4）：

- `SSHMGR_CACHE_DIR`：**cache 目录路由**恒走 override 目录（`CachePathsFor` 忽略 instance）——多实例 UI 的显示与实际路由脱节。
- `SSHMGR_CACHE_DEK`：目录路由不变（各实例仍各目录），但**全部实例共享同一份 DEK**——多实例"隔离"语义失真（A/B 实例材料可互相解密）。
- 两态共同点 = 单槽/共享语义，故统一处置：`[i]` 键禁用（footer 移除 `[i]`；按下给状态行提示"override env 覆盖中——单槽模式"）；§3.2 auto-picker 不触发；连接表单实例字段禁用（§4）；页顶横幅 `⚠ 单槽模式（SSHMGR_CACHE_DIR/SSHMGR_CACHE_DEK 覆盖中）——多实例 UI 已禁用`。
- **禁用而非适配**（rev1 拍板）：env = 测试/迁移 escape hatch（rev3 §2.2 语义）。`SSHMGR_CACHE_DEK_DIR` 不触发单槽模式（目录级连贯 seam，路由与隔离语义均未变）。

## 4. 连接表单实例字段

- huh 增可选输入"实例名"：空 = 默认（wizard 首拉走自动归位）；非空 = 显式路由（`instname.Valid` 即时校验，错误文案沿用 `invalid device name` 前缀）。
- **env 互斥即时提示**：`SSHMGR_CACHE_DIR`/`SSHMGR_CACHE_DEK` env 在场 → 字段禁用 + 单槽横幅（§3.5）；env 在场且字段非空（不可能态，禁用兜底）→ `checkInstanceFlag` 同款文案。
- prefill：选中命名实例 → 预填其名；默认实例 → 空。
- **面板模式 × 默认槽真空 × 字段空 → 表单校验拒**（rev1，择 b）：提示"默认实例无材料——首次 enroll 请走向导流程（自动归位），或填实例名显式路由"。自动归位触发面维持 R2-Q1 收敛的 {CLI 裸 pull, wizard 首拉} 两处，不开第三入口（面板保存即写会先落 auth 破坏真空——实验实证）。
- wizard 与面板模式共用此表单（同一 `editConnForm` 改造）。

## 5. auth 写序（R1-Q3："目标槽未知后移、已知即时"；rev2 补新实例建目录）

| 路径 | 写序 | 落槽 |
|---|---|---|
| CLI `cache pull` | DoPull 成功后（现状） | **`WriteCacheCredFor(res.Instance)`**（归位连动）；config 同槽 |
| TUI wizard 首拉 | 表单保存**不写盘**；pull 成功后写 | `WriteCacheCredFor(res.Instance)`；目标实例目录已由 DoPull 建（§0.13）；**写失败 = WARNING 行，文案写明恢复路径（rev2）：`auth 未落盘——本 TUI 的 [s] 同步不可用；恢复 = CLI \`cache pull\` 或重跑向导表单（输入已保留）`** |
| TUI 面板 `[c]` | 保存即写（现状时序） | **新实例槽先 `MkdirAll(0700, instances/<name>/)` 再 `WriteCacheCredFor`**（rev2——§0.13 实证不建父目录必败；默认槽目录天然存在）；保存后 `m.instance` 切到该槽 + refresh（§3.3） |

- wizard 侧改善登记：现状"表单保存即写 auth、pull 失败 auth 已落盘"（§0.11）→ 后移后**失败 auth 零写入**（表单重开保留输入，既有机制）。
- **auth-only 新实例边缘（rev2 登记）**：面板 `[c]` 路由新实例并保存后、首拉前，该实例为 auth-only 形态（目录+auth、无 bin）——与 §9.10 同族：门禁以 bin 为生效条件（不生效）、`[s]` 可完成首拉（bin 落盘即闭合）、误删目录重走表单即恢复。接受为已登记边缘。
- §9.10 auth-only 边缘收缩登记：随 wizard 后移 + §4 面板真空校验，"先写 auth"路径收缩至"面板模式 `[c]` 改连接（非真空槽）"与新实例 enroll（上条，已登记）。

## 6. 换码预防性警告（R1-Q6；rev2 静态化）

- **构建时静态判定**（按表单打开时的选中槽，不随字段输入动态重排——huh 静态表单）：
  - **默认槽 + bin 在**：表单顶部警告行——`默认实例已绑定设备 <meta.device_name 或 "(旧 cache 未登记)">——更换设备码前须清四件套（cache.auth.json + cache.bin + cache.meta.json + quarantine/）重 enroll，否则下次同步将被门禁拒绝；若是本机第二个 agent，请在"实例名"字段填新实例名。`
  - **命名实例槽 + bin 在**：精简版——`实例 <name> 已绑定设备 <device_name>——换码须删除该实例目录重 enroll，否则同步将被拒。`
  - **选中槽无 bin（真空/新实例）**：不显。
- 字段路由到**已存在实例**且其实为换码的场景：无静态警告（构建时字段值未知）——由拉取时的实例门禁 fail-closed 兜底（§13 登记）。
- 纯 UX 层：不阻断提交；实际拦截仍由门禁 fail-closed。

## 7. 向导接入卡（R1-Q2 落点）

- `clientFinishScreen(serveURL)` → `clientFinishScreen(serveURL, instance string)`。
- `instance != ""`：offline 形态 args = `["mcp", "--cache", "--instance", "<name>"]` + 注释行"本机 cache 位于实例槽 `instances/<name>/`"；online http 形态**不变**（无实例概念）。
- instance 来源 = 首拉 `PullResult.Instance`（显式字段 enroll 与自动归位同源，卡片一致）。
- 首拉成功后 **TUI 自动选中生效实例**（R2-Q2a）：面板直接落新实例视图，不再退空默认页。

## 8. `cache config` 子命令（R1-Q7；rev2 钉不存在实例行为）

```
ssh-manager cache config [--instance <name>] [--max-offline <dur>]
```

- **仅对已存在实例目录可写/可显**（rev2）：`--instance <name>` 给出的目录不存在 → 报错 `instance <name> not found (directory <dir> does not exist) — enroll first (cache pull --instance <name>)`（不预配置、不 MkdirAll——预配置未 enroll 实例 YAGNI；`atomicWriteUnique` 不建父目录（§0.13），报错即正确行为，文案化误用）。默认槽目录不存在同理（真机上由 wizard 预建，理论态报错即可）。
- **无 `--max-offline`**：只读显示——该实例实际目录 + 生效 cap + 来源标注（`env`（含值）/ `file` / `off`）；显示与 `resolveMaxOffline` 同源（env 在场即 env 值，file 次之）。
- **`--max-offline <dur>`**：`ValidateMaxOffline` → `WriteCacheConfig(CachePathsFor(instance) 目录)`；`SSHMGR_CACHE_MAX_OFFLINE` env 在场 → 既有 WARNING（env 清除前 config 不生效）。
- **不做 off 开关**：删 cap = 手动删该实例 `cache.config.json`（文档写明；env 只能覆盖不能关闭——rev3 §3 语义）。
- `--instance` 走 `checkInstanceFlag` 互斥。
- 本命令不 pull——无 plaintext 语义、不触发归位。

## 9. 安全分析

- **归位不新增暴露**：头来自 pinned TLS（明文不可注入，rev3 §2.3）；非法名拒；物理碰撞/半写态检查在归位路径同样生效（§1.2-4）；**归位与未归位任何拒绝分支 = 零写盘（含 DEK——创建后置于全部门禁/cap 之后，§1.2-6）**。
- **半开状态不因归位引入、不因"跳默认 precheck"复活**（rev2）：cap/明文拒校验统一作用于**最终目标槽**（归位=实例目录、未归位=默认槽补检，§1.2-5），维持 Plan 37 "拉得动必载得动"于全部路径。
- **单槽 override env × 归位互斥**（rev1）：env 语义下归位材料与 env 清除后的加载路径不可达一致（DEK 覆盖实证）——不归位即不制造"env 依赖型坏实例"。
- **警告是体验层非安全层**（§6）：门禁仍是唯一拦截者；警告缺失不构成敞口。
- **auth 后移不降低凭据保护**：落盘时机后移，`atomicWriteUnique` + `HardenACL` 不变；wizard 失败路径 auth 零写入反而缩小凭据落盘面。
- **picker 不解密**：列表行不触 DEK（§3.1）——DEK 缺损实例仍可被列出与切换（载入错误由 refresh 路径如实报）。

## 10. 兼容性

- **v0.12.0 client × v0.11.0 serve**：全功能（头在，归位/字段路由/接入卡 `--instance` 全可用）。
- **v0.12.0 client × 老 serve（≤v0.10.0）**：无归位（头缺失 → 默认目录 + 既有 WARNING）；表单实例字段非空 → pull 拒（既有 `--instance requires a Plan-40 serve` 文案）；接入卡无 `--instance`（默认槽 enroll）。
- **存量零迁移**：无 flag 的一切现状行为不变；picker/字段/子命令全是新增面。
- **DoPull 签名变更**：internal 包，无外部 API 面。
- compat-matrix 登记 v0.12.0 行（含 ×老 serve 受限面）。

## 11. 测试计划

1. **归位触发**：真空机裸 `cache pull`（**两 override env 均未设**）→ 材料落 `instances/<name>/` + auth 同槽 + CLI 提示行断言 + meta `device_name` 记录。
2. **归位不触发五态**：auth 在 bin 无（→默认目录+补记）；头缺失（→默认目录+WARNING）；`SSHMGR_CACHE_DIR` 在场（→override 目录）；**`SSHMGR_CACHE_DEK` 在场（rev1：→默认目录、材料用 env DEK——断言不落 `instances/`）**；plaintext（→默认目录）。`SSHMGR_CACHE_DEK_DIR` 在场 → **归位照常**（DEK 树整体迁移，断言实例 DEK 落 env dir）。
3. **归位目标物理冲突**：目标实例目录异 identity / bin 在 meta 不可读 → 拒 + 清理文案（gateNamedInstance 形态复用断言）。
4. **归位拒绝零写盘**：目标实例目录预置非法 `cache.config.json` → 拒 + 默认槽与目标槽 bin/meta **及 DEK 文件**全零新增（sha256/目录清单前后比对，rev1 扩 DEK 断言）。
5. **cap 校验统一于最终目标槽**（rev2 扩）：① 真空机默认槽预置非法 `cache.config.json` + 归位候选 → 归位照常成功（cap 只看目标槽）；② **真空候选 + 头缺失（老 serve）+ 默认槽非法 config → 写盘前拒、零写盘**；③ **真空候选 + plaintext + B-on（env）→ 写盘前拒（明文拒分支补检）**；④ 非真空路径时序不变（默认槽非法 config 仍 HTTP 前拒——现状回归）。
6. **签名迁移零语义变化**：全部既有 DoPull 测试机械迁移后绿；`PullResult.Instance` 在无 flag 默认路径 == ""（既有路径不变断言）。
7. **TUI picker**：开/选/切换 per-instance 读写断言；真真空（cred+bin 均缺）+ instances → 自动开 picker；**bin 在 auth 缺 → 不弹**（rev1）；**默认行「（默认实例）」与实例行区分显示**（rev2）；`[s]` 带选中实例（`PullOpts.Instance` 断言）；`[c]` prefill/写槽/**保存后 m.instance 切换 + refresh**（rev1）；消息门登记路由测试（Plan 30 形态）。
8. **TUI 单槽模式**（rev1）：`SSHMGR_CACHE_DIR` 或 `SSHMGR_CACHE_DEK` 在场 → `[i]` 禁用/auto-picker 不触发/字段禁用/横幅在场；`SSHMGR_CACHE_DEK_DIR` 在场 → 多实例 UI 照常。
9. **表单字段**：非法名即校验拒；空 = 默认；**面板模式真空默认 × 空 field → 校验拒文案**（rev1）；wizard 模式空 field 真空 → 自动归位（不拒）。
10. **wizard 流**：真空首拉 → 归位 + 自动选中 + 接入卡含 `--instance <name>`；首拉失败 → 默认槽与实例槽 **auth 均零写入**（改善断言）；显式字段 enroll → 卡片 `--instance <字段值>`；**pull 成功但 auth 写失败（目录占位形态触发，实证无需代码 seam）→ WARNING 行含恢复路径文案（rev2 断言）+ 接入卡仍出 + 下次成功 pull 恢复**。
11. **换码警告形态**（rev2 静态化）：默认槽有材料 / 命名实例 / 无 bin 槽不显——构建时静态，不随字段输入变化。
12. **config 子命令**：显示三源（env/file/off）+ 目录；写入后读回；env 在场 WARNING；`--instance` × env 互斥；**不存在实例目录 → 报错文案含 enroll 指引（rev2）**。
13. **e2e 双实例 enroll 全程 TUI 化**：真空机 agent A 首拉归位（无字段）→ **默认槽占用机 agent B 表单字段 enroll：断言 MkdirAll 生效（instances/B/ 建立）+ auth 落 B + m.instance 切 B（rev2）** → `[s]` 首拉 → 两实例 `mcp --cache --instance` 各自起、各自裁剪视图正确（批1 e2e 的 enroll 路径升级版）。

## 12. 文档联动

- `docs/tui-multi-machine.md`：`[i]` 实例 picker、表单实例字段、换码警告形态、单槽模式横幅。
- `docs/multi-machine.md`：自动归位语义（真空定义 + 不归位分支含 env 两态）、双 agent enroll 全程 TUI 流程、`cache config` 子命令（仅已存在实例）、删 cap = 手动删文件。
- `docs/agent-access.md`：一句话指向 multi-machine（离线多 agent 的 enroll 现支持 TUI 全程）。
- `README.md`：`cache config` 子命令 + TUI `[i]`。
- `docs/compat-matrix.md`：v0.12.0 行（×v0.11.0 全功能 / ×老 serve 受限面）。
- `docs/backlog.md`：销项第二批五项；登记 doctor 多实例仍跟随 Plan 38。

## 13. 残余与登记

1. **doctor 多实例不在本批**（R1-Q8a）——仅命名实例机器 doctor 误报"cache 缺失"维持（Plan 38 体系内解决）。
2. **picker 行不解密**：服务器数等解密信息不进列表行（§3.1 取舍）。
3. **config 无 off 开关**：手动删文件（R1-Q7 拍板，文档写明）。
4. **面板 `[c]` 换码仍先写 auth**（表单路由槽已知，写序即时——R1-Q3 表格）：错位由换码警告 + 门禁兜底；§0.11 例外收缩至"面板改连接（非真空槽）+ 面板新实例 enroll"两条（后者见下条）。
5. **wizard 首拉成功后 auth 写失败** = WARNING 不阻断（含恢复路径文案，§5/§11.10）——同 CLI 既有语义。
6. **会话内 picker 不记忆**：下次 TUI 启动回默认实例（R1-Q1 取舍，零新增状态文件）。
7. **真空定义不看默认槽残留 `cache.meta.json`/`quarantine/`**（rev1 登记）：归位后旧身份 meta 孤留默认槽——无安全后果（门禁生效条件是 bin 在，rev3 §2.4；残留 meta 不参与任何判定），如需清理随换码 runbook 四件套手动处理。
8. **auth-only 新实例**（rev2 登记）：面板 `[c]` 路由新实例保存后、首拉前的形态（目录+auth 无 bin）——§9.10 同族，门禁不生效、`[s]` 首拉即闭合；与 wizard"真空候选跳过默认 precheck"的交互为零（auth 落的是**实例**槽非默认槽，不破坏默认槽真空判定）。
9. **实例名 `default` 的同名歧义**（rev2 登记）：服务端可合法发码 `default`（instname 白名单不保留，§0.14）——picker/CLI list 视图以「（默认实例）」/目录路径区分显示歧义已消，但**语义上**"实例 default"与默认槽仍是两个槽；命名纪律建议避开 `default`（文档写，不强制——不改服务端白名单规则）。
10. **字段路由到已存在实例的换码无静态警告**（rev2 §6 静态化的代价）：构建时字段值未知，该场景由拉取时实例门禁 fail-closed 兜底（拒绝文案含清理指引）。
