# Plan 43 — doctor serve 探活二期（绿/黄/红语义）设计 spec · rev2.1

- 日期：2026-08-28（rev2 同日：复审轮 2 家 8 条反馈闭环全吸收——含 3 处修法 owner 拍板；rev0→rev1 变更见 §9.1，rev1→rev2 见 §9.2，rev2→rev2.1 见 §9.3）
- 来源：docs/backlog.md P2 #5（「doctor serve 探活二期（绿/黄/红语义）——现状：doctor 首版只做本机自检」）
- 前置：Plan 38-doctor（exit 2 管道已接线，**两项预埋验收挂本 plan**，见 §7）；doctor 多实例感知已落地（2026-08-27，branch `doctor-multi-instance`，commit 63b8ed5）——探针的实例枚举复用其构件
- 状态：rev2.1 定稿待 owner 审（rev2 技术内容零改动；rev2.1 = 编号适配 + 与易用性改造 Plan 42 的冲突审查协调，见 §9.3）

## 1. 目标与非目标

**目标**：`doctor --probe` 对本机角色相关的 serve broker 做一次**带码、pin 级身份验证**的活性探查，把「cache 为什么不刷新 / broker 是否在岗」变成 doctor 的主动诊断面：

1. **client 机**（含每个命名实例）：探 `cache.auth.json` 的远端 broker——URL 可达 + SPKI 指纹匹配 + 设备码生死（401 提前可见；revoked/unknown 词为展示字段）。
2. **server 机**：探本地 loopback serve（`serve status` http 信号同款姿势，进 doctor 面）。
3. 接通 **exit 2 的第一个生产产源**（Plan 38 预埋验收①）+ 帮助文本 exit 2 行（预埋验收②）。

**非目标**（§5 详列）：serve 侧零改动；不做 403 unbound 探测；不做 TUI/`cache status` 接入；不做并行探针/结果缓存；不做 `--probe-addr`。

## 2. 事实基础（实证，2026-08-28）

平台/代码事实，全部过实验或源码核验——**评审以此为锚，不得凭直觉推翻**：

- **F1（实验，Go 1.25 stdlib）**：HEAD 响应的 body 客户端侧恒空（`httptest` + `http.Error(w,"invalid cache token: revoked",401)` → HEAD 读得 `bodyLen=0`；同端点 GET 读得全量 29 字节）。即：**HEAD 拿不到 401 reason，GET 拿得到**。
- **F2（实验，同上）**：GET 200 即使 client 从不 Read body、立即 Close，**服务端仍全量序列化 + 执行成功路径副作用**（模拟 1 MiB body 已写出）。即：对 `/snapshot` 用 GET 探活码 = 每次探查都让 serve 序列化整个快照 + `TouchCacheToken`（污染 `last_pull` 诊断信号）。
- **F3（源码 + F9 实验坐实，serve.go:248+271）**：`/snapshot` 链 = `cacheAuth(handleSnapshot)`——**auth 中间件先于 method 检查**；`handleSnapshot` 首行 `Method != GET → 405`（早于 TokenInfo/绑定检查/`TouchCacheToken`）。由此得**探针阶梯**：HEAD 带码 → `405` = auth 过 = 活码；`401` = 死码。
- **F4（源码，serve.go:146-168）**：401 的 reason（`revoked`/`unknown`，8 字符前缀回查，碰撞可 mislabel——已接受）只写进**响应 body**（`invalid cache token: <reason>`，serve_test.go:462 钉住）。结合 F1：**HEAD 探不出 reason，需 401 时补一发 GET**（死码 GET 永在 auth 层被拒 → 永不 200 → 零 touch 零序列化，F2 的副作用面不触及）。
- **F5（源码，clientops）**：`pinningTransport(pin)`（VerifyConnection 常时比 SPKI 指纹，mismatch 报 `server fingerprint mismatch (expected %s, got %s)`）；pull 的 URL 构造 = `url+"/snapshot"` 直拼 + `CheckRedirect: ErrUseLastResponse`（Plan 37 全局禁跟）；`SplitTokenPin` 拆 `<code>:<pin>`；`LoadCacheCred(dir)` 读 `cache.auth.json{url,token,pin}`。
- **F6（源码，cli/serve_service.go:630）**：`probeServeHTTP(addr)`——**1s** TLS GET（skip-verify，自签证书 + 身份不归此信号管），401/200 = 活。
- **F7（Plan 38 spec §3.4 预埋）**：#5 的内部错误检查必须经 `NewExitCodeError(2, ...)`；plan 必须含帮助文本补行。两项均为本 plan 验收。
- **F8（Plan 34 rev4 §1 原文；rev2 合规化）**：Plan 34 写明 401 reason 是 observability-only，「The client NEVER branches on this text」。**本设计字面合规**：ProbeClass 的判定**只依赖 HTTP 状态码**（401 = 死码），revoked/unknown 词降为 Detail 的**非分支展示字段**（不驱动 class、不驱动 fix、不驱动任何控制流）——探针是「读状态码定类 + 展示文本点缀」，reason 词 mislabel 碰撞面（8 字符前缀，serve 侧已接受）对探针仅影响一个形容词。
- **F9（实验，真 ServeRunner + 真临时 store + 真码，2026-08-28）**：
  - HEAD(活码) → **405, bodyLen=0**；HEAD(垃圾码) → **401**——F3 阶梯在**真实 handler** 上坐实（非模拟）。
  - GET(活码) → **200 且 `LastPullAt` 从零值被推进**——touch 副作用实锤（F2 的成功路径面）。
  - revoke 后同 name 重发 → **新 token ≠ 旧 token**；同 bearer 串的 invalid→valid 迁移**经 store API 不可构造**（revocation 终态 + GenerateToken 随机铸码）——§3.1-5 的竞态分支**构造不出来**（带外手工改库除外）。
- **F10（实验，2026-08-28）**：敌意 401 body「access revoked by upstream policy」——裸子串 `revoked` 匹配**误标 revoked**；加 `invalid cache token:` 前缀闸后正确归 unknown 展示。真 serve 两种 body 两规则判定一致。→ reason 词提取必须带前缀闸。

## 3. 设计

### 3.1 探针原语：`internal/clientops/probe.go`

```go
type ProbeClass string

const (
    ProbeActive       ProbeClass = "active"         // 活码（405/200）
    ProbeRejected     ProbeClass = "rejected"       // 401 = 死码（reason 词为 Detail 展示字段，非分支）
    ProbeLocalRefusal ProbeClass = "local-refusal"  // 本地闸拒绝（无 pin / 非 https）——零网络
    ProbePinMismatch  ProbeClass = "pin-mismatch"   // TLS SPKI ≠ pin
    ProbeUnreachable  ProbeClass = "unreachable"    // 传输层失败（超时/拒绝/DNS/非指纹 TLS 错）
    ProbeBadStatus    ProbeClass = "bad-status"     // 其他 HTTP 状态（含 3xx——恒不跟；含 GET 补发的非 200/401）
    ProbeInternal     ProbeClass = "internal"       // 仅本地构造/编程错误（如 NewRequest 失败）→ doctor exit 2 源
)

type ProbeResult struct {
    Class   ProbeClass
    Detail  string        // 人读：状态码/reason 词/错误类——绝不含码值/URL 凭据段（§3.5）
    Elapsed time.Duration // 探查墙钟（成功类进 Detail，如 "87ms"）
}

func ProbeBroker(cred CacheCred, timeout time.Duration) ProbeResult
```

**流程（冻结）**：

1. `code, _, _ := SplitTokenPin(cred.Token)`；`pin := cred.Pin`。
2. **本地闸（零网络短路）**：`pin == ""` **或** `cred.URL` 非 `https://` scheme → **不发任何请求**，直接返回 `ProbeLocalRefusal` + 固定文案——pin 空用「credential has no pin (plaintext-grade relic) — refusing to send the device code over an unpinned channel; re-pull with a pinned code」；非 https 用「credential URL is not https — refusing to send the device code in cleartext; re-pull with an https URL」。设备码**只在 pin 保护的信道上发送**（与 pull 的明文拒绝同反射）。
3. transport：`pinningTransport(pin)`（构造错 → ProbeInternal）。
4. **HEAD** `cred.URL+"/snapshot"`（与 pull 同构直拼，F5）+ `Authorization: Bearer <code>` + `CheckRedirect: ErrUseLastResponse` + `client.Timeout = timeout`（doctor 传 3s）。
5. 响应阶梯：
   - `405` **或 `200`**（前瞻兼容：未来 serve 若允许 HEAD 直达 200）→ **ProbeActive**（Detail：`auth gate passed (405)` / `(200)` + elapsed）。
   - `401` → **ProbeRejected**（class 判定到此为止，只看状态码）。**补一发 GET**（同 transport 同 URL 同码）以**取 reason 词做展示**：读 body ≤8KiB（pull 同款上限）→ **先匹配 `invalid cache token:` 前缀**，命中且含 `revoked` → Detail 带 `revoked`；其他（前缀不命中 / 含 `unknown` / body 截断 ~60 字节）→ Detail 带 `unknown` 或截断摘要——**任何 GET 补发的走向都不改变 ProbeRejected 这个 class**（F8 合规：非分支）：
     - GET `200`（HEAD↔GET 间码被换活——**经 store API 不可构造**，F9-D；仅带外手工改库可造）→ **例外地改判 ProbeActive** + Detail 注 `(race: code re-activated between HEAD and GET)`（这是「状态码变了」而非「文本分支」——401→200 是状态码信息，合规）；
     - GET 其他状态（500/403/302 等，kimi#1 缝隙闭合）→ class 仍 ProbeRejected，Detail 注 `(follow-up GET answered <status>; reason indeterminate)`；
     - GET 传输错 / 401 body 读 io 错（codex#1 归位）→ class 仍 ProbeRejected，Detail 注 `(reason unreadable: <错误类短词>)`——码死这一事实已由 HEAD 的 401 确立，丢失的只是 reason 词。
   - 其他 HEAD 状态（3xx/5xx/…，3xx 因不跟而停在此）→ **ProbeBadStatus**（状态码进 Detail）。
6. `client.Do` 错误分类：错误串含 `server fingerprint mismatch`（pinningTransport 的 VerifyConnection 文案，F5）→ **ProbePinMismatch**（Detail 只报 mismatch 事实，**不回显期望/实际指纹**）；其他传输错（超时/拒绝/DNS/非指纹 TLS 错，含证书过期）→ **ProbeUnreachable**（错误类短词进 Detail，如 `timeout` / `connection refused`，不透传原始串）。**残余**（**仅本地构造/编程错误**——NewRequest 失败等；rev2 收窄：body 读错已归 ProbeRejected，网络/TLS 错已归 Unreachable）→ **ProbeInternal**。
7. **零副作用承诺**：探针只读——永不 Quarantine、永不写任何文件；服务端唯一效应 = 401 时的 stderr 日志行 + 前缀回查（既有可观测性面，F4）。**已登记例外**：若带外手工改库造出 HEAD-401→GET-200 竞态（F9-D：经 API 不可构造），单次 touch + 单次快照序列化发生——有界、已登记、不视为承诺破坏。

### 3.2 doctor 集成（`internal/cli/doctor.go`）

- **flag `--probe`**（bool，缺省 false）：缺省 = 现状逐字节不变——探针行零输出、探针 seam 零调用（契约钉子测试锁死）。
- **执行模型**：`doctorCheckFuncs` 静态表不动；runDoctor 在 flag 置位时追加调用 `checkServeProbe()`（探针行落在全部默认行之后、`overall:` 之前）。
- **seam**（`serveServiceState` 先例，doctor 测试零真网络）：
  ```go
  var probeBroker   = clientops.ProbeBroker
  var probeLoopback = probeServeHTTP          // server 角色：addr 恒 "127.0.0.1:7878"（沿用其自带 1s 超时）
  var probeTimeout  = 3 * time.Second         // 仅 client broker 探针（server 侧 1s 是 probeServeHTTP 自带）
  ```
- **角色路由**（一个 check 函数输出全部行）：
  - 无 role → 一行 INFO `serve-probe`：「no role — nothing to probe」。
  - `server` → 若 serve-svc 状态 NOT INSTALLED → INFO「serve not in use — nothing to probe」；否则 `probeLoopback("127.0.0.1:7878")` → 行 `serve-probe`：true = PASS「serve responding over TLS (401 = auth gate up)」（无 elapsed——`probeServeHTTP` 只回 bool）；false = **WARN**「serve not responding on 127.0.0.1:7878 (default addr; custom --addr installs or broker down — verify with `ssh-manager serve status`)」——歧义态（custom-addr 装机健康机不可误红 exit 1；真崩溃由 serve-svc 行的 Stopped WARN 承担红旗）。**server 侧探针无 FAIL 分支**。
  - `standalone` → INFO「standalone machine — no serve broker to probe」。
  - `client` → 默认 + `ListInstances()` 每实例各一行：行名 `serve-probe`（默认）/ `serve-probe[<name>]`；材料 = 各自 `LoadCacheCred(dir)`。cred 缺 → INFO「no pull credential — nothing to probe (see client-cache row)」；cred 损坏 → WARN「cache.auth.json unreadable: <err>」（fix: 重 pull）。
- **client 裁决映射（冻结表）**：

| ProbeClass | 行裁决 | Detail 骨架 | fix |
|---|---|---|---|
| Active | 🟢 PASS | `instance <n>: broker reachable, device code active (auth gate passed, <elapsed>)` | — |
| Rejected | 🔴 FAIL | `instance <n>: device code REJECTED (401<, reason 词或 unreadable/indeterminate 注记>) — the cache will self-destruct on next pull` | `owner: run `cache-tokens ls` to check the code; then re-enroll with a fresh device code (`cache pull --instance <n>`)` |
| LocalRefusal | 🔴 FAIL | `instance <n>: <本地闸固定文案（无 pin / 非 https）>` | `re-pull with a pinned https credential (`cache pull --instance <n>`)` |
| PinMismatch | 🔴 FAIL | `instance <n>: server certificate fingerprint does not match the pinned pin — cert rotated (re-pin via `ssh-manager pair` / re-pull) or a MITM (investigate)` | `verify the new fingerprint out-of-band (`serve cert-info` on the server), then re-pull to re-pin (or re-run `ssh-manager pair`)` |
| Unreachable | 🟡 WARN | `instance <n>: broker unreachable (<错误类>) — offline mode; cache age <X> vs max-offline <Y/off>`（age/cap 读 bin mtime + EffectiveMaxOffline，与 client-cache 行同源；bin 缺席则省略 age 从句——cred 在而 bin 无的态本就异常，client-cache 行已 FAIL 之） | `check network / serve status on the broker machine` |
| BadStatus | 🟡 WARN | `instance <n>: broker answered <status> (expected 405)` | `inspect serve on the broker machine (serve status / serve.log)` |

（server 角色三态已在路由给出：PASS / WARN / INFO。Internal 不在本表——它不是被诊对象的发现，不产 doctor 行，见 §3.3。）

### 3.3 exit 2 边界、优先级与 Detail 脱敏（Plan 38 预埋验收①）

- **Internal 定义（rev2 收窄）**：仅**本地构造/编程错误**（NewRequest 失败等 doctor 自身机械故障）——网络/TLS/body 读错误全部归被诊对象的发现类（Unreachable / Rejected），**外部故障永不冒充 doctor 内部错误触发 exit 2**。
- 任一探针 `Class == ProbeInternal` → runDoctor 返回 `NewExitCodeError(2, fmt.Errorf("doctor: probe machinery error (%d instance(s)): <第一处错误类>", n))`。
- **继续语义（rev2 明示）**：某实例命中 internal **不中止**其余探查——探针照常探完全部实例（诊断完整性），退出码最后取 2。
- **优先级**：internal(2) > findings(1) > clean(0)。同时存在 FAIL 行与 internal 时：FAIL 行照常打印，退出码取 **2**。
- doctor 行渲染：internal 探针**不单独成行**，以 stderr 错误 + exit 2 表达。此语义写进帮助文本。
- **internal Detail 脱敏**：internal 错误**永不透传原始 err 串**（URL parse/NewRequest 错误常内嵌原始 URL，userinfo 形态可泄）——固定安全文案 + 错误类短词（如 `request construction failed`）。与 §3.1-6 传输错误的「不透传原始串」同一纪律。

### 3.4 帮助文本（Plan 38 预埋验收②，冻结文案）

Long 段改写（新增句）：

> By default doctor makes no network calls. With `--probe` it additionally
> performs liveness probes: client machines send a pinned, authenticated
> HEAD /snapshot to their broker (per cache instance), plus an authenticated
> GET follow-up when the server rejects the code (to surface the rejection
> reason); server machines probe the local serve listener. Probe findings
> follow the same PASS/WARN/FAIL contract below.

Exit codes 段：

> Exit codes (stable, for scripts): 0 = no FAIL findings (warnings allowed),
> 1 = at least one FAIL finding, 2 = doctor internal error.

### 3.5 Detail 信息纪律

- 探针 Detail **绝不含**：码值（任何形态/前缀）、URL 的凭据段、pin 指纹值（含 mismatch 时的期望/实际）、internal 错误的原始 err 串。允许：状态码、reason 展示词（revoked/unknown）、错误类短词、elapsed、实例名、age/cap。
- 实例名来自 `ListInstances()`（磁盘目录名，本就非秘密）。

## 4. 测试策略

### 4.1 clientops（真探针——httptest TLS + 真 SPKI pin，`expiry_pull_test.go` 姿势）

handler 模拟真 serve 阶梯（auth-by-fixed-code + method check → 405/401）：

1. 活码：HEAD→405 → ProbeActive；断言 server 只收到 HEAD（无 GET、200 路径零执行）。
2. 死码 revoked：HEAD→401 + GET→401 body `invalid cache token: revoked` → **ProbeRejected** + Detail 含 `revoked`。
3. 死码 unknown：同上 body 换 `…unknown` → **ProbeRejected** + Detail 含 `unknown`（rev2：同 class，仅 Detail 词不同）。
4. 竞态分支：HEAD→401 但 GET→200 → ProbeActive + race 注记。
5. pin mismatch：换证书的 server → ProbePinMismatch。
6. unreachable：server.Close() 后探 → ProbeUnreachable。
7. bad-status：500 与 302（302 断言不被跟、Detail 报 302）。
8. **本地闸**：pin 空与 URL 非 https 两腿 → **ProbeLocalRefusal** + 固定文案 + **零网络断言**（httptest 计数为零）。
9. **reason 前缀闸**：GET 401 敌意 body「access revoked by upstream policy」→ **ProbeRejected** + Detail 归 `unknown`（非 revoked，F10）。
10. **GET 补发异常腿（rev2）**：①GET 返回 500 → ProbeRejected + `(follow-up GET answered 500; reason indeterminate)`；②401 body 中途断流（handler 写半截断连）→ ProbeRejected + `(reason unreadable: <错误类>)`——两腿 class 恒 Rejected。
11. **零副作用钉子**：探针前后 cache 目录快照逐字节不变（无 quarantine/ 产物、无文件 mtime 变化）。
12. **真实 internal 触发腿**：cred.URL 含过本地闸的非法控制字符（url.Parse 容忍、`http.NewRequest` 拒绝的形态）→ ProbeInternal + 固定安全文案断言（不含原始串）。
13. Detail 纪律：全表断言不含码值/pin 串/原始 err 串。

### 4.1b 真 handler 契约测试（mcpserver 包——`serve_test.go` 既有真 ServeRunner 姿势）

- 真 ServeRunner.HTTPHandler + 真临时 store + 真码（**零模拟**，断言集 = F9 实验已验）：
  - HEAD /snapshot（活码）→ **405**、body 空；
  - HEAD /snapshot（垃圾码）→ **401**（auth 先于 method 检查的代码序被真实钉住——serve 未来重排此序即红，fail-loud）；
  - GET /snapshot（活码）→ 200（既有行为锚，防阶梯语义漂移）。

### 4.2 cli doctor（stub seams）

1. 裁决映射全表：stub `probeBroker` 逐类返回 → 断言行名/裁决/Detail 骨架/fix（rev2：含 Rejected 单类两词、LocalRefusal FAIL）。
2. exit 2 优先级 + **消息脱敏断言（rev2）**：internal 与 FAIL 并存 → `ExitCodeFor(err) == 2`、FAIL 行仍打印、**且 error 消息文本不含原始 err 串/URL**。
3. **默认契约钉子**：无 `--probe` 时 stub 计数器断言 `probeBroker`/`probeLoopback` 零调用 + 输出零探针行。
4. 角色路由四态（server/client/standalone/fresh）+ client 多实例行名 + **internal 不中止腿（rev2：两实例，首实例 internal、次实例照常出 PASS 行）**。
5. server 分支：NOT INSTALLED→INFO / loopback true→PASS / false→WARN（歧义态文案含 custom-addr 提示）。
6. 帮助文本断言：`--help` 输出含 exit 2 行（无 probe 括号）+ no-network-by-default 句 + **两阶段请求描述（rev2）**。
7. client Unreachable 行的 age/cap 联动（seed 老 bin + cap → Detail 带 age vs max-offline；bin 缺席省略 age 从句）。

## 5. 明确不做 / 边界

- **serve 侧零改动**（阶梯靠既有代码序 F3/F9；405|200 双收 §3.1-5 前瞻兼容）。
- **不做 403 unbound 探测**：HEAD 阶于 method 检查之后不可达（F3）；拉取路径已有 403 分类。登记为探针盲区。
- **不做** TUI / `cache status` 接入（`ProbeBroker` 原语已可用，接入另立项）。
- **不做** `--probe-addr`（server 探默认 addr；custom-addr 装机的歧义以 WARN + 指引表达）。
- **不做**并行探针/结果缓存（顺序逐实例，**per-instance 最坏 2×3s=6s**（死码 HEAD+补发 GET 两发各吃满 timeout），N 实例总上界 N×6s——诊断场景可接受）。
- **不做** TLS 深诊断（证书过期等非指纹 TLS 错归 Unreachable，Detail 带错误类——细分无行动价值）。

## 6. 文档联动

- doctor 帮助文本（§3.4）。
- README / getting-started doctor 节：`--probe` 用法一句 + 三色语义一行。
- multi-machine.md：client 探活 =「cache 为什么不刷新」诊断入口（含 401 死码提前可见）。
- docs/backlog.md #5 销项；compat-matrix 下一版行（与 doctor 多实例同船，占位注释已存在——本 plan 落地时扩写该注释）。
- threat-model：`--probe` 发送设备码（**仅 pin 保护信道上**——无 pin/非 https 本地短路拒绝；与 pull 同信道同险；401 时多一发 GET 取 reason）；doctor 输出永不回显码值——一句话登记。

## 7. 验收清单

1. **Plan 38 预埋①**：internal 经 `NewExitCodeError(2, ...)`（4.2-2 测试钉）。
2. **Plan 38 预埋②**：帮助文本 exit 2 行上线（4.2-6 断言）。
3. **默认契约**：无 `--probe` 的 doctor **默认诊断报告输出**与本改动前逐字节等价（范围限定：新增 flag/帮助文本变更不计入——`--help` 输出按 §3.4 变更）。
4. clientops 探针全表绿（4.1 十三项）+ 真 handler 契约测试绿（4.1b）。
5. 全仓测试绿。
6. 真机（发版后双端）：NUC10 `--probe` = `serve-probe PASS`（loopback）；笔记本 `--probe` = `serve-probe PASS`（pinned 405 活码）。owner gate。

## 8. 风险与备选

- **R1**：阶梯依赖 `handleSnapshot` method-check-after-auth 代码序——**已被 4.1b 真 handler 契约测试钉住**（F9 实验先行验证）；serve 未来重排此序即红（fail-loud），且 405|200 双收已吸收「允许 HEAD」的未来演进。
- **R2**：pin mismatch 判别靠 `server fingerprint mismatch` 串匹配（pinningTransport 自有文案）——同串判定发生在 clientops 包内（自家代码），非外部依赖；若文案变更，4.1-5 红。
- **R3**：探针与 lazy-pull 并发撞同一 cred 的 server 侧日志噪声（两次 401 行）——纯可观测性，无正确性影响。
- **R4（备选记录）**：若评审否决 HEAD/405 阶梯（嫌依赖代码序），备选 = serve 加 reason 响应头 + HEAD 直达——引入 serve 改动与版本耦合，本设计不取。
- **R5**：server 侧探针无 FAIL 分支（owner 拍板 WARN 化）——「broker 机真崩 + service 状态误报 Running」的复合态下 doctor 总判 WARN 非 FAIL；部署验证脚本若依赖 exit 1 捕获 server 崩溃，需改看 serve-svc 行或 serve status。接受面：serve-svc Stopped 已 WARN、真崩通常伴随 Stopped/NOT INSTALLED。
- **R6（rev2 登记）**：ProbeRejected 合并单类的代价——revoked 与 unknown 共享一条 fix 文案（owner 查码 + 重 enroll 两步都覆盖），牺牲了两词各自的精确指引；换来 Plan 34 字面合规与 class 判定只依赖状态码的简单性。owner 拍板接受。
- **R7（rev2.1 登记，关联易用性改造 Plan 42 grilling 定案 2026-08-28）**：易用性改造（其批1，目标 v0.11.0）将**移除 ②a 在线 HTTP 直连**（serve mux 撤 MCP streamable HTTP handler）。与本 plan 的协调三点：①**`probeServeHTTP` 共享 seam**（本 plan `probeLoopback` 与 `serve status` 同源，F6）——其根路径 401 来自 MCP 路由的 auth 层，②a 移除后根路径落 404，若不处理则 server 侧探针 + `serve status` **全线假 WARN**；该 plan 批1 已含「探活重指向 `/snapshot`」清单项（未带码 GET → `cacheAuth` 401，「401 = auth gate up」语义保真；auth 层先拒故零序列化零 touch，F2 副作用面不触及）——**一处改两受益，两个 plan 任一先落地都必须携带此 seam 改动**。②client TUI wizard 将被 `ssh-manager pair` 取代——本 rev 已将 PinMismatch 两处文案先行改指 pair / re-pull。③本 plan 探针阶梯打的是 `/snapshot` 链（F3/F9），**不受 MCP handler 移除影响**，全部技术前提继续成立。

## 9. 变更记录

### 9.1 rev0 → rev1（首审 13 条闭环）

1. §3.1-2：pin 空 / 非 https → 本地短路零网络。
2. §3.1-5：reason 判定加 `invalid cache token:` 前缀闸。
3. §3.1-7：零副作用承诺精化——竞态例外有界登记；竞态「经 store API 不可构造」。
4. §3.2：server loopback 不可达 FAIL→WARN（owner 拍板）。
5. §3.3：internal Detail 脱敏冻结。
6. §3.4：exit 2 行去括号。
7. §4.1：+本地闸两腿、+前缀闸腿、+真实 internal 触发腿。
8. §4.1b：真 handler 契约测试新增。
9. §5：上界 2×3s；custom-addr WARN 化。
10. §7-3：等价范围限定。
11. §2/§8：+F9/F10；R1 加强、R5 新登记。

### 9.2 rev1 → rev2（复审 8 条闭环，3 处 owner 拍板）

1. **class 合并（owner 拍板，codex#3+kimi 一致方向）**：`ProbeRevoked`/`ProbeUnknownCode` → 单一 **`ProbeRejected`**（class 只看 401 状态码；revoked/unknown 词降为 Detail 非分支展示字段）——**F8 从「偏离登记」改写为「字面合规」**；fix 文案合一（代价登记 R6）。
2. **本地闸独立类（owner 拍板，codex#2+kimi#2 共识）**：`ProbeLocalRefusal` + **FAIL** 裁决（确定性不可用配置必须动作）——同时解除 BadStatus 语义过载（回归纯「意外 HTTP 状态」）。
3. **body 读错归位（owner 拍板，codex#1）**：GET-401 body 读 io 错 → ProbeRejected + `(reason unreadable: <错误类>)`；**Internal 收窄为仅本地构造/编程错误**——外部故障永不触发 exit 2。
4. **GET 补发全走向冻结（kimi#1）**：非 200/401 状态 → class 仍 Rejected + indeterminate 注记。
5. **internal 继续语义明示（kimi#4）**：不中止其余实例，探完取 2。
6. **帮助文本两阶段描述（codex#4）**：Long 补「plus an authenticated GET follow-up when the server rejects the code」。
7. **4.2-2 加 exit 2 消息脱敏断言（kimi#3）**。
8. §4.1 测试腿重排为十三项（含 GET 补发异常两腿）；§4.2-1/4/6 相应更新。

### 9.3 rev2 → rev2.1（2026-08-28，编号适配 + 与易用性改造冲突审查协调；技术内容零改动）

来源：owner 以 2026-08-28 易用性改造 grilling 共识（后定名 **Plan 42**）为主审基准，对本 spec 做冲突审查——结论**无硬冲突**，以下为元数据/文案级适配：

1. **编号：Plan 42 → Plan 43**（与易用性改造撞号；owner 拍板易用性保 42，本 plan 改 43。文件家族三份同步改名，标题同步）。
2. **PinMismatch 两处文案 "wizard" → `ssh-manager pair` / re-pull**（易用性批1 将删除 client TUI wizard/connect-form，被 `ssh-manager pair` 一条龙取代；§3.2 裁决表相应行）。
3. **新增 R7**：`probeServeHTTP` 共享 seam 随 ②a 移除重指向 `/snapshot` 的协调登记（两个 plan 任一先落地都必须携带；本 plan 阶梯/裁决表/测试策略全部原封不动）。
4. **一处笔误修正（编辑性，零语义变化）**：§3.3「internal 探针不单独产行」→「不单独成行」（rev2 原稿笔误，誊入 rev2.1 时订正；除此外正文与 rev2 逐字一致——diff 已核）。
