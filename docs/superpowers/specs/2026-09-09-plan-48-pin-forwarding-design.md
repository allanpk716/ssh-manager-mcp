# Plan 48 设计:锚定转发与带外锚定——「仅缓存客户端可达」目标的首信闭环

> 反馈来源:`C:\WorkSpace\urit_things\sshmgr-feedback-cache-client-toufu-hostkey.md`(2026-09-09,v0.14.0,生化高速工控板 192.168.1.108 仅笔记本一块网卡可达)。2026-09-09 grilling 两轮拍板、本文不重议:**自动路径=锚定转发**(跳板首连独立立项进 backlog)、**带外锚定同批做**、**拒绝本地可写覆盖层**、**本地专属服务器条目非目标**、**单计划捆发 v0.15.0**(doctor 计数差一作同批发版 rider)、**护栏三条不可协商——仅可新增、仅在线转发、审计;默认全部有效设备码可转发,v1 不加按设备开关**、**命令行双输入(`--fingerprint` 主路径 / `--from-keyscan` 批量路径)+ owner `--force` 唯一覆盖通道**、**发版后真机验收,发版门含反馈场景的持续集成复现**。术语以根目录 `CONTEXT.md` 为准(锚定 / 锚定转发 / 带外锚定);取舍记录见 ADR 0002。

## 0. 目标与缺口

首次信任(锚定)同时需要**到目标的网络通路**与**可写的权威存储**。当前架构把二者分在两台机器:

| 侧 | 通路 | 写权限 | 结果 |
|---|---|---|---|
| 缓存客户端(`mcp --cache`) | 有(直连网卡到 192.168.1.x) | 无(快照只读,`SaveHostKey` → `ErrReadOnly`) | 握手成功后卡在「save host key」 |
| serve broker(NUC10) | 无(对目标网段无路由) | 有 | TCP 都到不了,首连无法发起 |

反馈另证实的两个事实,构成本设计的边界:

- **锚的归属键无条件是「主机:端口」**(`internal/store/hostkeys.go` 的 `hostKeyID`,连 22 端口也不裸写主机名)。反馈 §六 的端口桥接临时方案因此必然产生孤儿锚——桥接期间锚落在临时地址上,条目改回真实地址后本地查找返回空,首次信任重来。
- **v0.11.0 起 serve 只暴露 `/snapshot`(设备码闸)与 `/pair/*`(自闸配对面),项目令牌不再是远程凭据**;设备码强制绑定轮廓,`/snapshot` 按 `ExportSnapshotForProfile` 分发轮廓范围快照。锚定转发的闸因此可以比「条目存在」更紧:**条目须在该设备码绑定轮廓的授予范围内**。

目标:凡「broker 永不可达、某缓存客户端可达」的目标,在录入后由该客户端直接完成首连(零仪式);真离线或 owner 想人工把关时,有带外登记通道。

## 1. 锚定转发协议:`POST /pin-hostkey`(serve 侧)

`HTTPHandler` 的路径分发(`internal/mcpserver/serve.go:186`)新增一条:`/pin-hostkey` → 与 `/snapshot` **同一个** `cacheAuth`(`RequireBearerToken(verifyCacheToken)`)包裹 → `handlePinHostkey`。同一监听器、同一传输层安全、同一设备码闸;不新增任何认证形态。

### 1.1 请求 / 响应

```json
// POST /pin-hostkey   Authorization: Bearer <设备码>
{ "host": "192.168.1.108", "port": 22, "key_blob": "<base64(SSH wire format 主机公钥)>" }

// 201 Created
{ "fingerprint": "SHA256:…", "server_name": "生化高速工控板" }

// 409 Conflict(该「主机:端口」已有锚——仅可新增,绝不覆盖)
{ "error": "already pinned", "current_fingerprint": "SHA256:…" }
```

`key_blob` 与 `HostKeyTOFU` 回调里 `remote.Marshal()` 的字节同源(客户端转发它在握手里实际看到的密钥,无任何加工)。`fingerprint` 用 `ssh.PublicKey.FingerprintSHA256` 规范形态(`SHA256:` 前缀 + base64 无填充),与 `ssh-keygen -lf` 输出一致。

### 1.2 守卫执行序(钉死)

```
handlePinHostkey:
  ① method != POST → 405
  ② TokenInfo → GetCacheToken → 绑定 profileID;空 → 403(fail-closed)
  ③ 条目匹配:ListServersForProfile(bound) 中 host+port 精确相等者;
     无匹配 → 403(错误文本不回显轮廓内容——与既有 403 语义一致)
  ④ GetHostKey(host,port) 已有锚 → 409 + 现存指纹(键与值都不动)
  ⑤ SaveHostKey(锚格式='blob') + WriteAudit:
       action   = "pin-forward"
       server_id= 首个匹配条目(ORDER BY 保证确定性)
       project_id 空(设备发起,非项目)
       command  = "host=<host>:<port> fp=<SHA256:…> device=<设备码名>"
  ⑥ 201 + 落库指纹 + 条目名
```

- **401 语义**:无效或已吊销设备码由 `RequireBearerToken` 返回 401。**转发路径的 401 不触发 Plan 34 的缓存隔离**——隔离只活在拉取路径(`DoPull` 见 401 销毁本地缓存);吊销设备的下一次拉取自然会走到隔离。转发侧把 401 当普通失败处理,避免「一次带错令牌的转发尝试误杀整个本地缓存」。
- **版本偏斜**:旧 broker(无此路由)落到 `http.NotFound` → 404。客户端把裸 404 解读为「broker 过旧或路径不存在」,文案指引升级或带外锚定(§4)。
- **请求体上限**:与 `/snapshot` 相同的保守读取上限(实现取 64 KiB 足够——密钥 + 元数据远小于此),超限 413,防御滥发。

## 2. 客户端侧:转发式主机密钥存储

### 2.1 形态

新类型实现 `sshbroker.HostKeyStore` 接口(`GetHostKey`/`SaveHostKey` 两方法):

- **`GetHostKey`** 委托当前水合存储——通过 `cacheStoreHolder.Current` 取值,热水合重建后自动跟随新快照。
- **`SaveHostKey(host, port, blob)`** = 转发 → 落本地内存态:
  - 组请求,POST 到当前实例 `cache.auth.json` 的 `url`,令牌走 `Authorization` 头(`SplitTokenPin` 剥离可能的 `<码>:<pin>` 复合格式),传输层用与 `DoPull` 同源的 `pinningTransport`(同一 pin、同样不跟随重定向)。
  - **pin 为空(明文 http)→ 拒绝转发**。锚定转发必须钉扎传输层安全:它是一个安全敏感的变更通道,比拉取的 `--allow-plaintext` 逃生门更严是有意为之。
  - **201** → `ApplyForwardedHostKey(host, port, blob)` 写入当前内存态存储,回调返回 nil,**握手当场继续**(这是整个设计的体验核心:首连一次成功,不重试、不重拉)。
  - **409** → 触发一次 holder 的 `reload`(拉新快照 + 热水合)后重新取锚比对:一致 → 通过;仍无或仍异 → 硬错(文案指引 `cache pull` 后重试)。覆盖「另一台机器先一步转发了不同密钥」的竞态。
  - **404** → 「broker 不支持锚定转发(版本过旧?)」fail-closed 文案。
  - **网络不可达 / 超时 / 5xx** → §4 的统一 fail-closed 文案。
  - 转发请求带 10 秒超时:它在 SSH 握手的关键路径上,不能让一个挂死的 broker 拖死整个连接。

### 2.2 注入点

`NewServerFromSource` 增加主机密钥存储的提供者(默认 = 存储本体,vault 模式行为零变化);缓存模式注入转发式实现。工具层 16 处 `HostKeyTOFU(st, srv.Host, srv.Port)` 调用点(`core.go`/`bgtools.go`/`context.go` 等)统一改走该提供者——机械替换,语义由提供者单点决定。`HostKeyTOFU` 本体不改:接口两方法就是它的全部依赖,这正是 `HostKeyStore` 接口存在的意义。

### 2.3 本地内存态落锚的窄缝

`Store` 新增 `ApplyForwardedHostKey(host string, port int, marshaledKey []byte) error`:**唯一**在只读态被允许写 `host_keys` 的方法,契约钉死——只允许插入(该「主机:端口」已有锚时返回错误,客户端此时应走 409 分支而不是调它);仅供转发回执使用;不写审计(broker 侧已记权威行)。`SaveHostKey` 本体在只读态**仍然**返回 `ErrReadOnly`(直连可测,护栏不松)。

## 3. 带外锚定命令(owner 侧):`servers pin-hostkey`

挂入 `newServersCmd`(`internal/cli/servers.go:15`),条目解析 `name-or-id`(`GetServerByName`,与 `rm`/`edit` 同款)。三种形态:

| 调用 | 语义 |
|---|---|
| `sshmgr servers pin-hostkey <name>` | **显示**当前锚:指纹 + 格式(密钥字节/指纹)+ 登记时间;无锚则明说 |
| `… --fingerprint SHA256:… [--force]` | 按指纹锚定。格式严格 = OpenSSH 规范 `SHA256:<base64 无填充>`(前后空白剥除);已有锚且无 `--force` → 拒绝并**显示现存指纹** |
| `… --from-keyscan <file或-> [--force]` | 解析 known_hosts 行锚定(复用 `internal/conformance/knownhosts.go` 的 `ParseKnownHostsLine`),按条目 host:port 匹配渲染形态(22 端口裸主机名,否则 `[host]:port`) |

- **`--force` 是唯一覆盖通道**:输出先显示旧指纹再显示新指纹,然后覆盖(含格式切换)。转发的锚永无覆盖权(§1.2 ④),构成「设备可增不可改、owner 可改」的两极。
- **keyscan 多算法匹配 → 拒绝**:known_hosts 对同一主机常有多行(每算法一行),而存储模型是**一「主机:端口」一锚**;猜算法等于赌服务器下次呈现哪个,赌错=假性不匹配。拒绝文案指引自愈闭环:在能到达目标的机器上首连(触发自动转发),或从 `ssh-keyscan` 输出中选定一个算法用 `--fingerprint` 传入;若首连报「呈现指纹 ≠ 已锚指纹」,错误文本(rider,§9)带双指纹,`--force` 补救。
- 成功输出统一为:`pinned <name> host=<host>:<port> fp=SHA256:… (format=<blob|fingerprint>, forced=<bool>)`。

## 4. 只读 fail-closed 文案(逐字钉死)

替换现 `ErrReadOnly` 尾巴 `store is read-only (offline cache); connect to the server to mutate`(指代不明、不可执行——反馈 #4)。缓存模式下「未知锚且无法转发」的最终错误统一为:

```
host key for <host>:<port> is unknown and cannot be pinned here (offline cache, broker unreachable or too old). Retry while the broker is reachable — the pin is then forwarded and audited automatically — or ask the owner to run: sshmgr servers pin-hostkey <name> --fingerprint SHA256:… (fingerprint via ssh-keyscan on any machine that can reach the target)
```

分支文案:`404` → `… broker does not support pin forwarding (upgrade the broker to >= v0.15.0) or pin out-of-band …`;`409 残留`(reload 后仍异)→ 指引 `cache pull` 后重试。测试对主文案**逐字断言**。

## 5. 存储与快照:锚格式双模(跨版本协议)

- `host_keys` 表加列 `pin_format TEXT NOT NULL DEFAULT 'blob'`(值 `blob` | `fingerprint`),走既有加列迁移模式。指纹锚的 `key_blob` 列存指纹字符串的字节。
- **比较语义双模**:`blob` → 字节相等;`fingerprint` → 呈现密钥的 `FingerprintSHA256` 与存串等值比较。sha256 抗碰撞,两种比较安全等价。`GetHostKey` 的返回随之携带格式(接口形状由实现期定——`HostKeyStore` 仅两方法,涟漪可控,语义钉死即可)。
- `SnapshotHostKey` 加 `pin_format`(`omitempty`;**缺省=blob**,v0.14.0 及更早快照无缝兼容)。`ExportSnapshot` / `ExportSnapshotForProfile` / `ListHostKeys` / `ImportSnapshot` 全链携带。
- 旧客户端导入含指纹锚的新快照:不知格式的旧代码按密钥字节解读 → 比对必不匹配 → 拒绝。可接受的降级(升级即解),文档写明。
- **known_hosts 序列化**:指纹锚没有密钥字节,不可渲染 → 跳过并计数提示(实现期确认 `FormatKnownHostsLine` 的全部调用方)。

## 6. 审计

- **broker 侧权威行**(§1.2 ⑤):`action="pin-forward"`。`sshmgr audit` 的 action 是自由文本,零结构变化直接可见。
- **本地 sidecar**:维持现状——触发转发的执行本行按既有词汇表记 status;转发成败由 broker 权威行承载,不双记。

## 7. 版本与打包

- 捆发 **v0.15.0**(doctor 计数 rider 同批)。
- 兼容矩阵:新客户端 + 旧 broker → 转发 404 → fail-closed 文案(可继续用带外锚定,但命令在 owner 侧本就需要新版本);旧客户端 + 新 broker → 指纹锚降级为拒绝(§5);新新 → 全功能。

## 8. 测试矩阵

| # | 测试 | 断言 |
|---|---|---|
| T1 | `serve_pin_test.go`:合法设备码 + 条目在绑定轮廓内 | 201;store 落锚(blob);审计行 `pin-forward`(server_id/设备名/指纹);响应指纹 == 落库键指纹 |
| T2 | 同上但该「主机:端口」已有锚 | 409;**键与值原封不动**;响应带现存指纹 |
| T3 | 条目不在绑定轮廓 / 无匹配条目 | 403;错误不回显轮廓内容 |
| T4 | 项目令牌打 `/pin-hostkey` | 401(两闸延伸断言);无效/吊销设备码 → 401 |
| T5 | 非 POST → 405;超限请求体 → 413 | — |
| T6 | `mcp --cache` 集成(复用 testsshd + 进程内 serve):条目+凭据在快照、无锚 | `exec_command` **一次成功**;serve 侧 store 出现锚 + 审计行;本进程后续连接无二次转发(内存态已生效) |
| T7 | 同上但 serve 关停 | 失败;错误文案 == §4 主文案(逐字) |
| T8 | 同上但 serve 路由不存在(模拟旧 broker) | 失败;文案含 `does not support pin forwarding` |
| T9 | broker 预置**不同**锚后客户端首连 | 409 → reload → 比对异 → `ErrHostKeyMismatch`,文本带双指纹(rider) |
| T10 | 存储层:pin_format 迁移/双模比较/`ApplyForwardedHostKey` 只读态可插、重复插报错、`SaveHostKey` 只读态仍 `ErrReadOnly` | — |
| T11 | 快照:指纹锚 round-trip(导出→导入→比较通过);**v0.14 形态快照(无 pin_format)导入 = blob 语义不变** | — |
| T12 | 命令行:显示/指纹锚定/`--force` 覆盖(旧→新指纹都打印)/格式错误的指纹拒绝/keyscan 单行成功/多行拒绝 | — |
| T13 | 回归:`go test ./...` 全绿;`gofmt -l` 空;`go vet` 净;eval 主面零变化(锚定转发不新增任何 agent 可见工具) | — |

持续集成里的「broker 无路由」以逻辑等价方式覆盖(T6–T9 断言机制:转发、落库、审计、降级),物理不可达性由 §12 真机验收承担——测试进程在本机永远「有路由」,这不是缺陷而是环境边界,如实记之。

## 9. riders(同批发版)

1. **doctor 计数差一**(反馈 #5;`doctor.go:414` copy-probe 报 11 台而 `servers ls` 12 台)。假设:探针只统计持有凭据的条目,而库中存在一条无凭据条目(Plan 20 C0 允许),差一即它。修复以断言钉死:要么计数与 `servers ls` 相等,要么 Detail 写明口径(「N of M servers(无凭据条目不计)」)。
2. **mismatch 双指纹文案**(§3):`ErrHostKeyMismatch` 错误文本携带「呈现指纹 vs 已锚指纹」,把 `--force` 补救闭环。错误只增指纹值,不增任何其他密钥材料。
3. **文档**(反馈 #6):配对/入门文档新增前置条件节「目标首次连接的发起位置」与「目标 broker 不可达怎么办」;缓存操作文档补锚定转发行为与指纹锚说明。

## 10. Non-goals(v1 明确不做)

本地可写覆盖层(零合并约束);本地专属服务器条目(权威不分裂);按设备的转发开关(未来硬化项:一列 schema + 一个命令行字段);离线排队/暂存转发(不留本地状态);TUI/Web 管理界面里的锚定操作(miss 了再加);一「主机:端口」多锚 any-of;跳板首连(独立 backlog 立项——通用跳板能力自成一课)。

## 11. 实施触点(文件级)

| 文件 | 变更 |
|---|---|
| `internal/mcpserver/serve.go` | 路径分发 + `/pin-hostkey` 分支(同一 `cacheAuth` 包裹);`handlePinHostkey`(§1.2 守卫序) |
| `internal/store/store.go` | `host_keys` 加列迁移(`pin_format`) |
| `internal/store/hostkeys.go` | `SaveHostKey` 带格式;`GetHostKey` 带格式返回;`ApplyForwardedHostKey`(只读态窄缝,仅插不改) |
| `internal/store/export.go` | `SnapshotHostKey.PinFormat`;`ListHostKeys`/两条导出路径/`ImportSnapshot` 携带 |
| `internal/sshbroker/hostkey.go` | 回调双模比较;`ErrHostKeyMismatch` 文本带双指纹 |
| `internal/mcpserver/run.go` | `RunStdioCache` 接线:构造转发式实现(holder.Current + 实例设备码/pin),注入 `NewServerFromSource` |
| `internal/mcpserver/{core,bgtools,context,…}.go` | 16 处 `HostKeyTOFU(st,…)` 改走提供者(机械) |
| `internal/clientops/`(新文件) | 转发 HTTP 客户端构造(复用 `pinningTransport`/`SplitTokenPin`/实例路径解析);错误映射 |
| `internal/cli/pinhostkey.go`(新) | `servers pin-hostkey` 三形态 + `--force` |
| `internal/cli/servers.go` | 注册子命令 |
| `internal/cli/doctor.go` | rider 1:计数口径修复 |
| 测试 | T1–T12 各就各位;`readonly_test.go` 补 `ApplyForwardedHostKey` 断言 |
| `docs/`(缓存/配对/README) | rider 3 |

## 12. 真机验收(发版后)

NUC10(升级 v0.15.0,自更新通道)+ 笔记本(同步)+ 生化高速工控板(192.168.1.108,直连网卡):

1. 反馈 §二 三步原样复跑:① 笔记本 `exec_command` 一次成功;② NUC10 `sshmgr audit` 可见 `pin-forward` 行(设备名+指纹);③ `servers pin-hostkey 生化高速工控板` 显示已锚指纹一致。
2. 拔 VLAN(真离线)对**另一**新目标首连 → §4 新文案逐字;`ssh-keyscan` → NUC10 带外锚定 → 回网 `cache pull` → 连接成功。
3. NUC10 上 `doctor` 计数与 `servers ls` 口径一致(rider 1 真机面)。

## 13. 参照

- 反馈文件(场景与证据):`C:\WorkSpace\urit_things\sshmgr-feedback-cache-client-toufu-hostkey.md`
- ADR 0002(边缘首信取舍);ADR 0001(格式参照)
- Plan 12 设计 §3 决策 8(「离线未知锚 → fail-closed」——本设计修订其「刷新可解」假设,修订理由即 §0 死锁)
- Plan 34(401 隔离语义,§1.2 的不触发约定);Plan 39(轮廓范围快照);Plan 40(多实例路径);Plan 45/46(配对与实例——转发随实例设备码自动生效)
- `internal/conformance/knownhosts.go`(`--from-keyscan` 解析复用)
