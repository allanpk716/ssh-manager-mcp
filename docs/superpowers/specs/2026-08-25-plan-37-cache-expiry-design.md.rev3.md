# Plan 37 设计：B 时限快照——离线到龄自废（SSHMGR_CACHE_MAX_OFFLINE）

> backlog P1 #3b（Plan 34 砍出项回归）。2026-08-25 grilling 已拍板、本文不重议的决策：**到龄销毁（复用 QuarantineCache，报文按 manifest.Reason 分派）**、**时间锚源 = 标准 HTTP `Date` 头（零 server 改动，本 plan 纯 client 侧）**、**单调时间锥合并进 `meta.PulledAt`（无独立 watermark 文件、无锁，load 路径零锚写入）**、**默认关 seam 开关**（Plan 34 grilling 既定）。三件配套按 Plan 34 R2/R3 评审结论起手（.xcheck/20260824-142036、163734 留底），两处对留底处方的主动偏离见 §6。本版 = rev3（定稿；三轮外部评审收敛——R1 机制级 → R2 绑定级 → R3 措辞级单调坍缩，owner 裁决书面级修订免复审）。相对 rev2 的 9 条修订全为措辞/一致性/登记级：§7 承诺收窄（无定时执行器，no-trigger 残余登记）、到龄错误恒裹哨兵、meta 写失败残差改双向有界 ≤2K、`server_anchored` 去 omitempty 恒序列化、矩阵 18 归位 T2、复查失败独立文案、§0.4 点明五步含 meta 删除、销毁后报文路径登记。

## 0. 目标与代码现状事实（全部 2026-08-25 于 12d0b47 核实）

1. **B 零落地**：`MAX_OFFLINE` / watermark / 服务器时间头全仓无匹配——Plan 34 rev4 把 B 整体砍出，A（切断失效）单独落地。
2. **锚现状 = 客户端钟、无来源标记**：`DoPull` 写 `cacheMeta{URL, PulledAt int64}`，`PulledAt = time.Now().Unix()`（clientops.go:375），`cacheMeta` 仅两字段（clientops.go:182-185）——正是 R2 codex#1 判定的回拨重锚敞口（回拨时钟 + 活码重拉 → 假时基）。**现行落盘顺序 = bin 原子写成功 → meta 原子写**（clientops.go:367 → 375）。
3. **Go 服务端逐响应自动携带标准 `Date` 头**（RFC 7231 IMF-fixdate，GMT，秒粒度；`net/http` 默认行为，handler 未预置时服务器写响应时补上）——包括未升级的旧 serve。**pinned TLS 连接上它不可注入/篡改**（端到端加密+SPKI pin，无中间人位）。
4. **A 机器齐全可直接复用**：`QuarantineCache(reason)`（quarantine.go——**六步**：0 manifest intent（best-effort）→ 1 DEK 删除 → 2 cache.auth.json 删除 → 3 bin rename 进 quarantine/ → **4 cache.meta.json 删除（非关键步，失败不 degrade）** → 5 manifest done；DEK-first、幂等 absent-ok、DEGRADED 语义）；`QuarantineReport(loadErr)`（quarantine_report.go——三层归因 + manifest.ts > meta.pulled_at 时效闸）；`ErrCacheQuarantined` 哨兵 + `MaybeLazyPull` 进程级哨兵。注意其代码注释自述「跨进程互斥刻意缺席（rev4：以 401 = 权威触发为前提，幂等性把损害限制在汇报不精确）」——到龄触发不享有 401 的权威性，故本设计为销毁路径补「销毁前复查」（§3.4）而非依赖该旧裁决。
5. **报文单源**：`cache status`（cli/cache.go:120-124）与 `mcp --cache`（cli/mcp.go:59-63）都是 `LoadCacheSnapshot` 失败 → `QuarantineReport(err)` 分派文案——reason 分派落进 helper 后双调用方零改动受益。
6. **meta 是明文、独立于 load**：`cache.meta.json` 明文 JSON，现行 `LoadCacheSnapshot`（clientops.go:253）完全不读它（只 loadDEK → 读 bin → 解密 → 反序列化）；meta 唯一写者是 `DoPull`（原子整写，单响应自洽）。`QuarantineReport` 已在读它（pulled_at 时效闸）——load 侧读 meta 有先例、无新信任面。
7. **运行中会话语义**：`CacheReloader.Check` 的 bin-unchanged 路径不调 `LoadCacheSnapshot`（只试 `MaybeLazyPull`），reload-on-change 才调——load 侧新闸自动只在 spawn/边界/变更时生效。
8. **重定向现状（复审计实验实证，2026-08-25）**：pinned client = `&http.Client{Transport: pinningTransport(...)}`（clientops.go:315），无 CheckRedirect 策略——Go 默认跟随 ≤10 跳。实验（httptest TLS 302 → 明文 http）：**DoPull 跟随并把未钉服务器的 200 响应体当 snapshot 写盘**。该洞与 B 无关（B 关现行即活，快照体可走明文 = 传输级泄露面）；对 B 更直接（Date 锚可被导出 pin 信任域）。

## 1. 开关语义 `SSHMGR_CACHE_MAX_OFFLINE`

- Go duration 文法（如 `168h`）；**缺省 / 空 / `0` = 关**；**下限 = 1h（= §5 的 K）**：不可解析、为负、或 `< 1h` → fail-closed 拒绝（`LoadCacheSnapshot` 与 `DoPull` 都返回错误——spawn 失败、pull 拒绝、status 报错），冻结文案（单文本覆盖全部非法形态）：

  ```
  invalid SSHMGR_CACHE_MAX_OFFLINE %q: must be a Go duration >= 1h (e.g. 168h; unset/0 disables expiry)
  ```

  下限语义定位：**1h 是测试/验收下限，不是生产建议值**。生产建议 **≥ 24h**——§5 的 K=1h 容差意味着 server 钟落后本地接近 1h 时，max=1h 的缓存刚拉即接近龄限（可用期趋零）；本设计**明确接受**该近零有效期语义（fail-closed 方向：宁可早销毁 re-pull，不宽限）。
- 读取点：`LoadCacheSnapshot` 与 `DoPull` **每次调用即时解析**（client 侧无长驻进程，env seam 即时生效——`SSHMGR_BG_*` 同款纪律）。解析逻辑收敛为一个 helper（`cacheMaxOffline() (time.Duration, error)`）：unset/空/`0` → `(0, nil)`；合法且 ≥ 1h → `(d, nil)`；`< 1h` 或解析失败 → 错误（上列文案）。
- **B 关 = 现行为不动，唯二例外**：① meta 序列化新增 `server_anchored` 字段（**恒序列化显式 `false`**——无 omitempty，纯增量，旧读端忽略未知字段）；② **全局禁重定向**（§2.0，安全修复——复审计实验实证 B 关现行即有明文泄露面，属修洞不属行为冻结）。

## 2. pull 侧：信任锚闸（B 开时 2.1-2.4 生效；2.0 全局生效）

`DoPull` 入口先调 helper 解析 env——非法 → 直接返回该错误（不发起 HTTP）。

0. **禁重定向（全局，B 开与 B 关都生效）**：pinned client 设 `CheckRedirect` 返回错误（不跟随任何 30x）——重定向响应按非 200 错误处理，冻结文案模板：

   ```
   pull: server returned %d (redirects are not followed)
   ```

   动机：复审计实验实证 pinned 302→明文 hop 会被跟随、响应体（与头，含 Date）取自未钉连接——B 关现行即活的传输级泄露面，必须全局封死；**响应体与时间锚只取「直接 200 于 pinned 连接」**。明文 pull（`--allow-plaintext`，B 关拓扑）同样不跟随——明文本就无信任可言，跟随只会更糟。
1. **明文拒**（B 开时，连接前）：`pin == ""`（含 `--allow-plaintext` 路径）→ 拒拉，冻结文案：

   ```
   SSHMGR_CACHE_MAX_OFFLINE is set: refusing plaintext pull — the time anchor requires a pinned TLS server (unset the cap or remove --allow-plaintext)
   ```

2. **Date 解析**（200 成功分支、写盘之前）：`res.Header.Get("Date")` 缺头**或存在但 `http.ParseTime` 解析失败** → 拒拉（fail-closed），冻结文案：

   ```
   pull succeeded but the response has no valid Date header — refusing to anchor cache time (SSHMGR_CACHE_MAX_OFFLINE requires a trusted server clock)
   ```

3. **skew 闸**：`|serverTime − clientNow| > 1h` → 拒拉，冻结文案（两头时刻 RFC3339）：

   ```
   server clock skew too large (server %s vs local %s, cap 1h) — refusing pull: SSHMGR_CACHE_MAX_OFFLINE depends on an accurate clock; fix system time sync
   ```

4. **锚写入（带 provenance；提交顺序钉死）**：`cacheMeta{URL, PulledAt: serverTime.Unix(), ServerAnchored: true}`（B 开，四闸全过才写）。**B 关时维持现行 `time.Now().Unix()` 且 `ServerAnchored: false`**。`cacheMeta` 增加字段 `ServerAnchored bool \`json:"server_anchored"\``（**无 omitempty——恒序列化**，false 显式出现在 B 关 pull 的 meta 里；旧 meta 无此字段 → 零值 false，正是 provenance 语义所需）。`PulledAt` 字段注释同步改为「拉取时刻：服务器钟锚定（ServerAnchored=true）或本地钟（false）」。
   **提交顺序（钉死，与现行代码一致并封死危险顺序）**：bin 原子写成功 → meta 原子写（**禁止 meta 先于 bin**）。崩溃/失败窗口分析：bin 写失败 → pull 失败、盘上无新锚（安全）；bin 成功 + meta 写失败/中途崩溃 → 新 bin + 旧锚 → **龄期按旧锚计算，误差双向有界 ≤ 2K = 2h**（旧锚与新 pull 的 Date 各经一次 skew 闸 ⇒ 互相偏差 ≤ 2K；服务器钟回修或并发交错可致高估或低估，不宣称恒 fail-closed 方向）——登记为残余而非不变量；「新锚 + 旧超龄 bin」的 fail-open 组合按提交顺序**不可达**。
- **401 quarantine 分支**：env 合法且请求已进入 pinned TLS 通道后的 401 行为不变（A 语义原样）。B 诱导的**前置**拦截（env 预检、明文拒）发生在 HTTP 之前——被拦截的请求根本到不了 401 分支，这不是「401 分支受影响」，是请求未发出。
- 非法 env 在 `DoPull` 也拒——与 load 侧同 fail-closed 纪律，不允许「拉得动但载不动」的半开状态。

## 3. load 侧：三闸 + 到龄销毁（解密前；meta 是明文，无需 DEK）

`LoadCacheSnapshot` 顶部（`CachePaths` 之后、`loadDEK` 之前）：

1. helper 解析 env——非法 → 返回错误（§1 文案）。关 → 现行路径零变化（§1 例外清单除外）。
2. **meta 读**：读 `cache.meta.json`；**缺失或解析失败 → 拒不销毁**（无证据不销毁；含「尚未 pull 的首跑」情形——全新机器没有 meta 是正常态，不是损坏），冻结文案：

   ```
   SSHMGR_CACHE_MAX_OFFLINE is set but cache.meta.json is missing or corrupt (or this machine never pulled) — refusing cache (no time anchor); run cache pull
   ```

3. **provenance 闸**：`meta.ServerAnchored != true` → **拒不销毁**（B 关期间拉的、或 Plan 37 之前旧版拉的——客户端钟锚未过任何闸，不得当服务器锚信任），冻结文案：

   ```
   cache.meta.json has no server-anchored time (pulled while SSHMGR_CACHE_MAX_OFFLINE was unset or by an older client) — refusing cache; run cache pull to establish a server time anchor
   ```

   迁移语义：从关到开（或从旧版升级）后第一次 load 即被此闸拦下 → 一次 re-pull 建立服务器锚，之后照常。**运维前提：启用 B 需在联网状态**——离线时开 B 会让缓存立即不可用且无法自愈（fail-closed 可接受，文档明示）。
4. **超龄闸**：`clientNow − meta.PulledAt > max` → **销毁前复查再销毁**：
   - **复查（re-read meta）**：紧邻 `QuarantineCache` 之前重读 `cache.meta.json`——若 `PulledAt` 已较判龄时读到的值前进且 `ServerAnchored == true`（并发 `DoPull` 刚落盘新锚），**中止销毁**，按新 meta 重走 §3.3-3.5 各闸（通常通过）；**复查读取/解析失败 → 拒不销毁**（销毁 = 「正向到龄证据 + 复查确认」双条件，确认无法完成即不满足），独立冻结文案（不复用 §3.2——那是「从未有锚」的语境）：

     ```
     cache expiry re-check failed (%v) — refusing cache; run cache pull
     ```

     未变 → 照常销毁。
   - 销毁：调 `QuarantineCache`（reason 见 §4），返回错误——**恒包裹 `ErrCacheQuarantined`**（干净与 DEGRADED 一致：`errors.Is` 调用方统一把到龄销毁识别为 quarantine 类；DEGRADED 步骤清单在文本内）：

     ```
     cache snapshot expired (offline %s > cap %s) — snapshot destroyed; run cache pull to re-enroll
     ```

   - **残差如实登记**：复查通过到销毁步骤之间仍有毫秒级窗口——交错后果 = 误隔离一次刚刷新的健康缓存，re-pull 自愈，无安全损失。复查不依赖互斥锁，靠「锚只由信任 pull 单写者前进」的单调事实收窄窗口。
5. **回拨闸**：`clientNow < meta.PulledAt − 1h` → **拒不销毁**（无正向到龄证据；钟事故与钟欺诈同观感，拒载最稳），冻结文案：

   ```
   system clock is behind the snapshot's server time anchor (local %s, anchor %s, tolerance 1h) — refusing cache (clock fault or tampering); fix system time, then run cache pull
   ```

6. 各闸通过 → 现行解密路径（loadDEK → bin → decrypt → unmarshal）零变化。

**触发面 = spawn/load 边界**（`mcp --cache` 启动、`cache status`、reload-on-change）。**B 无定时执行器**：超龄销毁只在上述边界发生——运行中 bin-unchanged 会话继续服务至进程退出；关机失窃的机器不运行本客户端，盘上材料保留至下次运行（no-trigger 残余，§7 登记）。reload-on-change 遇到龄 → 销毁发生、`Check` 返回错误、holder 继续服务旧 store 至进程退出——与 A 的懒语义一致。

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
- **销毁后报文路径（钉死）**：到龄销毁完成后（bin 已隔离、meta 已删），下一次 load 先撞 §3.2「meta 缺失」闸——上层 `QuarantineReport` 走 tier-1（meta 缺失 = 无 on-record，manifest 计 fresh）按 reason 命中**到龄文案**，不落「never pulled」误导。矩阵 3 以此断言。

## 5. 冻结常数 K = 1h（双职，自洽论证）

skew 闸（§2.3）与回拨容差（§3.5）**同一常数 K=1h**。自洽性：load 只对 `ServerAnchored == true` 的锚执行回拨闸（§3.3 provenance 闸先拦下一切本地锚），而服务器锚的写入必经 pull 侧四闸（pinned + 无重定向 + Date + skew ≤ K）——故 pull 后紧随的 load 必然 `clientNow ≥ PulledAt − K`，**过 pull 闸的钟差绝不可能立刻触发回拨闸**（误杀为零的设计保证，前提 = provenance 闸维持「锚恒为服务器锚」不变量）。残差如实登记：到龄判定的龄期误差 ≤ K + 离线期 RTC 漂移（ppm 级）；§1 下限 `max ≥ K` 使误差恒 ≤ max——但**有界 ≠ 可用期为正**：server 钟落后本地接近 K 时，max 取下限（1h）的缓存刚拉即近龄限（§1 已定位 1h 为测试值、生产建议 ≥24h 并明确接受该语义）。meta 写失败残差按 §2.4 为**双向 ≤ 2K**。死 CMOS（年级偏差）在 pull 即被闸住 → 拒拉直到修钟（fail-closed by design，文档明示）。健康 NTP 机器实测钟差秒级，闸形同虚设——它防的是错钟事故与锚毒化，不防已出范围的控钟对手（§7）。

## 6. 三件配套对照（R3 留底处方 → 本设计落法）

| 配套（R2/R3 结论） | 本设计落法 | 偏离与论证 |
|---|---|---|
| 服务器时间锚信任边界：仅 pinned 采信 + skew 校验 + 缺头 fail-closed | §2 全落；锚源 = 标准 `Date` 头；**无重定向**（§2.0，实验驱动） | **偏离①**：R2 提议 `X-Sshmgr-Time` 自定义头。改用 Date：零 server 改动（本 plan 纯 client）、混布窗口不存在（旧 Go serve 恒带 Date）、信任强度等价（TLS pin 端到端 + 禁重定向保证「最终响应 = pinned 直连响应」）。Date 的语义细节（发送时刻、GMT）对本用途无差（秒粒度 vs 小时级 max） |
| 跨进程写串行化：文件锁 / CAS / 版本化写 | watermark 并入 `meta.PulledAt`；**load 路径零锚写入**；RMW 竞态设计性消失；**锚与快照以提交顺序绑定** | **偏离②**：删除 load 侧锚写者后只剩信任 pull 单写者，meta 原子整写自单响应、无 read-modify-write——**锚维护**的竞态不存在、锁不需要。到龄**销毁**确是破坏性写：以三件替代锁——①provenance 闸（销毁判定只信服务器锚）；②销毁前 meta 复查（§3.4）把交错窗口缩到毫秒级，残余后果 = 误隔离一次健康缓存（re-pull 自愈、无安全损失）如实登记；③**提交顺序钉死 bin→meta**（§2.4）使「新锚 + 旧超龄 bin」的 fail-open 组合不可达。锁在此不保护任何锚不变量，属纯装饰 |
| 前向毒化自愈 | 采信口闸门（skew 拒未来值混入）+ 可重写锚 | 无偏离（形态更简）：万一未来值混入 meta（构造途径仅剩 FS 级伪造——已出范围），下次信任 pull 整文件重写 `PulledAt` 即愈，无需手工清 watermark 文件 |

## 7. 安全边界（诚实登记）

- **B 只约束「仍按原配置运行、时钟基本诚实」的已安装客户端**。兑现价值（措辞如实）：真实长期离线后**下次 load/spawn 边界自废**——凭据材料（DEK/设备码）在该边界物理消失；以及阻断「回拨时钟 + 活码重拉」的重锚路径（skew 闸 + 服务器锚 + provenance）。
- **no-trigger 与 running-session 残余（如实登记）**：B **无定时执行器**——关机失窃的机器不运行本客户端，盘上材料保留至下次运行；已运行的 bin-unchanged 会话继续服务至进程退出。threat-model / multi-machine 按此登记（补后台定时器 = 反 YAGNI，明确不做）。
- **回拨闸捕获面如实登记**：§3.5 回拨闸只捕获 **> 1h** 的回拨；**单次容差（1h）内回拨可静默延长有效期 ≤ 1h**；**反复回拨/冻结时钟可任意续命——属控钟对手**，与 FS 控制同级，明确出范围，读者不得据此高估回拨闸价值。
- **控钟对手出范围**：与 FS 控制同级（后者可还原旧 bin+DEK+meta 组合绕 B，含 backup-restore 恢复路径），**根治仍只有轮换服务器凭据**（Plan 34 既定口径）。threat-model 残余清单按此登记。
- **运维后果如实登记**：错钟**前跳** > max（VM 快照恢复、手工校时、坏 NTP）同样触发销毁——恢复 = 联网 re-pull（§10 文档联动明示）；错钟偏移在 pull 侧被 skew 闸拦截为拒拉。
- 明文拓扑不开 B（§2.1 拒拉）——B 的信任链以 pinned TLS 为前提。

## 8. 测试矩阵

1. **B 关全默认零变化**（回归锚）：现行 pull/load/status 路径行为与文案逐字不变（唯二例外 = §1：meta 恒多 `server_anchored: false` 字段；禁重定向全局生效）。
2. **env 四态**：unset/`0` = 关；`168h` = 开；`abc` / `-1h` / `30m` → 拒（文案含原值，load 与 pull 双入口）。
3. **超龄销毁六断言**：bin 进 quarantine/、cache.auth.json 与 DEK 消失、manifest（state=done, reason=到龄常量）、meta 删除、load 返回到龄错误（**断言 `errors.Is(err, ErrCacheQuarantined)`**）、**销毁后再次 load 的报文 = 到龄文案**（§4 路径）。
4. **回拨拒不销毁**：`clientNow < PulledAt − 1h`（伪造未来 PulledAt）→ 拒载文案（含 "fix system time" 尾句）；bin/auth/DEK 原位。
5. **龄内正常载**：`PulledAt = now − max/2`（ServerAnchored=true）→ 正常加载。
6. **pull 锚两态**：httptest TLS pinned server 可控 Date——B 开 → `meta.PulledAt` = Date 秒 + `ServerAnchored: true`；B 关 → client now + **显式 `server_anchored: false` 字段出现在 meta**。
7. **skew 超限拒拉**：Date 偏 ±2h → 拒拉（文案含两头时刻）、cache 不动。
8. **Date 两态坏形拒拉**：缺头（`w.Header()["Date"] = nil` 压制）与**存在但不可解析**（如 `Date: not-a-date`）→ B 开均拒拉；B 关照拉。
9. **B 开明文拒**：`--allow-plaintext` + B 开 → §2.1 文案，不发 HTTP。
10. **毒化自愈**：手工写未来值 meta（ServerAnchored=true）→ 回拨拒 → 成功信任 pull → meta 重写 → 正常载。
11. **meta 缺失/坏 + B 开** → §3.2 文案，拒不销毁。
12. **报文 reason 分派**：到龄 manifest → 到龄文案（含 DEGRADED/started 变体）；server-rejection manifest → 现行文案逐字（回归钉）。
13. **并发 load 零锚写入**：两个 goroutine 并发 `LoadCacheSnapshot` 后 meta 内容/字节不变（锚维护竞态消解的行为学证据）。
14. **到龄 mid-session 不断**：已水合 store 在 bin 过龄后继续服务（holder 回归，reload-on-change 路径返回错误但旧 store 存活）。
15. **provenance 迁移**：B 关 pull（`server_anchored: false` 显式）→ B 开 load → §3.3 拒载文案 + 不销毁；随后 B 开 pull → 正常载；旧版形态 meta（无 server_anchored 字段）→ 同拒载。
16. **销毁-vs-pull 竞态（编排确定性，双分支）**：经 T2 测试 seam（§11）在判龄/复查/销毁三挂点编排——分支 A：判龄读到旧 meta → 并发 `DoPull` 落盘新锚 → 复查发现 → **中止销毁**、bin/auth/DEK 原位；分支 B：复查通过后、销毁步骤前交错完成新 pull → 销毁照常发生（**残余窗口的行为学钉子**：断言后果 = 新缓存被隔离 + 可 re-pull 自愈）；**复查读失败注入**（seam）→ 独立文案拒载不销毁。
17. **重定向不变量（复用实验场景）**：pinned TLS server 302 → 明文 http → DoPull **拒拉**（`redirects are not followed` 文案）+ 明文 hop **零命中**；B 开与 B 关两态都断言（全局禁）。
18. **提交顺序（load 侧断言）**：meta 写失败注入（seam 或只读目录构造）→ pull 仍成功（现行 WARNING 语义）+ 盘上旧 meta 未更新 → 断言**龄期按旧锚计算**（不断言方向——双向有界 ≤2K 残余，§2.4）。

## 9. 明确不做（scope 纪律）

- **server 端任何改动**（含 `X-Sshmgr-Time` 头）。
- **独立 watermark 文件与跨进程锁原语**（§6 偏离②：复查 + provenance + 提交顺序绑定替代）。
- **回拨/provenance 缺失/复查读失败触发销毁**（只拒载——销毁仅限「正向到龄证据 + 复查确认」一条路）。
- **mid-session 强断与后台定时销毁器**（spawn 边界语义，与 A 一致；关机失窃 no-trigger 残余如实登记而非机制补偿）。
- **服务端下发 max-age**（Plan 34 既定：纯 client seam）。
- **C epoch/serial 防回滚**（维持 Plan 34 砍除决议）。
- **跨端审计聚合**（backlog 既定不做）。
- **重定向跟随的兼容开关**（不留「允许跟随」逃生门——30x 在本拓扑无正当用途）。

## 10. 文档联动

- **threat-model.md**：B 语义登记（**下次 load/spawn 边界自废** + 重锚阻断）+ §7 全部残余如实登记（no-trigger：关机失窃材料保留至下次运行；running-session；<1h 单次回拨 ≤1h 延长与反复/冻结属控钟；FS/backup-restore）+ 禁重定向的传输级修复登记。
- **multi-machine.md**：B 配置节——笔记本侧 `SSHMGR_CACHE_MAX_OFFLINE=168h` 用例 + 两端时钟同步（NTP）前提 + **错钟双后果**（偏移被 skew 闸拦为拒拉；**前跳 > max 触发销毁**，恢复 = 联网 re-pull）+ **首次启用需联网**（provenance 闸强制一次 re-pull 建立服务器锚）+ **生产建议 ≥24h**（1h 为测试下限，近零有效期语义）+ **销毁只在下次运行本客户端时发生**（关机失窃不自动擦盘）。
- **compat-matrix.md**：纯增量行（新 env `SSHMGR_CACHE_MAX_OFFLINE`，默认关；meta 新增 `server_anchored` 字段恒序列化——旧客户端读新 meta 忽略未知字段，新客户端读旧 meta（无字段）视为 false，双向兼容；pinned pull 不再跟随重定向——行为修复登记）。
- **README.md**：env 表加行（语义一句话 + 指向 multi-machine 详节）。

## 11. 任务骨架（SDD 3 任务）

- **T1 pull 侧**：`cacheMaxOffline` helper（含 ≥1h 下限）+ `DoPull` 五闸（env 预检/**禁重定向（全局）**/明文拒/Date+skew/锚写入带 provenance 与提交顺序）+ 测试矩阵 2/6/7/8/9/17。
- **T2 load 侧**：`LoadCacheSnapshot` 三闸 + 销毁前复查（含读失败支路）+ 到龄销毁接线（恒裹哨兵）+ `QuarantineReport` reason 分派 + **测试 seam 设施**（判龄/复查/销毁三挂点的包级 hook，`...ForTest` 命名惯例，供矩阵 16 双分支编排）+ 测试矩阵 1/3/4/5/10/11/12/13/14/15/16/**18**。
- **T3 文档 + 收尾**：§10 四处文档 + 全量回归 + 验收清单核对。

## 12. 验收

- §8 全绿（18 组）；全仓 `go test ./...` 绿；gofmt 净。
- **owner 手工复验（真机，随发版窗口）**：笔记本 `SSHMGR_CACHE_MAX_OFFLINE=1h` + 正常 pull → 断网 1h+ → spawn 报到龄销毁 → 重新 pull 恢复；NUC10 侧无任何感知（serve 零改动自证）。
