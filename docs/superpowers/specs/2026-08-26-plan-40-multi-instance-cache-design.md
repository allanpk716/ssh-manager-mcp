# Plan 40 设计：多实例离线缓存（同机多 agent 各授权各 profile）+ 锚事实/策略解离 + MAX_OFFLINE 持久化

> 2026-08-26 grilling 定案（两轮 11 问 + bug 插入，owner 全部确认），本文不重议的决策：**client 标准姿态 = cache-first 单入口**（stdio `mcp --cache` 为准；http 直连降为辅助形态——Q1a）、**离线授权时效 = `SSHMGR_CACHE_MAX_OFFLINE` 24h 维持 + 吊销纪律（先 device code 后 project token）文档化**（Q2）、**多 cache 实例做成一等公民**（owner 明确否掉"仅文档化 workaround"——Q3）、**接受每实例凭据落盘（ACL + 时效双纪律）**（Q4）、**实例身份键 = 设备码 name**（Q5A）、**`--instance` flag 为一等形态 + `SSHMGR_CACHE_DIR` env 保留为完全覆盖 escape hatch**（Q6A）、**无 `--instance` = 默认实例，存量零迁移**（Q7A）、**每实例一份 DEK**（Q8A）、**批次：CLI 闭环先行 / TUI·向导二批 / doctor 跟随**（Q9）、**P0 bug 修法 = A（pinned pull 永远记锚）+ C（MAX_OFFLINE 持久化 per-instance），B（meta merge）与 D（load 缺锚降级）否决**（Q10）、**持久化作用域 = per-instance、优先级 env > 文件、内容只搬 MAX_OFFLINE**（Q11）。
>
> 与 Plan 37（B 时限快照）的关系：本 plan 不改 Plan 37 的闸门机器（meta 三闸、销毁前复查、K=1h 双职），只改**锚的记录条件**与**策略的读取来源**；与 Plan 39（设备码绑 profile）的关系：实例模型直接建立在 Plan 39 的裁剪之上（实例 = 设备码 = profile 三位一体）。

## 0. 现状事实（2026-08-26 于 master 核实；函数名锚定，行号以当时为准）

1. **cache 机器级单份**：`CachePaths()`（clientops）返回 `SSHMGR_CACHE_DIR` env 覆盖或 `UserConfigDir()/ssh-manager/` 下的 `cache.bin` / `cache.meta.json` / `cache-audit.log`；`CacheCredPath()` 同目录下 `cache.auth.json`。一机一份——同机第二个 agent（不同 profile 的 project token）在 `mcp --cache` 的 token 验证处 spawn 即失败：裁剪 snapshot 的 `Projects` 只含设备码所绑 profile 的 projects（`ExportSnapshotForProfile`，store/export.go）。fail-closed 不越权，但第二个 agent 无离线能力。
2. **锚被 pull 进程的策略视图决定存亡（P0 bug 根因）**：`DoPull` 仅当 **pull 进程自己的** env `SSHMGR_CACHE_MAX_OFFLINE > 0` 时解析 pinned 200 的 `Date` 头记锚（`ServerAnchored=true`）；否则 `anchored=false` 且**无条件覆盖写** meta。client TUI `[s]同步` 走同一 `DoPull`，TUI 进程无该 env → 抹锚 → `LoadCacheSnapshot`（mcp 进程有 env）provenance 闸拒载 → ssh MCP 起不来 → agent 会话工具全消失。**循环陷阱**：错误文案让用户 "run cache pull"，用户再从 TUI/裸 CLI pull 又抹一次。这是 v0.10.0 部署记录"env 必须同时在拉取路径与加载路径"坑的第三个实例（前两个：计划任务 wrapper、`.claude.json` env 块）——每堵一个进程上下文就冒出下一个，证明该类问题不能靠"把 env 铺满所有进程"修。
3. **DEK 现状**：`paths.CacheDekPath()` = `SSHMGR_CACHE_DEK` env 覆盖或 `VaultDir()/cache-dek.key`（Windows `C:\ProgramData\ssh-manager\`）；`DekProvider()`（dek_unix/dek_windows 平台分装）包装之；`loadOrCreateDEK`/`loadDEK` 唯一消费。DEK 与 cache 目录**分离**（ProgramData vs UserConfigDir）——有意布局：cache 目录可能被同步/备份工具带走，DEK 不跟。
4. **设备码 name 即天然主键**：`cache_tokens.name` 有 UNIQUE 约束（`AddCacheToken` 事务先回收同名 revoked 行）；`handleSnapshot` 鉴权后 `GetCacheToken` 在手 `ct.Name` 与 `ct.ProfileID`。name 现为自由文本。
5. **响应头先例**：Plan 39 已加 `X-Sshmgr-Snapshot-Scope: profile`（DoPull 读入 `meta.Scoped`）——实例 name 走同款通道。
6. **`CachePaths` 消费面**：15 处调用（cli/mcp、cli/cache、cli/clear、cli/doctor、clientops 内部、tui/clientpage 等）——实例化改造必须全量过一遍，漏一处即错位。
7. **角色判定读默认目录**：`roles.resolveMode` 的 `cachePresent()` 只查默认 cache 目录——只有命名实例的机器会被误判为 wizard 态。
8. **Plan 37 闸门机器（本 plan 不动）**：load 侧三闸（meta 读、provenance、超龄销毁前复查 + 回拨闸）；K=1h（skew 闸 + 回拨容差同常数）；提交顺序 bin→meta；plaintext pull 在 B-on 时被拒（锚需要 pinned TLS）。

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
- meta 结构、提交顺序（bin→meta）、`ServerAnchored` 无 omitempty 恒序列化——全部不变。

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

## 2. 多实例设计

### 2.1 实例模型

**实例 = 设备码 = profile 授权单元，三位一体。** 一台 client 机上 N 个 agent（各持不同 project token）→ N 个实例 → N 份独立 cache/DEK/设备码/审计/时效策略。

- **实例名 = 设备码 name**（owner 在 `cache-tokens add --name` 时定，`UNIQUE(name)` 即主键；运维台账与磁盘目录一一对应）。命名纪律建议 `机器-实例`（如 `laptop-agentA`），写文档不强制。
- **name 白名单**：`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`（首字符字母数字，长度 1–64）。**双端校验，fail-closed**：
  - server 端：`cache-tokens add`（CLI + TUI 签发表单）源头拒绝非法 name（存量非法 name 设备码不追溯，pull 时由客户端兜底拦）；
  - client 端：`DoPull` 消费 name 前防御性校验（name 进路径拼接的每个点），非法 → 拒写盘，文案引导 owner 改名重发。
  - 动机：name 来自 server 响应并用作目录/文件名——白名单封死 `../` 类路径穿越与非法文件名字符。

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

- **路径解析优先级**：`SSHMGR_CACHE_DIR`（显式路径，**完全覆盖、忽略 instance**——既有 escape hatch 语义不变）> `--instance <name>`（实例目录）> 默认目录。
- 实现形态：`CachePaths()` → `CachePathsFor(instance string)`（`CachePaths()` ≡ `CachePathsFor("")`，15 个存量 caller 零改动兼容）；`paths.CacheDekPath()` → `CacheDekPathFor(instance)`（`SSHMGR_CACHE_DEK` 完全覆盖、**不按实例派生**——显式给了就是给了）；`DekProvider()` 同步实例参数化。**§0.6 的消费面清单全量接线**（`CacheCredPath`、`QuarantineCache`、`CacheReloader`、audit sidecar、`cli/doctor` 等——实现时以 grep `CachePaths(` 逐点核对，漏一处即错位）。
- 实例目录创建：沿用 `DoPull` 的 `MkdirAll(0700)`；`cache.auth.json` 沿用 `HardenACL`；实例内 quarantine/ 子结构不变。

### 2.3 server 侧：实例名下发

`handleSnapshot` 在现有 `X-Sshmgr-Snapshot-Scope` 旁新增响应头：

```
X-Sshmgr-Device-Name: <ct.Name>
```

`ct` 鉴权后已在手，零查询成本。**非安全边界**（name 非秘密；pinned TLS 已防篡改）——仅作路由与一致性校验数据。老 client 忽略之（向前兼容）。

### 2.4 pull 写入规则（`cache pull [--instance <name>]`）

| 场景 | 目标目录 | 一致性校验 |
|---|---|---|
| `--instance <name>` 显式 | `instances/<name>/` | name 过白名单；**响应头 name ≠ `<name>` → fail-closed 拒写盘**（防 owner 发码张冠李戴——实例目录与授权错位）；**响应头缺失（旧 serve）→ 拒 + 提示升级 serve**（校验无法执行时不放行；升级顺序铁律本就 client 先 serve 后） |
| 无 `--instance`，默认目录已有 `cache.auth.json` | 默认目录（**存量机器行为完全不变**） | 无 |
| 无 `--instance`，默认目录无 `cache.auth.json`（首次 enroll） | **自动归位** `instances/<响应头 name>/`，输出行告知实例名 + 后续 `--instance` 用法 | name 过白名单（非法 → 拒写盘） |
| 无 `--instance`，首次 enroll，响应头缺失（旧 serve） | 默认目录（现状行为 + 提示） | 无 |

自动归位**只在首次 enroll**发生：存量机器（默认目录已有凭据）永不自动迁移——显式 `--instance` 才进实例目录（Q7 零迁移承诺的精确化；登记 §9）。

### 2.5 读取规则（`mcp --cache [--instance <name>]` / `cache status [--instance <name>]`）

- 无 `--instance` = 默认目录（现状）。默认目录无 cache 且 `instances/` 下存在实例 → 错误文案**列出实例清单**并指引 `--instance <name>`（**不自动猜**——读到哪个实例必须显式，杜绝静默错位）。
- `--instance <name>`：白名单校验后读 `instances/<name>/`。
- **lazy pull 天然 per-instance**：`MaybeLazyPull` 读实例目录内的 `cache.auth.json`（`CacheCredPath` 实例化后自动成立），拉取写回同实例目录。进程级 quarantine 哨兵语义不变（一个 `mcp --cache` 进程服务一个实例）。
- **quarantine / 到龄销毁 / 审计 sidecar 天然 per-instance**：全部落实例目录内，`QuarantineCache` 走实例化路径即成。

### 2.6 `cache status` 多实例视图

- 无 `--instance`：列**全部**实例——默认实例一行 + `instances/*/` 每行。字段：实例名 / profile（仅 `scoped=true` 时显示，Plan 39 溯源纪律）/ servers / creds / age / 锚状态 / quarantine 概要。
- `--instance <name>`：单实例详情（现状格式复用）。

### 2.7 `clear` 与角色判定

- `ssh-manager clear`：在现有清理清单上**追加**——`instances/` 整树 + `VaultDir()/cache-dek-*.key` 全部变体（残留实例目录 = 残留凭据，违反 clear 的存在目的）。默认实例清理清单不变。
- `roles.resolveMode` 的 `cachePresent()`：改为"默认 cache 目录存在 **或** `instances/` 下任一实例存在"——否则只有命名实例的机器误判 wizard 态。

### 2.8 TUI / 向导 / doctor（第二批，边界先行钉死）

- **第一批 TUI client 页保持默认实例视图不动**（读默认目录、`[s]` 同步默认实例）——命名实例用户第一批用 CLI/计划任务同步。第二批：实例列表 + 切换 + 向导 client 接入卡生成带 `--instance` 的 `.mcp.json`。
- doctor：第一批不动（client-cache 检查继续查默认实例），第二批枚举全部实例诊断（并入 Plan 38-doctor 体系）。

## 3. MAX_OFFLINE 持久化（per-instance 配置）

**P0 治的是"事实被策略抹除"；本节治的是"策略本身跨进程不一致"——env 是进程属性，配置文件是机器（实例）属性。**

- **文件**：实例目录内 `cache.config.json`，v1 内容单一字段：

  ```json
  {"max_offline": "24h"}
  ```

  值为 Go duration 文法字符串（与 env 值同构）。明文——是策略不是凭据。
- **解析优先级**：`SSHMGR_CACHE_MAX_OFFLINE` env > `cache.config.json` > `(0, nil)` 关。env 保留为应急/测试 override（seam 纪律：env 显式给了就是给了）。
- **校验复用**：config 文件值走与 env 完全相同的规则（≥1h、不可解析/为负/<1h → fail-closed 拒绝，冻结文案沿用 Plan 37 §1 并注明来源 file/env）。现行 `cacheMaxOffline()` 的全部读取点（`DoPull`、`LoadCacheSnapshot` 等）切换到实例感知的 `resolveMaxOffline(instanceDir)`。
- **写入口**：`cache pull --max-offline <dur>`——pull 成功后顺带写**该实例**的 config（不传 flag 则不动现有 config）。第二批追加独立 `cache config [--instance] --max-offline` 子命令。
- **只搬 MAX_OFFLINE**：`--cache-max-age` 等其余 env/flag 不进 config（YAGNI，Q11 拍板）。

## 4. 安全分析

- **攻击面变化（接受，Q4 拍板）**：N 实例 = N 份（各自 profile 的）凭据集落盘。缓解双纪律：cache 目录 ACL 硬化（0700/HardenACL 现状规则）+ MAX_OFFLINE 时效（凭据落盘暴露 = ≤时效窗口内的旧快照）。
- **实例间隔离**：① DEK per-instance——A 实例目录连同其 DEK 单独泄露，解不开 B 实例 cache.bin（分离布局下"目录被同步工具带走"不含 DEK）；② token×cache 交叉 fail-closed——B 的 project token 在 A 实例 spawn 即验证失败（既有机制：裁剪 snapshot 只含 A profile 的 projects 行，MCP 层授权过滤本来就对）。
- **路径穿越**：name 白名单双端校验；`instances/<name>` 的 name 恒过白名单才进 `filepath.Join`。
- **错配防护**：`--instance` 与响应头 name 强一致校验（owner 发码张冠李戴在 pull 时即断）。
- **失窃响应粒度**：吊销单个设备码 → 该实例下次 pull 401 → `QuarantineCache` 销毁该实例材料——**只断该实例，不连坐他实例**。文档化（§7）。
- **锚完整性**：P0 后所有 pinned pull 恒产出过闸锚；plaintext 永不锚（现状）。
- **残余（非新增）**：同机恶意进程可读所有实例材料（文件 ACL/DPAPI 不挡同机）——与单实例现状同级。

## 5. 兼容性

- **存量单实例机器**：零迁移零行为变化（默认实例 = 现路径；无 flag 的 pull/mcp/status 全走默认目录）。
- **新 serve + 老 client**：多一个响应头，老 client 忽略——零影响。
- **老 serve + 新 client**：`--instance` 拒（提示升 serve，§2.4）；无 flag 路径行为同现状（+提示）。升级顺序铁律（client 先 serve 后）继续成立并在 compat-matrix 强调。
- **compat-matrix**：登记 v0.11.0×v0.10.0 组合（老 serve 受限面：`--instance` 不可用、自动归位降级写默认目录）与 v0.11.0×v0.11.0 全功能行。

## 6. 测试计划（摘要）

P0 矩阵见 §1.4。多实例：

1. 双实例 e2e：A/B 各 pull 各实例 → 各自 `mcp --cache --instance` 起得来、各自裁剪视图正确。
2. 交叉 fail-closed：B token 在 A 实例 spawn 失败（错误文案可辨认）。
3. `--instance` × 响应头 name 错配 → 拒写盘；头缺失（旧 serve fixture）→ 拒 + 升级提示。
4. name 白名单：server `add` 拒非法；client 端非法 name 头拒写（含 `../` 形态）。
5. DEK 隔离：A 实例 DEK 解不开 B 实例 cache.bin（解密失败路径）。
6. 首次 enroll 自动归位：默认目录无 auth → 写 `instances/<name>/` + 输出含实例名；默认目录有 auth → 仍写默认目录。
7. 存量兼容：无 flag 读写默认目录行为与现版逐字节等价（现有测试零改动通过）。
8. config 优先级：env > file > 关；file 非法值 fail-closed；`pull --max-offline` 写入后 env 清空仍生效。
9. `clear` 清全部实例 + DEK 变体；`roles` 判定：仅命名实例的机器判为 client。
10. lazy pull / quarantine / 到龄销毁在命名实例上的 per-instance 行为（复用 Plan 34/37 测试形态，路径换实例目录）。

## 7. 文档联动

- `docs/multi-machine.md`：多实例章节（enroll 双 agent 流程、`--instance` 用法、失窃响应粒度、**吊销纪律**：快速断 agent = 先吊 device code（下次 pull 销毁该实例 cache）再吊 project token（等 pull/过期）、**过渡期应急纪律** §1.3）。
- `docs/threat-model.md` §1.1：多凭据集落盘登记（Q4 取舍）。
- `docs/agent-access.md`：离线多 agent 形态一句话指向 multi-machine。
- `README.md`：`--instance` / `--max-offline` flag 说明 + cache-first 标准姿态措辞（在线 http 直连降为辅助）。
- `docs/compat-matrix.md`：§5 两行。
- `docs/backlog.md`：销项（多实例）/登记（TUI 实例列表二批、doctor 二批、cacheMaxAge 不进 config 的 YAGNI 决策）。
- 二批：`tui-multi-machine.md` 实例列表、向导 client 接入卡 `--instance` 形态。

## 8. 交付物与批次

- **P0（§1）**：独立小 PR，可单独合并并发 hotfix 版（几行改动 + §1.4 矩阵；不依赖本 spec 其余任何部分）。
- **第一批**：多实例 CLI 闭环——`CachePathsFor`/`CacheDekPathFor`/`DekProvider` 参数化 + 全消费面接线（§2.2）、name 白名单双端（§2.1）、server name 头（§2.3）、pull 写入规则（§2.4）、mcp/status 读取规则（§2.5-2.6）、clear + roles（§2.7）、MAX_OFFLINE 持久化（§3）、文档联动（§7 首批部分）。
- **第二批**：TUI client 页实例列表、向导 `--instance` 接入卡、`cache config` 子命令。
- **跟随**：doctor 多实例诊断（并入 Plan 38-doctor 体系）。

## 9. 残余与登记

1. **存量机器永不自动迁移到实例目录**：自动归位仅首次 enroll；要进实例形态需显式 `--instance` 重新 enroll（零迁移承诺的有意代价）。
2. **第一批 TUI 同步仅限默认实例**：命名实例用户第一批用 CLI/计划任务（`schtasks` wrapper 内 `--instance` flag 即可）。
3. `cacheMaxAge`（`--cache-max-age`）不进 config——只搬 MAX_OFFLINE（YAGNI）。
4. 同机恶意进程可读全部实例材料——与现状同级，非新增。
5. `SSHMGR_CACHE_DEK` 显式路径不按实例派生——单 escape hatch 语义，多实例测试需每实例独立设值（测试注意项）。
6. Plan 37 的 B-off 半开禁令在 P0 后只剩一个触发面：plaintext + B-on（env 或 config 任一开）——分支保留即为此。
