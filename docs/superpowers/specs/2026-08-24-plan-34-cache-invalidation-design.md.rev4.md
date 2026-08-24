# Plan 34 设计：离线 cache 切断失效（pinned-401 隔离）

> backlog P1 #3。2026-08-24 grilling 已拍板的决策不在本文重议：**A 切断失效必做**（永离线残余威胁文档登记，唯一根治 = 轮换服务器凭据——backlog 原文口径）、**销毁语义 = cache.bin 隔离 + 设备码/DEK 物理删除**、**服务端 401 加 reason 字段**（revoked/unknown，纯可观测性，客户端判定不依赖）、**验收 = 自动化 e2e + owner 手工复验**（随本 plan，不等发版）。本文为实现设计。
> 本版为第五版（定稿）。二版吸收首轮 9 项；三版吸收二轮 10 项；四版 scope 降级（owner 拍板 B 时限机器砍出回 backlog，附三件配套复杂度预警）+ 吸收三审 A 修复；**本版吸收四轮 7 项（零高、零新机制，全部边界钉死）**：换码打错场景登记 + CLI 写码时序钉死、manifest 三级降级链、归因时间约束（重置崩溃安全）、keyring fake 三态测试、quarantine 同目录钉死、cache.meta.json 入销毁清单、并发互斥接受登记。终态：收敛（4 轮修订含 scope 降级，owner 裁决书面级修订免复审）。

## 0. 目标与缺口

### 0.1 现状事实链（代码实证，2026-08-24）

1. **project token revoke 已闭环，零新工作**：`SnapshotProject.Status` 随快照携带（export.go:71），离线水合用 `VerifyToken`（status='active' 过滤，store 层）——owner 吊销 project token 后，笔记本回连 ≤30min（lazy pull cadence）刷快照，离线态随之拒绝。
2. **真缺口 = cache-token（设备码）revoke**：`RevokeCacheToken` 只断「拉新」（VerifyCacheToken 过滤 status='active' → 下次 pull 401）；盘上——`cache.bin` 密文 + **本地 DEK**（cache-dek.key / keyring slot）+ `cache.auth.json`（设备码明文）+ `cache.meta.json`——永续可用。lazy pull 遇 401 现状**静默失败**，revoke 对已落盘快照零作用。
3. **信任锚现成**：pull 走 `pinningTransport`（TLS SPKI pin）——**pinned 连接上拿到 401 = 权威服务器在通过 pin 验证后明确拒绝**；明文 pull 的 401 不可信（HTTP 劫持可伪造——误删面）；网络错误/TLS 失败根本没到权威判定。
4. **根本事实（threat-model 诚实登记的边界）**：笔记本持有「密文 + 解密钥 + 二进制」三件，**永离线的失窃机没有任何服务端机制能远程废掉本地解密能力**。本 plan 把 revoke 的生效形态从「永不动」提级到「**回连即销毁**」；永离线根治仍只有轮换服务器凭据。

### 0.2 目标形态

revoke 设备码 → 笔记本下一次自动（≤30min lazy）或手动 pull → pinned 401 → **本地 cache 侧四件销毁**（§2）→ 此后 spawn 报明确错误、无凭据不再自动 pull → 重新 enroll（`cache pull` 新码）恢复。

## 1. 服务端：401 reason 字段（可观测性）

`ServeRunner.verifyCacheToken`（serve.go）：**active 验证未命中**（`VerifyCacheToken` 对 revoked/未知码均返回 `(nil, nil)`——cachetoken.go:58 的约定；**即 nil 分支就是回查入口**）后、返回 401 错误**之前**，按 token 明文的前缀回查 cache_tokens 的 revoked 行（`token_prefix` 明文前缀存库，可匹配）：

- 命中 revoked 行 → verifier 错误文本 `invalid cache token: revoked`
- 未命中 → `invalid cache token: unknown`
- serve stderr 各记一行（含 revoked 行 name——owner 排查「哪台设备刚被切断」）

**prefix 匹配是可观测性近似**：不同设备码可能共享前缀（碰撞时把 unknown 误标 revoked）——客户端判定不依赖 reason（下条），误标只影响 owner 日志读数，不做精确匹配（YAGNI）。

**客户端判定永不依赖 reason**：一切按「pinned 401 = 拒绝」处理（§3）——reason 字段可被降级/篡改也不改变销毁语义；reason 只进 owner 的日志与响应体。

## 2. 客户端销毁例程 `QuarantineCache(reason string) error`

clientops 新增。**执行顺序（DEK-first 钥匙先死；manifest 是 best-effort 记录不是前置闸）**——任何一步后崩溃，最坏状态 = 密文留在原地但 DEK 已亡（不可解），**crash-safe**；**不做自动续跑**（crash 残态由下次 spawn 的 §4 报文暴露 + re-enroll 自愈）：

0. **manifest 意图（best-effort）**：`os.MkdirAll(<cache目录>/quarantine/)`（**quarantine/ 恒为 cache.bin 所在目录的子目录（rev4 钉死）**——同卷保证 rename 原子性，跨卷必败）→ 原子写 `quarantine/manifest.json` = `{state:"started", reason, ts}`。**写失败（目录建不了/磁盘满/权限）只记日志并继续**——manifest 的价值是跨进程持久化汇报（§4），**绝不构成销毁的前置条件**。
1. **DEK 物理删除**：Windows `cache-dek.key` 文件（`SSHMGR_CACHE_DEK` seam 路径优先）；Unix keyring slot（`SSHMGR_KEYRING_SERVICE` 体系下 cache-dek slot）。
2. `cache.auth.json` **物理删除**（设备码明文，零容忍）。
3. `cache.bin` → `quarantine/cache.bin.quarantined-<unix秒>`（rename；**单份保留**——新隔离先删旧文件）。
4. `cache.meta.json` **物理删除**（rev4：与 pull 落盘的 cache.bin+meta 两件对齐，销毁清单不留无害残留口径缺口）。
5. **manifest 完成（best-effort）**：更新为 `{state:"done", steps:{dek,auth,bin,meta}, degraded:<失败步骤清单或空>, reason, ts}`；写失败同样只记日志。
6. stderr 一行：`cache QUARANTINED by server rejection (<reason>): snapshot isolated to quarantine/, device code + DEK deleted — re-enroll with a fresh device code`。

**关键步与 DEGRADED（钉死）**：①DEK / ②auth / ③bin rename **全部是关键步**（meta 删除为非关键步，失败仅日志）——任一关键步删除/移动**出错** → 返回值、stderr 与 manifest（若可写）记 **DEGRADED + 失败步骤清单**。**幂等例外**：目标**已不存在**（文件缺失 / keyring slot 不存在）= 该步**幂等成功**，不计失败。单步失败记日志并继续其余步骤（尽力而为），但**如实汇报**。

**quarantine 保留物的口径**：DEK 已删 → 隔离目录里的 cache.bin 密文**不可解密**——保留价值是**痕迹/审计**，**不是数据恢复路径**；误隔离的恢复 = 设备码仍活则重新 `cache pull` 即全量重建。

**跨进程互斥（rev4 接受登记）**：`QuarantineCache` **不加跨进程锁**——多 MCP 进程 lazy pull / 手动 pull 并发触发时，幂等语义已缓解破坏性（目标不存在=成功），最坏影响 = manifest 写入与单份文件替换交错导致**汇报可能不精确**（非销毁正确性）；lazy pull 自身进程内串行 + 30min TTL 使窗口极窄。文件锁等串行化超本 plan scope。

## 3. 触发点与不触发面（钉死）

**触发**（唯一路径）：`DoPull` 内 `pin != "" && res.StatusCode == 401` → 先 `QuarantineCache("server rejected device code")` → 返回哨兵 `ErrCacheQuarantined`（`errors.Is` 可判；wrap 携带服务端 reason 文本与 DEGRADED 标记仅作展示）。

- **触发不区分码的来源（rev4 登记预期形态）**：pinned 401 的权威性优先于便利——**换码时打错/用过期新码 = 销毁现有 cache + 用正确码重新 pull 恢复**，这是 fail-closed 的预期代价（可用旧码恢复则旧码仍 active，重 pull 即全量重建）。配套实现时序钉死：**CLI/DoPull 仅在 pull 成功后写 `cache.auth.json`**（任何失败含 401 不落盘新码）——错码不残留，下次 lazy pull 继续用盘上旧码。
- **lazy 路径**（`MaybeLazyPull`，spawn + 每次 tool-call 边界）：捕获哨兵 → stderr 日志、**不进 backoff、不计失败窗口**；并置**进程级「已隔离」标记**——本进程后续边界不再自动尝试 pull（auth 残留也只在**下个 spawn** 重试一次——每 spawn ≤1 次销毁尝试）。**残余形态登记**：持久性删除失败下每 spawn 一次 401+销毁+DEGRADED 循环直至人工介入——预期形态非异常。
- **手动 `cache pull`**：CLI 捕获哨兵 → 明确文案（`cache was QUARANTINED: the server rejected this device code (revoked?). Re-enroll: obtain a fresh device code and run cache pull again.`）。
- **明文 pull**（`pin == ""`，含 `--allow-plaintext`）的 401：**永不触发**。**网络错误 / TLS 失败 / 非 401 状态码**：**永不触发**。
- **project token 不在销毁清单（口径钉死）**：销毁目标 = **cache 侧四件**（cache.bin / cache.auth.json / DEK / cache.meta.json）。`.claude.json` 里的 project token 是用户自己的 agent 配置——客户端程序不改写用户配置文件；其失效路径 = project token revoke。**失窃响应口径**：revoke cache token ≠ 切断该设备上的 project token——**失窃处置 = 两个都 revoke**（文档与 `cache-tokens revoke` 的 CLI 输出均提示此点）。

**运行中会话不断**（与 revoke 懒语义一致）：已水合 store 在内存继续服务至进程退出；隔离在 **spawn 边界**生效。

## 4. spawn / status 报文（manifest 驱动 + 三级降级链 + 时间约束）

`LoadCacheSnapshot` 报 cache.bin 缺失（或解密失败）时，上层（`mcp --cache` 启动与 `cache status`）按**三级降级链**判定（helper 收敛，两处调用零漂移）：

1. **manifest 可读**（`quarantine/manifest.json` 存在且可解析）且 **`manifest.ts > cache.meta.json 的 PulledAt`（rev4 时间约束——meta 在每次成功 pull 后更新，旧 manifest 自然失效；meta 缺失时保守跳过本级）**：
   - `state=done` + `degraded` 非空 → `cache quarantined by server rejection (token revoked?) [DEGRADED: <失败步骤>] — re-enroll via cache pull with a fresh device code; manual cleanup may be needed`
   - `state=done` → `cache quarantined by server rejection (token revoked?) — re-enroll via cache pull with a fresh device code`
   - `state=started` → `cache quarantine was interrupted — the snapshot may still exist; re-enroll via cache pull, or inspect quarantine/manifest.json`
2. **manifest 不可读但 `quarantine/` 目录存在**（rev4 降级级：manifest 写失败而销毁已发生）→ 无细节归因：`cache was quarantined (details unavailable — quarantine/manifest.json missing); re-enroll via cache pull`。
3. **目录也不存在** → 维持现有 missing-cache / 通用 decrypt 错误文本（不做 quarantine 归因）。

**cache.bin 在位但解密失败**：manifest 存在且过时间约束 → interrupted-quarantine 同族报文；否则**通用 decrypt 错误**（keyring 暂不可用/路径配置错/无关损坏不做 quarantine 归因，防误报）。

**归因防误报与重置**：成功 pull 落盘时删除/标记 `superseded` manifest；**即使重置崩溃/失败残留旧 manifest，§4.1 的时间约束（manifest.ts > meta.PulledAt）也使其自动失效**（rev4 崩溃安全——旧 manifest 的时间戳早于新 pull 的 meta，永不误报）。

## 5. 文档联动

- **threat-model.md** (b) 切断失效条目改写：复合前提（失窃 + 已 revoke）的兑现路径 = **revoke + 回连即销毁**（≤30min lazy cadence）；永离线残余 = 轮换服务器凭据（唯一根治）；**fail-closed 代价登记**：pinned 401 不区分 revoked/unknown 且不区分新码打错——非攻击场景（服务端数据丢失/重建、换码手滑）也触发销毁（安全优先取舍，恢复 = 正确码重 pull）；**失窃响应口径**：cache token 与该设备上的 project token 都要 revoke。
- **multi-machine.md** revoke 语义节：`cache-tokens revoke` 从「只断拉新」改为「断拉新 + 回连销毁本地 cache」；销毁清单（cache 侧四件）+ DEGRADED/manifest 语义 + quarantine 痕迹口径 + 换码打错预期形态 + 两 token 失窃处置。
- **backup-restore.md 运维注记（按事实）**：`ExportSnapshot` **不含 cache_tokens**（export.go 零处读该表）——从备份恢复后 cache_tokens 表**为空** → 所有设备下次回连拿 unknown 401 → **批量切断**（预期行为、非事故）；恢复流程含「逐设备重新发码 + enroll」步骤。**带外警示**：raw-DB 文件直拷恢复会使 cache_tokens 连历史状态回滚——**已 revoked 码可能复活**，此后必须逐行审计并重发。
- **agent-access.md** 断连语义四层的第四层（离线 cache）改写；**README / concepts.md** cache 相关节核对。
- **CLI 输出提示**：`cache-tokens revoke` 成功输出附一行 `reminder: also revoke project tokens issued to that device if it may be compromised`。
- **compat-matrix.md**：纯增量行（revoke 语义增强；无新 env、无新响应头）。

## 6. 测试矩阵

- **DoPull pinned-401 → 四件销毁**（httptest TLS 自签 + pin 形态：先 pull 成功 → 服务端侧 revoke → 回连 401 → 断言 cache.bin 进 quarantine、auth.json/DEK/meta.json 消失、manifest state=done degraded 空、返回 `ErrCacheQuarantined`）。
- **不触发面三断言**：明文 pull 401 不销毁；网络中断不销毁；非 401（如 500）不销毁。
- **reason 两态**：revoked 前缀命中 → `invalid cache token: revoked` + stderr 行含设备名；未知码 → unknown。
- **CLI 写码时序（rev4）**：pull 401 后断言 `cache.auth.json` **未被新码覆写**（盘上旧码原样）；成功后才写。
- **DEGRADED 汇报（两平台形态）**：Windows 路径 mock（DEK 文件置只读目录）→ DEGRADED + 步骤名 + §4 透传；**Unix keyring fake 三态（rev4，go-keyring MockInit 仓内先例）**：slot 删除失败 → DEGRADED；slot 不存在 → 幂等成功；provider 不可用 → DEGRADED 且其余步骤照跑。
- **manifest best-effort**：quarantine 目录置不可写 → manifest 写失败仅日志、**四件销毁照常完成** + 返回值仍汇报；§4 落到降级级 2（目录存在无 manifest）报文断言。
- **幂等重隔离**：cache.bin 已缺失 → bin 步幂等成功、无假 DEGRADED。
- **归因时间约束（rev4）**：伪造旧 manifest（ts 早于 meta.PulledAt）+ bin 缺失 → 报 missing 而非 quarantined（重置崩溃安全）。
- **manifest 中断态**：只写 started → interrupted 报文；bin 在位 + 解密失败 + manifest 过时间约束 → 同族报文；无 manifest → 通用 decrypt 错误。
- **归因防误报**：隔离 → 重新 enroll → 手删 cache.bin → 报 missing。
- **lazy 哨兵传播 + 已隔离标记**：stderr 日志、backoff 零变化、同进程后续边界零尝试（含 auth 残留对抗用例）。
- **quarantine 单份覆盖**；**e2e 全链**（pull → revoke → 回连销毁 → spawn 报 quarantined → 无 cred 不再自动 pull → 重新 enroll 恢复）。
- **回归锚**：正常 pull/load 零变化；运行中会话不断。

## 7. 明确不做（scope 纪律）

- **B 时限快照（砍出，owner 2026-08-24 拍板）**：`SSHMGR_CACHE_MAX_OFFLINE` / watermark / `X-Sshmgr-Time` 服务器时间锚**整体回 backlog**——完整正确性需「时间锚信任边界（仅 pinned 采信 + skew 校验 + 缺头 fail-closed）+ 跨进程写串行化 + 前向毒化自愈」三件配套（三轮评审实证）；backlog 注记留复杂度预警。
- **C epoch/serial 防回滚**（grilling 拍板砍）。
- **tunnels revoke 级联**（#15）；**audit CLI**（#16）。
- **强制断运行中会话**（spawn 边界生效与 revoke 懒语义一致）。
- **销毁自动续跑/事务回滚**（DEK-first 已使任何崩溃点安全；manifest 只做记录）。
- **QuarantineCache 跨进程文件锁**（幂等已缓解破坏性；互斥只影响汇报精度，窗口极窄——rev4 接受登记）。
- **销毁回滚/撤销命令**；**auth 删除失败的跨 spawn 持久重试预算**。

## 8. 验收

- **自动化**：§6 全绿。
- **owner 手工复验（随本 plan，不等发版）**：真 NUC10 `cache-tokens revoke laptop` → 笔记本手动 `cache pull` → 观察销毁（cache 消失 + quarantine 目录 + manifest + 报文）→ spawn `mcp --cache` 报 quarantined → `cache-tokens add` 重新发码 enroll 恢复 → NUC10 侧 stderr 见 revoked 行。回写验收记录。
