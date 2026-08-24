# Plan 34 设计：离线 cache 切断失效（pinned-401 隔离）

> backlog P1 #3。2026-08-24 grilling 已拍板的决策不在本文重议：**A 切断失效必做**（永离线残余威胁文档登记，唯一根治 = 轮换服务器凭据——backlog 原文口径）、**销毁语义 = cache.bin 隔离 + 设备码/DEK 物理删除**、**服务端 401 加 reason 字段**（revoked/unknown，纯可观测性，客户端判定不依赖）、**验收 = 自动化 e2e + owner 手工复验**（随本 plan，不等发版）。本文为实现设计。
> 本版为第四版。二版吸收首轮 9 项；三版吸收二轮 10 项（服务器时间锚/manifest 持久化/DEK-first 等）；**本版 scope 降级（owner 2026-08-24 拍板）：B 时限快照（SSHMGR_CACHE_MAX_OFFLINE + watermark + X-Sshmgr-Time 服务器时间锚）整体砍出本 plan 回 backlog**——三轮评审实证其完整正确性需「时间锚信任边界（pinned 采信/skew 校验/缺头 fail-closed）+ 跨进程写串行化 + 前向毒化自愈」三件配套，修复面连续膨胀（每轮新机器引出新评审面），而它是默认关的可选开关；A（切断失效）核心链路三轮零核心异议。本版同时吸收三审对 A 的修复：manifest **best-effort**（写失败不阻断销毁）、MkdirAll 前置、报文归因门槛。

## 0. 目标与缺口

### 0.1 现状事实链（代码实证，2026-08-24）

1. **project token revoke 已闭环，零新工作**：`SnapshotProject.Status` 随快照携带（export.go:71），离线水合用 `VerifyToken`（status='active' 过滤，store 层）——owner 吊销 project token 后，笔记本回连 ≤30min（lazy pull cadence）刷快照，离线态随之拒绝。
2. **真缺口 = cache-token（设备码）revoke**：`RevokeCacheToken` 只断「拉新」（VerifyCacheToken 过滤 status='active' → 下次 pull 401）；盘上三件套——`cache.bin` 密文 + **本地 DEK**（cache-dek.key / keyring slot）+ `.claude.json` 里的 project token——永续可用。lazy pull 遇 401 现状**静默失败**（backoff 后退出），revoke 对已落盘快照零作用。
3. **信任锚现成**：pull 走 `pinningTransport`（TLS SPKI pin）——**pinned 连接上拿到 401 = 权威服务器在通过 pin 验证后明确拒绝**；明文 pull 的 401 不可信（HTTP 劫持可伪造——误删面）；网络错误/TLS 失败根本没到权威判定。
4. **根本事实（threat-model 诚实登记的边界）**：笔记本持有「密文 + 解密钥 + 二进制」三件，**永离线的失窃机没有任何服务端机制能远程废掉本地解密能力**。本 plan 把 revoke 的生效形态从「永不动」提级到「**回连即销毁**」；永离线根治仍只有轮换服务器凭据。

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

clientops 新增。**执行顺序（DEK-first 钥匙先死；manifest 是 best-effort 记录不是前置闸）**——任何一步后崩溃，最坏状态 = 密文留在原地但 DEK 已亡（不可解），**crash-safe**；**不做自动续跑**（crash 残态由下次 spawn 的 §5 报文暴露 + re-enroll 自愈）：

0. **manifest 意图（best-effort，rev3 钉死）**：`os.MkdirAll(quarantine/)` → 原子写 `quarantine/manifest.json` = `{state:"started", reason, ts}`。**写失败（目录建不了/磁盘满/权限）只记日志并继续**——manifest 的价值是跨进程持久化汇报（§5），**绝不构成销毁的前置条件**；销毁的正确性不依赖它（后续步骤照跑，结果经返回值与 stderr 汇报）。
1. **DEK 物理删除**：Windows `cache-dek.key` 文件（`SSHMGR_CACHE_DEK` seam 路径优先）；Unix keyring slot（`SSHMGR_KEYRING_SERVICE` 体系下 cache-dek slot）。
2. `cache.auth.json` **物理删除**（设备码明文，零容忍）。
3. `cache.bin` → `quarantine/cache.bin.quarantined-<unix秒>`（rename；**单份保留**——新隔离先删旧文件）。
4. **manifest 完成（best-effort）**：更新为 `{state:"done", steps:{dek,auth,bin}, degraded:<失败步骤清单或空>, reason, ts}`；写失败同样只记日志。
5. stderr 一行：`cache QUARANTINED by server rejection (<reason>): snapshot isolated to quarantine/, device code + DEK deleted — re-enroll with a fresh device code`。

**关键步与 DEGRADED（钉死）**：①DEK / ②auth / ③bin rename **全部是关键步**——任一删除/移动**出错** → 返回值、stderr 与 manifest（若可写）记 **DEGRADED + 失败步骤清单**（例：`[DEGRADED: dek delete failed: <err>] — the old snapshot may still be decryptable; delete it manually`）。**幂等例外**：目标**已不存在**（文件缺失 / keyring slot 不存在）= 该步**幂等成功**，不计失败——重隔离、首 enroll 打错码、用户手删过 cache.bin 等路径不产生假 DEGRADED。单步失败记日志并继续其余步骤（尽力而为），但**如实汇报**（绝不静默当成功）。

**quarantine 保留物的口径**：DEK 已删 → 隔离目录里的 cache.bin 密文**不可解密**——保留价值是**痕迹/审计**（manifest + 文件时间戳，事后核对「何时被切、切得干不干净」），**不是数据恢复路径**；误隔离的恢复 = 设备码仍活则重新 `cache pull` 即全量重建。

## 3. 触发点与不触发面（钉死）

**触发**（唯一路径）：`DoPull` 内 `pin != "" && res.StatusCode == 401` → 先 `QuarantineCache("server rejected device code")` → 返回哨兵 `ErrCacheQuarantined`（`errors.Is` 可判；wrap 携带服务端 reason 文本与 DEGRADED 标记仅作展示）。

- **lazy 路径**（`MaybeLazyPull`，spawn + 每次 tool-call 边界）：捕获哨兵 → stderr 日志、**不进 backoff、不计失败窗口**；并置**进程级「已隔离」标记**——本进程后续 tool-call 边界不再自动尝试 pull（即使 auth.json 删除失败 cred 残留，也只在**下个 spawn** 重试一次——收敛性钉死：每 spawn ≤1 次销毁尝试）。**残余形态登记**：若删除失败是持久性的（只读目录/权限），每个 spawn 都会产生一次 401+销毁+DEGRADED 循环直至人工介入——「每 spawn ≤1 次」语义的自然余项，验收时按预期形态对待。
- **手动 `cache pull`**：CLI 捕获哨兵 → 明确文案（`cache was QUARANTINED: the server rejected this device code (revoked?). Re-enroll: obtain a fresh device code and run cache pull again.`）。
- **明文 pull**（`pin == ""`，含 `--allow-plaintext`）的 401：**永不触发**（可被劫持伪造）。
- **网络错误 / TLS 失败 / 非 401 状态码**：**永不触发**（没拿到 pinned 401 = 没到权威判定）。
- **project token 不在销毁清单（口径钉死）**：销毁目标 = **cache 侧三件**（cache.bin / cache.auth.json / DEK）。`.claude.json` 里的 project token 是**用户自己的 agent 配置**——客户端程序不改写用户配置文件；其失效路径 = project token revoke（§0.1 已闭环机制）。**失窃响应口径**：revoke cache token ≠ 切断该设备上的 project token——**失窃处置 = 两个都 revoke**（文档与 `cache-tokens revoke` 的 CLI 输出均提示此点）。

**运行中会话不断**（与 revoke 懒语义一致）：已水合 store 在内存继续服务至进程退出（cacheStoreHolder 现有「换库不 Close 旧库」语义不变）；隔离在 **spawn 边界**生效。

## 4. spawn / status 报文（manifest 驱动 + 归因门槛）

`LoadCacheSnapshot` 报 cache.bin 缺失（或解密失败）时，上层（`mcp --cache` 启动与 `cache status`）读 **`quarantine/manifest.json`** 判定（判定收敛为一个 helper，两处调用零漂移）：

- manifest 存在且 `state=done` 且 `degraded` 非空 → `cache quarantined by server rejection (token revoked?) [DEGRADED: <失败步骤>] — re-enroll via cache pull with a fresh device code; manual cleanup may be needed`。
- manifest 存在且 `state=done` → `cache quarantined by server rejection (token revoked?) — re-enroll via cache pull with a fresh device code`。
- manifest 存在且 `state=started`（销毁中途崩溃/未完成）→ `cache quarantine was interrupted — the snapshot may still exist; re-enroll via cache pull, or inspect quarantine/manifest.json`。
- **cache.bin 在位但解密失败：以 manifest 存在为归因门槛（rev3）**——manifest 在 → interrupted-quarantine 同族报文（DEK-first 顺序的崩溃残态，§2）；**manifest 不在 → 维持现有通用 decrypt 错误文本**（keyring 暂不可用/路径配置错/无关 DEK 损坏等不做 quarantine 归因，防误报）。
- manifest 不存在且 cache.bin 缺失 → 维持现有 missing-cache 文案。
- **归因防误报**：成功 pull 落盘时**重置 manifest 归因**（删除 manifest 或标记 `superseded`）——重新 enroll 后 cache.bin 若再因无关原因缺失，报 missing 而非 quarantined。

## 5. 文档联动

- **threat-model.md** (b) 切断失效条目改写：复合前提（失窃 + 已 revoke）的兑现路径 = **revoke + 回连即销毁**（≤30min lazy cadence）；永离线残余 = 轮换服务器凭据（唯一根治）；**fail-closed 代价登记**：pinned 401 不区分 revoked/unknown——非攻击场景（服务端数据丢失/重建等）也会触发全体设备回连销毁（安全优先的取舍）；**失窃响应口径**：cache token 与该设备上的 project token 都要 revoke。
- **multi-machine.md** revoke 语义节：`cache-tokens revoke` 从「只断拉新」改为「断拉新 + 回连销毁本地 cache」；销毁清单（cache 侧三件）+ DEGRADED/manifest 语义 + quarantine 痕迹口径 + project token 口径与两 token 失窃处置。
- **backup-restore.md 运维注记（按事实）**：`ExportSnapshot` **不含 cache_tokens**（export.go 零处读该表）——从备份恢复后 cache_tokens 表**为空** → 所有设备下次回连拿 unknown 401 → **批量切断**（预期行为、非事故）；恢复流程含「逐设备重新发码 + enroll」步骤。**带外警示**：若用 raw-DB 文件直拷恢复（不走 import），cache_tokens 会连历史状态一起回滚——**已 revoked 的码可能复活**，此类操作后必须逐行审计 cache_tokens 并重发。
- **agent-access.md** 断连语义四层的第四层（离线 cache）改写；**README / concepts.md** cache 相关节核对。
- **CLI 输出提示**：`cache-tokens revoke` 成功输出附一行 `reminder: also revoke project tokens issued to that device if it may be compromised`。
- **compat-matrix.md**：纯增量行（revoke 语义增强；无新 env、无新响应头——B 已砍出本 plan）。

## 6. 测试矩阵

- **DoPull pinned-401 → 三件套销毁**（httptest TLS 自签 + pin 形态：先 pull 成功 → 服务端侧 revoke → 回连 401 → 断言 cache.bin 进 quarantine、auth.json/DEK 消失、manifest state=done degraded 空、返回 `ErrCacheQuarantined`）。
- **不触发面三断言**：明文 pull 401 不销毁；网络中断不销毁；非 401（如 500）不销毁。
- **reason 两态**：revoked 前缀命中 → `invalid cache token: revoked` + stderr 行含设备名；未知码 → unknown。
- **DEGRADED 汇报**：mock 关键步失败（DEK 文件置只读目录）→ 返回值与 stderr 含 DEGRADED + 步骤名 + §4 报文透传断言（manifest 可写时同记）。
- **manifest best-effort（rev3）**：quarantine 目录置不可写 → manifest 写失败仅日志、**三件套销毁照常完成**（DEK/auth/bin 断言）+ 返回值仍 DEGRADED 汇报关键步。
- **幂等重隔离**：cache.bin 已缺失（模拟重隔离/首 enroll 打错码）→ bin 步幂等成功、无假 DEGRADED。
- **manifest 中断态**：只写 started 不走完 → §4 报 interrupted 文案；**归因门槛（rev3）**：bin 在位 + DEK 删 + manifest 在 → interrupted 同族报文；bin 在位 + 解密失败 + **manifest 不在** → 通用 decrypt 错误（非 quarantine 归因）。
- **归因防误报**：隔离 → 重新 enroll（pull 成功）→ manifest 重置 → 手删 cache.bin → 报 missing 而非 quarantined。
- **lazy 哨兵传播 + 已隔离标记**：MaybeLazyPull 捕获 → stderr 日志、backoff 零变化；同进程后续边界不再自动 pull 断言（含 auth.json 残留对抗用例）。
- **quarantine 单份覆盖**；**e2e 全链**（pull → revoke → 回连销毁 → spawn 报 quarantined → 无 cred 不再自动 pull → 重新 enroll 恢复）。
- **回归锚**：正常 pull/load 路径零变化（既有 cache 测试全绿）；运行中会话不断。

## 7. 明确不做（scope 纪律）

- **B 时限快照（rev3 砍出，owner 2026-08-24 拍板）**：`SSHMGR_CACHE_MAX_OFFLINE` / watermark / `X-Sshmgr-Time` 服务器时间锚**整体回 backlog**——三轮评审实证完整正确性需「时间锚信任边界（仅 pinned 采信 + skew 校验 + 缺头 fail-closed）+ 跨进程写串行化 + 前向毒化自愈」三件配套，修复面连续膨胀；backlog 注记留复杂度预警，未来要做按本轮评审结论起手。
- **C epoch/serial 防回滚**（grilling 拍板砍）。
- **tunnels revoke 级联**（#15）；**audit CLI**（#16）。
- **强制断运行中会话**（spawn 边界生效与 revoke 懒语义一致）。
- **销毁自动续跑/事务回滚**（DEK-first 顺序已使任何崩溃点安全；中断残态由 spawn 报文暴露 + 人工 re-enroll 自愈——manifest 只做状态记录不做恢复执行器）。
- **销毁回滚/撤销命令**（quarantine 是痕迹不是恢复路径）。
- **auth 删除失败的跨 spawn 持久重试预算**（进程级标记已保证每 spawn ≤1 次；持久循环形态已登记 §3）。

## 8. 验收

- **自动化**：§6 全绿。
- **owner 手工复验（随本 plan，不等发版）**：真 NUC10 `cache-tokens revoke laptop` → 笔记本手动 `cache pull` → 观察销毁（cache 消失 + quarantine 目录 + manifest + 报文）→ spawn `mcp --cache` 报 quarantined → `cache-tokens add` 重新发码 enroll 恢复 → NUC10 侧 stderr 见 revoked 行。回写验收记录。
