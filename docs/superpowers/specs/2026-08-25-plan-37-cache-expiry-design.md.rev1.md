# Plan 37 设计：B 时限快照——离线到龄自废（SSHMGR_CACHE_MAX_OFFLINE）

> backlog P1 #3b（Plan 34 砍出项回归）。2026-08-25 grilling 已拍板、本文不重议的决策：**到龄销毁（复用 QuarantineCache，报文按 manifest.Reason 分派）**、**时间锚源 = 标准 HTTP `Date` 头（零 server 改动，本 plan 纯 client 侧）**、**单调时间锥合并进 `meta.PulledAt`（无独立 watermark 文件、无锁，load 路径零锚写入）**、**默认关 seam 开关**（Plan 34 grilling 既定）。三件配套按 Plan 34 R2/R3 评审结论起手（.xcheck/20260824-142036、163734 留底），两处对留底处方的主动偏离见 §6。本版 = rev1（第 1 轮外部评审后修订，2026-08-25）：吸收 8 条——锚 provenance（`ServerAnchored` 字段）、到龄销毁前 meta 复查、max ≥ 1h 下限、测试矩阵 +2 组、401 措辞收窄、两处文档联动与文案补齐、§5 自洽论证前提补全。

## 0. 目标与代码现状事实（全部 2026-08-25 于 12d0b47 核实）

1. **B 零落地**：`MAX_OFFLINE` / watermark / 服务器时间头全仓无匹配——Plan 34 rev4 把 B 整体砍出，A（切断失效）单独落地。
2. **锚现状 = 客户端钟、无来源标记**：`DoPull` 写 `cacheMeta{URL, PulledAt int64}`，`PulledAt = time.Now().Unix()`（clientops.go:375），`cacheMeta` 仅两字段（clientops.go:182-185）——正是 R2 codex#1 判定的回拨重锚敞口（回拨时钟 + 活码重拉 → 假时基）。
3. **Go 服务端逐响应自动携带标准 `Date` 头**（RFC 7231 IMF-fixdate，GMT，秒粒度；`net/http` 默认行为，handler 未预置时服务器写响应时补上）——包括未升级的旧 serve。**pinned TLS 连接上它不可注入/篡改**（端到端加密+SPKI pin，无中间人位）。
4. **A 机器齐全可直接复用**：`QuarantineCache(reason)`（quarantine.go——DEK-first 五步、幂等 absent-ok、manifest best-effort、DEGRADED 语义）；`QuarantineReport(loadErr)`（quarantine_report.go——三层归因 + manifest.ts > meta.pulled_at 时效闸）；`ErrCacheQuarantined` 哨兵 + `MaybeLazyPull` 进程级哨兵。注意其代码注释自述「跨进程互斥刻意缺席（rev4：以 401 = 权威触发为前提，幂等性把损害限制在汇报不精确）」——到龄触发不享有 401 的权威性，故本设计为销毁路径补「销毁前复查」（§3.3）而非依赖该旧裁决。
5. **报文单源**：`cache status`（cli/cache.go:120-124）与 `mcp --cache`（cli/mcp.go:59-63）都是 `LoadCacheSnapshot` 失败 → `QuarantineReport(err)` 分派文案——reason 分派落进 helper 后双调用方零改动受益。
6. **meta 是明文、独立于 load**：`cache.meta.json` 明文 JSON，现行 `LoadCacheSnapshot`（clientops.go:253）完全不读它（只 loadDEK → 读 bin → 解密 → 反序列化）；meta 唯一写者是 `DoPull`（原子整写，单响应自洽）。`QuarantineReport` 已在读它（pulled_at 时效闸）——load 侧读 meta 有先例、无新信任面。
7. **运行中会话语义**：`CacheReloader.Check` 的 bin-unchanged 路径不调 `LoadCacheSnapshot`（只试 `MaybeLazyPull`），reload-on-change 才调——load 侧新闸自动只在 spawn/边界/变更时生效，天然「运行中不断」。

## 1. 开关语义 `SSHMGR_CACHE_MAX_OFFLINE`

- Go duration 文法（如 `168h`）；**缺省 / 空 / `0` = 关**；**下限 = 1h（= §5 的 K）**：不可解析、为负、或 `< 1h` → fail-closed 拒绝（`LoadCacheSnapshot` 与 `DoPull` 都返回错误——spawn 失败、pull 拒绝、status 报错），冻结文案（单文本覆盖全部非法形态）：

  ```
  invalid SSHMGR_CACHE_MAX_OFFLINE %q: must be a Go duration >= 1h (e.g. 168h; unset/0 disables expiry)
  ```

  下限理由：K=1h 的时钟容差主导小 max 的龄期误差（`30m` 时误差 >100%），语义失效；下限取 K 本身使「误差 ≤ K ≤ max」恒有界。
- 读取点：`LoadCacheSnapshot` 与 `DoPull` **每次调用即时解析**（client 侧无长驻进程，env seam 即时生效——`SSHMGR_BG_*` 同款纪律）。解析逻辑收敛为一个 helper（`cacheMaxOffline() (time.Duration, error)`）：unset/空/`0` → `(0, nil)`；合法且 ≥ 1h → `(d, nil)`；`< 1h` 或解析失败 → 错误（上列文案）。
- **B 关 = 现行为逐字节不动**（kimi#5-R2 钉死：默认关用户零新行为，包括回拨检查也不执行）。

## 2. pull 侧：信任锚闸（B 开时全部生效）

`DoPull` 入口先调 helper 解析 env——非法 → 直接返回该错误（不发起 HTTP）。B 开时：

1. **明文拒**（连接前）：`pin == ""`（含 `--allow-plaintext` 路径）→ 拒拉，冻结文案：

   ```
   SSHMGR_CACHE_MAX_OFFLINE is set: refusing plaintext pull — the time anchor requires a pinned TLS server (unset the cap or remove --allow-plaintext)
   ```

2. **Date 解析**（200 成功分支、写盘之前）：`res.Header.Get("Date")` 缺头或 `http.ParseTime` 失败 → 拒拉（fail-closed），冻结文案：

   ```
   pull succeeded but the response has no valid Date header — refusing to anchor cache time (SSHMGR_CACHE_MAX_OFFLINE requires a trusted server clock)
   ```

3. **skew 闸**：`|serverTime − clientNow| > 1h` → 拒拉，冻结文案（两头时刻 RFC3339）：

   ```
   server clock skew too large (server %s vs local %s, cap 1h) — refusing pull: SSHMGR_CACHE_MAX_OFFLINE depends on an accurate clock; fix system time sync
   ```

4. **锚写入（带 provenance）**：`cacheMeta{URL, PulledAt: serverTime.Unix(), ServerAnchored: true}`（B 开，四闸全过才写）。**B 关时维持现行 `time.Now().Unix()` 且 `ServerAnchored: false`**。`cacheMeta` 增加字段 `ServerAnchored bool \`json:"server_anchored,omitempty"\``（旧 meta 无此字段 → 零值 false——正是 provenance 语义所需）。`PulledAt` 字段注释同步改为「拉取时刻：服务器钟锚定（ServerAnchored=true）或本地钟（false）」。
- **401 quarantine 分支**：env 合法且请求已进入 pinned TLS 通道后的 401 行为不变（A 语义原样）。B 诱导的**前置**拦截（env 预检、明文拒）发生在 HTTP 之前——被拦截的请求根本到不了 401 分支，这不是「401 分支受影响」，是请求未发出。
- 非法 env 在 `DoPull` 也拒——与 load 侧同 fail-closed 纪律，不允许「拉得动但载不动」的半开状态。

## 3. load 侧：三闸 + 到龄销毁（解密前；meta 是明文，无需 DEK）

`LoadCacheSnapshot` 顶部（`CachePaths` 之后、`loadDEK` 之前）：

1. helper 解析 env——非法 → 返回错误（§1 文案）。关 → 现行路径零变化。
2. **meta 读**：读 `cache.meta.json`；**缺失或解析失败 → 拒不销毁**（无证据不销毁；含「尚未 pull 的首跑」情形——全新机器没有 meta 是正常态，不是损坏），冻结文案：

   ```
   SSHMGR_CACHE_MAX_OFFLINE is set but cache.meta.json is missing or corrupt (or this machine never pulled) — refusing cache (no time anchor); run cache pull
   ```

3. **provenance 闸**：`meta.ServerAnchored != true` → **拒不销毁**（B 关期间拉的、或 Plan 37 之前旧版拉的——客户端钟锚未过任何闸，不得当服务器锚信任），冻结文案：

   ```
   cache.meta.json has no server-anchored time (pulled while SSHMGR_CACHE_MAX_OFFLINE was unset or by an older client) — refusing cache; run cache pull to establish a server time anchor
   ```

   迁移语义：从关到开（或从旧版升级）后第一次 load 即被此闸拦下 → 一次 re-pull 建立服务器锚，之后照常。
4. **超龄闸**：`clientNow − meta.PulledAt > max` → **销毁前复查再销毁**：
   - **复查（re-read meta）**：紧邻 `QuarantineCache` 之前重读 `cache.meta.json`——若 `PulledAt` 已较判龄时读到的值前进且 `ServerAnchored == true`（并发 `DoPull` 刚落盘新锚），**中止销毁**，按新 meta 重走 §3.3-3.5 各闸（通常通过）；未变 → 照常销毁。
   - 销毁：调 `QuarantineCache`（reason 见 §4），返回错误：

     ```
     cache snapshot expired (offline %s > cap %s) — snapshot destroyed; run cache pull to re-enroll
     ```

   销毁 DEGRADED 时（`QuarantineCache` 返回非 nil 错误）该错误包裹之（沿用 A 的 `ErrCacheQuarantined` 可观测性）；干净销毁返回裸文本。
   - **残差如实登记**：复查通过到销毁步骤之间仍有毫秒级窗口——交错后果 = 误隔离一次刚刷新的健康缓存，re-pull 自愈，fail-closed 方向、无安全损失。复查不依赖互斥锁，靠「锚只由信任 pull 单写者前进」的单调事实收窄窗口。
5. **回拨闸**：`clientNow < meta.PulledAt − 1h` → **拒不销毁**（无正向到龄证据；钟事故与钟欺诈同观感，拒载最稳——下次信任 pull 重写 meta 即愈），冻结文案：

   ```
   system clock is behind the snapshot's server time anchor (local %s, anchor %s, tolerance 1h) — refusing cache (clock fault or tampering); a fresh cache pull re-anchors
   ```

6. 各闸通过 → 现行解密路径（loadDEK → bin → decrypt → unmarshal）零变化。

**触发面 = spawn/load 边界**（`mcp --cache` 启动、`cache status`、reload-on-change）。运行中会话不断：in-session 的 bin-unchanged 路径不触发闸（§0.7）；reload-on-change 遇到龄 → 销毁发生、`Check` 返回错误、holder 继续服务旧 store 至进程退出——与 A 的懒语义一致。

## 4. 报文与 reason 分派

- 到龄销毁的 manifest reason 冻结为常量：

  ```
  snapshot expired (offline beyond SSHMGR_CACHE_MAX_OFFLINE)
  ```

- `QuarantineReport` 按 `manifest.Reason` **精确等值分派**（reason 全部出自本仓两处常量，不做子串匹配——旧 manifest 的 `"server rejected device code"` 精确落在现行文案上，回归零漂移）：
  - reason == 到龄常量 → 新文案（三变体逐字冻结）：
    - done：`cache expired: offline beyond SSHMGR_CACHE_MAX_OFFLINE — snapshot destroyed; run cache pull (the device code is still valid unless revoked)`
    - done+DEGRADED：`cache expired: offline beyond SSHMGR_CACHE_MAX_OFFLINE [DEGRADED: %v] — snapshot destroyed; run cache pull (the device code is still valid unless revoked)`
    - started：`cache expiry destruction was interrupted — the snapshot may still exist; re-enroll via cache pull, or inspect quarantine/manifest.json`
  - 其余 → **现行文案逐字不动**（`cache quarantined by server rejection (token revoked?) ...`）。

## 5. 冻结常数 K = 1h（双职，自洽论证）

skew 闸（§2.3）与回拨容差（§3.5）**同一常数 K=1h**。自洽性（rev1 补全前提）：load 只对 `ServerAnchored == true` 的锚执行回拨闸（§3.3 provenance 闸先拦下一切本地锚），而服务器锚的写入必经 pull 侧四闸（pinned + Date + skew ≤ K）——故 pull 后紧随的 load 必然 `clientNow ≥ PulledAt − K`，**过 pull 闸的钟差绝不可能立刻触发回拨闸**（误杀为零的设计保证，前提 = provenance 闸维持「锚恒为服务器锚」不变量）。残差如实登记：到龄判定的龄期误差 ≤ K + 离线期 RTC 漂移（ppm 级）；§1 下限 `max ≥ K` 使误差恒 ≤ max（正常配置 168h 时 0.6%）。死 CMOS（年级偏差）在 pull 即被闸住 → 拒拉直到修钟（fail-closed by design，文档明示）。健康 NTP 机器实测钟差秒级，闸形同虚设——它防的是错钟事故与锚毒化，不防已出范围的控钟对手（§7）。

## 6. 三件配套对照（R3 留底处方 → 本设计落法）

| 配套（R2/R3 结论） | 本设计落法 | 偏离与论证 |
|---|---|---|
| 服务器时间锚信任边界：仅 pinned 采信 + skew 校验 + 缺头 fail-closed | §2 全落；锚源 = 标准 `Date` 头 | **偏离①**：R2 提议 `X-Sshmgr-Time` 自定义头。改用 Date：零 server 改动（本 plan 纯 client）、混布窗口不存在（旧 Go serve 恒带 Date——R3 codex#1 高危「未升级 server 无头」的攻击面直接消失）、信任强度等价（TLS pin 端到端，同一连接同一信任边界）。Date 的语义细节（发送时刻、GMT）对本用途无差（秒粒度 vs 小时级 max） |
| 跨进程写串行化：文件锁 / CAS / 版本化写 | watermark 并入 `meta.PulledAt`；**load 路径零锚写入**；RMW 竞态设计性消失 | **偏离②（rev1 收窄措辞）**：R3 codex#4 的处方默认保留「load 侧也写 watermark（max(now,旧值)）」才需要锁。删除该写者后只剩信任 pull 单写者，meta 原子整写自单响应、无 read-modify-write——**锚维护**的竞态不存在、锁不需要。到龄**销毁**确是破坏性写（与并发 pull 的交错理论存在）：本设计不引锁，以两件替代——①provenance 闸使销毁判定只信服务器锚；②销毁前 meta 复查（§3.4）把交错窗口缩到毫秒级，残余后果 = 误隔离一次健康缓存（re-pull 自愈、fail-closed 方向、无安全损失）如实登记。锁在此不保护任何锚不变量，属纯装饰 |
| 前向毒化自愈 | 采信口闸门（skew 拒未来值混入）+ 可重写锚 | 无偏离（形态更简）：万一未来值混入 meta（构造途径仅剩 FS 级伪造——已出范围），下次信任 pull 整文件重写 `PulledAt` 即愈，无需手工清 watermark 文件 |

## 7. 安全边界（诚实登记；R3 codex#5 收窄措辞）

- **B 只约束「仍按原配置运行、时钟基本诚实」的已安装客户端**。其兑现价值：真实长期离线（失窃后在包里/关机躺了几周）→ 到龄自废，凭据材料（DEK/设备码）物理消失；以及阻断「回拨时钟 + 活码重拉」的重锚路径（skew 闸 + 服务器锚 + provenance）。
- **控钟对手出范围**：能冻结/回拨客户端钟的攻击者可让龄期计算失真——与 FS 控制同级（后者可还原旧 bin+DEK+meta 组合绕 B，含 backup-restore 恢复路径），**根治仍只有轮换服务器凭据**（Plan 34 既定口径）。threat-model 残余清单按此登记。
- **运维后果如实登记**：错钟**前跳** > max（VM 快照恢复、手工校时、坏 NTP）同样触发销毁——恢复 = 联网 re-pull（§10 文档联动明示）；错钟偏移在 pull 侧被 skew 闸拦截为拒拉。
- 明文拓扑不开 B（§2.1 拒拉）——B 的信任链以 pinned TLS 为前提。

## 8. 测试矩阵

1. **B 关全默认零变化**（回归锚）：现行 pull/load/status 路径行为与文案逐字不变（含 meta 写入 `ServerAnchored: false`——字段新增对 B 关路径是纯增量序列化）。
2. **env 四态**：unset/`0` = 关；`168h` = 开；`abc` / `-1h` / `30m` → 拒（文案含原值，load 与 pull 双入口）。
3. **超龄销毁五断言**：bin 进 quarantine/、cache.auth.json 与 DEK 消失、manifest（state=done, reason=到龄常量）、meta 删除、load 返回到龄错误。
4. **回拨拒不销毁**：`clientNow < PulledAt − 1h`（伪造未来 PulledAt）→ 拒载文案；bin/auth/DEK 原位。
5. **龄内正常载**：`PulledAt = now − max/2`（ServerAnchored=true）→ 正常加载。
6. **pull 锚两态**：httptest TLS pinned server 可控 Date——B 开 → `meta.PulledAt` = Date 秒 + `ServerAnchored: true`；B 关 → client now + false。
7. **skew 超限拒拉**：Date 偏 ±2h → 拒拉（文案含两头时刻）、cache 不动。
8. **缺 Date 拒拉**：handler 置 `w.Header()["Date"] = nil` 压制 → B 开拒拉；B 关照拉。
9. **B 开明文拒**：`--allow-plaintext` + B 开 → §2.1 文案，不发 HTTP。
10. **毒化自愈**：手工写未来值 meta（ServerAnchored=true）→ 回拨拒 → 成功信任 pull → meta 重写 → 正常载。
11. **meta 缺失/坏 + B 开** → §3.2 文案，拒不销毁。
12. **报文 reason 分派**：到龄 manifest → 到龄文案（含 DEGRADED/started 变体）；server-rejection manifest → 现行文案逐字（回归钉）。
13. **并发 load 零锚写入**：两个 goroutine 并发 `LoadCacheSnapshot` 后 meta 内容/字节不变（锚维护竞态消解的行为学证据）。
14. **到龄 mid-session 不断**：已水合 store 在 bin 过龄后继续服务（holder 回归，reload-on-change 路径返回错误但旧 store 存活）。
15. **provenance 迁移**：B 关 pull（ServerAnchored=false）→ B 开 load → §3.3 拒载文案 + 不销毁；随后 B 开 pull → 正常载；旧版形态 meta（无 server_anchored 字段）→ 同拒载。
16. **销毁-vs-pull 竞态（编排确定性）**：判龄读到旧 meta（ServerAnchored=true、超龄）→ 并发 `DoPull` 落盘新锚（PulledAt 前进 + ServerAnchored=true）→ 销毁前复查发现 → 中止销毁、按新锚通过、bin/auth/DEK 原位未隔离。

## 9. 明确不做（scope 纪律）

- **server 端任何改动**（含 `X-Sshmgr-Time` 头）。
- **独立 watermark 文件与跨进程锁原语**（§6 偏离②：复查 + provenance 替代）。
- **回拨/provenance 缺失触发销毁**（只拒载——销毁仅限「正向到龄证据 + 复查确认」一条路）。
- **mid-session 强断**（spawn 边界语义，与 A 一致）。
- **服务端下发 max-age**（Plan 34 既定：纯 client seam）。
- **C epoch/serial 防回滚**（维持 Plan 34 砍除决议）。
- **跨端审计聚合**（backlog 既定不做）。

## 10. 文档联动

- **threat-model.md**：B 语义登记（到龄自废 + 重锚阻断）+ §7 收窄措辞 + FS/控钟/backup-restore 残余清单。
- **multi-machine.md**：B 配置节——笔记本侧 `SSHMGR_CACHE_MAX_OFFLINE=168h` 用例 + 两端时钟同步（NTP）前提 + **错钟双后果**（偏移被 skew 闸拦为拒拉；**前跳 > max 触发销毁**，恢复 = 联网 re-pull）。
- **compat-matrix.md**：纯增量行（新 env `SSHMGR_CACHE_MAX_OFFLINE`，默认关零行为变化；meta 新增 `server_anchored` 字段——旧客户端读新 meta 忽略未知字段，新客户端读旧 meta 视为 false，双向兼容）。
- **README.md**：env 表加行（语义一句话 + 指向 multi-machine 详节）。

## 11. 任务骨架（SDD 3 任务）

- **T1 pull 侧**：`cacheMaxOffline` helper（含 ≥1h 下限）+ `DoPull` 四闸（env 预检/明文拒/Date+skew/锚写入带 provenance）+ 测试矩阵 2/6/7/8/9。
- **T2 load 侧**：`LoadCacheSnapshot` 三闸 + 销毁前复查 + 到龄销毁接线 + `QuarantineReport` reason 分派 + 测试矩阵 1/3/4/5/10/11/12/13/14/15/16。
- **T3 文档 + 收尾**：§10 四处文档 + 全量回归 + 验收清单核对。

## 12. 验收

- §8 全绿（16 组）；全仓 `go test ./...` 绿；gofmt 净。
- **owner 手工复验（真机，随发版窗口）**：笔记本 `SSHMGR_CACHE_MAX_OFFLINE=1h` + 正常 pull → 断网 1h+ → spawn 报到龄销毁 → 重新 pull 恢复；NUC10 侧无任何感知（serve 零改动自证）。
