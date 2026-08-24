# Plan 34 设计：离线 cache 切断失效（pinned-401 隔离 + 可选时限）

> backlog P1 #3。2026-08-24 grilling 已拍板的决策不在本文重议：**A 切断失效必做 + B 时限做成 seam 开关默认关**（永离线残余威胁文档登记，唯一根治 = 轮换服务器凭据——backlog 原文口径）、**销毁语义 = cache.bin 隔离 + 设备码/DEK 物理删除**、**服务端 401 加 reason 字段**（revoked/unknown，纯可观测性，客户端判定不依赖）、**验收 = 自动化 e2e + owner 手工复验**（随本 plan，不等发版）、**C epoch/serial 砍**（YAGNI——FS 攻破者同样绕过 A/B）。本文为实现设计。
> 本版为第二版（2026-08-24 吸收首轮评审 9 项：8 证实 + 1 证伪采纳措辞）。修入项：watermark **前向时钟毒化**修复（pull 覆盖式重置 + 报错恢复指引 + forward 用例）、**假销毁信号**修复（per-step 汇报 + degraded 显式化）、§1 措辞钉死（active 未命中 = (nil,nil)）、project token 口径钉死、TTL spawn 边界残余登记、backup-restore 运维注记、quarantine 口径改痕迹保留、lazy 进程级已隔离标记、watermark 恒写钉死。

## 0. 目标与缺口

### 0.1 现状事实链（代码实证，2026-08-24）

1. **project token revoke 已闭环，零新工作**：`SnapshotProject.Status` 随快照携带（export.go:71），离线水合用 `VerifyToken`（status='active' 过滤，store 层）——owner 吊销 project token 后，笔记本回连 ≤30min（lazy pull cadence）刷快照，离线态随之拒绝。
2. **真缺口 = cache-token（设备码）revoke**：`RevokeCacheToken` 只断「拉新」（VerifyCacheToken 过滤 status='active' → 下次 pull 401）；盘上三件套——`cache.bin` 密文 + **本地 DEK**（cache-dek.key / keyring slot）+ `.claude.json` 里的 project token——永续可用。lazy pull 遇 401 现状**静默失败**（backoff 后退出），revoke 对已落盘快照零作用。
3. **信任锚现成**：pull 走 `pinningTransport`（TLS SPKI pin）——**pinned 连接上拿到 401 = 权威服务器在通过 pin 验证后明确拒绝**；明文 pull 的 401 不可信（HTTP 劫持可伪造——误删面）；网络错误/TLS 失败根本没到权威判定。
4. **根本事实（threat-model 诚实登记的边界）**：笔记本持有「密文 + 解密钥 + 二进制」三件，**永离线的失窃机没有任何服务端机制能远程废掉本地解密能力**。本 plan 把 revoke 的生效形态从「永不动」提级到「**回连即销毁**」+ 可选「到龄自废」；永离线根治仍只有轮换服务器凭据。

### 0.2 目标形态

revoke 设备码 → 笔记本下一次自动（≤30min lazy）或手动 pull → pinned 401 → **本地 cache 三件套销毁** → 此后 spawn 报明确错误、无凭据不再自动 pull → 重新 enroll（`cache pull` 新码）恢复。

## 1. 服务端：401 reason 字段（可观测性）

`ServeRunner.verifyCacheToken`（serve.go）：**active 验证未命中**（`VerifyCacheToken` 对 revoked/未知码均返回 `(nil, nil)`——cachetoken.go:58 的约定；**即 nil 分支就是回查入口**）后、返回 401 错误**之前**，按 token 明文的前缀回查 cache_tokens 的 revoked 行（`token_prefix` 明文前缀存库，可匹配）：

- 命中 revoked 行 → verifier 错误文本 `invalid cache token: revoked`
- 未命中 → `invalid cache token: unknown`
- serve stderr 各记一行（含 revoked 行 name——owner 排查「哪台设备刚被切断」）

**prefix 匹配是可观测性近似**：不同设备码可能共享前缀（碰撞时把 unknown 误标 revoked）——客户端判定不依赖 reason（下条），误标只影响 owner 日志读数，不做精确匹配（那要把完整 hash 比对搬进 verifier，为纯日志付结构改动，YAGNI）。

**客户端判定永不依赖 reason**：一切按「pinned 401 = 拒绝」处理（§3）——reason 字段可被降级/篡改（未来 TLS 配置失误、代理注入）也不改变销毁语义；reason 只进 owner 的日志与响应体。

## 2. 客户端销毁例程 `QuarantineCache(reason string) error`

clientops 新增，五步（顺序钉死；**per-step 汇报（rev1）**——每步成败记录在案，见下）：

1. `cache.bin` → 同目录 `quarantine/cache.bin.quarantined-<unix秒>`（MkdirAll + rename；**单份保留**——新隔离先删旧文件）
2. `cache.auth.json` **物理删除**（设备码明文，零容忍）
3. **DEK 物理删除**：Windows `cache-dek.key` 文件（`SSHMGR_CACHE_DEK` seam 路径优先）；Unix keyring slot（`SSHMGR_KEYRING_SERVICE` 体系下的 cache-dek slot，删除失败仅记录——slot 缺失即天然失效）
4. `cache.watermark`（§4）物理删除（B 状态重置）
5. stderr 一行：`cache QUARANTINED by server rejection (<reason>): snapshot isolated to quarantine/, device code + DEK deleted — re-enroll with a fresh device code`

**销毁完整性汇报（rev1，防假销毁信号）**：单步失败**记日志并继续**其余步骤（尽力而为不变），但 QuarantineCache 返回**步骤清单结果**；任一**关键步**（①cache.bin rename / ③DEK 删除）失败 → 哨兵 wrap 文本与 stderr 显式 **`DEGRADED` + 失败步骤清单**（例：`cache QUARANTINED [DEGRADED: dek delete failed: <err>] — the old snapshot may still be decryptable; delete it manually`）——owner 拿到的是真状态而非统一「已隔离」。§5 的 spawn 报文同样透传 DEGRADED 标记。

**quarantine 保留物的口径（rev1）**：DEK 已删 → 隔离目录里的 cache.bin 密文**不可解密**——保留价值是**痕迹/审计**（存在性 + 时间戳 + 大小，事后核对「何时被切」），**不是数据恢复路径**；误隔离的恢复 = 设备码仍活则重新 `cache pull` 即全量重建。

## 3. 触发点与不触发面（钉死）

**触发**（唯一路径）：`DoPull` 内 `pin != "" && res.StatusCode == 401` → 先 `QuarantineCache("server rejected device code")` → 返回哨兵 `ErrCacheQuarantined`（`errors.Is` 可判；wrap 携带服务端 reason 文本与 DEGRADED 标记仅作展示）。

- **lazy 路径**（`MaybeLazyPull`，spawn + 每次 tool-call 边界）：捕获哨兵 → stderr 日志、**不进 backoff、不计失败窗口**；并置**进程级「已隔离」标记（rev1）**——本进程后续 tool-call 边界不再自动尝试 pull（即使 auth.json 删除失败 cred 残留，也只在**下个 spawn** 重试一次——收敛性钉死：每 spawn ≤1 次销毁尝试，不构成 401 循环）。
- **手动 `cache pull`**：CLI 捕获哨兵 → 明确文案（`cache was QUARANTINED: the server rejected this device code (revoked?). Re-enroll: obtain a fresh device code and run cache pull again.`）。
- **明文 pull**（`pin == ""`，含 `--allow-plaintext`）的 401：**永不触发**（可被劫持伪造）。
- **网络错误 / TLS 失败 / 非 401 状态码**：**永不触发**（没拿到 pinned 401 = 没到权威判定）。
- **project token 不在销毁清单（rev1 口径钉死）**：销毁目标 = **cache 侧三件**（cache.bin / cache.auth.json / DEK）。`.claude.json` 里的 project token 是**用户自己的 agent 配置**——客户端程序不改写用户配置文件；其失效路径 = project token revoke（§0.1 已闭环机制：revoke 后任何一次成功 pull 刷新快照，离线水合即拒）。§0.1 第 2 条列举它只为陈述威胁面完整，不是销毁对象。

**运行中会话不断**（与 revoke 懒语义一致）：已水合 store 在内存继续服务至进程退出（cacheStoreHolder 现有「换库不 Close 旧库」语义不变）；隔离在 **spawn 边界**生效。

## 4. B 时限开关 `SSHMGR_CACHE_MAX_OFFLINE`（默认关）

- Go duration 文法（如 `720h`）；缺省/`0` = 关；**不可解析或为负 = 拒绝加载**（fail-closed，SSHMGR_BG_* 同款——`mcp --cache` spawn 失败、`cache status` 报错，错误文本含原值）。读取点：`LoadCacheSnapshot` 每次调用解析（client 侧无长驻进程，env seam 即时生效）。
- `LoadCacheSnapshot`（hydration 与 `cache status` 共用入口）加两条检查——**在解密之前**（cache.meta.json 是明文，检查不需要 DEK，超龄/回拨直接拒、省无谓解密）：
  1. **超龄**：`now − meta.PulledAt > max` → 错误 `cache snapshot expired (pulled %s ago, cap %s) — run cache pull to refresh`。
  2. **时钟回拨**：cache 目录 `cache.watermark` 文件存「见过的最大时钟」；`now < watermark − 5min 容差` → 错误 `system clock moved backwards past the cache watermark — refusing cache (possible tampering). If this machine's clock is now correct, delete cache.watermark (or re-pull with a live device code) to recover.`（rev1 附恢复指引）。
- **watermark 写时机（rev1 钉死）**：**恒写，独立于 B 开关**（中途开 B 即有基线，无回拨窗口；代价 = 每次成功 load 一次小文件原子写）。两种写法**刻意不同**：
  - **load 通过全部检查后**：写 `max(now, 旧值)`（单调推进——回拨检测的基线）。
  - **pull 成功落盘后**：写 **now（覆盖式重置，rev1）**——pull 需要活码 + pinned 服务器，能完成 pull 就证明「现在」是服务器认可的真实时点，覆盖可清掉历史前向毒化；攻击者回拨后 re-pull 也无增益（拿到的仍是服务器最新数据）。**前向时钟毒化的自愈路径**：时钟误设未来期间写过一次毒 watermark → 时钟恢复后 load 拒（附指引）→ owner 删 watermark 或直接 re-pull（码活则 pull 覆盖重置即愈）。
- **生效边界（rev1 登记）**：到龄/回拨检查在 **spawn 边界**（LoadCacheSnapshot）生效——与 §3「运行中会话不断」同语义一致：已水合的长会话可继续用内存快照超过 max 期限（残余窗口 = 会话时长；B 本就定位「提门槛非根除」，FS 攻破者另有绕法）。**残余登记，不做 tool-call 边界复查**（owner 2026-08-24 拍板：改运行语义+每调用开销，不值）。
- **提门槛非根除**（threat-model 措辞钉死）：FS 控制的攻击者可还原旧 cache.bin + 旧 watermark + 旧 DEK 备份绕过；B 的价值 = 无 FS 攻破能力的「捡走/拷走盘上文件」级窃取 + 真实长期离线到龄自废。

## 5. spawn / status 报文（钉死）

`LoadCacheSnapshot` 报 cache.bin 缺失时，上层（`mcp --cache` 启动与 `cache status`）检查 quarantine 目录**非空** → 统一文本：`cache quarantined by server rejection (token revoked?) — re-enroll via cache pull with a fresh device code`（DEGRADED 时追加失败步骤清单与手动清理指引）；否则维持现有 missing-cache 文案。判定收敛为一个 helper（两处调用零漂移）。

## 6. 文档联动

- **threat-model.md** (b)（prompt injection / 二次泄露）切断失效条目改写：复合前提（失窃 + 已 revoke）的兑现路径 = **revoke + 回连即销毁**（≤30min lazy cadence）；永离线残余 = 轮换服务器凭据（唯一根治）；B 开关与 watermark 的门槛性质如实登记；**fail-closed 代价登记（rev1）**：pinned 401 不区分 revoked/unknown——服务端 DB 从旧备份恢复 / cache_tokens 表重建等**非攻击场景**也会触发全体设备回连销毁（安全优先的取舍，恢复流程见 backup-restore 注记）。
- **multi-machine.md** revoke 语义节：`cache-tokens revoke` 从「只断拉新」改为「断拉新 + 回连销毁本地 cache」；销毁清单（cache 侧三件）+ DEGRADED 语义 + quarantine 痕迹口径 + project token 不在销毁清单的口径。
- **backup-restore.md 运维注记（rev1）**：从备份恢复 vault 后，**预期所有设备回连即销毁本地 cache**（设备码不在备份外的活跃态/或状态回滚成未知码 → pinned 401 → 切断）——恢复流程含「逐设备重新发码 + enroll」步骤，这是特性不是事故。
- **agent-access.md** 断连语义四层的第四层（离线 cache）改写；**README / concepts.md** cache 相关节核对。
- **compat-matrix.md**：纯增量行（revoke 语义增强 + 新 env `SSHMGR_CACHE_MAX_OFFLINE`）。

## 7. 测试矩阵

- **DoPull pinned-401 → 三件套销毁**（httptest TLS 自签 + pin 形态：先 pull 成功 → 服务端侧 revoke → 回连 401 → 断言 cache.bin 进 quarantine、auth.json/DEK/watermark 消失、返回 `ErrCacheQuarantined`）。
- **不触发面三断言**：明文 pull 401 不销毁；网络中断（连不上的地址）不销毁；非 401（如 500）不销毁。
- **reason 两态**：revoked 前缀命中 → `invalid cache token: revoked` + stderr 行含设备名；未知码 → unknown。
- **lazy 哨兵传播 + 已隔离标记（rev1）**：MaybeLazyPull 捕获 → stderr 日志、backoff 零变化；**同进程后续边界不再自动 pull 断言**（含 auth.json 残留的对抗用例——置残留后第二次边界零尝试）。
- **B seam 四态**：关（默认）= 超龄照用；开 = 超龄拒（文本断言）；回拨拒（篡改 watermark 往前）+ 容差内不误杀 + watermark 单调推进断言（load 路径）；非法值（`abc`/`-1h`）拒绝加载。
- **前向时钟毒化自愈（rev1）**：置毒 watermark（未来值）→ load 拒且报文含恢复指引；删 watermark → 恢复；**re-pull 后 watermark = now 覆盖重置**（毒值被清）断言。
- **DEGRADED 汇报（rev1）**：mock 关键步失败（如 DEK 文件置只读目录）→ 哨兵文本含 DEGRADED + 步骤名；§5 spawn 报文透传。
- **quarantine 单份覆盖**（两次隔离只剩最新）；**销毁后 spawn 报文**（quarantine 非空 → 统一文本）；**e2e 全链**（pull → revoke → 回连销毁 → spawn 报 quarantined → 无 cred 不再自动 pull → 重新 enroll 恢复）。
- **回归锚**：正常 pull/load 路径零变化（既有 cache 测试全绿）；运行中会话不断（隔离后已水合 store 继续服务的单测——holder 现有语义）。

## 8. 明确不做（scope 纪律）

- **C epoch/serial 防回滚**（grilling 拍板砍——FS 攻破者同样绕过 A/B，价值场景窄）。
- **tunnels revoke 级联**（#15 一个 plan 落地）。
- **audit CLI**（#16）。
- **服务端下发 max-age**（B 纯 client seam——owner 单人运维，两端同步配置反而复杂）。
- **强制断运行中会话 / tool-call 边界到龄复查**（spawn 边界生效与 revoke 懒语义一致；B 残余登记，owner 2026-08-24 拍板）。
- **销毁回滚/撤销命令**（quarantine 是痕迹不是恢复路径；恢复 = 码活则 re-pull，owner 手动删 watermark 文件）。
- **auth 删除失败的持久重试预算**（进程级标记已保证每 spawn ≤1 次；跨 spawn 持久计数超 scope）。

## 9. 验收

- **自动化**：§7 全绿。
- **owner 手工复验（随本 plan，不等发版）**：真 NUC10 `cache-tokens revoke laptop` → 笔记本手动 `cache pull` → 观察销毁（cache 消失 + quarantine 目录 + 报文）→ spawn `mcp --cache` 报 quarantined → `cache-tokens add` 重新发码 enroll 恢复 → NUC10 侧 stderr 见 revoked 行。回写验收记录。
