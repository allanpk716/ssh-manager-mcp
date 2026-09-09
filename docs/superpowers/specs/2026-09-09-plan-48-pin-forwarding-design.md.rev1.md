# Plan 48 设计(rev1):锚定转发与带外锚定——「仅缓存客户端可达」目标的首信闭环

> 反馈来源:`C:\WorkSpace\urit_things\sshmgr-feedback-cache-client-toufu-hostkey.md`(2026-09-09,v0.14.0,生化高速工控板 192.168.1.108 仅笔记本一块网卡可达)。2026-09-09 grilling 两轮拍板、本文不重议:**自动路径=锚定转发**(跳板首连独立立项进 backlog)、**带外锚定同批做**、**拒绝本地可写覆盖层**、**本地专属服务器条目非目标**、**单计划捆发 v0.15.0**(doctor 计数 rider 同批)、**护栏三条不可协商——仅可新增、仅在线转发、审计;默认全部有效设备码可转发,v1 不加按设备开关**、**命令行双输入(`--fingerprint` 主路径 / `--from-keyscan` 批量路径)+ owner `--force` 唯一覆盖通道**、**发版后真机验收,发版门含反馈场景的持续集成复现**。术语以根目录 `CONTEXT.md` 为准(锚定 / 锚定转发 / 带外锚定);取舍记录见 ADR 0002。
>
> rev1 变更(盲评第一轮三路——安全/代码事实/完备性——共 31 条发现,全部处置):**落锚改原子 insert-only**(rev0 的查→写两步存在并发覆盖竞态,且 `SaveHostKey` 现状是 UPSERT);**锚加来源元数据(`pin_source`/`pin_device`)并新增 `--clear`**(被盗设备 revoke 后的清毒原语);**owner `--force`/`--clear` 落审计**;**砍掉 409 自动重拉**(CacheReloader 只读盘、握手内嵌拉取继承隔离语义,承诺名不副实);**409 不回显指纹**(跨轮廓探测);**服务端先解析规范化再落库**(畸形密钥 400);**客户端分支表补 401/403 映射**;**HostKeyTOFU 回调确实要改**(rev0「本体不改」自相矛盾);**调用点计数修正为生产 10 处并点名 relay.go**;doctor rider 病因改写(WAL 少计,非凭据过滤);若干文案与测试钉死。

## 0. 目标与缺口

首次信任(锚定)同时需要**到目标的网络通路**与**可写的权威存储**。当前架构把二者分在两台机器:

| 侧 | 通路 | 写权限 | 结果 |
|---|---|---|---|
| 缓存客户端(`mcp --cache`) | 有(直连网卡到 192.168.1.x) | 无(快照只读,`SaveHostKey` → `ErrReadOnly`) | 握手成功后卡在「save host key」 |
| serve broker(NUC10) | 无(对目标网段无路由) | 有 | TCP 都到不了,首连无法发起 |

反馈另证实的三个事实,构成本设计的边界:

- **锚的归属键无条件是「主机:端口」**(`internal/store/hostkeys.go` 的 `hostKeyID`,连 22 端口也不裸写主机名)。反馈 §六 的端口桥接临时方案因此必然产生孤儿锚。锚与条目是**多对一现实的单向映射**:锚按「主机:端口」全局唯一,同地址多轮廓条目共享同一锚;条目改地址后旧锚不迁移、不回收,而转发上线后**任意轮廓内设备下次首连会对新地址静默重锚**(与既有首次信任语义一致,非新增行为,但值得明说;改地址后 owner 可用 `--clear` 清旧锚)。
- **v0.11.0 起 serve 只暴露 `/snapshot`(设备码闸)与 `/pair/*`(自闸配对面),项目令牌不再是远程凭据**;设备码强制绑定轮廓,`/snapshot` 按 `ExportSnapshotForProfile` 分发轮廓范围快照(`internal/mcpserver/serve.go:243`,锚按授予条目的「主机:端口」过滤,`internal/store/export.go:410-436`)。锚定转发的闸因此可以比「条目存在」更紧:**条目须在该设备码绑定轮廓的授予范围内**。
- 现有 `SaveHostKey` 是 UPSERT(`ON CONFLICT … DO UPDATE`,`hostkeys.go:34-38`)——写态下重新保存同键会静默覆盖。**「仅可新增」是新约束,不是现状**;本设计以新存储原语落实(§1.2 ⑤),不顺带收紧 vault 侧既有首次信任行为(写态唯一写者本就是本机进程,无并发面)。

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
{ "error": "already pinned" }

// 400 Bad Request(密钥解析失败/字段缺失/格式错)
{ "error": "unparseable key_blob" }
```

- `key_blob` 与 `HostKeyTOFU` 回调里 `remote.Marshal()` 的字节同源(客户端转发它在握手里实际看到的密钥,无任何加工)。`fingerprint` 用 `ssh.PublicKey.FingerprintSHA256` 规范形态(`SHA256:` 前缀 + base64 无填充),与 `ssh-keygen -lf` 输出一致。
- **请求体上限 = 新设 64 KiB 常量**(`MaxBytesReader`;仓库先例是 `/pair/*` 的 1 KiB 上限,`/snapshot` 是无 body 的 GET、无可复用上限),超限 413。
- **409 不回显现存指纹**:host_keys 是全局键,同「主机:端口」可同时在多个轮廓有条目,回显会构成跨轮廓锚状态探测(指纹虽可自连目标取得,但不必经此提供)。owner 侧可见性走 `servers pin-hostkey <name>`(§3)。

### 1.2 守卫执行序(钉死)

```
handlePinHostkey:
  ① method != POST → 405;请求体 > 64 KiB → 413
  ② TokenInfo → GetCacheToken → 绑定 profileID;空 → 403(fail-closed)
  ③ 匹配下沉 store 层:ServersForProfile(bound) 取全部 server id → 逐条 GetServer,
     host+port 精确相等者为候选(实现加显式 ORDER BY id,保证确定性);
     无候选 → 403(错误不回显轮廓内容)
  ④ ssh.ParsePublicKey(base64 解码后的 key_blob) → 失败 400;
     成功 → 以 re-Marshal 的规范化字节为落库值(与 remote.Marshal() 字节可比),
     并计算 FingerprintSHA256
  ⑤ 原子落锚(单事务,insert-only):
       INSERT INTO host_keys(host_port, key_blob, pin_format, pin_source, pin_device, created_at)
       VALUES(…, 'blob', 'forward', <设备码名>, …)
       ON CONFLICT(host_port) DO NOTHING;
     RowsAffected == 0 → 回滚本轮审计意图 → 409 {"error":"already pinned"}
     成功 → 同事务 writeAuditTx(复用既有「变更与其历史原子提交」模式,store/audit.go):
       action   = "pin-forward"
       server_id= 候选中 id 排序最小者
       project_id 空(设备发起,非项目)
       command  = "host=<host>:<port> fp=<SHA256:…> device=<设备码名> via=forward"
       status   = "ok"
  ⑥ 201 { "fingerprint": <④计算值>, "server_name": <③候选之名> }
```

- **401 语义**:无效或已吊销设备码由 `RequireBearerToken` 返回 401。**转发路径的 401 不触发 Plan 34 的缓存隔离**——隔离只活在拉取路径(`DoPull` 见 401 销毁本地缓存);吊销设备的下一次拉取自然会走到隔离。转发侧把 401 当普通失败处理(§2.1),避免「一次带错令牌的转发尝试误杀整个本地缓存」。
- **版本偏斜**:旧 broker(无此路由)落到 `http.NotFound` → 404,客户端解读见 §2.1。

## 2. 客户端侧:转发式主机密钥存储

### 2.1 分支映射表(钉死;错误文本逐字进 §4)

| broker 响应 / 错误 | 客户端行为 |
|---|---|
| **201** | `ApplyForwardedHostKey`(§2.3)写当前内存态 → 回调返回 nil → **握手当场继续**(体验核心:首连一次成功,不重试、不重拉) |
| **400 / 413** | 硬错;透传服务端文本并附 `<host>:<port>`(客户端发的密钥服务端解析不了,属缺陷态) |
| **401** | 硬错,文案:`device code rejected (invalid or revoked) — ask the owner to check cache-tokens; your local cache is NOT affected by this attempt`。**不触发隔离**(§1.2) |
| **403** | 硬错,文案:`the broker refused pin forwarding for <host>:<port> — the target's server entry is probably not granted to this device's bound profile; ask the owner to grant it (or rebind via cache-tokens bind) and cache pull again`(最高频运营成因:录入了目标但忘了 grant/绑定) |
| **404** | 硬错,文案:`the broker does not support pin forwarding (upgrade the broker to >= v0.15.0); out-of-band pinning also works — see servers pin-hostkey` |
| **409** | 硬错,文案:`the broker already has a pin for <host>:<port> — run cache pull and retry; if it still fails, ask the owner to compare fingerprints (servers pin-hostkey <name>)`。**不做自动重拉**(rev0 曾承诺「409 → reload → 复核」,砍掉:`CacheReloader.Check` 只做盘上 cache.bin 哈希比对不会联网,握手回调内嵌一次完整拉取又继承隔离与时间锚全套闸门——复杂度不抵收益;恢复路径 = 用户按指引拉取后重试,等值锚场景一轮即愈,§8 T8) |
| **5xx / 网络不可达 / 超时(10 秒)** | §4 主文案 |
| **pin 为空(明文 http)** | 拒绝发起转发,主文案变体(注明锚定转发要求钉扎传输层安全)。锚定转发是安全敏感变更通道,比拉取的明文逃生门更严是有意为之 |

HTTP 客户端构造:当前实例 `cache.auth.json` 的 `url`/`token`/`pin`(`CacheCred`,`internal/clientops/clientops.go:273`),令牌经 `SplitTokenPin` 剥离复合形态后走 `Authorization` 头,传输层用与 `DoPull` 同源的 `pinningTransport` + 不跟随重定向;转发超时 10 秒(它在 SSH 握手关键路径上)。

### 2.2 注入点与改动实况(如实,不称「机械」)

- `HostKeyStore` 接口(`internal/sshbroker/hostkey.go:17-21`)的 `GetHostKey` 返回升级为**锚结构**(密钥字节 + 格式)——双模比较需要格式;接口假体(`conformance/differential_test.go`、`sshbroker/hostkey*_test.go`、`mcpserver/tunnels_control_test.go`)同步。
- **`HostKeyTOFU` 回调本体要改**(rev0「本体不改」与 §5 自相矛盾,修正):比较升级为双模(密钥字节相等 / 指纹串等值),`ErrHostKeyMismatch` 文本携带双指纹(rider 2)。
- 生产调用点 **10 处**:`core.go` 5、`bgtools.go` 1、`context.go` 1、`relay.go` 2、`cli/ssh.go` 1。前 9 处(mcpserver 内)改走从 `NewServerFromSource` 构造处取得的主机密钥存储提供者——工具函数是只收 `*store.Store` 的包级函数,提供者需**穿参改造**(约 9 个签名 + 对应闭包,受控但非零改动,测试策略随之);`cli/ssh.go`(owner 命令,vault 可写态)保持直连存储不变。vault 模式提供者 = 存储本体,行为零变化。

### 2.3 本地内存态落锚的窄缝

`Store` 新增 `ApplyForwardedHostKey(host string, port int, marshaledKey []byte) error`:**唯一**在只读态被允许写 `host_keys` 的方法,契约钉死——insert-only;该「主机:端口」已有**等值**锚(字节相同,如热水合恰在 201 返回前带入同一锚)→ 视为成功放行(竞态宽容);**不等值**已有锚 → 返回错误(此时走 §2.1 409 分支文案);仅供转发回执使用;不写本地审计(broker 侧已记权威行)。`SaveHostKey` 本体在只读态**仍然**返回 `ErrReadOnly`(直连可测,护栏不松)。

## 3. 带外锚定命令(owner 侧):`servers pin-hostkey`

挂入 `newServersCmd`(`internal/cli/servers.go:15`),条目解析 `name-or-id`(`GetServerByName`,与 `rm`/`edit` 同款)。四种形态:

| 调用 | 语义 |
|---|---|
| `sshmgr servers pin-hostkey <name>` | **显示**当前锚:指纹 + 格式(密钥字节/指纹)+ **来源(自动首次信任/转发/手动)+ 转发设备名** + 登记时间;无锚则明说 |
| `… --fingerprint SHA256:… [--force]` | 按指纹锚定。格式严格 = OpenSSH 规范 `SHA256:<base64 无填充>`(前后空白剥除);已有锚且无 `--force` → 拒绝并显示现存指纹与来源 |
| `… --from-keyscan <file或-> [--force]` | 解析 known_hosts 行锚定(复用 `internal/conformance/knownhosts.go` 的 `ParseKnownHostsLine`) |
| `… --clear` | **删锚**(该条目地址上的锚整行移除,回到待首信态;下次任意合法路径首连重新锚定)。与 `--force`/`--fingerprint`/`--from-keyscan` 互斥。**清毒原语**:设备失窃 → revoke 设备码 → 逐条 `--clear` 其转发锚 → 合法路径重锚 |

- **keyscan 匹配规则(钉死)**:文件每行 patterns 按逗号拆分,元素与条目地址的 known_hosts 渲染形态(22 端口裸主机名,否则 `[host]:port`)字面相等才算匹配;`|1|…` 哈希主机名行与 `@cert-authority`/`@revoked` 前缀行**跳过并计数提示**(不支持,不误匹配);无匹配 → 错误列出文件内出现的形态与条目期望形态。
- **keyscan 多算法匹配 → 拒绝**:known_hosts 对同一主机常有多行(每算法一行),而存储模型是**一「主机:端口」一锚**;猜算法等于赌服务器下次呈现哪个,赌错=假性不匹配。拒绝文案指引自愈闭环:在能到达目标的机器上首连(触发自动转发),或从 `ssh-keyscan` 输出中选定一个算法用 `--fingerprint` 传入;若首连报「呈现指纹 ≠ 已锚指纹」,错误文本带双指纹,`--force` 补救。
- **owner 侧全部动作落审计**(沿用 `writeAuditTx`):`--fingerprint`/`--from-keyscan` 新锚 → `action="pin-manual"`(command 含 host:port + 新指纹 + 输入来源,`--force` 时含旧→新双指纹);`--clear` → `action="pin-clear"`(command 含被删锚的指纹与来源)。显示形态不落审计。
- **`--force` 是覆盖的唯一通道**:输出先显示旧指纹再显示新指纹,然后覆盖(含格式与来源切换为手动)。
- 成功输出统一为:`pinned <name> host=<host>:<port> fp=SHA256:… (format=<blob|fingerprint>, source=manual, forced=<bool>)`;`--clear` 输出 `unpinned <name> host=<host>:<port> (was fp=SHA256:…, source=<…>)`。

## 4. 只读 fail-closed 文案(逐字钉死)

替换现 `ErrReadOnly` 尾巴 `store is read-only (offline cache); connect to the server to mutate`(`store.go:65`,指代不明、不可执行——反馈 #4)。缓存模式下「未知锚且无法转发」的最终错误统一为(两处 `SHA256:…` 均为**呈现密钥指纹实值**——回调手里就有对端公钥,回填让 owner 可一键复制;`<name>` 不回填,owner 按「主机:端口」找条目):

```
host key for <host>:<port> is unknown and cannot be pinned here (presented fingerprint: SHA256:<呈现指纹>). Retry while the broker is reachable — the pin is then forwarded and audited automatically — or ask the owner to run: sshmgr servers pin-hostkey <name> --fingerprint SHA256:<呈现指纹>
```

其余分支文案见 §2.1 表(401/403/404/409/明文变体),主文案与各分支文案均**逐字断言**(§8)。错误文本只含快照内已有信息(host:port、指纹)+ 补救指引,无敏感泄露(盲评核对通过)。

## 5. 存储与快照:锚格式与来源双维(跨版本协议)

- `host_keys` 加三列(既有 guarded `ALTER TABLE ADD COLUMN … DEFAULT` 迁移模式,`store.go:221-223` 先例):
  - `pin_format TEXT NOT NULL DEFAULT 'blob'`(`blob` | `fingerprint`):指纹锚的 `key_blob` 列存指纹字符串字节;
  - `pin_source TEXT NOT NULL DEFAULT 'tofu'`(`tofu` | `forward` | `manual`):既有行与既有快照缺省即 `tofu`,天然回填;
  - `pin_device TEXT NOT NULL DEFAULT ''`:转发来源的设备码名(其余来源为空)。
- **比较语义双模**:`blob` → 字节相等;`fingerprint` → 呈现密钥的 `FingerprintSHA256` 与存串等值。sha256 抗碰撞,两种比较安全等价。
- `SnapshotHostKey` 加 `pin_format`/`pin_source`/`pin_device`(全部 `omitempty`;**缺省 = blob/tofu/空**,v0.14.0 及更早快照无缝兼容)。`ExportSnapshot` / `ExportSnapshotForProfile` / `ListHostKeys` / `ImportSnapshot` 全链携带。
- 旧客户端导入含指纹锚的新快照:不知格式的旧代码按密钥字节解读 → 比对必不匹配 → 拒绝。可接受的降级,但**其呈现形态是「possible MITM」假警报**(`hostkey.go:15` 既有文案)——混布窗口内每台旧设备连接即告警。rider 3 文档必须明写此现象与升级解法,防操作者对真告警脱敏。
- **known_hosts 序列化:仓库无生产调用方**(`FormatKnownHostsLine` 仅 conformance 测试引用),本 plan 无需处理;若未来引入生产序列化,须知指纹锚无密钥字节不可渲染。

## 6. 审计

- **broker 侧权威行**(§1.2 ⑤,与落锚同事务):`action="pin-forward"`。`sshmgr audit` 的 action 是自由文本,零结构变化直接可见。
- **owner 侧命令行**:`pin-manual` / `pin-clear`(§3,同样 `writeAuditTx` 原子)。覆盖两极中更危险的一极(owner 被骗 `--force` 洗白攻击者密钥)必须留痕——与 `projects` 轮换/删除落审计的仓库惯例对齐。
- **本地 sidecar**:维持现状——触发转发的执行本行按既有词汇表记 status;转发成败由 broker 权威行承载,不双记。

## 7. 版本与打包

- 捆发 **v0.15.0**(doctor rider 同批)。
- 兼容矩阵(如实):新客户端 + 旧 broker → 转发 404 → fail-closed 文案;**带外锚定同样要求 owner 侧先升到 v0.15.0**(命令本体在新版里),不存在免升级退路。旧客户端 + 新 broker → 指纹锚降级为「possible MITM」拒绝(§5,文档写明)。新新 → 全功能。

## 8. 测试矩阵

| # | 测试 | 断言 |
|---|---|---|
| T1 | `serve_pin_test.go`:合法设备码 + 条目在绑定轮廓内 | 201;store 落锚(`pin_format=blob`、`pin_source=forward`、`pin_device=设备名`);**审计行与锚同事务**(强制失败注入断言原子性);响应指纹 == 落库键指纹;server_name 正确 |
| T2 | **并发不覆盖**:对同一未锚「主机:端口」并发 N 个不同 key 的 POST | 恰一 201,其余 409;胜者键 = 落库键(insert-only 原语的核心断言,顺序版 T2a + 非确定性压力版 T2b 可选) |
| T3 | 无候选条目/不在绑定轮廓 → 403 且不回显轮廓;项目令牌与坏设备码 → 401;非 POST → 405;超限 → 413;**畸形 key_blob → 400 且零落库** | — |
| T4 | 客户端分支映射:403 文案指引 grant/绑定;401 文案断言 + **本地缓存未销毁**;pin 为空 → 拒绝文案 | — |
| T5 | `mcp --cache` 集成(testsshd + 进程内 serve):条目+凭据在快照、无锚 | `exec_command` **一次成功**;serve 侧出现锚+审计行;本进程后续连接无二次转发(内存态生效) |
| T6 | 同 T5 但 serve 关停 | 失败;错误 == §4 主文案**逐字**(含呈现指纹实值) |
| T7 | 同 T5 但路由不存在(旧 broker 模拟) | 失败;文案 == §2.1 404 行逐字 |
| T8 | **409 两分支**:① owner 已带外锚定**等值**真钥、客户端快照陈旧 → 转发 409 → 硬错文案 → 手动 `cache pull` → 重连成功(等值恢复路径);② broker 预置**不同**锚 → 409 → pull 后重连 → `ErrHostKeyMismatch` 文本带双指纹 | — |
| T9 | 存储层:三列迁移;双模比较;`ApplyForwardedHostKey` 只读态可插、等值放行、不等值报错;`SaveHostKey` 只读态仍 `ErrReadOnly` | — |
| T10 | 快照:三新列 round-trip;v0.14 形态快照(无新列)导入 = blob/tofu 兼容 | — |
| T11 | 命令行:显示(含来源/设备)/指纹锚定/格式错拒/`--force`(审计行含旧→新)/`--clear`(审计行)/单行 keyscan 成功/多行拒/`|1|` 哈希行跳过计数/无匹配错误文案 | — |
| T12 | doctor rider:探针前 checkpoint(或连 -wal/-shm 同拷)后,doctor 计数 == `servers ls` 计数 | — |
| T13 | 回归:`go test ./...` 全绿;`gofmt -l` 空;`go vet` 净;eval 主面零变化(锚定转发不新增任何 agent 可见工具) | — |

持续集成里的「broker 无路由」以逻辑等价方式覆盖(T5–T8 断言机制:转发、落库、审计、降级),物理不可达性由 §12 真机验收承担——测试进程在本机永远「有路由」,这是环境边界,如实记之。

## 9. riders(同批发版)

1. **doctor 计数差一**(反馈 #5;`doctor.go:414` copy-probe 报 11 台而 `servers ls` 12 台)。**病因(盲评修正)**:探针与 ls 两侧计数口径其实一致(均全量 servers);已文档化的少计机制是 probe 拷贝 store.db 时**不带 -wal/-shm**,未 checkpoint 的帧缺失 → 读到偏旧的一致快照(`doctor.go:323-329` 注释原文「undercount」)。修法:拷贝前 `Checkpoint()`(或连同 sidecar 文件一起拷),断言见 T12。
2. **mismatch 双指纹文案**(§2.2):`ErrHostKeyMismatch` 错误文本携带「呈现指纹 vs 已锚指纹」,把 `--force` 补救闭环。错误只增指纹值,不增任何其他密钥材料。
3. **文档**(反馈 #6):配对/入门文档新增前置条件节「目标首次连接的发起位置」与「目标 broker 不可达怎么办」;缓存操作文档补锚定转发行为、来源元数据、`--clear` 清毒流程(设备失窃 → revoke → clear → 重锚);**混布窗口现象**:pre-v0.15 客户端会把指纹锚报为「possible MITM」假警报,升级即解。

## 10. Non-goals(v1 明确不做)

本地可写覆盖层(零合并约束);本地专属服务器条目(权威不分裂);按设备的转发开关(未来硬化项:三列之外再加一列 + 一个命令行字段);离线排队/暂存转发(不留本地状态);**409 自动重拉**(砍掉,理由见 §2.1——恢复路径 = 指引拉取后重试);TUI/Web 管理界面里的锚定操作(miss 了再加);一「主机:端口」多锚 any-of;跳板首连(独立 backlog 立项);转发面主动告警/速率限制(补偿 = 来源元数据 + 审计 + `--clear`,见 ADR 0002 Consequences)。

## 11. 实施触点(文件级)

| 文件 | 变更 |
|---|---|
| `internal/mcpserver/serve.go` | 路径分发 + `/pin-hostkey` 分支(同一 `cacheAuth` 包裹);`handlePinHostkey`(§1.2 守卫序,含解析规范化与 insert-only) |
| `internal/store/store.go` | `host_keys` 三列迁移(`pin_format`/`pin_source`/`pin_device`) |
| `internal/store/hostkeys.go` | 新 insert-only 原语(带来源参数,与审计同事务由调用方组);`GetHostKey` 返回锚结构(字节+格式);`SaveHostKey` 保持(UPSERT,vault 首次信任路径);`ApplyForwardedHostKey`(只读态窄缝,等值放行) |
| `internal/store/export.go` | `SnapshotHostKey` 三新字段;`ListHostKeys`/两条导出路径/`ImportSnapshot` 携带 |
| `internal/store/audit.go` | 复用 `writeAuditTx`(无结构变化,确认签名适配) |
| `internal/sshbroker/hostkey.go` | `HostKeyStore.GetHostKey` 返回锚结构;`HostKeyTOFU` 回调双模比较;`ErrHostKeyMismatch` 文本带双指纹 |
| `internal/mcpserver/run.go` | `RunStdioCache` 接线:构造转发式实现(holder.Current + 实例设备码/pin),注入 `NewServerFromSource` |
| `internal/mcpserver/server.go` | `NewServerFromSource` 增加主机密钥存储提供者参数或等价注入缝 |
| `internal/mcpserver/{core,bgtools,context,relay}.go` | 9 处生产 `HostKeyTOFU` 调用改走提供者(穿参改造,受控;relay.go 的两处**含 Plan 47 中继目标侧拨号**,转发锚定对中继同样生效) |
| `internal/clientops/`(新文件) | 转发 HTTP 客户端(复用 `pinningTransport`/`SplitTokenPin`/`CacheCred`/实例路径);§2.1 分支映射与全部文案 |
| `internal/cli/pinhostkey.go`(新) | `servers pin-hostkey` 四形态(`--fingerprint`/`--from-keyscan`/`--force`/`--clear`)+ owner 审计 |
| `internal/cli/servers.go` | 注册子命令 |
| `internal/cli/doctor.go` | rider 1:probe 前 Checkpoint |
| 测试 | T1–T12 各就各位;接口假体更新(conformance×2、sshbroker hostkey 系、tunnels_control_test) |
| `docs/`(缓存/配对/README) | rider 3(含混布假警报与清毒流程) |

## 12. 真机验收(发版后)

NUC10(升级 v0.15.0,自更新通道)+ 笔记本(同步)+ 生化高速工控板(192.168.1.108,直连网卡):

1. 反馈 §二 三步原样复跑:① 笔记本 `exec_command` 一次成功;② NUC10 `sshmgr audit` 可见 `pin-forward` 行(设备名+指纹);③ `servers pin-hostkey 生化高速工控板` 显示已锚指纹一致、来源=forward、设备=笔记本。
2. 拔 VLAN(真离线)对**另一**新目标首连 → §4 新文案逐字(含呈现指纹);`ssh-keyscan` → NUC10 带外锚定 → 回网 `cache pull` → 连接成功。
3. 混布抽查:一台仍跑 v0.14 的客户端连接指纹锚目标 → 观察「possible MITM」假警报形态(留档即升级)。
4. NUC10 上 `doctor` 计数与 `servers ls` 口径一致(rider 1 真机面)。
5. (可选演练)revoke 笔记本设备码 → `--clear` 其转发锚 → 合法重锚,验证清毒闭环。

## 13. 参照

- 反馈文件(场景与证据):`C:\WorkSpace\urit_things\sshmgr-feedback-cache-client-toufu-hostkey.md`
- ADR 0002(边缘首信取舍,rev1 同步更新);ADR 0001(格式参照)
- Plan 12 设计 §3 决策 8(「离线未知锚 → fail-closed… a refresh resolves it」——本设计修订其「刷新可解」假设,修订理由即 §0 死锁)
- Plan 34(401 隔离语义,§1.2 的不触发约定);Plan 31(ServerInfo 打码/无端口——守卫 ③ 下沉 store 层的原因);Plan 39(轮廓范围快照);Plan 40(多实例路径);Plan 45/46(配对与实例——转发随实例设备码自动生效)
- `internal/conformance/knownhosts.go`(`--from-keyscan` 解析复用)
