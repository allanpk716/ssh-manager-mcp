# Plan 42 — doctor serve 探活二期（绿/黄/红语义）设计 spec

- 日期：2026-08-28
- 来源：docs/backlog.md P2 #5（「doctor serve 探活二期（绿/黄/红语义）——现状：doctor 首版只做本机自检」）
- 前置：Plan 38-doctor（exit 2 管道已接线，**两项预埋验收挂本 plan**，见 §7）；doctor 多实例感知已落地（2026-08-27，branch `doctor-multi-instance`，commit 63b8ed5）——探针的实例枚举复用其构件
- 状态：设计定稿待 owner 审

## 1. 目标与非目标

**目标**：`doctor --probe` 对本机角色相关的 serve broker 做一次**带码、pin 级身份验证**的活性探查，把「cache 为什么不刷新 / broker 是否在岗」变成 doctor 的主动诊断面：

1. **client 机**（含每个命名实例）：探 `cache.auth.json` 的远端 broker——URL 可达 + SPKI 指纹匹配 + 设备码生死（revoked/unknown 提前可见）。
2. **server 机**：探本地 loopback serve（`serve status` http 信号同款姿势，进 doctor 面）。
3. 接通 **exit 2 的第一个生产产源**（Plan 38 预埋验收①）+ 帮助文本 exit 2 行（预埋验收②）。

**非目标**（§5 详列）：serve 侧零改动；不做 403 unbound 探测；不做 TUI/`cache status` 接入；不做并行探针/结果缓存；不做 `--probe-addr`。

## 2. 事实基础（实证，2026-08-28）

平台/代码事实，全部过实验或源码核验——**评审以此为锚，不得凭直觉推翻**：

- **F1（实验，Go 1.25 stdlib）**：HEAD 响应的 body 客户端侧恒空（`httptest` + `http.Error(w,"invalid cache token: revoked",401)` → HEAD 读得 `bodyLen=0`；同端点 GET 读得全量 29 字节）。即：**HEAD 拿不到 401 reason，GET 拿得到**。
- **F2（实验，同上）**：GET 200 即使 client 从不 Read body、立即 Close，**服务端仍全量序列化 + 执行成功路径副作用**（模拟 1 MiB body 已写出）。即：对 `/snapshot` 用 GET 探活码 = 每次探查都让 serve 序列化整个快照 + `TouchCacheToken`（污染 `last_pull` 诊断信号）。
- **F3（源码，serve.go:248+271）**：`/snapshot` 链 = `cacheAuth(handleSnapshot)`——**auth 中间件先于 method 检查**；`handleSnapshot` 首行 `Method != GET → 405`（早于 TokenInfo/绑定检查/`TouchCacheToken`）。由此得**探针阶梯**：HEAD 带码 → `405` = auth 过 = 活码；`401` = 死码。
- **F4（源码，serve.go:146-168）**：401 的 reason（`revoked`/`unknown`，8 字符前缀回查，碰撞可 mislabel——已接受）只写进**响应 body**（`invalid cache token: <reason>`，serve_test.go:462 钉住）。结合 F1：**HEAD 探不出 reason，需 401 时补一发 GET**（死码 GET 永在 auth 层被拒 → 永不 200 → 零 touch 零序列化，F2 的副作用面不触及）。
- **F5（源码，clientops）**：`pinningTransport(pin)`（VerifyConnection 常时比 SPKI 指纹，mismatch 报 `server fingerprint mismatch (expected %s, got %s)`）；pull 的 URL 构造 = `url+"/snapshot"` 直拼 + `CheckRedirect: ErrUseLastResponse`（Plan 37 全局禁跟）；`SplitTokenPin` 拆 `<code>:<pin>`；`LoadCacheCred(dir)` 读 `cache.auth.json{url,token,pin}`。
- **F6（源码，cli/serve_service.go:630）**：`probeServeHTTP(addr)`——1s TLS GET（skip-verify，自签证书 + 身份不归此信号管），401/200 = 活。
- **F7（Plan 38 spec §3.4 预埋）**：#5 的内部错误检查必须经 `NewExitCodeError(2, ...)`；plan 必须含帮助文本补行。两项均为本 plan 验收。
- **F8（Plan 34 rev4 §1 原文）**：401 reason 是 observability-only，「The client NEVER branches on this text」。**本设计的偏离与理由**：探针消费该文本**仅用于人读报告**（Detail 里的 revoked/unknown 词），不驱动任何控制流/破坏性动作——observability 信号被 observability 消费；mislabel 碰撞面与 serve 侧日志同接（8 字符前缀碰撞，接受）。

## 3. 设计

### 3.1 探针原语：`internal/clientops/probe.go`

```go
type ProbeClass string

const (
    ProbeActive      ProbeClass = "active"        // 活码（405/200）
    ProbeRevoked     ProbeClass = "revoked"       // 401 + reason=revoked
    ProbeUnknownCode ProbeClass = "unknown-code"  // 401 + 其他
    ProbePinMismatch ProbeClass = "pin-mismatch"  // TLS SPKI ≠ pin
    ProbeUnreachable ProbeClass = "unreachable"   // 传输层失败（超时/拒绝/DNS/非指纹 TLS 错）
    ProbeBadStatus   ProbeClass = "bad-status"    // 其他 HTTP 状态（含 3xx——恒不跟）
    ProbeInternal    ProbeClass = "internal"      // 残余：探针机械错误 → doctor exit 2 源
)

type ProbeResult struct {
    Class   ProbeClass
    Detail  string        // 人读：状态码/reason 词/错误类——绝不含码值/URL 凭据段
    Elapsed time.Duration // 探查墙钟（成功类进 Detail，如 "87ms"）
}

func ProbeBroker(cred CacheCred, timeout time.Duration) ProbeResult
```

**流程（冻结）**：

1. `code, _, _ := SplitTokenPin(cred.Token)`；`pin := cred.Pin`。
2. transport：`pin != ""` → `pinningTransport(pin)`（构造错 → ProbeInternal）；`pin == ""`（明文级遗物 cred，现实不可达——pull 拒明文、Pin 由成功 pull 写入）→ skip-verify transport，**结果恒降为 ProbeBadStatus**，Detail 固定前缀「credential has no pin (plaintext-grade relic) — pull itself refuses this; re-pull with a pinned code」。
3. **HEAD** `cred.URL+"/snapshot"`（与 pull 同构直拼，F5）+ `Authorization: Bearer <code>` + `CheckRedirect: ErrUseLastResponse` + `client.Timeout = timeout`（doctor 传 3s）。
4. 响应阶梯：
   - `405` **或 `200`**（前瞻兼容：未来 serve 若允许 HEAD 直达 200）→ **ProbeActive**（Detail：`auth gate passed (405)` / `(200)` + elapsed）。
   - `401` → **补一发 GET**（同 transport 同 URL 同码）→ 读 body ≤8KiB（pull 同款上限）→ 子串含 `revoked` → **ProbeRevoked**；否则 **ProbeUnknownCode**（reason 词或 body 截断 ~60 字节进 Detail）。GET 若 `200`（HEAD↔GET 间码被换活的竞态：revoke 终态不可逆，仅同 name 重发新码且值恰同——实际不可达）→ ProbeActive + Detail 注 `(race: code re-activated between HEAD and GET)`；GET 传输错 → 按第 5 步分类。
   - 其他状态（3xx/5xx/…，3xx 因不跟而停在此）→ **ProbeBadStatus**（状态码进 Detail）。
5. `client.Do` 错误分类：错误串含 `server fingerprint mismatch`（pinningTransport 的 VerifyConnection 文案，F5）→ **ProbePinMismatch**（Detail 只报 mismatch 事实，**不回显期望/实际指纹**——指纹虽公开，探针 Detail 保持最小面）；其他传输错（超时/拒绝/DNS/非指纹 TLS 错，含证书过期）→ **ProbeUnreachable**（错误类短词进 Detail，如 `timeout` / `connection refused`，不透传原始串——防错误文本携带内部 URL 细节）。**残余**（NewRequest 失败、GET 补发的 body 读 io 错等无法归类者）→ **ProbeInternal**。
6. **零副作用承诺**：探针只读——永不 Quarantine、永不写任何文件；服务端唯一效应 = 401 时的 stderr 日志行 + 前缀回查（既有可观测性面，F4）。

### 3.2 doctor 集成（`internal/cli/doctor.go`）

- **flag `--probe`**（bool，缺省 false）：缺省 = 现状逐字节不变——探针行零输出、探针 seam 零调用（契约钉子测试锁死）。
- **执行模型**：`doctorCheckFuncs` 静态表不动；runDoctor 在 flag 置位时追加调用 `checkServeProbe()`（探针行落在全部默认行之后、`overall:` 之前）。
- **seam**（`serveServiceState` 先例，doctor 测试零真网络）：
  ```go
  var probeBroker   = clientops.ProbeBroker
  var probeLoopback = probeServeHTTP          // server 角色：addr 恒 "127.0.0.1:7878"
  var probeTimeout  = 3 * time.Second         // per-probe 上限
  ```
- **角色路由**（一个 check 函数输出全部行）：
  - 无 role → 一行 INFO `serve-probe`：「no role — nothing to probe」。
  - `server` → 若 serve-svc 状态 NOT INSTALLED → INFO「serve not in use — nothing to probe」；否则 `probeLoopback("127.0.0.1:7878")` → 行 `serve-probe`：true = PASS「serve responding over TLS (401 = auth gate up)」（无 elapsed——`probeServeHTTP` 只回 bool）；false = **FAIL**「serve not responding on 127.0.0.1:7878 (default addr; custom --addr installs: `ssh-manager serve status`)」。
  - `standalone` → INFO「standalone machine — no serve broker to probe」。
  - `client` → 默认 + `ListInstances()` 每实例各一行：行名 `serve-probe`（默认）/ `serve-probe[<name>]`；材料 = 各自 `LoadCacheCred(dir)`。cred 缺 → INFO「no pull credential — nothing to probe (see client-cache row)」；cred 损坏 → WARN「cache.auth.json unreadable: <err>」（fix: 重 pull）。
- **client 裁决映射（冻结表）**：

| ProbeClass | 行裁决 | Detail 骨架 | fix |
|---|---|---|---|
| Active | 🟢 PASS | `instance <n>: broker reachable, device code active (auth gate passed, <elapsed>)` | — |
| Unreachable | 🟡 WARN | `instance <n>: broker unreachable (<错误类>) — offline mode; cache age <X> vs max-offline <Y/off>`（age/cap 读 bin mtime + EffectiveMaxOffline，与 client-cache 行同源） | `check network / serve status on the broker machine` |
| PinMismatch | 🔴 FAIL | `instance <n>: server certificate fingerprint does not match the pinned pin — cert rotated (re-pin via wizard) or a MITM (investigate)` | `verify the new fingerprint out-of-band (`serve cert-info` on the server), then re-run the wizard / re-pull to re-pin` |
| Revoked | 🔴 FAIL | `instance <n>: device code REVOKED — the cache will self-destruct on next pull` | `re-enroll: fresh device code from the owner (cache-tokens add), then `cache pull --instance <n>`` |
| UnknownCode | 🔴 FAIL | `instance <n>: device code rejected (unknown) — not an active code on the server` | `owner: `cache-tokens ls` to check the code; re-enroll with a fresh code` |
| BadStatus | 🟡 WARN | `instance <n>: broker answered <status> (expected 405) <—或 no-pin 前缀>` | `inspect serve on the broker machine (serve status / serve.log)` |

（server 角色二态已在路由给出：PASS（无 elapsed——`probeServeHTTP` 只回 bool）/ FAIL。Internal 不在本表——它不是被诊对象的发现，不产 doctor 行，见 §3.3。Unreachable 行的 age 从句以该实例 cache.bin 在场为前提——bin 缺席则省略（cred 在而 bin 无的态本就异常，client-cache 行已 FAIL 之）。）

### 3.3 exit 2 边界与优先级（Plan 38 预埋验收①）

- 任一探针 `Class == ProbeInternal` → runDoctor 返回 `NewExitCodeError(2, fmt.Errorf("doctor: probe machinery error (%d instance(s)): <第一处 Detail>", n))`。
- **优先级**：internal(2) > findings(1) > clean(0)。同时存在 FAIL 行与 internal 时：FAIL 行照常打印（诊断完整），退出码取 **2**——「doctor 自身坏」压过「被诊对象坏」。
- doctor 行渲染：internal 探针**不单独产行**（它不是被诊对象的发现，是 doctor 的机械故障），以 stderr 错误 + exit 2 表达。此语义写进帮助文本。

### 3.4 帮助文本（Plan 38 预埋验收②，冻结文案）

Long 段改写（新增句）：

> By default doctor makes no network calls. With `--probe` it additionally
> performs liveness probes: client machines send a pinned, authenticated
> HEAD /snapshot to their broker (per cache instance); server machines probe
> the local serve listener. Probe findings follow the same PASS/WARN/FAIL
> contract below.

Exit codes 段：

> Exit codes (stable, for scripts): 0 = no FAIL findings (warnings allowed),
> 1 = at least one FAIL finding, 2 = doctor internal error (probe machinery).

### 3.5 Detail 信息纪律

- 探针 Detail **绝不含**：码值（任何形态/前缀）、URL 的凭据段、pin 指纹值（含 mismatch 时的期望/实际）。允许：状态码、reason 词、错误类短词、elapsed、实例名、age/cap。
- 实例名来自 `ListInstances()`（磁盘目录名，本就非秘密）。

## 4. 测试策略

### 4.1 clientops（真探针——httptest TLS + 真 SPKI pin，`expiry_pull_test.go` 姿势）

handler 模拟真 serve 阶梯（auth-by-fixed-code + method check → 405/401）：

1. 活码：HEAD→405 → ProbeActive；断言 server 只收到 HEAD（无 GET、200 路径零执行）。
2. 死码 revoked：HEAD→401 + GET→401 body `invalid cache token: revoked` → ProbeRevoked。
3. 死码 unknown：同上 body 换 `…unknown` → ProbeUnknownCode。
4. 竞态分支：HEAD→401 但 GET→200 → ProbeActive + race 注记。
5. pin mismatch：换证书的 server → ProbePinMismatch。
6. unreachable：server.Close() 后探 → ProbeUnreachable。
7. bad-status：500 与 302（302 断言不被跟、Detail 报 302）。
8. 无 pin 降级：cred.Pin 空 → 恒 ProbeBadStatus + 固定前缀。
9. **零副作用钉子**：探针前后 cache 目录快照逐字节不变（无 quarantine/ 产物、无文件 mtime 变化）。
10. Detail 纪律：全表断言不含码值/pin 串。

### 4.2 cli doctor（stub seams）

1. 裁决映射全表：stub `probeBroker` 逐类返回 → 断言行名/裁决/Detail 骨架/fix。
2. exit 2 优先级：internal 与 FAIL 并存 → `ExitCodeFor(err) == 2` 且 FAIL 行仍打印。
3. **默认契约钉子**：无 `--probe` 时 stub 计数器断言 `probeBroker`/`probeLoopback` 零调用 + 输出零探针行。
4. 角色路由四态（server/client/standalone/fresh）+ client 多实例行名。
5. server 分支：NOT INSTALLED→INFO / Running+loopback true→PASS / false→FAIL（Detail 含 default addr 指引）。
6. 帮助文本断言：`--help` 输出含 exit 2 行 + no-network-by-default 句。
7. client Unreachable 行的 age/cap 联动（seed 老 bin + cap → Detail 带 age vs max-offline）。

## 5. 明确不做 / 边界

- **serve 侧零改动**（阶梯靠既有代码序 F3；405|200 双收 §3.1-4 前瞻兼容）。
- **不做 403 unbound 探测**：HEAD 阶于 method 检查之后不可达（F3）；拉取路径已有 403 分类。登记为探针盲区。
- **不做** TUI / `cache status` 接入（`ProbeBroker` 原语已可用，接入另立项）。
- **不做** `--probe-addr`（server 探默认 addr；自定义装机 Detail 指引 `serve status`）。
- **不做**并行探针/结果缓存（顺序逐实例，N×3s 上界可接受——诊断场景）。
- **不做** TLS 深诊断（证书过期等非指纹 TLS 错归 Unreachable，Detail 带错误类——细分无行动价值）。

## 6. 文档联动

- doctor 帮助文本（§3.4）。
- README / getting-started doctor 节：`--probe` 用法一句 + 三色语义一行。
- multi-machine.md：client 探活 =「cache 为什么不刷新」诊断入口（含 revoked 提前可见）。
- docs/backlog.md #5 销项；compat-matrix 下一版行（与 doctor 多实例同船，占位注释已存在——本 plan 落地时扩写该注释）。
- threat-model：`--probe` 发送设备码（pin 保护下，与 pull 同信道同险；doctor 输出永不回显码值）——一句话登记。

## 7. 验收清单

1. **Plan 38 预埋①**：internal 经 `NewExitCodeError(2, ...)`（4.2-2 测试钉）。
2. **Plan 38 预埋②**：帮助文本 exit 2 行上线（4.2-6 断言）。
3. **默认契约**：无 `--probe` 的 doctor 输出与本改动前逐字节等价（4.2-3）。
4. clientops 探针全表绿（4.1 十项）。
5. 全仓测试绿。
6. 真机（发版后双端）：NUC10 `--probe` = `serve-probe PASS`（loopback）；笔记本 `--probe` = `serve-probe PASS`（pinned 405 活码）。owner gate。

## 8. 风险与备选

- **R1**：阶梯依赖 `handleSnapshot` method-check-after-auth 代码序（F3）——被 4.1 的 handler 模拟钉住；若 serve 未来重排，probe 契约测试红（fail-loud），且 405|200 双收已吸收「允许 HEAD」的未来演进。
- **R2**：pin mismatch 判别靠 `server fingerprint mismatch` 串匹配（pinningTransport 自有文案）——同串判定发生在 clientops 包内（自家代码），非外部依赖；若文案变更，4.1-5 红。
- **R3**：探针与 lazy-pull 并发撞同一 cred 的 server 侧日志噪声（两次 401 行）——纯可观测性，无正确性影响。
- **R4**（备选记录）：若评审否决 HEAD/405 阶梯（嫌依赖代码序），备选 = serve 加 reason 响应头 + HEAD 直达——引入 serve 改动与版本耦合，本设计不取。
