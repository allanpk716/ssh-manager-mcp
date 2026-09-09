# Plan 48: 锚定转发与带外锚定 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 修「仅缓存客户端可达」目标的首信死锁——`mcp --cache` 客户端把首次信任学到的新主机密钥经设备码认证的受审计变更转发权威 vault(锚定转发),owner 侧 `servers pin-hostkey` 提供带外锚定/覆盖/清除/枚举;反馈文件三步场景全绿,捆发 v0.15.0(含 doctor 计数 rider)。

**Architecture:** serve 新增 `/pin-hostkey`(与 `/snapshot` 同一 `cacheAuth` 闸,单事务 insert-only 落锚+审计+事务内 equal 判定);客户端 `HostKeyStore` 接口注入转发式实现(回调内实时经 `holder.Current` 解析当代存储,201/409-equal 双路本地落锚放行);`host_keys` 三新列(`pin_format`/`pin_source`/`pin_device`)成为跨版本快照协议;owner CLI 六形态(显示/`--list`/`--fingerprint`/`--from-keyscan`/`--force`/`--clear --hostport`),全部单事务落审计。

**Tech Stack:** Go 1.25;`golang.org/x/crypto/ssh`(ParsePublicKey/FingerprintSHA256);`github.com/modelcontextprotocol/go-sdk/mcp`(仅复用,无新工具);既有 testsshd/conformance/eval 设施。

**Spec:** `docs/superpowers/specs/2026-09-09-plan-48-pin-forwarding-design.md.rev3.md`(**收敛版**,三轮盲评 7 代理 70 条发现全处置;执行者必读,冲突以 spec 为准)+ `docs/adr/0002-edge-first-trust-pin-forwarding.md` + 根 `CONTEXT.md` 术语(锚定/锚定转发/带外锚定)。

## Global Constraints

- **护栏三条不可协商**:仅可新增(转发路径 insert-only,`--force` 是唯一覆盖、`--clear` 是唯一删除,均限 owner CLI);仅在线转发(broker 不可达/凭据缺失 → fail-closed,零队列零本地状态);审计(broker 侧 `pin-forward`、owner 侧 `pin-manual`/`pin-clear`,与写同事务)。
- **equal 语义**:409 响应带布尔 `equal`(事务内判定,双模);客户端 `equal=true` → 本地落锚放行。全系统等值判定唯一 = spec §5 双模(blob 字节等 / fingerprint 呈现指纹等)——TOFU 回调、`ApplyForwardedHostKey`、`InsertForwardedPin`、`--force` 前比对共用。
- **实时解析铁律**:转发式实现的读与写都在 `HostKeyTOFU` 回调内经 `holder.Current` 解析当代存储,禁止构造期捕获 store 指针(热重建窗口丢锚竞态,T5b 断言)。
- **哨兵不动**:`store.go:65` `ErrReadOnly` 文本不改;新文案由转发包装层在失败点组装;`SaveHostKey` 只读态仍返回哨兵。
- **实例显式**:转发构造器由 `internal/cli/mcp.go`(`--instance` 解析处)构造传入 `RunStdioCache`;run.go 内**禁止**默认实例兜底(Plan 40/46「读实例永远显式」)。
- **401 不隔离**:转发路径 401 当普通失败;缓存隔离只活在拉取路径(T4 断言本地缓存存活)。
- **agent 可见面零变化**:BrokerTools 集合、工具名、schema 全部不动(锚定转发无新 MCP 工具)——eval/conformance 工具集合断言零联动。
- **明文拒绝**:pin 为空(明文 http)不发起转发(逐字文案见 spec §2.1);请求体上限 64 KiB 新常量(`MaxBytesReader`,先例 `/pair/*` 1 KiB)。
- **JSON/字段校验先于守卫 ③**:不可解码/字段缺失 → 400,不得落入 403「probably not granted」误导文案。
- **doctor rider 修法钉死**:对生产库免密钥免迁移裸 SQLite 连接 `PRAGMA wal_checkpoint(TRUNCATE)` 后拷贝;**永不**连同 -wal/-shm 拷(代码注释明言撕裂风险)。
- 每 task 末 scoped 测试绿 + commit(尾行 `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`);T8 末全量 `go test ./...`。Windows 上禁无起始路径 `find`;本 plan 全程无远程操作。

---

### Task 1: store 层——三列迁移 + 锚结构 + 双模原语 + insert-only 落锚

**Files:**
- Modify: `internal/store/store.go`(schemaSQL + guarded ADD COLUMN 三列,`store.go:221-223` 先例)
- Modify: `internal/store/hostkeys.go`、`internal/store/export.go`
- Test: `internal/store/hostkeys_test.go` 追加、`internal/store/export_test.go` 追加、`internal/store/readonly_test.go` 追加

**Interfaces:**
- 锚结构(形如 `Pin{Blob []byte; Format string}`,Format ∈ `blob`|`fingerprint`)——**定义位置**:sshbroker(接口所属包),store 提供匹配方法;若实现期发现既有依赖方向不容(store→sshbroker 成环,理论不成环因 sshbroker 不 import store),则反转定义到 store,sshbroker 引用;不引入第三包。
- `GetHostKey(host, port) (*Pin, error)`(返回结构升级;nil 仍 = 无锚)
- `InsertForwardedPin(host string, port int, blob []byte, device string, audit AuditRow) (equal bool, err error)`——**单事务**:`INSERT … ON CONFLICT(host_port) DO NOTHING`(pin_format='blob', pin_source='forward', pin_device=device)→ RowsAffected==0 → **同事务内** SELECT 既有锚算双模 equal → 回滚(审计未写)返回 equal;>0 → 同事务 `writeAuditTx` → commit。
- `ApplyForwardedHostKey(host, port, blob) error`——只读态唯一可写 host_keys 的窄缝:insert-only;双模等值(含指纹锚对呈现指纹)→ 放行;不等值 → error;写当代;本地行元数据 = blob/forward/设备名(与 broker 落库一致)。
- `SaveHostKey` **保持 UPSERT 原样**(vault 首次信任路径,零行为变化)。
- `SnapshotHostKey` + `pin_format`/`pin_source`/`pin_device`(全 omitempty);`ListHostKeys`/`ExportSnapshot`/`ExportSnapshotForProfile`/`ImportSnapshot` 全链携带;**ImportSnapshot 空新列归一 blob/tofu**(ADD COLUMN DEFAULT 不管显式插入)。

- [ ] **Step 1: 失败测试先行**(锚 spec §8 T9/T10):三列迁移(旧库升级 + 新库建表两态);双模比较四象限(blob-blob 等值/不等值、blob-呈现指纹、fingerprint-指纹串等值/不等值);`InsertForwardedPin`:成功落行(三列正确)+审计行同事务(**注入审计写失败 → 锚不落**,原子性);冲突路径 409 语义(equal 两态)+ 既有键原封不动;**并发 insert-only**(两 goroutine 同键不同值,恰一成功);`ApplyForwardedHostKey`:只读态可插/等值放行(含指纹锚)/不等值 error/写当代/元数据一致;`SaveHostKey` 只读态仍 `ErrReadOnly`(哨兵逐字);快照 round-trip 三新列;**v0.14 形态快照(无新列)导入 = blob/tofu 归一**;既有 readonly_test 全量零回归。
- [ ] **Step 2: 实现**(hostkeys.go 主体 + export.go 链 + store.go 迁移)。
- [ ] **Step 3:** scoped `go test ./internal/store/` 绿 → commit。

---

### Task 2: sshbroker 层——接口锚结构 + TOFU 双模 + 双指纹 mismatch

**Files:**
- Modify: `internal/sshbroker/hostkey.go`
- Test: `internal/sshbroker/hostkey_test.go` 追加;假体同步:`internal/conformance/differential_test.go`(conformance 唯一真接口假体)、`internal/sshbroker/hostkey_readonly_test.go`、`internal/mcpserver/tunnels_control_test.go`;store 侧直呼修复:`internal/store/hostkeys_test.go`、`internal/store/export_test.go:125` 一带、`internal/cli/gc_test.go:128`

**Interfaces:**
- `HostKeyStore.GetHostKey(host, port) (*Pin, error)`(返回锚结构;Task 1 的 Pin)。
- `HostKeyTOFU` 回调:双模比较(blob 锚 bytes.Equal / fingerprint 锚 `FingerprintSHA256(presented)` 等值);`ErrHostKeyMismatch` 文本携带双指纹(rider 2:`…presented <fp> != pinned <fp>…`,只增指纹值)。

- [ ] **Step 1: 失败测试先行**:双模匹配矩阵;指纹锚 + 呈现等值 → 放行零写;不等值 → `ErrHostKeyMismatch` **文本含两个指纹**(逐字断言前缀);blob 锚旧语义零回归;假体编译期全绿(本步即修复全部假体)。
- [ ] **Step 2: 实现**(比较逻辑单点函数,供 T1/T6 复用——若 T1 已落共享位置则引用,不二写)。
- [ ] **Step 3:** scoped `go test ./internal/sshbroker/ ./internal/conformance/ ./internal/mcpserver/ -run 'HostKey|TOFU|Mismatch|Tunnel'` 绿 + 全仓 `go build ./...` 绿(假体修复完成判据)→ commit。

---

### Task 3: serve 端点——handlePinHostkey 全守卫序

**Files:**
- Modify: `internal/mcpserver/serve.go`(HTTPHandler 分发 + handler)
- Test: `internal/mcpserver/serve_pin_test.go`(新)

**Interfaces:**
- `HTTPHandler` 新增 `/pin-hostkey` 分支:**同一个** `cacheAuth` 实例包裹(零新认证形态)。
- `handlePinHostkey`:spec §1.2 ①–⑥ **逐字落**——① method/64KiB/JSON/字段缺失 → 405/413/400;② GetCacheToken(存储故障 → 500+stderr;无行/未绑定 → 403);③ 匹配下沉 store 层(`ServersForProfile`→`GetServer` 逐条 host+port,显式 ORDER BY id;无候选 → 403 不回显轮廓);④ `ssh.ParsePublicKey` → 失败 400,成功 re-Marshal 规范化 + FingerprintSHA256;⑤ `InsertForwardedPin`(T1)→ equal?409 `{"error":"already pinned","equal":…}`(**不回显指纹**):201;⑥ 响应 fingerprint + server_name(多候选 = 审计归属条目)。审计 command 含 `host=<H>:<port> fp=… device=<名> via=forward affects=N entries: …`(owner 全量视角现算)。
- **每次请求(无论成败)stderr 一行**:`sshmgr serve: pin-hostkey <host>:<port> -> <status> (device <名>)`(仿 verifyCacheToken 惯例;探测痕迹最低保障)。

- [ ] **Step 1: 失败测试先行**(锚 spec §8 T1/T2/T2b/T3):201 全链(落行三列 + 审计行字段逐一 + affects 清单 + 响应指纹==落库指纹 + server_name 确定性);**并发不覆盖**(N 并发不同 key 恰一 201,其余 409 equal=false,胜者=落库键);既有等值锚 POST 同 key → 409 equal=true 原封不动;无候选/不在轮廓 → 403 不回显;**有效设备码未绑定 → 403**;**JSON 不可解码/缺字段 → 400**;项目令牌/坏设备码 → 401;非 POST → 405;超限 → 413;**畸形 key_blob → 400 且零落库**;GetCacheToken 故障 → 500;审计原子性(注入失败锚不落)。
- [ ] **Step 2: 实现**。
- [ ] **Step 3:** scoped `go test ./internal/mcpserver/ -run 'PinHostkey|Snapshot'` 绿(serve 既有测试零回归)→ commit。

---

### Task 4: 客户端转发器——HTTP 客户端 + 分支映射 + 全部文案

**Files:**
- Create: `internal/clientops/forward.go` + `internal/clientops/forward_test.go`
- Consumes: `pinningTransport`/`SplitTokenPin`(pin.go)、`CacheCred`/`ReadCacheCredFor`(clientops.go)

**Interfaces:**
- `Forwarder` 构造器:`NewPinForwarder(cred CacheCred) (*PinForwarder, error)`——pin 为空 → 构造出「无转发能力」实例(SaveHostKey 直接返回主文案错误);cred 缺失(ReadCacheCredFor nil)→ 同「无转发能力」。
- `(f *PinForwarder) SaveHostKey(host, port, blob) error` + `GetHostKey` 委托:spec §2.1 分支表**逐字落**——201 → 调用方 Apply(本 task 只返回信号,Apply 接线在 T5);400/413 透传附 host:port;401/403/404/409-equal=false/5xx/网络/10s 超时 → 各自逐字文案;409 equal=true → 特定哨兵值(供 T5 走 Apply 放行);令牌 SplitTokenPin 剥复合;不跟随重定向。
- 全部文案常量集中一处(spec §2.1 表 + §4 主文案,**逐字**,占位符规则:呈现指纹含 SHA256: 前缀、第二处不再带字面前缀)。

- [ ] **Step 1: 失败测试先行**(httptest 模拟 serve 各状态码):每分支文案逐字;明文拒绝文案;401 分支**不触碰任何 cache 文件**(缓存存活);超时注入(挂起 handler)→ 10s 上限;SplitTokenPin 复合令牌剥析。
- [ ] **Step 2: 实现**。
- [ ] **Step 3:** scoped `go test ./internal/clientops/ -run 'Forward'` 绿 → commit。

---

### Task 5: 接线——提供者注入 + 9 调用点 + RunStdioCache 加参

**Files:**
- Modify: `internal/mcpserver/server.go`(NewServerFromSource 提供者参数)、`internal/mcpserver/run.go`(RunStdioCache 加参 + 转发式实现构造)、`internal/cli/mcp.go`(`--instance` 处构造)
- Modify: `internal/mcpserver/{core,bgtools,context,relay}.go`(9 处 `HostKeyTOFU(st,…)` 改走提供者,穿参)
- Test: `internal/cli/mcp_cache_test.go` 追加(T4/T6/T7/T8/T8b 全景)、`internal/mcpserver/run_test.go` 追加(T5b)

**Interfaces:**
- `NewServerFromSource` 增主机密钥存储提供者(默认 = store 本体;vault 模式零变化——RunStdio 等既有调用方传默认)。
- 转发式实现(`mcpserver` 或 `clientops`):`GetHostKey` = 回调内 `holder.Current().GetHostKey`;`SaveHostKey` = Forwarder(T4)→ 201/409-equal-true → `ApplyForwardedHostKey`(T1)→ nil;其余 → 文案错误。**实时解析铁律落此**。
- `RunStdioCache(token, snap, auditPath, reload, forwardCtor)`——转发构造器由 cli 传入;`internal/cli/mcp.go` 在 `--instance` 解析后构造 `CacheCred` → `NewPinForwarder`。

- [ ] **Step 1: 失败测试先行**(锚 spec §8 T4–T8b):`mcp --cache` + testsshd + 进程内 serve 无锚 → `exec_command` **一次成功** + serve 侧锚/审计行 + **本进程后续连接零二次转发**;**T5b 换库代际**:201 与 Apply 间注入热重建 → 锚在当代、重连不白费;serve 关停 → §4 主文案**逐字**(占位符实值);路由不存在 → 404 文案逐字;**T8 两分支**:broker 预置等值真钥(含指纹锚形态)+陈旧快照 → 409 equal=true 自动放行零仪式;预置异锚 → equal=false 硬错 → pull → 重连 mismatch 双指纹;**T8b 挂起 serve → 10s 超时主文案不悬挂**;明文实例(pin 空)→ 明文文案;**cache.auth.json 缺失 → 主文案**;vault 模式(RunStdio)全部既有测试零回归。
- [ ] **Step 2: 实现**(穿参改造受控推进,每文件编译即验)。
- [ ] **Step 3:** scoped `go test ./internal/cli/ ./internal/mcpserver/` 绿 → commit。

---

### Task 6: owner CLI——servers pin-hostkey 六形态

**Files:**
- Create: `internal/cli/pinhostkey.go` + `internal/cli/pinhostkey_test.go`
- Modify: `internal/cli/servers.go`(注册)
- Consumes: `ParseKnownHostsLine`(conformance)、T1 store 原语、`ListServers`(受影响条目清单)、`ListHostKeys`(--list)

**Interfaces:** spec §3 六形态:
- 显示:`pin-hostkey <name>` → 指纹+格式+来源+设备+时间(无锚明说)。
- `--list`:全量锚清单,孤儿(无任何条目指向)标 `[orphan]`。
- `--fingerprint`:`SHA256:<base64 无填充>` 格式 + **base64 解码恰 32 字节**双校验;已有锚无 `--force` → 拒并显示现存指纹与来源。
- `--from-keyscan`:逗号拆 patterns、字面量匹配渲染形态(22 裸主机名/[host]:port)、`|1|` 哈希行与 `@` 前缀行跳过计数、无匹配错误列双形态、多算法匹配拒(文案指引首连转发或选算法)。
- `--clear`(与 `--force/--fingerprint/--from-keyscan` 互斥):有锚 → 删 + 输出/审计带受影响条目清单;**无锚 → 幂等成功不落审计**(`no pin present at <host>:<port> (nothing cleared)`)。
- `--clear --hostport <host>:<port>`(无位置参数):孤儿锚直达清除。
- `--force` = 唯一覆盖:**单事务 读旧→UPSERT(manual)→审计**(store 新组合 API,T1 同族);输出先旧后新指纹。
- 审计:`pin-manual`(含输入来源,force 含旧→新双指纹)/`pin-clear`(含指纹、来源、受影响清单)。

- [ ] **Step 1: 失败测试先行**(锚 spec §8 T11,逐形态):显示含来源/设备;`--list` 含 `[orphan]`;指纹**解码非 32 字节拒**(如截一位/改一字符);格式错拒;`--force` 审计行含事务内旧→新(并发注入验证事务性);`--clear` 有锚审计含清单/无锚幂等零审计行;`--hostport` 直达孤儿;keyscan 单行成功/多行拒/哈希行跳过计数/无匹配错误文案;成功输出统一格式逐字。
- [ ] **Step 2: 实现**。
- [ ] **Step 3:** scoped `go test ./internal/cli/ -run 'PinHostkey'` 绿 → commit。

---

### Task 7: doctor rider——probe 前 checkpoint

**Files:**
- Modify: `internal/cli/doctor.go`(copy-probe 段)
- Test: `internal/cli/doctor_test.go` 追加

**Interfaces:** 拷贝 store.db 前,对生产库开**免密钥、免迁移的裸 SQLite 连接**(`database/sql` 直开 + `PRAGMA wal_checkpoint(TRUNCATE)`;migrate-path 有开生产库先例;**不用** `store.Open`——建库/迁移/ACL 副作用;**永不**拷 -wal/-shm)。

- [ ] **Step 1: 失败测试先行**:WAL 有未 checkpoint 帧时(写后不 checkpoint)probe 计数 == `servers ls` 计数(修复前少一);checkpoint 失败 → 现有降级路径不恶化(FAIL 行,不假 PASS)。
- [ ] **Step 2: 实现**。
- [ ] **Step 3:** scoped `go test ./internal/cli/ -run 'Doctor'` 绿 → commit。

---

### Task 8: 文档 + backlog + 发版回写

**Files:**
- Modify: `docs/缓存.md`(或 multi-machine.md 缓存节)、配对/入门文档、`docs/backlog.md`、`docs/compat-matrix.md`(发版占位)、`README.md`/`docs/README.md` 交叉链接
- Content: spec §9 rider 3 全量——前置条件节「目标首次连接的发起位置」+「目标 broker 不可达怎么办」;锚定转发行为与来源元数据;**清毒完整时序**(revoke → `--list` 找转发锚含孤儿 → 逐条 `--clear` → 合法重锚 → **所有设备 pull 到位才算消毒完成** → **clear 后才打备份**);条目删除/改地址锚残留与 `--list`/`--hostport` 补救;跨会话 equal=true 自动闭合说明;混布窗口「possible MITM」假警报现象与升级解法。

- [ ] **Step 1:** 文档全量落笔(上列内容逐一)。
- [ ] **Step 2:** backlog 销项登记 + compat-matrix 发版占位注释(历史惯例)。
- [ ] **Step 3:** 交叉引用自查 → **全量 `go test ./...` 绿 + `gofmt -l` 空 + `go vet ./...` 净 + eval 零联动确认(无新工具)** → commit。

---

## 验收对账(发版门)

- CI:spec §8 T1–T13 全绿(含逐字文案、并发 insert-only、equal 两分支、换库代际、超时、审计原子性/清单)。
- 发版后真机(spec §12):反馈场景三步复跑、真离线新文案+带外锚定+回网闭环、跨会话 equal 自动放行、混布假警报抽查、doctor 计数一致、清毒演练(可选)。
