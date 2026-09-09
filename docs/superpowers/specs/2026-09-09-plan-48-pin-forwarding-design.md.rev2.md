# Plan 48 设计(rev2):锚定转发与带外锚定——「仅缓存客户端可达」目标的首信闭环

> 反馈来源:`C:\WorkSpace\urit_things\sshmgr-feedback-cache-client-toufu-hostkey.md`(2026-09-09,v0.14.0,生化高速工控板 192.168.1.108 仅笔记本一块网卡可达)。2026-09-09 grilling 两轮拍板、本文不重议:**自动路径=锚定转发**(跳板首连独立立项进 backlog)、**带外锚定同批做**、**拒绝本地可写覆盖层**、**本地专属服务器条目非目标**、**单计划捆发 v0.15.0**(doctor 计数 rider 同批)、**护栏三条不可协商——仅可新增、仅在线转发、审计;默认全部有效设备码可转发,v1 不加按设备开关**、**命令行双输入(`--fingerprint` 主路径 / `--from-keyscan` 批量路径)+ owner `--force` 唯一覆盖通道**、**发版后真机验收,发版门含反馈场景的持续集成复现**。术语以根目录 `CONTEXT.md` 为准(锚定 / 锚定转发 / 带外锚定);取舍记录见 ADR 0002。
>
> **rev2 变更**(第二轮盲评两路——安全/一致性——共 22 条,全部处置;rev1 变更见文末附录):**409 响应加 `equal` 布尔判定**(提交者持有自己提交的密钥,学习「权威是否同意」零泄露;`equal=true` → 客户端本地落锚放行——同时闭合跨会话重复转发与并发等值竞态两个场景);**`ApplyForwardedHostKey` 的「等值」从字节比较改为与全系统一致的双模语义**;**哨兵文本不动、新文案由转发包装层组装**(rev1 误把共享哨兵当修改点);**转发构造参数从 cli/mcp.go 传入**(`--instance` 掌握方,禁止 run.go 默认实例兜底);**读写在回调内实时解析 `holder.Current`,禁止构造期捕获 store 指针**;**`--clear` 输出与审计带受影响条目清单**(地址全局粒度可见化);**导出组合式落锚 API**(`writeAuditTx` 未导出,「调用方组事务」不可实现);**GetCacheToken 存储故障回 500 不压进 403**;多候选确定性(`server_name` 与审计同条目);明文拒绝文案逐字给出;模板双前缀钉死;ADR 投毒范围改「入口受轮廓约束、效果全局」;测试矩阵补透传/未绑定/超时/换库代际变体。

## 0. 目标与缺口

首次信任(锚定)同时需要**到目标的网络通路**与**可写的权威存储**。当前架构把二者分在两台机器:

| 侧 | 通路 | 写权限 | 结果 |
|---|---|---|---|
| 缓存客户端(`mcp --cache`) | 有(直连网卡到 192.168.1.x) | 无(快照只读,`SaveHostKey` → `ErrReadOnly`) | 握手成功后卡在「save host key」 |
| serve broker(NUC10) | 无(对目标网段无路由) | 有 | TCP 都到不了,首连无法发起 |

反馈另证实的三个事实,构成本设计的边界:

- **锚的归属键无条件是「主机:端口」**(`internal/store/hostkeys.go` 的 `hostKeyID`,连 22 端口也不裸写主机名)。反馈 §六 的端口桥接临时方案因此必然产生孤儿锚。锚与条目是**多对一现实的单向映射**:锚按「主机:端口」全局唯一,同地址多轮廓条目共享同一锚;条目改地址后旧锚不迁移、不回收(条目**删除**后锚同样残留——同地址重新录入即静默继承,期间服务器换钥则首连假性不匹配,`--clear` 是补救)。转发上线后,轮廓内设备可对**未锚定地址**静默落锚(含条目改址后的重锚)——重锚语义与既有首次信任一致,**新增的是「谁能触发它」**(这正是 ADR 0002 如实承认的投毒增量,措辞不再用「非新增行为」)。
- **v0.11.0 起 serve 只暴露 `/snapshot`(设备码闸)与 `/pair/*`(自闸配对面),项目令牌不再是远程凭据**;设备码强制绑定轮廓,`/snapshot` 按 `ExportSnapshotForProfile` 分发轮廓范围快照(`internal/mcpserver/serve.go:243`,锚按授予条目的「主机:端口」过滤,`internal/store/export.go:410-436`)。锚定转发的闸因此可以比「条目存在」更紧:**条目须在该设备码绑定轮廓的授予范围内**。注意入站闸受轮廓约束、**落锚效果是全局的**(锚键无轮廓维度,同地址其他轮廓条目同样被影响——见 §1.2 ③)。
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
// equal = 既有锚与提交密钥按 §5 双模语义是否等值(仅布尔,不回显指纹)
{ "error": "already pinned", "equal": false }

// 400 Bad Request(密钥解析失败/字段缺失/格式错)
{ "error": "unparseable key_blob" }
```

- `key_blob` 与 `HostKeyTOFU` 回调里 `remote.Marshal()` 的字节同源(客户端转发它在握手里实际看到的密钥,无任何加工)。`fingerprint` 用 `ssh.PublicKey.FingerprintSHA256` 规范形态(`SHA256:` 前缀 + base64 无填充),与 `ssh-keygen -lf` 输出一致。
- **请求体上限 = 新设 64 KiB 常量**(`MaxBytesReader`;仓库先例是 `/pair/*` 的 1 KiB 上限,`/snapshot` 是无 body 的 GET、无可复用上限),超限 413。
- **409 不回显指纹,但带 `equal` 布尔**:提交者提交的是自己手里的密钥,学习「权威既有锚是否与之等值」不泄露既有锚的任何可利用信息(攻击者只能测试自己持有的公钥,猜中受害主机公钥在计算上不可行)。`equal=true` 是跨会话重复转发与并发等值竞态的自动闭合键(§2.1)。锚存在性与等值性在请求者自己的轮廓快照里本就可见,不构成新探测信道。
- **`server_name` 确定性**:多候选(同「主机:端口」在绑定轮廓内有多条授予条目)时,与审计行归属**同一条目**(候选中 id 排序最小者,§1.2 ③⑤)。

### 1.2 守卫执行序(钉死)

```
handlePinHostkey:
  ① method != POST → 405;请求体 > 64 KiB → 413
  ② TokenInfo → GetCacheToken:
       存储故障 → 500 + stderr(不压进 403——沿用 handleSnapshot 的教训注释
       serve.go:224-233:403 会诱导 owner 白跑 cache-tokens bind 而 DB 错误被埋)
       无 TokenInfo/无该行 → 403(fail-closed)
       有行但绑定 profileID 为空 → 403
  ③ 匹配下沉 store 层:ServersForProfile(bound) 取全部 server id → 逐条 GetServer,
     host+port 精确相等者为候选(显式 ORDER BY id,保证确定性);
     无候选 → 403(错误不回显轮廓内容)。
     注:入站闸在轮廓内,落锚效果全局(锚键无轮廓维度,同地址其他轮廓条目
     共享该锚——owner 的检测面是快照中的 pin_source/pin_device)
  ④ ssh.ParsePublicKey(base64 解码后的 key_blob) → 失败 400;
     成功 → 以 re-Marshal 的规范化字节为落库值(与 remote.Marshal() 字节可比),
     并计算 FingerprintSHA256
  ⑤ 原子落锚——**新导出组合式 store API(单事务,内部实现)**:
       tx: INSERT INTO host_keys(host_port, key_blob, pin_format, pin_source, pin_device, created_at)
           VALUES(…, 'blob', 'forward', <设备码名>, …) ON CONFLICT(host_port) DO NOTHING;
       RowsAffected == 0 → 中止事务(此时审计行尚未写入,无回滚需要) → 读取既有锚
         → 按 §5 双模语义算 equal → 409 {"error":"already pinned","equal":…}
       RowsAffected > 0 → 同事务 writeAuditTx(复用既有「变更与其历史原子提交」模式):
           action   = "pin-forward"
           server_id= 候选中 id 排序最小者
           project_id 空(设备发起,非项目;审计巡检面影响见 §6)
           command  = "host=<host>:<port> fp=<SHA256:…> device=<设备码名> via=forward"
           status   = "ok"
         → commit
  ⑥ 201 { "fingerprint": <④计算值>, "server_name": <⑤审计归属条目之名> }
```

- **为何组合式导出 API**:`writeAuditTx` 是 store 包内私有(`internal/store/audit.go:51`),mcpserver 调用方无法自行组装同事务对;新 API(形如 `InsertForwardedPin(...) (equal bool, err error)`)把 insert-only + 同事务审计封装在 store 层,调用方只见结果。
- **401 语义**:无效或已吊销设备码由 `RequireBearerToken` 返回 401。**转发路径的 401 不触发 Plan 34 的缓存隔离**——隔离只活在拉取路径(`DoPull` 见 401 销毁本地缓存);吊销设备的下一次拉取自然会走到隔离。转发侧把 401 当普通失败处理(§2.1),避免「一次带错令牌的转发尝试误杀整个本地缓存」。
- **版本偏斜**:旧 broker(无此路由)落到 `http.NotFound` → 404,客户端解读见 §2.1。

## 2. 客户端侧:转发式主机密钥存储

### 2.1 分支映射表(钉死;错误文本逐字进 §4)

| broker 响应 / 错误 | 客户端行为 |
|---|---|
| **201** | `ApplyForwardedHostKey`(§2.3)写**回调内实时解析的当前存储** → 回调返回 nil → **握手当场继续**(体验核心:首连一次成功) |
| **400 / 413** | 硬错;透传服务端文本并附 `<host>:<port>`(客户端发的密钥服务端解析不了,属缺陷态) |
| **401** | 硬错,文案:`device code rejected (invalid or revoked) — ask the owner to check cache-tokens; your local cache is NOT affected by this attempt`。**不触发隔离**(§1.2) |
| **403** | 硬错,文案:`the broker refused pin forwarding for <host>:<port> — the target's server entry is probably not granted to this device's bound profile; ask the owner to grant it (or rebind via cache-tokens bind) and cache pull again`(最高频运营成因:录入了目标但忘了 grant/绑定) |
| **404** | 硬错,文案:`the broker does not support pin forwarding (the owner must upgrade the broker to >= v0.15.0, then retry)`(rev1 曾指引 `servers pin-hostkey`,但 404 场景意味着 owner 侧同体未升级、该命令尚不存在,指引不可执行——已改) |
| **409, `equal=true`** | **自动闭合**:本地 `ApplyForwardedHostKey`(权威已持等值锚)→ 回调返回 nil → 握手继续。覆盖两个场景:①同进程并发首连(exec + forward / relay 目标侧拨号并行打同一新目标,后到者 409);②**跨会话重复转发**(进程退出内存态即失、下次 spawn 缓存仍新鲜未含该锚 → 重新转发 → 409 equal=true → 放行;代价只是一次多余的 POST,零用户仪式) |
| **409, `equal=false`** | 硬错,文案:`the broker already has a different pin for <host>:<port> — run cache pull and retry; if it still fails, ask the owner to compare fingerprints (servers pin-hostkey)`。恢复路径 = 拉取后重试;拉到权威锚后按双模比较自会分晓(等值即通过、异值报双指纹不匹配) |
| **5xx / 网络不可达 / 超时(10 秒)** | §4 主文案 |
| **pin 为空(明文 http)** | 拒绝发起转发,逐字文案:`pin forwarding requires a pinned TLS server — no plaintext forwarding (set the server pin used by cache pull); the host key for <host>:<port> stays unpinned`。锚定转发是安全敏感变更通道,比拉取的明文逃生门更严是有意为之 |

HTTP 客户端构造:**由 `internal/cli/mcp.go`(掌握 `--instance` 解析处)构造并传入 `RunStdioCache`**(签名加参:转发构造器或等价注入)——`RunStdioCache`(`internal/mcpserver/run.go:230`)现签名无实例通路,若在 run.go 内默认实例兜底会静默错拿 `CacheCred`,违反 Plan 40/46「读实例永远显式」铁律。构造内容:当前实例 `cache.auth.json` 的 `url`/`token`/`pin`(`CacheCred`,`internal/clientops/clientops.go:273`),令牌经 `SplitTokenPin` 剥离复合形态后走 `Authorization` 头,传输层用与 `DoPull` 同源的 `pinningTransport` + 不跟随重定向;转发超时 10 秒(它在 SSH 握手关键路径上)。

**不做 409 自动重拉**(rev0 曾承诺,rev1 砍掉,理由如实重述):`CacheReloader.Check` 只能显影**已在盘上**的 cache.bin 变化——其 unchanged 分支的惰性拉取受 maxAge+退避双闸,不保证同步取回新快照;而握手回调内嵌一次强制完整拉取会继承隔离与时间锚全套闸门。`equal` 判定已覆盖自动闭合的正当场景,剩余 `equal=false` 由用户按文案一轮恢复。

### 2.2 注入点与改动实况(如实,不称「机械」)

- `HostKeyStore` 接口(`internal/sshbroker/hostkey.go:17-21`)的 `GetHostKey` 返回升级为**锚结构**(密钥字节 + 格式)——双模比较需要格式;接口假体(`conformance/differential_test.go`、`sshbroker/hostkey*_test.go`、`mcpserver/tunnels_control_test.go`)与直呼 store 方法的 store 侧测试(`internal/store/hostkeys_test.go`、`export_test.go:125`、`internal/cli/gc_test.go:128`)同步。
- **`HostKeyTOFU` 回调本体要改**(rev0「本体不改」与 §5 自相矛盾,rev1 修正):比较升级为双模(密钥字节相等 / 指纹串等值),`ErrHostKeyMismatch` 文本携带双指纹(rider 2)。
- 生产调用点 **10 处**:`core.go` 5、`bgtools.go` 1、`context.go` 1、`relay.go` 2、`cli/ssh.go` 1。前 9 处(mcpserver 内)改走从 `NewServerFromSource` 构造处取得的主机密钥存储提供者——工具函数是只收 `*store.Store` 的包级函数,提供者需**穿参改造**(约 9 个签名 + 对应闭包,受控但非零改动,测试策略随之);`cli/ssh.go`(owner 命令,vault 可写态)保持直连存储不变。vault 模式提供者 = 存储本体,行为零变化。
- **实时解析铁律(钉死)**:转发式实现的读与写都必须在 `HostKeyTOFU` 回调内**实时**经 `holder.Current` 解析当前存储——禁止构造期捕获 store 指针。否则转发 HTTP 调用(≤10 s)夹在 Get 与 Apply 之间的窗口里,任何并发工具触发的热重建会换下临时库,锚写进已被换下的代际 → 下次连接重新转发 → 白费;本铁律同时决定 `ApplyForwardedHostKey` 落当代、读亦当代(§2.3)。§8 T5 变体断言之。

### 2.3 本地内存态落锚的窄缝

`Store` 新增 `ApplyForwardedHostKey(host string, port int, marshaledKey []byte) error`:**唯一**在只读态被允许写 `host_keys` 的方法,契约钉死——insert-only;该「主机:端口」已有锚时,按**与 §5 全系统一致的双模语义**判等值(rev1 曾写「字节相同」,与指纹锚语义冲突——热重载恰在 201 返回前带入 owner 指纹锚的场景会误判硬错,已改):等值(含指纹锚对呈现密钥指纹)→ 视为成功放行(竞态宽容);不等值 → 返回错误(走 §2.1 409/`equal=false` 分支);写入对象 = 调用时刻的当代存储(由 §2.2 实时解析铁律保证);仅供转发回执使用;不写本地审计(broker 侧已记权威行)。`SaveHostKey` 本体在只读态**仍然**返回 `ErrReadOnly`(直连可测,护栏不松)。

## 3. 带外锚定命令(owner 侧):`servers pin-hostkey`

挂入 `newServersCmd`(`internal/cli/servers.go:15`),条目解析 `name-or-id`(`GetServerByName`,与 `rm`/`edit` 同款)。四种形态:

| 调用 | 语义 |
|---|---|
| `sshmgr servers pin-hostkey <name>` | **显示**当前锚:指纹 + 格式(密钥字节/指纹)+ **来源(自动首次信任/转发/手动)+ 转发设备名** + 登记时间;无锚则明说 |
| `… --fingerprint SHA256:… [--force]` | 按指纹锚定。格式严格 = OpenSSH 规范 `SHA256:<base64 无填充>`(前后空白剥除);已有锚且无 `--force` → 拒绝并显示现存指纹与来源 |
| `… --from-keyscan <file或-> [--force]` | 解析 known_hosts 行锚定(复用 `internal/conformance/knownhosts.go` 的 `ParseKnownHostsLine`) |
| `… --clear` | **删锚**(该条目地址上的锚整行移除,回到待首信态)。与 `--force`/`--fingerprint`/`--from-keyscan` 互斥。**清毒原语**:设备失窃 → revoke 设备码 → 逐条 `--clear` 其转发锚 → 合法路径重锚 |

- **`--clear` 的地址全局粒度(可见化)**:锚按「主机:端口」全局唯一,clear 删的是全局锚行,**共享该地址的全部轮廓条目一起回到待首信态**。输出与审计行都带受影响条目清单(rev1 输出只报一个条目名,静默剥离其他轮廓的合法锚——已改):`unpinned host=<host>:<port> (was fp=SHA256:…, source=<…>) — affects N entries: <名1>, <名2>, …`;owner 若此刻在共享目标上持有刚重建的合法锚,清单让它知情。
- **keyscan 匹配规则(钉死)**:文件每行 patterns 按逗号拆分,元素与条目地址的 known_hosts 渲染形态(22 端口裸主机名,否则 `[host]:port`)字面相等才算匹配;`|1|…` 哈希主机名行与 `@cert-authority`/`@revoked` 前缀行**跳过并计数提示**(不支持,不误匹配);无匹配 → 错误列出文件内出现的形态与条目期望形态。
- **keyscan 多算法匹配 → 拒绝**:known_hosts 对同一主机常有多行(每算法一行),而存储模型是**一「主机:端口」一锚**;猜算法等于赌服务器下次呈现哪个,赌错=假性不匹配。拒绝文案指引自愈闭环:在能到达目标的机器上首连(触发自动转发),或从 `ssh-keyscan` 输出中选定一个算法用 `--fingerprint` 传入;若首连报「呈现指纹 ≠ 已锚指纹」,错误文本带双指纹,`--force` 补救。
- **owner 侧全部动作落审计**(沿用 `writeAuditTx`):`--fingerprint`/`--from-keyscan` 新锚 → `action="pin-manual"`(command 含 host:port + 新指纹 + 输入来源,`--force` 时含旧→新双指纹);`--clear` → `action="pin-clear"`(command 含被删锚的指纹、来源与**受影响条目清单**)。显示形态不落审计。
- **`--force` 是覆盖的唯一通道**:输出先显示旧指纹再显示新指纹,然后覆盖(含格式与来源切换为手动)。
- 成功输出统一为:`pinned <name> host=<host>:<port> fp=SHA256:… (format=<blob|fingerprint>, source=manual, forced=<bool>)`;`--clear` 输出见上。

## 4. 只读 fail-closed 文案(逐字钉死)

**修改点澄清(rev1 误指)**:`store.go:65` 的共享哨兵 `ErrReadOnly` 文本**不动**——它被全部只读变更复用,且新文案含逐调用占位符,不可能活在包级哨兵里;新文案由**转发包装层在失败点组装呈现**(包裹 `save host key: …` 链)。`SaveHostKey` 在只读态仍原样返回哨兵(§2.3/T9)。

缓存模式下「未知锚且无法转发」的最终呈现错误统一为。占位符规则(rev1 模板会渲染出 `SHA256:SHA256:`,已钉死):`<呈现指纹>` = `FingerprintSHA256` 的完整返回值(**含** `SHA256:` 前缀),模板第二处直接引用 `<呈现指纹>`、不再带字面前缀:

```
host key for <host>:<port> is unknown and cannot be pinned here (presented fingerprint: <呈现指纹>). Retry while the broker is reachable — the pin is then forwarded and audited automatically — or ask the owner to run: sshmgr servers pin-hostkey <name> --fingerprint <呈现指纹>
```

其余分支文案见 §2.1 表(400/413/401/403/404/409 两分支/明文变体——rev2 起全部有逐字文本),主文案与各分支文案均**逐字断言**(§8)。错误文本只含快照内已有信息(host:port、指纹)+ 补救指引,无敏感泄露(两轮盲评核对通过);`<name>` 不回填,owner 按「主机:端口」找条目。

## 5. 存储与快照:锚格式与来源双维(跨版本协议)

- `host_keys` 加三列(既有 guarded `ALTER TABLE ADD COLUMN … DEFAULT` 迁移模式,`store.go:221-223` 先例):
  - `pin_format TEXT NOT NULL DEFAULT 'blob'`(`blob` | `fingerprint`):指纹锚的 `key_blob` 列存指纹字符串字节;
  - `pin_source TEXT NOT NULL DEFAULT 'tofu'`(`tofu` | `forward` | `manual`):既有行与既有快照缺省即 `tofu`,天然回填;
  - `pin_device TEXT NOT NULL DEFAULT ''`:转发来源的设备码名(其余来源为空)。
- **比较语义双模**:`blob` → 字节相等;`fingerprint` → 呈现密钥的 `FingerprintSHA256` 与存串等值。sha256 抗碰撞,两种比较安全等价。**本语义是全系统唯一等值定义**——TOFU 回调、`ApplyForwardedHostKey`、`/pin-hostkey` 的 `equal` 计算、`--force` 前比对,全部引用它(rev1 曾在 §2.3 写了第二套字节判据,已统一)。
- `SnapshotHostKey` 加 `pin_format`/`pin_source`/`pin_device`(全部 `omitempty`;**缺省 = blob/tofu/空**,v0.14.0 及更早快照无缝兼容)。`ExportSnapshot` / `ExportSnapshotForProfile` / `ListHostKeys` / `ImportSnapshot` 全链携带。
- **`pin_device` 入快照的横向可见性(有意)**:绑定轮廓内每台设备都能从快照看到锚由哪台设备名转发(即「该目标曾被边缘首信过」)。这是新增的跨设备活动可见性,同时是 §0 投毒增量的检测面——接受并明示,不当作意外。
- 旧客户端导入含指纹锚的新快照:不知格式的旧代码按密钥字节解读 → 比对必不匹配 → 拒绝。可接受的降级,但**其呈现形态是「possible MITM」假警报**(`hostkey.go:15` 既有文案)——混布窗口内每台旧设备连接即告警。rider 3 文档必须明写此现象与升级解法,防操作者对真告警脱敏。
- **known_hosts 序列化:仓库无生产调用方**(`FormatKnownHostsLine` 仅 conformance 测试引用),本 plan 无需处理;若未来引入生产序列化,须知指纹锚无密钥字节不可渲染。

## 6. 审计

- **broker 侧权威行**(§1.2 ⑤,与落锚同事务):`action="pin-forward"`。`sshmgr audit` 的 action 是自由文本,零结构变化直接可见。
- **审计巡检面副作用(声明)**:`pin-forward` 行 `project_id` 为空,会落入 `QueryAudit` OwnerOnly(「owner actions」)过滤结果——owner 侧「仅看 owner 操作」的巡检从此混入设备发起的锚变更。这是**有意**的(锚变更本就该进 owner 巡检视野),以 action 前缀区分可筛。
- **owner 侧命令行**:`pin-manual` / `pin-clear`(§3,同样 `writeAuditTx` 原子)。覆盖两极中更危险的一极(owner 被骗 `--force` 洗白攻击者密钥)必须留痕——与 `projects` 轮换/删除落审计的仓库惯例对齐。
- **本地 sidecar**:维持现状——触发转发的执行本行按既有词汇表记 status;转发成败由 broker 权威行承载,不双记。

## 7. 版本与打包

- 捆发 **v0.15.0**(doctor rider 同批)。
- 兼容矩阵(如实):新客户端 + 旧 broker → 转发 404 → fail-closed 文案(指引 owner 升级后重试);**带外锚定同样要求 owner 侧先升到 v0.15.0**(命令本体在新版里),不存在免升级退路。旧客户端 + 新 broker → 指纹锚降级为「possible MITM」拒绝(§5,文档写明)。新新 → 全功能。

## 8. 测试矩阵

| # | 测试(归属包) | 断言 |
|---|---|---|
| T1(mcpserver) | `serve_pin_test.go`:合法设备码 + 条目在绑定轮廓内 | 201;store 落锚(`pin_format=blob`、`pin_source=forward`、`pin_device=设备名`);**审计行与锚同事务**(注入失败断言原子性:审计写失败则锚不落);响应指纹 == 落库键指纹;server_name == 审计归属条目(多候选确定性) |
| T2(mcpserver) | **并发不覆盖**:对同一未锚「主机:端口」并发 N 个不同 key 的 POST | 恰一 201,其余 409 且 `equal=false`;胜者键 = 落库键(insert-only 原语核心断言) |
| T2b(mcpserver) | 既有**等值**锚时 POST 同 key | 409 + `equal=true`;既有键原封不动 |
| T3(mcpserver) | 无候选条目/不在绑定轮廓 → 403 且不回显轮廓;**有效设备码未绑定轮廓 → 403**;项目令牌与坏设备码 → 401;非 POST → 405;超限 → 413;**畸形 key_blob → 400 且零落库**;GetCacheToken 存储故障 → 500 | — |
| T4(cli,`mcp_cache_test.go` 系) | 客户端分支映射:403 文案指引 grant/绑定;401 文案断言 + **本地缓存未销毁**;404 新文案;409 `equal=false` 文案;400/413 透传附 host:port;pin 为空 → §2.1 明文文案逐字 | — |
| T5(mcpserver+cli 集成) | `mcp --cache`(testsshd + 进程内 serve):条目+凭据在快照、无锚 | `exec_command` **一次成功**;serve 侧出现锚+审计行;本进程后续连接无二次转发(内存态生效) |
| T5b(mcpserver) | **换库代际变体**:在 201 与 Apply 之间注入 store 热重建 | 锚落在当代存储(实时解析铁律);重连不白费转发 |
| T6(cli) | 同 T5 但 serve 关停 | 失败;错误 == §4 主文案**逐字**(占位符按 §4 规则填充实值) |
| T7(cli) | 同 T5 但路由不存在(旧 broker 模拟) | 失败;文案 == §2.1 404 行逐字 |
| T8(cli) | **409 两分支**:① broker 预置**等值**真钥(含**指纹锚**形态)、客户端快照陈旧 → 转发 409 `equal=true` → **自动放行**(零仪式);② broker 预置**不同**锚 → 409 `equal=false` → 硬错文案 → pull 后重连 → `ErrHostKeyMismatch` 文本带双指纹 | — |
| T8b(cli) | **10 秒超时**:serve 挂起不响应(慢响应注入) | 超时分支 → §4 主文案,不悬挂握手 |
| T9(store) | 三列迁移;双模比较;`ApplyForwardedHostKey` 只读态可插、**双模等值放行**(含指纹锚对呈现指纹)、不等值报错、写入当代;`SaveHostKey` 只读态仍 `ErrReadOnly`;`InsertForwardedPin` insert-only + 同事务审计 | — |
| T10(store) | 快照:三新列 round-trip;v0.14 形态快照(无新列)导入 = blob/tofu 兼容 | — |
| T11(cli) | 命令行:显示(含来源/设备)/指纹锚定/格式错拒/`--force`(审计行含旧→新)/`--clear`(审计行含**受影响条目清单**)/单行 keyscan 成功/多行拒/`|1|` 哈希行跳过计数/无匹配错误文案 | — |
| T12(cli) | doctor rider:探针前 checkpoint 后,doctor 计数 == `servers ls` 计数 | — |
| T13 | 回归:`go test ./...` 全绿;`gofmt -l` 空;`go vet` 净;eval 主面零变化(锚定转发不新增任何 agent 可见工具) | — |

持续集成里的「broker 无路由」以逻辑等价方式覆盖(T5–T8 断言机制:转发、落库、审计、降级),物理不可达性由 §12 真机验收承担——测试进程在本机永远「有路由」,这是环境边界,如实记之。

## 9. riders(同批发版)

1. **doctor 计数差一**(反馈 #5;`doctor.go:414` copy-probe 报 11 台而 `servers ls` 12 台)。**病因(盲评修正)**:探针与 ls 两侧计数口径其实一致(均全量 servers);已文档化的少计机制是 probe 拷贝 store.db 时**不带 -wal/-shm**,未 checkpoint 的帧缺失 → 读到偏旧的一致快照(`doctor.go:323-329` 注释原文「undercount」)。修法:拷贝前 `Checkpoint()`(或连同 sidecar 文件一起拷),断言见 T12。
2. **mismatch 双指纹文案**(§2.2):`ErrHostKeyMismatch` 错误文本携带「呈现指纹 vs 已锚指纹」,把 `--force` 补救闭环。错误只增指纹值,不增任何其他密钥材料。
3. **文档**(反馈 #6):配对/入门文档新增前置条件节「目标首次连接的发起位置」与「目标 broker 不可达怎么办」;缓存操作文档补锚定转发行为、来源元数据、`--clear` 清毒流程(设备失窃 → revoke → clear → 重锚,注明 clear 的地址全局粒度与受影响条目清单)、**条目删除/改地址后的锚残留与 `--clear` 补救**、**跨会话重复转发的 `equal=true` 自动闭合**(用户可见行为:仅一次多余请求,零仪式)、混布窗口现象(pre-v0.15 客户端会把指纹锚报为「possible MITM」假警报,升级即解)。

## 10. Non-goals(v1 明确不做)

本地可写覆盖层(零合并约束);本地专属服务器条目(权威不分裂);按设备的转发开关(未来硬化项);离线排队/暂存转发(不留本地状态);**409 自动重拉**(砍掉,理由见 §2.1——`equal` 判定已覆盖自动闭合的正当场景);TUI/Web 管理界面里的锚定操作(miss 了再加);一「主机:端口」多锚 any-of;跳板首连(独立 backlog 立项);转发面主动告警/速率限制(补偿 = 来源元数据 + 审计 + `--clear`);条件清除(`--clear --if-source=forward` 之类,backlog——v1 以受影响条目清单可见化替代)。

## 11. 实施触点(文件级)

| 文件 | 变更 |
|---|---|
| `internal/mcpserver/serve.go` | 路径分发 + `/pin-hostkey` 分支(同一 `cacheAuth` 包裹);`handlePinHostkey`(§1.2 守卫序,含解析规范化、500 分支、equal 计算) |
| `internal/store/store.go` | `host_keys` 三列迁移(`pin_format`/`pin_source`/`pin_device`) |
| `internal/store/hostkeys.go` | **新导出组合式 API `InsertForwardedPin`**(单事务:insert-only 落锚 + 同事务审计,返回 equal);`GetHostKey` 返回锚结构(字节+格式);`SaveHostKey` 保持(UPSERT,vault 首次信任路径);`ApplyForwardedHostKey`(只读态窄缝,双模等值放行,写当代) |
| `internal/store/export.go` | `SnapshotHostKey` 三新字段;`ListHostKeys`/两条导出路径/`ImportSnapshot` 携带 |
| `internal/sshbroker/hostkey.go` | `HostKeyStore.GetHostKey` 返回锚结构;`HostKeyTOFU` 回调双模比较;`ErrHostKeyMismatch` 文本带双指纹 |
| `internal/mcpserver/run.go` | `RunStdioCache` **加参**(转发构造器,由 cli 传入);构造转发式实现(holder.Current 实时解析),注入 `NewServerFromSource` |
| `internal/cli/mcp.go` | `--cache` 路径构造转发构造器(`--instance` 解析处;禁止 run.go 内默认实例兜底) |
| `internal/mcpserver/server.go` | `NewServerFromSource` 增加主机密钥存储提供者参数或等价注入缝 |
| `internal/mcpserver/{core,bgtools,context,relay}.go` | 9 处生产 `HostKeyTOFU` 调用改走提供者(穿参改造,受控;relay.go 两处**含 Plan 47 中继目标侧拨号**,转发锚定对中继同样生效) |
| `internal/clientops/`(新文件) | 转发 HTTP 客户端(复用 `pinningTransport`/`SplitTokenPin`/`CacheCred`/实例路径);§2.1 分支映射与全部文案 |
| `internal/cli/pinhostkey.go`(新) | `servers pin-hostkey` 四形态(`--fingerprint`/`--from-keyscan`/`--force`/`--clear`)+ owner 审计 + 受影响条目清单 |
| `internal/cli/servers.go` | 注册子命令 |
| `internal/cli/doctor.go` | rider 1:probe 前 Checkpoint |
| 测试 | T1–T12 各就各位(归属见 §8);接口假体与 store 侧直呼更新(conformance×2、sshbroker hostkey 系、tunnels_control_test、`store/hostkeys_test.go`、`store/export_test.go:125`、`cli/gc_test.go:128`) |
| `docs/`(缓存/配对/README) | rider 3(含混布假警报、清毒流程、锚残留、equal 自动闭合说明) |

## 12. 真机验收(发版后)

NUC10(升级 v0.15.0,自更新通道)+ 笔记本(同步)+ 生化高速工控板(192.168.1.108,直连网卡):

1. 反馈 §二 三步原样复跑:① 笔记本 `exec_command` 一次成功;② NUC10 `sshmgr audit` 可见 `pin-forward` 行(设备名+指纹);③ `servers pin-hostkey 生化高速工控板` 显示已锚指纹一致、来源=forward、设备=笔记本。
2. 拔 VLAN(真离线)对**另一**新目标首连 → §4 新文案逐字(含呈现指纹);`ssh-keyscan` → NUC10 带外锚定 → 回网 `cache pull` → 连接成功。
3. 跨会话复连:笔记本重启 MCP 进程(缓存仍新鲜)再连工控板 → 应经 409 `equal=true` 自动放行,零用户动作。
4. 混布抽查:一台仍跑 v0.14 的客户端连接指纹锚目标 → 观察「possible MITM」假警报形态(留档即升级)。
5. NUC10 上 `doctor` 计数与 `servers ls` 口径一致(rider 1 真机面)。
6. (可选演练)revoke 笔记本设备码 → `--clear` 其转发锚(核对受影响条目清单输出)→ 合法重锚,验证清毒闭环。

## 13. 参照

- 反馈文件(场景与证据):`C:\WorkSpace\urit_things\sshmgr-feedback-cache-client-toufu-hostkey.md`
- ADR 0002(边缘首信取舍,随 rev2 同步更新);ADR 0001(格式参照)
- Plan 12 设计 §3 决策 8(「离线未知锚 → fail-closed… a refresh resolves it」——本设计修订其「刷新可解」假设,修订理由即 §0 死锁)
- Plan 34(401 隔离语义,§1.2 的不触发约定);Plan 31(ServerInfo 打码/无端口——守卫 ③ 下沉 store 层的原因);Plan 39(轮廓范围快照);Plan 40/46(多实例——「读实例永远显式」铁律见 §2.1);Plan 45(配对)
- `internal/conformance/knownhosts.go`(`--from-keyscan` 解析复用)

---

## 附录:rev1 变更记录(自 rev0)

盲评第一轮三路(安全/代码事实/完备性)31 条:落锚原子 insert-only(并发覆盖竞态)+ 同事务审计;锚来源元数据 + `--clear` 清毒原语;owner `--force`/`--clear` 落审计;砍 409 自动重拉;409 不回显指纹;服务端解析规范化(畸形 400);客户端 401/403 映射;`HostKeyTOFU` 回调确认要改;调用点修正生产 10 处点名 relay.go;doctor rider 病因改写 WAL 少计;ADR 补预置投毒增量如实承认。
