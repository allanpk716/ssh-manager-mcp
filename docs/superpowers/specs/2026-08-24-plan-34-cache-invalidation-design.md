# Plan 34 设计：离线 cache 切断失效（pinned-401 隔离 + 可选时限）

> backlog P1 #3。2026-08-24 grilling 已拍板的决策不在本文重议：**A 切断失效必做 + B 时限做成 seam 开关默认关**（永离线残余威胁文档登记，唯一根治 = 轮换服务器凭据——backlog 原文口径）、**销毁语义 = cache.bin 隔离 + 设备码/DEK 物理删除**、**服务端 401 加 reason 字段**（revoked/unknown，纯可观测性，客户端判定不依赖）、**验收 = 自动化 e2e + owner 手工复验**（随本 plan，不等发版）、**C epoch/serial 砍**（YAGNI——FS 攻破者同样绕过 A/B）。本文为实现设计，初版（v1）。

## 0. 目标与缺口

### 0.1 现状事实链（代码实证，2026-08-24）

1. **project token revoke 已闭环，零新工作**：`SnapshotProject.Status` 随快照携带（export.go:71），离线水合用 `VerifyToken`（status='active' 过滤，store 层）——owner 吊销 project token 后，笔记本回连 ≤30min（lazy pull cadence）刷快照，离线态随之拒绝。
2. **真缺口 = cache-token（设备码）revoke**：`RevokeCacheToken` 只断「拉新」（VerifyCacheToken 过滤 status='active' → 下次 pull 401）；盘上三件套——`cache.bin` 密文 + **本地 DEK**（cache-dek.key / keyring slot）+ `.claude.json` 里的 project token——永续可用。lazy pull 遇 401 现状**静默失败**（backoff 后退出），revoke 对已落盘快照零作用。
3. **信任锚现成**：pull 走 `pinningTransport`（TLS SPKI pin）——**pinned 连接上拿到 401 = 权威服务器在通过 pin 验证后明确拒绝**；明文 pull 的 401 不可信（HTTP 劫持可伪造——误删面）；网络错误/TLS 失败根本没到权威判定。
4. **根本事实（threat-model 诚实登记的边界）**：笔记本持有「密文 + 解密钥 + 二进制」三件，**永离线的失窃机没有任何服务端机制能远程废掉本地解密能力**。本 plan 把 revoke 的生效形态从「永不动」提级到「**回连即销毁**」+ 可选「到龄自废」；永离线根治仍只有轮换服务器凭据。

### 0.2 目标形态

revoke 设备码 → 笔记本下一次自动（≤30min lazy）或手动 pull → pinned 401 → **本地 cache 三件套销毁** → 此后 spawn 报明确错误、无凭据不再自动 pull → 重新 enroll（`cache pull` 新码）恢复。

## 1. 服务端：401 reason 字段（可观测性）

`ServeRunner.verifyCacheToken`（serve.go）：active 验证（`VerifyCacheToken`）返回 nil 后，**按 token 明文的前缀回查 cache_tokens 的 revoked 行**（`token_prefix` 明文前缀存库，可匹配）：

- 命中 revoked 行 → verifier 错误文本 `invalid cache token: revoked`
- 未命中 → `invalid cache token: unknown`
- serve stderr 各记一行（含 revoked 行 name——owner 排查「哪台设备刚被切断」）

**prefix 匹配是可观测性近似**：不同设备码可能共享前缀（碰撞时把 unknown 误标 revoked）——客户端判定不依赖 reason（下条），误标只影响 owner 日志读数，不做精确匹配（那要把完整 hash 比对搬进 verifier，为纯日志付结构改动，YAGNI）。

**客户端判定永不依赖 reason**：一切按「pinned 401 = 拒绝」处理（§3）——reason 字段可被降级/篡改（未来 TLS 配置失误、代理注入）也不改变销毁语义；reason 只进 owner 的日志与响应体。

## 2. 客户端销毁例程 `QuarantineCache(reason string) error`

clientops 新增，五步（顺序钉死，任何一步失败记日志继续——销毁尽力而为，不因单步失败回滚）：

1. `cache.bin` → 同目录 `quarantine/cache.bin.quarantined-<unix秒>`（MkdirAll + rename；**单份保留**——新隔离先删旧文件；owner 取证/误操作恢复路径）
2. `cache.auth.json` **物理删除**（设备码明文，零容忍）
3. **DEK 物理删除**：Windows `cache-dek.key` 文件（`SSHMGR_CACHE_DEK` seam 路径优先）；Unix keyring slot（`SSHMGR_KEYRING_SERVICE` 体系下的 cache-dek slot，删除失败仅日志——slot 缺失即天然失效）
4. `cache.watermark`（§4）物理删除（B 状态重置）
5. stderr 一行：`cache QUARANTINED by server rejection (<reason>): snapshot isolated to quarantine/, device code + DEK deleted — re-enroll with a fresh device code`

## 3. 触发点与不触发面（钉死）

**触发**（唯一路径）：`DoPull` 内 `pin != "" && res.StatusCode == 401` → 先 `QuarantineCache("server rejected device code")` → 返回哨兵 `ErrCacheQuarantined`（`errors.Is` 可判；wrap 携带服务端 reason 文本仅作展示）。

- **lazy 路径**（`MaybeLazyPull`，spawn + 每次 tool-call 边界）：捕获哨兵 → stderr 日志、**不进 backoff、不计失败窗口**（设备码已删，后续 pull 因 `ReadCacheCred` 返回 nil 自然跳过自动 pull——现有逻辑零改动）。
- **手动 `cache pull`**：CLI 捕获哨兵 → 明确文案（`cache was QUARANTINED: the server rejected this device code (revoked?). Re-enroll: obtain a fresh device code and run cache pull again.`）。
- **明文 pull**（`pin == ""`，含 `--allow-plaintext`）的 401：**永不触发**（可被劫持伪造）。
- **网络错误 / TLS 失败 / 非 401 状态码**：**永不触发**（没拿到 pinned 401 = 没到权威判定）。

**运行中会话不断**（与 revoke 懒语义一致）：已水合 store 在内存继续服务至进程退出（cacheStoreHolder 现有「换库不 Close 旧库」语义不变）；隔离在 **spawn 边界**生效。

## 4. B 时限开关 `SSHMGR_CACHE_MAX_OFFLINE`（默认关）

- Go duration 文法（如 `720h`）；缺省/`0` = 关；**不可解析或为负 = 拒绝加载**（fail-closed，SSHMGR_BG_* 同款——`mcp --cache` spawn 失败、`cache status` 报错，错误文本含原值）。读取点：`LoadCacheSnapshot` 每次调用解析（client 侧无长驻进程，env seam 即时生效）。
- `LoadCacheSnapshot`（hydration 与 `cache status` 共用入口）加两条检查——**在解密之前**（cache.meta.json 是明文，检查不需要 DEK，超龄/回拨直接拒、省无谓解密）：
  1. **超龄**：`now − meta.PulledAt > max` → 错误 `cache snapshot expired (pulled %s ago, cap %s) — run cache pull to refresh`。
  2. **时钟回拨**：cache 目录 `cache.watermark` 文件存「见过的最大时钟」；`now < watermark − 5min 容差` → 错误 `system clock moved backwards past the cache watermark — refusing cache (possible tampering)`。
- watermark 写时机：**每次成功 pull（DoPull 落盘后）与每次成功 load（LoadCacheSnapshot 通过全部检查后）**写 `max(now, 旧值)`；写失败仅日志（不阻断 load——watermark 是防回拨门槛，不是审计）。
- **提门槛非根除**（threat-model 措辞钉死）：FS 控制的攻击者可还原旧 cache.bin + 旧 watermark + 旧 DEK 备份绕过；B 的价值 = 无 FS 攻破能力的「捡走/拷走盘上文件」级窃取 + 真实长期离线到龄自废。

## 5. spawn / status 报文（钉死）

`LoadCacheSnapshot` 报 cache.bin 缺失时，上层（`mcp --cache` 启动与 `cache status`）检查 quarantine 目录**非空** → 统一文本：`cache quarantined by server rejection (token revoked?) — re-enroll via cache pull with a fresh device code`；否则维持现有 missing-cache 文案。判定收敛为一个 helper（两处调用零漂移）。

## 6. 文档联动

- **threat-model.md** (b)（prompt injection / 二次泄露）切断失效条目改写：复合前提（失窃 + 已 revoke）的兑现路径 = **revoke + 回连即销毁**（≤30min lazy cadence）；永离线残余 = 轮换服务器凭据（唯一根治）；B 开关与 watermark 的门槛性质如实登记。
- **multi-machine.md** revoke 语义节：`cache-tokens revoke` 从「只断拉新」改为「断拉新 + 回连销毁本地 cache」；四件套销毁清单 + quarantine 恢复路径。
- **agent-access.md** 断连语义四层的第四层（离线 cache）改写；**README / concepts.md** cache 相关节核对。
- **compat-matrix.md**：纯增量行（revoke 语义增强 + 新 env `SSHMGR_CACHE_MAX_OFFLINE`）。

## 7. 测试矩阵

- **DoPull pinned-401 → 三件套销毁**（httptest TLS 自签 + `serve cert-info` pin 形态：先 pull 成功 → 服务端侧 revoke → 回连 401 → 断言 cache.bin 进 quarantine、auth.json/DEK/watermark 消失、返回 `ErrCacheQuarantined`）。
- **不触发面三断言**：明文 pull 401 不销毁；网络中断（连不上的地址）不销毁；非 401（如 500）不销毁。
- **reason 两态**：revoked 前缀命中 → `invalid cache token: revoked` + stderr 行含设备名；未知码 → unknown。
- **lazy 哨兵传播**：MaybeLazyPull 捕获 → stderr 日志、backoff 状态零变化、后续调用因无 cred 跳过。
- **B seam 三态**：关（默认）= 超龄照用；开 = 超龄拒（文本断言）；回拨拒（篡改 watermark 往前）+ 容差内不误杀 + watermark 单调推进断言；非法值（`abc`/`-1h`）拒绝启动。
- **quarantine 单份覆盖**（两次隔离只剩最新）；**销毁后 spawn 报文**（quarantine 非空 → 统一文本）；**e2e 全链**（pull → revoke → 回连销毁 → spawn 报 quarantined → 无 cred 不再自动 pull → 重新 enroll 恢复）。
- **回归锚**：正常 pull/load 路径零变化（既有 cache 测试全绿）；运行中会话不断（隔离后已水合 store 继续服务的单测——holder 现有语义）。

## 8. 明确不做（scope 纪律）

- **C epoch/serial 防回滚**（grilling 拍板砍——FS 攻破者同样绕过 A/B，价值场景窄）。
- **tunnels revoke 级联**（#15 一个 plan 落地）。
- **audit CLI**（#16）。
- **服务端下发 max-age**（B 纯 client seam——owner 单人运维，两端同步配置反而复杂）。
- **强制断运行中会话**（spawn 边界生效与 revoke 懒语义一致，不改 holder）。
- **销毁回滚/撤销**（quarantine 恢复 = owner 手动移文件 + 重新 pull，不做命令）。

## 9. 验收

- **自动化**：§7 全绿。
- **owner 手工复验（随本 plan，不等发版）**：真 NUC10 `cache-tokens revoke laptop` → 笔记本手动 `cache pull` → 观察销毁（cache 消失 + quarantine 目录 + 报文）→ spawn `mcp --cache` 报 quarantined → `cache-tokens add` 重新发码 enroll 恢复 → NUC10 侧 stderr 见 revoked 行。回写验收记录。
