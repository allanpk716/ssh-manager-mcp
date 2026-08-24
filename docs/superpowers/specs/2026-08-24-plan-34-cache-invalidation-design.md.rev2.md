# Plan 34 设计：离线 cache 切断失效（pinned-401 隔离 + 可选时限）

> backlog P1 #3。2026-08-24 grilling 已拍板的决策不在本文重议：**A 切断失效必做 + B 时限做成 seam 开关默认关**（永离线残余威胁文档登记，唯一根治 = 轮换服务器凭据——backlog 原文口径）、**销毁语义 = cache.bin 隔离 + 设备码/DEK 物理删除**、**服务端 401 加 reason 字段**（revoked/unknown，纯可观测性，客户端判定不依赖）、**验收 = 自动化 e2e + owner 手工复验**（随本 plan，不等发版）、**C epoch/serial 砍**（YAGNI——FS 攻破者同样绕过 A/B）。本文为实现设计。
> 本版为第三版。二版吸收首轮 9 项（8 证实 + 1 证伪采纳措辞：watermark 毒化自愈、DEGRADED 显式化、§1 措辞、project token 口径、TTL 残余登记、backup-restore 注记、quarantine 痕迹口径、lazy 已隔离标记、watermark 恒写）；本版吸收二轮 10 项（11 证实 + 1 前提证伪采纳方向）：**服务器时间锚**（pull 的 PulledAt/watermark 重置优先用 `/snapshot` 响应的 `X-Sshmgr-Time` 头——客户端时钟可回拨重锚）、**quarantine/manifest.json**（销毁结果跨进程持久化，DEGRADED 可见、幂等判定、归因）、**DEK-first 销毁顺序**（钥匙先死，任何崩溃点最坏 = 密文原地但不可解）、DEGRADED 关键步统一（bin/auth/DEK 全部 + 幂等例外）、watermark 写失败仅日志、失窃响应两 token 口径、backup-restore 注记按事实（恢复后 cache_tokens 为空）、回拨检查仅 B 开、DEGRADED 循环残余登记。

## 0. 目标与缺口

### 0.1 现状事实链（代码实证，2026-08-24）

1. **project token revoke 已闭环，零新工作**：`SnapshotProject.Status` 随快照携带（export.go:71），离线水合用 `VerifyToken`（status='active' 过滤，store 层）——owner 吊销 project token 后，笔记本回连 ≤30min（lazy pull cadence）刷快照，离线态随之拒绝。
2. **真缺口 = cache-token（设备码）revoke**：`RevokeCacheToken` 只断「拉新」（VerifyCacheToken 过滤 status='active' → 下次 pull 401）；盘上三件套——`cache.bin` 密文 + **本地 DEK**（cache-dek.key / keyring slot）+ `.claude.json` 里的 project token——永续可用。lazy pull 遇 401 现状**静默失败**（backoff 后退出），revoke 对已落盘快照零作用。
3. **信任锚现成**：pull 走 `pinningTransport`（TLS SPKI pin）——**pinned 连接上拿到 401 = 权威服务器在通过 pin 验证后明确拒绝**；明文 pull 的 401 不可信（HTTP 劫持可伪造——误删面）；网络错误/TLS 失败根本没到权威判定。
4. **根本事实（threat-model 诚实登记的边界）**：笔记本持有「密文 + 解密钥 + 二进制」三件，**永离线的失窃机没有任何服务端机制能远程废掉本地解密能力**。本 plan 把 revoke 的生效形态从「永不动」提级到「**回连即销毁**」+ 可选「到龄自废」；永离线根治仍只有轮换服务器凭据。

### 0.2 目标形态

revoke 设备码 → 笔记本下一次自动（≤30min lazy）或手动 pull → pinned 401 → **本地 cache 三件套销毁** → 此后 spawn 报明确错误、无凭据不再自动 pull → 重新 enroll（`cache pull` 新码）恢复。

## 1. 服务端：401 reason 字段 + 服务器时间头

`ServeRunner.verifyCacheToken`（serve.go）：**active 验证未命中**（`VerifyCacheToken` 对 revoked/未知码均返回 `(nil, nil)`——cachetoken.go:58 的约定；**即 nil 分支就是回查入口**）后、返回 401 错误**之前**，按 token 明文的前缀回查 cache_tokens 的 revoked 行（`token_prefix` 明文前缀存库，可匹配）：

- 命中 revoked 行 → verifier 错误文本 `invalid cache token: revoked`
- 未命中 → `invalid cache token: unknown`
- serve stderr 各记一行（含 revoked 行 name——owner 排查「哪台设备刚被切断」）

**prefix 匹配是可观测性近似**：不同设备码可能共享前缀（碰撞时把 unknown 误标 revoked）——客户端判定不依赖 reason（下条），误标只影响 owner 日志读数，不做精确匹配（那要把完整 hash 比对搬进 verifier，为纯日志付结构改动，YAGNI）。

**客户端判定永不依赖 reason**：一切按「pinned 401 = 拒绝」处理（§3）——reason 字段可被降级/篡改（未来 TLS 配置失误、代理注入）也不改变销毁语义；reason 只进 owner 的日志与响应体。

**服务器时间头（rev2）**：`handleSnapshot` 在成功响应上加 `X-Sshmgr-Time: <unix秒>`（服务器当前时钟）。客户端 pull 以它为**时间锚**（§4）——服务器时间是回拨攻击者伪造不了的时基（TLS pin 保证头来自真服务器）。

## 2. 客户端销毁例程 `QuarantineCache(reason string) error`

clientops 新增。**执行顺序（rev2，DEK-first 钥匙先死）**——任何一步后崩溃，最坏状态 = 密文留在原地但 DEK 已亡（不可解），**crash-safe**；**不做自动续跑**（crash 残态由下次 spawn 的 §5 报文暴露 + re-enroll 自愈）：

0. **写 `quarantine/manifest.json` 意图**（`{state:"started", reason, ts}` 原子写）——跨进程持久化的锚（§5 判定读它）。
1. **DEK 物理删除**：Windows `cache-dek.key` 文件（`SSHMGR_CACHE_DEK` seam 路径优先）；Unix keyring slot（`SSHMGR_KEYRING_SERVICE` 体系下 cache-dek slot）。
2. `cache.auth.json` **物理删除**（设备码明文，零容忍）。
3. `cache.bin` → 同目录 `quarantine/cache.bin.quarantined-<unix秒>`（MkdirAll + rename；**单份保留**——新隔离先删旧文件）。
4. `cache.watermark`（§4）物理删除（B 状态重置）。
5. **更新 manifest 完成**：`{state:"done", steps:{dek,auth,bin,watermark}, degraded:<失败步骤清单或空>, reason, ts}`。
6. stderr 一行：`cache QUARANTINED by server rejection (<reason>): snapshot isolated to quarantine/, device code + DEK deleted — re-enroll with a fresh device code`。

**关键步与 DEGRADED（rev2 统一）**：①DEK / ②auth / ③bin rename **全部是关键步**——任一删除/移动**出错** → manifest 与 stderr 记 **DEGRADED + 失败步骤清单**（例：`[DEGRADED: dek delete failed: <err>] — the old snapshot may still be decryptable; delete it manually`）。**幂等例外**：目标**已不存在**（文件缺失 / keyring slot 不存在）= 该步**幂等成功**，不计失败——重隔离、首 enroll 打错码、用户手删过 cache.bin 等路径下不会产生假 DEGRADED。单步失败记日志并继续其余步骤（尽力而为），但**如实落进 manifest**（绝不静默当成功）。

**quarantine 保留物的口径**：DEK 已删 → 隔离目录里的 cache.bin 密文**不可解密**——保留价值是**痕迹/审计**（manifest + 文件时间戳，事后核对「何时被切、切得干不干净」），**不是数据恢复路径**；误隔离的恢复 = 设备码仍活则重新 `cache pull` 即全量重建。

## 3. 触发点与不触发面（钉死）

**触发**（唯一路径）：`DoPull` 内 `pin != "" && res.StatusCode == 401` → 先 `QuarantineCache("server rejected device code")` → 返回哨兵 `ErrCacheQuarantined`（`errors.Is` 可判；wrap 携带服务端 reason 文本与 DEGRADED 标记仅作展示）。

- **lazy 路径**（`MaybeLazyPull`，spawn + 每次 tool-call 边界）：捕获哨兵 → stderr 日志、**不进 backoff、不计失败窗口**；并置**进程级「已隔离」标记**——本进程后续 tool-call 边界不再自动尝试 pull（即使 auth.json 删除失败 cred 残留，也只在**下个 spawn** 重试一次——收敛性钉死：每 spawn ≤1 次销毁尝试）。**残余形态登记（rev2）**：若删除失败是持久性的（只读目录/权限），每个 spawn 都会产生一次 401+销毁+DEGRADED 循环直至人工介入——这是「每 spawn ≤1 次」语义的自然余项，验收时按预期形态对待。
- **手动 `cache pull`**：CLI 捕获哨兵 → 明确文案（`cache was QUARANTINED: the server rejected this device code (revoked?). Re-enroll: obtain a fresh device code and run cache pull again.`）。
- **明文 pull**（`pin == ""`，含 `--allow-plaintext`）的 401：**永不触发**（可被劫持伪造）。
- **网络错误 / TLS 失败 / 非 401 状态码**：**永不触发**（没拿到 pinned 401 = 没到权威判定）。
- **project token 不在销毁清单（口径钉死）**：销毁目标 = **cache 侧三件**（cache.bin / cache.auth.json / DEK）。`.claude.json` 里的 project token 是**用户自己的 agent 配置**——客户端程序不改写用户配置文件；其失效路径 = project token revoke（§0.1 已闭环机制）。**失窃响应口径（rev2）**：revoke cache token ≠ 切断该设备上的 project token——**失窃处置 = 两个都 revoke**（文档与 `cache-tokens revoke` 的 CLI 输出均提示此点）。

**运行中会话不断**（与 revoke 懒语义一致）：已水合 store 在内存继续服务至进程退出（cacheStoreHolder 现有「换库不 Close 旧库」语义不变）；隔离在 **spawn 边界**生效。

## 4. B 时限开关 `SSHMGR_CACHE_MAX_OFFLINE`（默认关）

- Go duration 文法（如 `720h`）；缺省/`0` = 关；**不可解析或为负 = 拒绝加载**（fail-closed，SSHMGR_BG_* 同款——`mcp --cache` spawn 失败、`cache status` 报错，错误文本含原值）。读取点：`LoadCacheSnapshot` 每次调用解析（client 侧无长驻进程，env seam 即时生效）。
- `LoadCacheSnapshot`（hydration 与 `cache status` 共用入口）加两条检查——**在解密之前**（cache.meta.json 是明文，检查不需要 DEK）：
  1. **超龄**：`now − meta.PulledAt > max` → 错误 `cache snapshot expired (pulled %s ago, cap %s) — run cache pull to refresh`。
  2. **时钟回拨**：cache 目录 `cache.watermark` 文件存「见过的最大时钟」；`now < watermark − 5min 容差` → 错误 `system clock moved backwards past the cache watermark — refusing cache (possible tampering). If this machine's clock is now correct, delete cache.watermark (or re-pull with a live device code) to recover.`。
- **两条检查都只在 B 开启时执行**（rev2 钉死）；watermark 的**写**恒发生（独立于 B），写只是留基线。
- **watermark 写时机与语义（rev2 钉死）**：
  - **load 通过全部检查后**：写 `max(now, 旧值)`（单调推进——回拨检测的基线）。
  - **pull 成功落盘后**：写 **pull 的时间锚值（覆盖式重置）**。**时间锚优先服务器时间（rev2）**：`X-Sshmgr-Time` 响应头存在 → 用它（服务器时钟攻击者伪造不了——TLS pin 保证来源）；**缺头（老版本 serve）→ 回退客户端 now 并记日志**。pull 能完成 = 服务器在线认可该设备码，锚值即「服务器认可的真实时点」——既清掉历史前向毒化，又使「回拨 + 重拉重锚假时基」失效（重拉拿到的是服务器时间）。`meta.PulledAt` 同源（服务器时间优先）——到龄检查的分子分母同锚。
  - **写失败 = 仅日志，不阻断 pull/load**（rev2 钉死；watermark 是门槛不是审计——fail-closed 拒 load 会在 B 默认关时对只读 cache 目录引入新回归）。
  - **前向毒化自愈路径**：时钟误设未来期间写过毒 watermark → 时钟恢复后 load 拒（报文附指引）→ owner 删 watermark 或直接 re-pull（码活则 pull 以服务器时间覆盖重置即愈）。
- **生效边界（登记）**：到龄/回拨检查在 **spawn 边界**（LoadCacheSnapshot）生效——与 §3「运行中会话不断」同语义：已水合的长会话可继续用内存快照超过 max 期限（残余窗口 = 会话时长）。**残余登记，不做 tool-call 边界复查**（owner 2026-08-24 拍板）。
- **提门槛非根除**（threat-model 措辞钉死）：FS 控制的攻击者可还原旧 cache.bin + 旧 watermark + 旧 DEK 备份绕过；B 的价值 = 无 FS 攻破能力的「捡走/拷走盘上文件」级窃取 + 真实长期离线到龄自废。

## 5. spawn / status 报文（manifest 驱动，rev2）

`LoadCacheSnapshot` 报 cache.bin 缺失（或解密失败）时，上层（`mcp --cache` 启动与 `cache status`）读 **`quarantine/manifest.json`** 判定（判定收敛为一个 helper，两处调用零漂移）：

- manifest 存在且 `state=done` 且 `degraded` 非空 → `cache quarantined by server rejection (token revoked?) [DEGRADED: <失败步骤>] — re-enroll via cache pull with a fresh device code; manual cleanup may be needed`。
- manifest 存在且 `state=done` → 统一文本 `cache quarantined by server rejection (token revoked?) — re-enroll via cache pull with a fresh device code`。
- manifest 存在且 `state=started`（销毁中途崩溃/未完成）→ `cache quarantine was interrupted — the snapshot may still exist; re-enroll via cache pull, or inspect quarantine/manifest.json`。
- **cache.bin 在位但 DEK 缺失/解密失败**（DEK-first 顺序的崩溃残态）→ 归入同族报文：`cache decrypt failed and the DEK is absent — this looks like an interrupted quarantine; re-enroll via cache pull`。
- manifest 不存在 → 维持现有 missing-cache 文案。
- **归因防误报**：成功 pull 落盘时**重置 manifest 归因**（删除 manifest 或标记 `superseded`）——重新 enroll 后 cache.bin 若再因无关原因缺失，报 missing 而非 quarantined。

## 6. 文档联动

- **threat-model.md** (b) 切断失效条目改写：复合前提（失窃 + 已 revoke）的兑现路径 = **revoke + 回连即销毁**（≤30min lazy cadence）；永离线残余 = 轮换服务器凭据（唯一根治）；B 开关与 watermark 的门槛性质如实登记；**fail-closed 代价登记**：pinned 401 不区分 revoked/unknown——非攻击场景（服务端数据丢失/重建等）也会触发全体设备回连销毁（安全优先的取舍）；**失窃响应口径（rev2）**：cache token 与该设备上的 project token 都要 revoke。
- **multi-machine.md** revoke 语义节：`cache-tokens revoke` 从「只断拉新」改为「断拉新 + 回连销毁本地 cache」；销毁清单（cache 侧三件）+ DEGRADED/manifest 语义 + quarantine 痕迹口径 + project token 口径与两 token 失窃处置。
- **backup-restore.md 运维注记（rev2 按事实）**：`ExportSnapshot` **不含 cache_tokens**（export.go 零处读该表）——从备份恢复后 cache_tokens 表**为空** → 所有设备下次回连拿 unknown 401 → **批量切断**（预期行为、非事故）；恢复流程含「逐设备重新发码 + enroll」步骤。**带外警示**：若用 raw-DB 文件直拷恢复（不走 import），cache_tokens 会连历史状态一起回滚——**已 revoke 的码可能复活**，此类操作后必须逐行审计 cache_tokens 并重发。
- **agent-access.md** 断连语义四层的第四层（离线 cache）改写；**README / concepts.md** cache 相关节核对。
- **CLI 输出提示（rev2）**：`cache-tokens revoke` 成功输出附一行「reminder: also revoke project tokens issued to that device if it may be compromised」。
- **compat-matrix.md**：纯增量行（revoke 语义增强 + 新 env `SSHMGR_CACHE_MAX_OFFLINE` + 新响应头 `X-Sshmgr-Time`）。

## 7. 测试矩阵

- **DoPull pinned-401 → 三件套销毁**（httptest TLS 自签 + pin 形态：先 pull 成功 → 服务端侧 revoke → 回连 401 → 断言 cache.bin 进 quarantine、auth.json/DEK/watermark 消失、manifest state=done degraded 空、返回 `ErrCacheQuarantined`）。
- **不触发面三断言**：明文 pull 401 不销毁；网络中断不销毁；非 401（如 500）不销毁。
- **reason 两态**：revoked 前缀命中 → `invalid cache token: revoked` + stderr 行含设备名；未知码 → unknown。
- **服务器时间锚（rev2）**：响应带 `X-Sshmgr-Time` 头 → meta.PulledAt 与 watermark 重置用头值断言；**缺头 → 回退客户端时钟 + 日志断言**。
- **DEGRADED 汇报（rev2）**：mock 关键步失败（DEK 文件置只读目录）→ manifest degraded 含步骤名 + 哨兵文本含 DEGRADED + §5 报文透传断言。
- **幂等重隔离（rev2）**：cache.bin 已缺失（模拟重隔离/首 enroll 打错码）→ 关键步 bin 视为幂等成功、manifest degraded 空（无假 DEGRADED）。
- **manifest 中断态（rev2）**：只写 started 不走完 → §5 报 interrupted 文案；**DEK-first 崩溃残态**（bin 在位 + DEK 删）→ 报 decrypt-failed/interrupted-quarantine 文案。
- **归因防误报（rev2）**：隔离 → 重新 enroll（pull 成功）→ manifest 重置 → 手删 cache.bin → 报 missing 而非 quarantined。
- **lazy 哨兵传播 + 已隔离标记**：MaybeLazyPull 捕获 → stderr 日志、backoff 零变化；同进程后续边界不再自动 pull 断言（含 auth.json 残留对抗用例）。
- **B seam 四态**：关（默认）= 超龄照用；开 = 超龄拒（文本断言）；回拨拒 + 容差内不误杀 + watermark 单调推进断言（load 路径）；非法值（`abc`/`-1h`）拒绝加载。**B 关时回拨检查不执行断言**（rev2）。
- **前向毒化自愈**：置毒 watermark → load 拒且报文含恢复指引；删 watermark → 恢复；re-pull（带服务器时间头）后 watermark = 服务器时值覆盖重置断言。
- **quarantine 单份覆盖**；**e2e 全链**（pull → revoke → 回连销毁 → spawn 报 quarantined → 无 cred 不再自动 pull → 重新 enroll 恢复）。
- **回归锚**：正常 pull/load 路径零变化（既有 cache 测试全绿）；运行中会话不断。

## 8. 明确不做（scope 纪律）

- **C epoch/serial 防回滚**（grilling 拍板砍）。
- **tunnels revoke 级联**（#15）；**audit CLI**（#16）。
- **服务端下发 max-age**（B 纯 client seam）。
- **强制断运行中会话 / tool-call 边界到龄复查**（spawn 边界生效；owner 拍板登记残余）。
- **销毁自动续跑/事务回滚**（DEK-first 顺序已使任何崩溃点安全；中断残态由 spawn 报文暴露 + 人工 re-enroll 自愈——manifest 只做状态记录不做恢复执行器）。
- **销毁回滚/撤销命令**（quarantine 是痕迹不是恢复路径）。
- **auth 删除失败的跨 spawn 持久重试预算**（进程级标记已保证每 spawn ≤1 次；持久循环形态已登记 §3）。

## 9. 验收

- **自动化**：§7 全绿。
- **owner 手工复验（随本 plan，不等发版）**：真 NUC10 `cache-tokens revoke laptop` → 笔记本手动 `cache pull` → 观察销毁（cache 消失 + quarantine 目录 + manifest + 报文）→ spawn `mcp --cache` 报 quarantined → `cache-tokens add` 重新发码 enroll 恢复 → NUC10 侧 stderr 见 revoked 行。回写验收记录。
