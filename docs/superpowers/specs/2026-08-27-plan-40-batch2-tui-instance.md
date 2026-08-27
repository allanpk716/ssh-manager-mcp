# Plan 40 第二批设计：TUI/向导多实例接入 + 首次 enroll 自动归位 + `cache config` 子命令

> 2026-08-27 grilling 两轮定案（owner 全部确认）。**基 spec = `2026-08-26-plan-40-multi-instance-cache-design.md.rev3.md`**（下称 rev3）——本文件只做第二批五项的实现级设计，rev3 已钉死的边界（批次划分 §8、门禁语义 §2.4、目录布局 §2.2、老 serve 行为 §5）不重议。范围 = backlog「第二批（已裁决待开工）」五项：① TUI client 页实例列表+切换；② 向导 `--instance` 接入卡；③ 首次 enroll 自动归位；④ 连接编辑表单换码预防性警告；⑤ 独立 `cache config [--instance] --max-offline` 子命令。
>
> 本轮（2026-08-27）拍板、不重议的决策：
> **R1-Q1** TUI 切换 = `[i]` picker overlay（默认实例 + `ListInstances()` 各一行），**会话内有效**（不落盘，下次启动回默认实例）；默认槽无 cred 而 instances 有货 → 启动自动开 picker + 提示；picker 只列**已存在**实例（新实例名入口 = 表单字段）。
> **R1-Q2** 连接表单增**可选"实例名"字段**：空 = 默认（首拉自动归位）；非空 = 显式路由该实例（`instname.Valid` 即时校验 + 拉取时响应头强一致）。
> **R1-Q3** auth 写序 = **"目标槽未知则后移、已知则即时"**：TUI wizard 模式表单保存不写盘，pull 成功后按实际槽 `WriteCacheCredFor`（顺带根治 rev3 §0.11"表单先写 auth"例外）；TUI 面板模式保存即写**选中实例**槽；CLI 按 `DoPull` 返回的实际槽写。
> **R1-Q4** 自动归位触发条件 = **真空定义**：默认槽 `cache.bin` 与 `cache.auth.json` **均**不存在。auth 在而 bin 不在（§9.10 边缘）→ 不归位、默认目录写入 + 门禁补记（现状行为）。`SSHMGR_CACHE_DIR` env 在场 → 不归位（单槽完全覆盖语义优先）；响应头 name 过不了 `instname.Valid` → 拒写盘。
> **R1-Q5** `DoPull` 签名改为返回 `(PullResult, error)`（`PullResult{Instance string}`，""=默认槽）——编译器驱动迁移全部调用点（弃回调方案：可选接入可被静默遗忘）。
> **R1-Q6** 换码警告 = 纯 UX 层（动态读 meta，表单顶部插行，不阻断；实际拦截仍由门禁 fail-closed 兜底）。
> **R1-Q7** `cache config` 子命令：无 flag 只读显示生效 cap + 来源（env/file/off）；`--max-offline` 写入；**不做 off 开关**（删 cap = 手动删该实例 config 文件，文档写明）。
> **R1-Q8** doctor 多实例**不进本批**（跟随 Plan 38-doctor 体系独立立项）；版本 **v0.12.0**；流程 = 独立批2 spec（本文件）→ xcheck 收敛 → plan → SDD。
> **R2-Q1** 撤回"面板 `[s]` 归位提示"（R1 草案附加点）——真空定义下面板 `[s]` 前提是 cred 已加载 = 默认槽 auth 在 = **非真空**，归位逻辑上不可达。归位触发面收敛为：**CLI 裸 `cache pull`（真空机器）+ TUI wizard 首拉** 两处；CLI 补一行显式提示（§1.5）。
> **R2-Q2** 首拉成功后 TUI **自动选中实际生效实例**（面板直接落在新实例视图）；`[c]` 守卫放宽（选中槽无 cred 但机器存在实例 → 放行，空 code 提交校验兜底）。

## 0. 现状事实（2026-08-27 于批1 合并后代码核实；函数名锚定）

1. **TUI client 页是纯默认实例视图**：`clientModel`（tui/clientpage.go:32）无实例概念；`refreshDataCmd` 读默认槽 cred + `LoadCacheSnapshot()`（:94-118）；`[s]` → `syncCmdMode` → `DoPull(..., PullOpts{Timeout})` **不带 Instance**（:126-143）；`[c]` → `editConnForm` → `WriteCacheCred`（默认槽，:381）——rev3 §0.11 钉死的唯一"auth 先于 pull"路径。
2. **wizard client 流**：`enterClient`（tui/wizard.go:184）→ 连接表单 → `connSavedMsg` → `syncCmdMode(cred, wizard=true)` → `pullSucceededMsg` → `clientFinishScreen(serveURL)` 生成 `.mcp.json` 接入卡，offline 形态 `["mcp", "--cache"]`（clientpage.go:439-465）；online http 形态无实例概念。
3. **DoPull 结构**（clientops.go:442-637）：顶部按 `o.Instance` 解析 `CachePathsFor` → cap 预检 `resolveMaxOffline(dir)` → HTTP → 身份门禁（`gateNamedInstance`/`gateDefaultInstance`）→ 写盘。**归位的目标名（响应头 `X-Sshmgr-Device-Name`）在 HTTP 响应到手后才可知**——归位意味着路径/cap/DEK/门禁在拿到头之后对**最终目标目录**二次解析。
4. **CLI pull 的 auth 写序**：`DoPull` 成功后 `WriteCacheCredFor(instance="")`（cli/cache.go:119）+ `--max-offline` 时 config 写同槽（:122-128）。若 pull 归位到 `instances/<name>/` 而 auth 仍写默认槽 → lazy pull 读不到凭据（刷新链断）——归位必须连动 auth 落槽（§2、§5）。
5. **DoPull 调用面**：生产 5 处（cli/cache.go ×2[plain/pinned] + clientops.go:108 lazy + tui/clientpage.go:134 TUI 同步 + cli 内含一处 plaintext 分支）+ 测试 ~40 处（`err := DoPull(...)` 机械改 `_, err :=`）。
6. **config 积木全在**（clientops/config.go）：`resolveMaxOffline(dir)`（env > file > off，file 非法 fail-closed）、`WriteCacheConfig(dir, v)`、`ValidateMaxOffline(v)`——`cache config` 子命令是纯接线。
7. **多实例积木全在**（批1）：`ListInstances()`/`InstancesRoot()`、`ReadCacheCredFor`/`WriteCacheCredFor`/`LoadCacheSnapshotFor`、`checkInstanceFlag` 互斥（cli/common.go:18）、`cacheStatusList` 列表视图（cli/cache.go:233）。
8. **Plan 30 消息门**（clientpage.go:145-168）：`clientModel` 的 owned allowlist——**新增 client-owned 消息类型必须登记**（checklist 项，漏登记 = 消息被 overlay 吞）。
9. **面板 `[s]` 归位不可达**（R2-Q1 推论）：面板同步前提 `m.cred != nil` = 默认槽 `cache.auth.json` 在 = 非真空；lazy pull 同理（读默认槽 auth）。归位触发面 = {CLI 裸 pull（真空机）、TUI wizard 首拉（R1-Q3 后移后拉取时默认槽真空）}。
10. **`[c]` 现守卫**：面板模式 `m.cred == nil` → 拒绝开表单（clientpage.go:248-254）——真空默认 + instances 有货的机器上该守卫堵死 TUI enroll 入口（R2-Q2 放宽）。

## 1. 首次 enroll 自动归位（rev3 §2.4 二批列落地）

### 1.1 触发条件（真空定义，全部满足才归位）

| # | 条件 | 依据 |
|---|---|---|
| 1 | `o.Instance == ""`（无显式路由） | 显式路由已是命名实例 |
| 2 | `pin != ""` 且响应 200 且头 `X-Sshmgr-Device-Name` 非空 | plaintext 无头永不归位；老 serve 无头不归位（rev3 §2.4 二批列：不归位、落默认目录 + 升级提示——既有 WARNING 文案沿用） |
| 3 | 头 name 过 `instname.Valid` | 非法 → 拒写盘（owner 改名重发，既有文案） |
| 4 | **默认槽 `cache.bin` 与 `cache.auth.json` 均不存在**（真空） | R1-Q4；auth 在 bin 无（§9.10 边缘）→ 不归位、默认目录写入 + 门禁补记（现状行为，补记后窗口闭合） |
| 5 | `SSHMGR_CACHE_DIR` env 未设 | env = 单槽完全覆盖语义优先（rev3 §2.2 escape hatch 不掺和实例路由） |

### 1.2 归位机制（DoPull 内部，响应 200 之后、任何写盘之前）

1. 真空判定成立 → **retarget**：`dir/bin/metaPath` 重解析为 `instances/<头name>/`。
2. **cap 重解析 + fail-closed**：`resolveMaxOffline(新dir)`——目标实例目录已存 `cache.config.json` 且非法 → **拒绝本次 pull**（响应丢弃、零写盘；杜绝"拉得动载不动"半开状态随归位引入）。真空机首 enroll 目标目录通常不存在（cap=off），该分支主要拦"归位进已存在实例目录"的 re-enroll 形态。
3. **DEK 重取**：`loadOrCreateDEK(头name)`（per-instance DEK，批1 §2.2 布局）。
4. **门禁复用 `gateNamedInstance` 形态**（头==name 平凡成立；**物理碰撞 + 半写态检查对归位目标目录生效**：目录在而 meta 身份异 → 拒；bin 在而 meta 不可读 → 拒 + 清理路径文案——归位不是绕过物理检查的后门）。
5. 写盘（bin→meta，既有提交序）+ meta 记 `device_name`（既有）。

### 1.3 不归位分支（行为与批1完全一致，零变化）

- 头缺失（老 serve）：默认目录 + 既有升级 WARNING。
- §9.10 auth-only（auth 在 bin 无）：默认目录 + 门禁补记。
- env 在场：写 override 目录（单槽语义）。
- plaintext（`--allow-plaintext`）：无头 → 默认目录。

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

- 行 = 默认实例（`default`）+ `ListInstances()` 每实例；**行数据轻量**：name + bin mtime age + meta 的 profile（scoped 时）——**不解密**（解密需 per-instance DEK，列表行不值得；选中后 refresh 才解密；DEK 故障不阻断列表）。
- 选中 → `clientModel.instance` 置值 + `refreshDataCmdFor(instance)` 重读该实例 cred/snap/age（`ReadCacheCredFor`/`LoadCacheSnapshotFor`/`CachePathsFor`）。
- **会话内有效**：不落盘、不跨进程；下次启动回默认实例。
- footer 追加：`[s]同步 [i]实例 [c]编辑连接 [t]TTL  q 退出`；header 追加当前实例名（命名实例显示 `· 实例 <name>`，默认不显）。
- `busy` 中禁 `[i]`。

### 3.2 启动落点与空默认自动开 picker

启动 = 默认实例起步；若默认槽无 cred 而 `ListInstances()` 非空 → 首个 `dataReadyMsg`（errMsg 形态）后**自动打开 picker** + 提示"默认实例无凭据，选择一个实例"（否则面板开屏只有一条 errMsg）。

### 3.3 per-instance 动作

- `[s]`：`ReadCacheCredFor(m.instance)` 的 cred → `PullOpts{Instance: m.instance}`（选中实例无 cred → 既有"连接配置未加载"错误行）。
- `[c]`：表单 prefill 选中实例的 cred + **实例字段预填选中实例名**（默认实例 → 空）；提交写 `WriteCacheCredFor(m.instance 或表单路由结果, ...)`（§4/§5）。
- `[t]`：不变。

### 3.4 消息门登记（Plan 30 checklist）

新增 client-owned 消息类型（picker 选择结果、per-instance 数据就绪等）**必须**登记 `clientModel.Update` 的 owned allowlist——配套路由测试钉住。

## 4. 连接表单实例字段

- huh 增可选输入"实例名"：空 = 默认（首拉走自动归位）；非空 = 显式路由（`instname.Valid` 即时校验，错误文案沿用 `invalid device name` 前缀）。
- **env 互斥即时提示**：`SSHMGR_CACHE_DIR`/`SSHMGR_CACHE_DEK` env 在场且字段非空 → 表单 validate 即报（`checkInstanceFlag` 同款文案），不等 pull 时才炸。
- prefill：选中命名实例 → 预填其名；默认实例 → 空。
- wizard 与面板模式共用此表单（同一 `editConnForm` 改造）。

## 5. auth 写序（R1-Q3："目标槽未知后移、已知即时"）

| 路径 | 写序 | 落槽 |
|---|---|---|
| CLI `cache pull` | DoPull 成功后（现状） | **`WriteCacheCredFor(res.Instance)`**（归位连动）；config 同槽 |
| TUI wizard 首拉 | 表单保存**不写盘**；pull 成功后写 | `WriteCacheCredFor(res.Instance)`；写失败 = WARNING 行（拉取成功、刷新链暂断，下次成功 pull 恢复——同 CLI 语义） |
| TUI 面板 `[c]` | 保存即写（现状时序） | `WriteCacheCredFor(表单路由的目标槽)`（选中槽已知；实例字段改路由则按字段） |

- wizard 侧改善登记：现状"表单保存即写 auth、pull 失败 auth 已落盘"（§0.11）→ 后移后**失败 auth 零写入**（表单重开保留输入，既有机制）。
- §9.10 auth-only 边缘收缩登记：随 wizard 后移，"先写 auth"路径收缩至"面板模式 `[c]`"一条（换码警告 §6 缓解、门禁兜底）。

## 6. 换码预防性警告（R1-Q6）

表单构建时判定（动态读该槽 meta）：

- **默认槽 + bin 在**：表单顶部警告行——`默认实例已绑定设备 <meta.device_name 或 "(旧 cache 未登记)">——更换设备码前须清四件套（cache.auth.json + cache.bin + cache.meta.json + quarantine/）重 enroll，否则下次同步将被门禁拒绝；若是本机第二个 agent，请在"实例名"字段填新实例名。`
- **命名实例槽 + bin 在**：精简版——`实例 <name> 已绑定设备 <device_name>——换码须删除该实例目录重 enroll，否则同步将被拒。`
- **实例字段非空且 ≠ 已存在实例**：不显（显式路由新实例，无门禁冲突）；字段 == 已存在实例 → 显示该实例版。
- 纯 UX 层：不阻断提交；实际拦截仍由门禁 fail-closed。

## 7. 向导接入卡（R1-Q2 落点）

- `clientFinishScreen(serveURL)` → `clientFinishScreen(serveURL, instance string)`。
- `instance != ""`：offline 形态 args = `["mcp", "--cache", "--instance", "<name>"]` + 注释行"本机 cache 位于实例槽 `instances/<name>/`"；online http 形态**不变**（无实例概念）。
- instance 来源 = 首拉 `PullResult.Instance`（显式字段 enroll 与自动归位同源，卡片一致）。
- 首拉成功后 **TUI 自动选中生效实例**（R2-Q2a）：面板直接落新实例视图，不再退空默认页。

## 8. `cache config` 子命令（R1-Q7）

```
ssh-manager cache config [--instance <name>] [--max-offline <dur>]
```

- **无 `--max-offline`**：只读显示——该实例实际目录 + 生效 cap + 来源标注（`env`（含值）/ `file` / `off`）；显示与 `resolveMaxOffline` 同源（env 在场即 env 值，file 次之）。
- **`--max-offline <dur>`**：`ValidateMaxOffline` → `WriteCacheConfig(CachePathsFor(instance) 目录)`；`SSHMGR_CACHE_MAX_OFFLINE` env 在场 → 既有 WARNING（env 清除前 config 不生效）。
- **不做 off 开关**：删 cap = 手动删该实例 `cache.config.json`（文档写明；env 只能覆盖不能关闭——rev3 §3 语义）。
- `--instance` 走 `checkInstanceFlag` 互斥。
- 本命令不 pull——无 plaintext 语义、不触发归位。

## 9. 安全分析

- **归位不新增暴露**：头来自 pinned TLS（明文不可注入，rev3 §2.3）；非法名拒；物理碰撞/半写态检查在归位路径同样生效（§1.2-4）；归位任何拒绝分支 = 零写盘。
- **半开状态不因归位引入**：目标目录 cap 非法 → 归位即拒（§1.2-2），维持 Plan 37 "拉得动必载得动"。
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

1. **归位触发**：真空机裸 `cache pull` → 材料落 `instances/<name>/` + auth 同槽 + CLI 提示行断言 + meta `device_name` 记录。
2. **归位不触发四态**：auth 在 bin 无（→默认目录+补记）；头缺失（→默认目录+WARNING）；env 在场（→override 目录）；plaintext（→默认目录）。
3. **归位目标物理冲突**：目标实例目录异 identity / bin 在 meta 不可读 → 拒 + 清理文案（gateNamedInstance 形态复用断言）。
4. **归位目标 cap 非法**：预置非法 `cache.config.json` 于目标实例目录 → 拒 + 默认槽与实例槽全零写盘（sha256 前后比对）。
5. **签名迁移零语义变化**：全部既有 DoPull 测试机械迁移后绿；`PullResult.Instance` 在无 flag 默认路径 == ""（既有路径不变断言）。
6. **TUI picker**：开/选/切换 per-instance 读写断言；空默认 + instances → 自动开 picker；`[s]` 带选中实例（`PullOpts.Instance` 断言）；`[c]` prefill 与写槽；消息门登记路由测试（Plan 30 形态）。
7. **表单字段**：非法名即校验拒；空 = 默认；env 在场 + 字段非空 → 即时互斥报错。
8. **wizard 流**：真空首拉 → 归位 + 自动选中 + 接入卡含 `--instance <name>`；首拉失败 → 默认槽与实例槽 **auth 均零写入**（改善断言）；显式字段 enroll → 卡片 `--instance <字段值>`。
9. **换码警告三形态**：默认槽有材料 / 命名实例 / 字段非空新实例不显。
10. **config 子命令**：显示三源（env/file/off）+ 目录；写入后读回；env 在场 WARNING；`--instance` × env 互斥。
11. **e2e 双实例 enroll 全程 TUI 化**：真空机 agent A 首拉归位（无字段）→ 默认槽占用机 agent B 表单字段 enroll → 两实例 `mcp --cache --instance` 各自起、各自裁剪视图正确（批1 e2e 的 enroll 路径升级版）。

## 12. 文档联动

- `docs/tui-multi-machine.md`：`[i]` 实例 picker、表单实例字段、换码警告形态。
- `docs/multi-machine.md`：自动归位语义（真空定义 + 三不归位分支）、双 agent enroll 全程 TUI 流程、`cache config` 子命令、删 cap = 手动删文件。
- `docs/agent-access.md`：一句话指向 multi-machine（离线多 agent 的 enroll 现支持 TUI 全程）。
- `README.md`：`cache config` 子命令 + TUI `[i]`。
- `docs/compat-matrix.md`：v0.12.0 行（×v0.11.0 全功能 / ×老 serve 受限面）。
- `docs/backlog.md`：销项第二批五项；登记 doctor 多实例仍跟随 Plan 38。

## 13. 残余与登记

1. **doctor 多实例不在本批**（R1-Q8a）——仅命名实例机器 doctor 误报"cache 缺失"维持（Plan 38 体系内解决）。
2. **picker 行不解密**：服务器数等解密信息不进列表行（§3.1 取舍）。
3. **config 无 off 开关**：手动删文件（R1-Q7 拍板，文档写明）。
4. **面板 `[c]` 换码仍先写 auth**（选中槽已知，写序即时——R1-Q3 表格）：错位由换码警告 + 门禁兜底；§0.11 例外收缩至此一条。
5. **wizard 首拉成功后 auth 写失败** = WARNING 不阻断（刷新链暂断，下次成功 pull 恢复）——同 CLI 既有语义。
6. **会话内 picker 不记忆**：下次 TUI 启动回默认实例（R1-Q1 取舍，零新增状态文件）。
