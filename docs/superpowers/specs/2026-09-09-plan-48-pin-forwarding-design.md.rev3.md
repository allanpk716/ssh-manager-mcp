# Plan 48 设计(rev3·收敛版):锚定转发与带外锚定——「仅缓存客户端可达」目标的首信闭环

> 反馈来源:`C:\WorkSpace\urit_things\sshmgr-feedback-cache-client-toufu-hostkey.md`(2026-09-09,v0.14.0,生化高速工控板 192.168.1.108 仅笔记本一块网卡可达)。2026-09-09 grilling 两轮拍板、本文不重议:**自动路径=锚定转发**(跳板首连独立立项进 backlog)、**带外锚定同批做**、**拒绝本地可写覆盖层**、**本地专属服务器条目非目标**、**单计划捆发 v0.15.0**(doctor 计数 rider 同批)、**护栏三条不可协商——仅可新增、仅在线转发、审计;默认全部有效设备码可转发,v1 不加按设备开关**、**命令行双输入(`--fingerprint` 主路径 / `--from-keyscan` 批量路径)+ owner `--force` 唯一覆盖通道**、**发版后真机验收,发版门含反馈场景的持续集成复现**。术语以根目录 `CONTEXT.md` 为准(锚定 / 锚定转发 / 带外锚定);取舍记录见 ADR 0002。
>
> **rev3 变更**(第三轮盲评两路——equal 安全对抗 + 收敛复核——共 17 条,全部处置;结论:**equal 零泄露论断经全力反驳成立**,spec 达「可写实施 plan」成熟度):equal 论证锚点从「提交者自己手里的密钥」改为「锚值已随快照分发、equal ⊆ 快照可算」(POST 与握手无协议级绑定,原措辞依赖礼貌客户端假设);**`/pin-hostkey` 每次请求结果打 stderr 行**(探测阶段不再零痕迹);**equal 判定移入同一事务**(防 conflict 后重读撞上并发 --force/--clear 的瞬态错判);**`servers pin-hostkey --list` 全量锚枚举(含孤儿)与 `--clear` 的 host:port 直达形态**(条目已删/改址的毒锚从此可达);**`--force` 单事务 读旧→UPSERT→审计**(防审计行说谎);`--clear` 无锚=幂等成功不落审计(防取证面污染);`--fingerprint` 强制 base64 解码恰 32 字节(打错字符当场拒);pin-forward 审计 command 附受影响条目清单(与 pin-clear 对称);pin_device 措辞改「跨轮廓可见(有意)」;ImportSnapshot 空新列归一 blob/tofu;守卫 ① 补 JSON 解码/字段缺失 → 400;doctor rider 修法机制钉死(免迁移裸连接 checkpoint,永不拷 sidecar);rider 3 补清毒客户端收敛步与备份时序。
> rev2/rev1 变更记录见文末附录。

## 0. 目标与缺口

首次信任(锚定)同时需要**到目标的网络通路**与**可写的权威存储**。当前架构把二者分在两台机器:

| 侧 | 通路 | 写权限 | 结果 |
|---|---|---|---|
| 缓存客户端(`mcp --cache`) | 有(直连网卡到 192.168.1.x) | 无(快照只读,`SaveHostKey` → `ErrReadOnly`) | 握手成功后卡在「save host key」 |
| serve broker(NUC10) | 无(对目标网段无路由) | 有 | TCP 都到不了,首连无法发起 |

反馈另证实的三个事实,构成本设计的边界:

- **锚的归属键无条件是「主机:端口」**(`internal/store/hostkeys.go` 的 `hostKeyID`,连 22 端口也不裸写主机名)。反馈 §六 的端口桥接临时方案因此必然产生孤儿锚。锚与条目是**多对一现实的单向映射**:锚按「主机:端口」全局唯一,同地址多轮廓条目共享同一锚;条目改地址后旧锚不迁移、不回收(条目**删除**后锚同样残留——同地址重新录入即静默继承,期间服务器换钥则首连假性不匹配;**孤儿锚的枚举与清除**由 §3 的 `--list` 与 host:port 直达 `--clear` 覆盖)。转发上线后,轮廓内设备可对**未锚定地址**静默落锚(含条目改址后的重锚)——重锚语义与既有首次信任一致,**新增的是「谁能触发它」**(这正是 ADR 0002 如实承认的投毒增量,措辞不再用「非新增行为」)。
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

// 400 Bad Request(JSON 不可解码/字段缺失/密钥解析失败)
{ "error": "unparseable key_blob" }
```

- `key_blob` 与 `HostKeyTOFU` 回调里 `remote.Marshal()` 的字节同源(合规客户端转发它在握手里实际看到的密钥;注意 POST 与握手**无协议级绑定**——恶意客户端可凭空提交,这正是 ADR 0002 承认的投毒形态,防线上限=护栏三条)。`fingerprint` 用 `ssh.PublicKey.FingerprintSHA256` 规范形态(`SHA256:` 前缀 + base64 无填充),与 `ssh-keygen -lf` 输出一致。
- **请求体上限 = 新设 64 KiB 常量**(`MaxBytesReader`;仓库先例是 `/pair/*` 的 1 KiB 上限,`/snapshot` 是无 body 的 GET、无可复用上限),超限 413。
- **409 不回显指纹,但带 `equal` 布尔;零泄露论证(rev3 改锚点)**:锚值本就随快照分发(密钥字节或指纹串 + 来源 + 设备名全在快照里),`equal` 的全部信息 ⊆「同一凭据、同一监听器的一次拉取」可自算的内容,不依赖「提交者诚实」假设。主机公钥是公开的(可达目标者可 `ssh-keyscan`),故 equal 至多为攻击者提供「己方毒锚是否存活」的记账价值——同一信息从快照直接可得。`equal=true` 是跨会话重复转发与并发等值竞态的自动闭合键(§2.1)。
- **`server_name` 确定性**:多候选(同「主机:端口」在绑定轮廓内有多条授予条目)时,与审计行归属**同一条目**(候选中 id 排序最小者,§1.2 ③⑤)。

### 1.2 守卫执行序(钉死)

```
handlePinHostkey:
  ① method != POST → 405;请求体 > 64 KiB → 413;
     JSON 解码失败 / host·port·key_blob 任一缺失或类型不符 → 400
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
       RowsAffected == 0 → **同一事务内** SELECT 既有锚 → 按 §5 双模语义算 equal
         → 回滚(此时审计行尚未写入) → 409 {"error":"already pinned","equal":…}
         (事务内重读:conflict 与判等之间不允许并发 --force/--clear 换锚造成瞬态错判)
       RowsAffected > 0 → 同事务 writeAuditTx(复用既有「变更与其历史原子提交」模式):
           action   = "pin-forward"
           server_id= 候选中 id 排序最小者
           project_id 空(设备发起,非项目;审计巡检面影响见 §6)
           command  = "host=<host>:<port> fp=<SHA256:…> device=<设备码名> via=forward
                       affects=N entries: <同地址全部条目名>(owner 全量视角,与 pin-clear 对称)"
           status   = "ok"
         → commit
  ⑥ 201 { "fingerprint": <④计算值>, "server_name": <⑤审计归属条目之名> }

  每次请求(无论成败)向 serve stderr 打一行结果日志,仿 verifyCacheToken 惯例:
  "sshmgr serve: pin-hostkey <host>:<port> -> <status> (device <名>)"
  ——403/400/409 不落审计、无限速,探测阶段唯一的痕迹就是这行(rev3:失窃设备的
  探测不再完全静默;owner grep serve 日志可见异常试探)。
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
| **404** | 硬错,文案:`the broker does not support pin forwarding (the owner must upgrade the broker to >= v0.15.0, then retry)`(404 场景意味着 owner 侧同体未升级、带外命令尚不存在,指引只指升级) |
| **409,`equal=true`** | **自动闭合**:本地 `ApplyForwardedHostKey`(权威已持等值锚)→ 回调返回 nil → 握手继续。覆盖:①同进程并发首连输家(exec + forward / relay 目标侧拨号并行打同一新目标);②**跨会话重复转发**(进程退出内存态即失、下次 spawn 缓存仍新鲜未含该锚 → 重新转发 → 409 equal=true → 放行;代价只是一次多余 POST,零用户仪式) |
| **409,`equal=false`** | 硬错,文案:`the broker already has a different pin for <host>:<port> — run cache pull and retry; if it still fails, ask the owner to compare fingerprints (servers pin-hostkey)`。恢复路径 = 拉取后重试;拉到权威锚后按双模比较自会分晓(等值即通过、异值报双指纹不匹配) |
| **5xx / 网络不可达 / 超时(10 秒)** | §4 主文案 |
| **pin 为空(明文 http)** | 拒绝发起转发,逐字文案:`pin forwarding requires a pinned TLS server — no plaintext forwarding (set the server pin used by cache pull); the host key for <host>:<port> stays unpinned`。锚定转发是安全敏感变更通道,比拉取的明文逃生门更严是有意为之 |
| **cache.auth.json 缺失/不可读** | 转发构造器返回「无转发能力」→ 行为等同 broker 不可达,走 §4 主文案(cache 在而凭据文件被删 = 无法认证任何转发;不静默降级为本地状态) |

HTTP 客户端构造:**由 `internal/cli/mcp.go`(掌握 `--instance` 解析处)构造并传入 `RunStdioCache`**(签名加参:转发构造器或等价注入)——`RunStdioCache`(`internal/mcpserver/run.go:230`)现签名无实例通路,若在 run.go 内默认实例兜底会静默错拿 `CacheCred`,违反 Plan 40/46「读实例永远显式」铁律。构造内容:当前实例 `cache.auth.json` 的 `url`/`token`/`pin`(`CacheCred`,`internal/clientops/clientops.go:273`),令牌经 `SplitTokenPin` 剥离复合形态后走 `Authorization` 头,传输层用与 `DoPull` 同源的 `pinningTransport` + 不跟随重定向;转发超时 10 秒(它在 SSH 握手关键路径上)。

**不做 409 自动重拉**(rev0 曾承诺,rev1 砍掉,理由如实重述):`CacheReloader.Check` 只能显影**已在盘上**的 cache.bin 变化——其 unchanged 分支的惰性拉取受 maxAge+退避双闸,不保证同步取回新快照;而握手回调内嵌一次强制完整拉取会继承隔离与时间锚全套闸门。`equal` 判定已覆盖自动闭合的正当场景,剩余 `equal=false` 由用户按文案一轮恢复。

### 2.2 注入点与改动实况(如实,不称「机械」)

- `HostKeyStore` 接口(`internal/sshbroker/hostkey.go:17-21`)的 `GetHostKey` 返回升级为**锚结构**(密钥字节 + 格式)——双模比较需要格式;接口假体(`conformance/differential_test.go`——conformance 内唯一真接口假体,`relay_test.go` 传真 `*store.Store` 无需改)、`sshbroker/hostkey*_test.go`、`mcpserver/tunnels_control_test.go`)与直呼 store 方法的 store 侧测试(`internal/store/hostkeys_test.go`、`export_test.go:125`、`internal/cli/gc_test.go:128`)同步。
- **`HostKeyTOFU` 回调本体要改**(rev0「本体不改」与 §5 自相矛盾,rev1 修正):比较升级为双模(密钥字节相等 / 指纹串等值),`ErrHostKeyMismatch` 文本携带双指纹(rider 2)。
- 生产调用点 **10 处**:`core.go` 5、`bgtools.go` 1、`context.go` 1、`relay.go` 2、`cli/ssh.go` 1。前 9 处(mcpserver 内)改走从 `NewServerFromSource` 构造处取得的主机密钥存储提供者——工具函数是只收 `*store.Store` 的包级函数,提供者需**穿参改造**(约 9 个签名 + 对应闭包,受控但非零改动,测试策略随之);`cli/ssh.go`(owner 命令,vault 可写态)保持直连存储不变。vault 模式提供者 = 存储本体,行为零变化。
- **实时解析铁律(钉死)**:转发式实现的读与写都必须在 `HostKeyTOFU` 回调内**实时**经 `holder.Current` 解析当前存储——禁止构造期捕获 store 指针。否则转发 HTTP 调用(≤10 s)夹在 Get 与 Apply 之间的窗口里,任何并发工具触发的热重建会换下临时库,锚写进已被换下的代际 → 下次连接重新转发 → 白费;本铁律同时决定 `ApplyForwardedHostKey` 落当代、读亦当代(§2.3)。§8 T5b 断言之。

### 2.3 本地内存态落锚的窄缝

`Store` 新增 `ApplyForwardedHostKey(host string, port int, marshaledKey []byte) error`:**唯一**在只读态被允许写 `host_keys` 的方法,契约钉死——insert-only;该「主机:端口」已有锚时,按**与 §5 全系统一致的双模语义**判等值:等值(含指纹锚对呈现密钥指纹)→ 视为成功放行(竞态宽容);不等值 → 返回错误(知情说明:此路径的触发场景是 201 与 Apply 之间热重载恰好换入含异锚的代际——此时 broker 刚收下**本端**的钥,§2.1 的 409 文案字面为假,但「拉取后重试」的恢复指引仍正确;超稀有竞态,接受误导,不改文案);写入对象 = 调用时刻的当代存储(由 §2.2 实时解析铁律保证);本地行的元数据与 broker 落库一致(`pin_format='blob'`、`pin_source='forward'`、`pin_device=<设备码名>`——临时库行随进程消亡,一致性只为防未来消费面踩坑,T9 断言);仅供转发回执使用;不写本地审计(broker 侧已记权威行)。`SaveHostKey` 本体在只读态**仍然**返回 `ErrReadOnly`(直连可测,护栏不松)。

## 3. 带外锚定命令(owner 侧):`servers pin-hostkey`

挂入 `newServersCmd`(`internal/cli/servers.go:15`)。六种形态:

| 调用 | 语义 |
|---|---|
| `sshmgr servers pin-hostkey <name>` | **显示**该条目地址上的锚:指纹 + 格式(密钥字节/指纹)+ **来源(自动首次信任/转发/手动)+ 转发设备名** + 登记时间;无锚则明说 |
| `sshmgr servers pin-hostkey --list` | **全量锚清单**(store `ListHostKeys` 的 CLI 出口):每行 host:port + 指纹 + 来源 + 设备 + 登记时间;**孤儿锚(无任何条目指向)单独标注** `[orphan]`——条目已删/改址的锚从此可发现 |
| `… <name> --fingerprint SHA256:… [--force]` | 按指纹锚定。格式严格 = OpenSSH 规范 `SHA256:<base64 无填充>`(前后空白剥除),**且 base64 解码后恰 32 字节**(rev3:打错字符的指纹当场拒,杜绝「永不匹配的锚 → 409 恢复指引永不收敛 → 恐慌 --force」链);已有锚且无 `--force` → 拒绝并显示现存指纹与来源 |
| `… <name> --from-keyscan <file或-> [--force]` | 解析 known_hosts 行锚定(复用 `internal/conformance/knownhosts.go` 的 `ParseKnownHostsLine`) |
| `… <name> --clear` | **删锚**(该条目地址上的锚整行移除,回到待首信态)。与 `--force`/`--fingerprint`/`--from-keyscan` 互斥。**清毒原语** |
| `… --clear --hostport <host>:<port>` | **host:port 直达形态**(无位置参数):不经条目名解析,**孤儿锚唯一清除通道**——条目已删/改址后毒锚仍可达可删。输出与审计同 §3 清单规则 |

- **`--clear` 的地址全局粒度(可见化)**:锚按「主机:端口」全局唯一,clear 删的是全局锚行,**共享该地址的全部轮廓条目一起回到待首信态**。有锚删除:输出与审计行都带受影响条目清单(owner 全量视角现算):`unpinned host=<host>:<port> (was fp=SHA256:…, source=<…>) — affects N entries: <名1>, <名2>, …`(清单是 clear 时刻快照,与 DELETE 之间条目变动可能脱节——clear 不改条目本体,后果有界)。**无锚 clear = 幂等成功、不落审计**(rev3:防「was fp=…」的无中生有行污染投毒取证面),输出 `no pin present at <host>:<port> (nothing cleared)`。
- **keyscan 匹配规则(钉死)**:文件每行 patterns 按逗号拆分,元素与条目地址的 known_hosts 渲染形态(22 端口裸主机名,否则 `[host]:port`)字面相等才算匹配;`|1|…` 哈希主机名行与 `@cert-authority`/`@revoked` 前缀行**跳过并计数提示**(不支持,不误匹配);无匹配 → 错误列出文件内出现的形态与条目期望形态。
- **keyscan 多算法匹配 → 拒绝**:known_hosts 对同一主机常有多行(每算法一行),而存储模型是**一「主机:端口」一锚**;猜算法等于赌服务器下次呈现哪个,赌错=假性不匹配。拒绝文案指引自愈闭环:在能到达目标的机器上首连(触发自动转发),或从 `ssh-keyscan` 输出中选定一个算法用 `--fingerprint` 传入;若首连报「呈现指纹 ≠ 已锚指纹」,错误文本带双指纹,`--force` 补救。
- **owner 侧全部动作落审计**(沿用 `writeAuditTx`,**单事务 读旧→写新→审计**——rev3 钉死:与 serve 进程同库多进程并存,非事务 read-modify-write 会让审计行「was fp=X」在并发 `--clear`/`--force` 交错下说谎):`--fingerprint`/`--from-keyscan` 新锚 → `action="pin-manual"`(command 含 host:port + 新指纹 + 输入来源,`--force` 时含事务内读到的旧→新双指纹);`--clear` 有锚 → `action="pin-clear"`(command 含被删锚的指纹、来源与受影响条目清单)。显示与 `--list` 不落审计。
- **`--force` 是覆盖的唯一通道**:输出先显示事务内读到的旧指纹再显示新指纹,然后覆盖(含格式与来源切换为手动)。**备份恢复是叙事外通道,文档排序钉死(rider 3):clear 完成后才打备份**——毒化后、clear 前的备份在日后「删 store.db + import」恢复时原样带回毒锚,绕过全部审计。
- 成功输出统一为:`pinned <name> host=<host>:<port> fp=SHA256:… (format=<blob|fingerprint>, source=manual, forced=<bool>)`;`--clear` 输出见上。

## 4. 只读 fail-closed 文案(逐字钉死)

**修改点澄清(rev1 误指)**:`store.go:65` 的共享哨兵 `ErrReadOnly` 文本**不动**——它被全部只读变更复用,且新文案含逐调用占位符,不可能活在包级哨兵里;新文案由**转发包装层在失败点组装呈现**(包裹 `save host key: …` 链)。`SaveHostKey` 在只读态仍原样返回哨兵(§2.3/T9)。

缓存模式下「未知锚且无法转发」的最终呈现错误统一为。占位符规则(rev1 模板会渲染出 `SHA256:SHA256:`,已钉死):`<呈现指纹>` = `FingerprintSHA256` 的完整返回值(**含** `SHA256:` 前缀),模板第二处直接引用 `<呈现指纹>`、不再带字面前缀:

```
host key for <host>:<port> is unknown and cannot be pinned here (presented fingerprint: <呈现指纹>). Retry while the broker is reachable — the pin is then forwarded and audited automatically — or ask the owner to run: sshmgr servers pin-hostkey <name> --fingerprint <呈现指纹>
```

其余分支文案见 §2.1 表(400/413/401/403/404/409 两分支/明文/凭据缺失变体——全部有逐字文本),主文案与各分支文案均**逐字断言**(§8)。错误文本只含快照内已有信息(host:port、指纹)+ 补救指引,无敏感泄露(三轮盲评核对通过);`<name>` 不回填,owner 按「主机:端口」找条目。

## 5. 存储与快照:锚格式与来源双维(跨版本协议)

- `host_keys` 加三列(既有 guarded `ALTER TABLE ADD COLUMN … DEFAULT` 迁移模式,`store.go:221-223` 先例):
  - `pin_format TEXT NOT NULL DEFAULT 'blob'`(`blob` | `fingerprint`):指纹锚的 `key_blob` 列存指纹字符串字节;
  - `pin_source TEXT NOT NULL DEFAULT 'tofu'`(`tofu` | `forward` | `manual`):既有行与既有快照缺省即 `tofu`,天然回填;
  - `pin_device TEXT NOT NULL DEFAULT ''`:转发来源的设备码名(其余来源为空)。
- **比较语义双模**:`blob` → 字节相等;`fingerprint` → 呈现密钥的 `FingerprintSHA256` 与存串等值。sha256 抗碰撞,两种比较安全等价。**本语义是全系统唯一等值定义**——TOFU 回调、`ApplyForwardedHostKey`、`/pin-hostkey` 的 `equal` 计算、`--force` 前比对,全部引用它。
- `SnapshotHostKey` 加 `pin_format`/`pin_source`/`pin_device`(全部 `omitempty`;**缺省 = blob/tofu/空**)。`ExportSnapshot` / `ExportSnapshotForProfile` / `ListHostKeys` / `ImportSnapshot` 全链携带;**`ImportSnapshot` 对空 `pin_format`/`pin_source` 显式归一为 `blob`/`tofu`**(rev3:ADD COLUMN DEFAULT 只回填既有行、不约束显式插入——Go 零值 `""` 直插会存空串,朴素实现到 T10 才暴雷)。
- **`pin_device` 的跨轮廓可见性(有意,rev3 措辞如实)**:锚按「主机:端口」全局过滤进轮廓快照,故**任何**在该地址持有条目的轮廓(包括对转发设备毫无其他可见性的轮廓,如外包分区)都能从快照看到:该地址曾被哪台**设备名**首信过、何时。这是新增的跨设备/跨轮廓活动可见性,接受并明示——它同时是 §0 投毒增量的**主检测面**(跨轮廓继承毒锚的轮廓正是靠它发现异常来源);若未来判定命名体系本身敏感,可收紧为跨轮廓导出时省略 `pin_device`(backlog,一处投影过滤)。
- 旧客户端导入含指纹锚的新快照:不知格式的旧代码按密钥字节解读 → 比对必不匹配 → 拒绝。可接受的降级,但**其呈现形态是「possible MITM」假警报**(`hostkey.go:15` 既有文案)——混布窗口内每台旧设备连接即告警。rider 3 文档必须明写此现象与升级解法,防操作者对真告警脱敏。
- **known_hosts 序列化:仓库无生产调用方**(`FormatKnownHostsLine` 仅 conformance 测试引用),本 plan 无需处理;若未来引入生产序列化,须知指纹锚无密钥字节不可渲染。

## 6. 审计

- **broker 侧权威行**(§1.2 ⑤,与落锚同事务):`action="pin-forward"`,command 含受影响条目清单(与 pin-clear 对称——锚创建瞬间同地址全部条目被影响,只记 min-id 单条会让 `audit --server <其他条目>` 查不到「锚定过我」,继承历史不可重建)。`sshmgr audit` 的 action 是自由文本,零结构变化直接可见。
- **审计巡检面副作用(声明)**:`pin-forward` 行 `project_id` 为空,会落入 `QueryAudit` OwnerOnly(「owner actions」)过滤结果——owner 侧「仅看 owner 操作」的巡检从此混入设备发起的锚变更。这是**有意**的(锚变更本就该进 owner 巡检视野),以 action 前缀区分可筛。
- **owner 侧命令行**:`pin-manual` / `pin-clear`(§3,单事务原子)。覆盖两极中更危险的一极(owner 被骗 `--force` 洗白攻击者密钥)必须留痕——与 `projects` 轮换/删除落审计的仓库惯例对齐。
- **本地 sidecar**:维持现状——触发转发的执行本行按既有词汇表记 status;转发成败由 broker 权威行承载,不双记。
- **serve stderr 行**(§1.2):转发端点的每次请求(含 403/400/409 被拒)留一行——探测痕迹的最低保障,不进审计表。

## 7. 版本与打包

- 捆发 **v0.15.0**(doctor rider 同批)。
- 兼容矩阵(如实):新客户端 + 旧 broker → 转发 404 → fail-closed 文案(指引 owner 升级后重试);**带外锚定同样要求 owner 侧先升到 v0.15.0**(命令本体在新版里),不存在免升级退路。旧客户端 + 新 broker → 指纹锚降级为「possible MITM」拒绝(§5,文档写明)。新新 → 全功能。

## 8. 测试矩阵

| # | 测试(归属包) | 断言 |
|---|---|---|
| T1(mcpserver) | `serve_pin_test.go`:合法设备码 + 条目在绑定轮廓内 | 201;store 落锚(`pin_format=blob`、`pin_source=forward`、`pin_device=设备名`);**审计行与锚同事务**(注入失败断言原子性:审计写失败则锚不落);审计 command 含 affects 清单;响应指纹 == 落库键指纹;server_name == 审计归属条目(多候选确定性) |
| T2(mcpserver) | **并发不覆盖**:对同一未锚「主机:端口」并发 N 个不同 key 的 POST | 恰一 201,其余 409 且 `equal=false`;胜者键 = 落库键(insert-only 原语核心断言) |
| T2b(mcpserver) | 既有**等值**锚时 POST 同 key | 409 + `equal=true`(事务内判等);既有键原封不动 |
| T3(mcpserver) | 无候选条目/不在绑定轮廓 → 403 且不回显轮廓;**有效设备码未绑定轮廓 → 403**;**JSON 不可解码/字段缺失 → 400**;项目令牌与坏设备码 → 401;非 POST → 405;超限 → 413;**畸形 key_blob → 400 且零落库**;GetCacheToken 存储故障 → 500 | — |
| T4(cli,`mcp_cache_test.go` 系) | 客户端分支映射:403 文案指引 grant/绑定;401 文案断言 + **本地缓存未销毁**;404 新文案;409 `equal=false` 文案;400/413 透传附 host:port;pin 为空 → §2.1 明文文案逐字;cache.auth.json 缺失 → 主文案 | — |
| T5(mcpserver+cli 集成) | `mcp --cache`(testsshd + 进程内 serve):条目+凭据在快照、无锚 | `exec_command` **一次成功**;serve 侧出现锚+审计行;本进程后续连接无二次转发(内存态生效) |
| T5b(mcpserver) | **换库代际变体**:在 201 与 Apply 之间注入 store 热重建 | 锚落在当代存储(实时解析铁律);重连不白费转发 |
| T6(cli) | 同 T5 但 serve 关停 | 失败;错误 == §4 主文案**逐字**(占位符按 §4 规则填充实值) |
| T7(cli) | 同 T5 但路由不存在(旧 broker 模拟) | 失败;文案 == §2.1 404 行逐字 |
| T8(cli) | **409 两分支**:① broker 预置**等值**真钥(含**指纹锚**形态)、客户端快照陈旧 → 转发 409 `equal=true` → **自动放行**(零仪式);② broker 预置**不同**锚 → 409 `equal=false` → 硬错文案 → pull 后重连 → `ErrHostKeyMismatch` 文本带双指纹 | — |
| T8b(cli) | **10 秒超时**:serve 挂起不响应(慢响应注入) | 超时分支 → §4 主文案,不悬挂握手 |
| T9(store) | 三列迁移;双模比较;`InsertForwardedPin` insert-only + 同事务审计;`ApplyForwardedHostKey` 只读态可插、**双模等值放行**(含指纹锚对呈现指纹)、不等值报错、写当代、**本地行元数据 = blob/forward/设备名**;`SaveHostKey` 只读态仍 `ErrReadOnly`;`ImportSnapshot` 空新列归一 blob/tofu | — |
| T10(store) | 快照:三新列 round-trip;v0.14 形态快照(无新列)导入 = blob/tofu 兼容 | — |
| T11(cli) | 命令行:显示(含来源/设备)/`--list`(含 `[orphan]` 标注)/指纹锚定/**解码非 32 字节拒**/格式错拒/`--force`(审计行含事务内旧→新)/`--clear` 有锚(审计行含受影响条目清单)/**`--clear` 无锚幂等且不落审计**/`--clear --hostport` 直达孤儿/单行 keyscan 成功/多行拒/`|1|` 哈希行跳过计数/无匹配错误文案 | — |
| T12(cli) | doctor rider:探针前 checkpoint 后,doctor 计数 == `servers ls` 计数 | — |
| T13 | 回归:`go test ./...` 全绿;`gofmt -l` 空;`go vet` 净;eval 主面零变化(锚定转发不新增任何 agent 可见工具) | — |

持续集成里的「broker 无路由」以逻辑等价方式覆盖(T5–T8 断言机制:转发、落库、审计、降级),物理不可达性由 §12 真机验收承担——测试进程在本机永远「有路由」,这是环境边界,如实记之。

## 9. riders(同批发版)

1. **doctor 计数差一**(反馈 #5;`doctor.go:414` copy-probe 报 11 台而 `servers ls` 12 台)。**病因(盲评修正)**:探针与 ls 两侧计数口径其实一致(均全量 servers);已文档化的少计机制是 probe 拷贝 store.db 时**不带 -wal/-shm**,未 checkpoint 的帧缺失 → 读到偏旧的一致快照(`doctor.go:323-329` 注释原文「undercount」)。**修法(rev3 钉死,二选一废除)**:对生产库以**免密钥、免迁移的裸 SQLite 连接**执行 `PRAGMA wal_checkpoint(TRUNCATE)` 后再拷贝(migrate-path 有开生产库先例;`store.Open` 有建库/迁移/ACL 副作用故不用)——**永不**连同 -wal/-shm 一起拷(代码注释明言撕裂风险 = "exactly the false verdict this diagnostic cannot afford")。断言见 T12。
2. **mismatch 双指纹文案**(§2.2):`ErrHostKeyMismatch` 错误文本携带「呈现指纹 vs 已锚指纹」,把 `--force` 补救闭环。错误只增指纹值,不增任何其他密钥材料。
3. **文档**(反馈 #6):配对/入门文档新增前置条件节「目标首次连接的发起位置」与「目标 broker 不可达怎么办」;缓存操作文档补:锚定转发行为、来源元数据、`--clear` 清毒流程(**完整时序:revoke 设备码 → `--list` 找其转发锚(含孤儿)→ 逐条 `--clear` → 合法重锚 → **作用域内所有设备 pull 到位前不算消毒完成**(cache.bin 里的毒锚拷贝不被 clear 收回;未 pull 的设备对毒钥继续放行、对合法重锚钥报假 MITM)→ **clear 完成后才打备份**(更早的备份恢复会带回毒锚))、条目删除/改地址后的锚残留与 `--list`/`--hostport` 补救、跨会话重复转发的 `equal=true` 自动闭合(用户可见:仅一次多余请求,零仪式)、混布窗口现象(pre-v0.15 客户端把指纹锚报为「possible MITM」假警报,升级即解)。

## 10. Non-goals(v1 明确不做)

本地可写覆盖层(零合并约束);本地专属服务器条目(权威不分裂);按设备的转发开关(未来硬化项);离线排队/暂存转发(不留本地状态);**409 自动重拉**(砍掉,理由见 §2.1——`equal` 判定已覆盖自动闭合的正当场景);TUI/Web 管理界面里的锚定操作(miss 了再加);一「主机:端口」多锚 any-of;跳板首连(独立 backlog 立项);转发面主动告警/速率限制(补偿 = stderr 行 + 来源元数据 + 审计 + `--clear`);条件清除(`--clear --if-source=forward` 之类,backlog——v1 以受影响条目清单与 `--list` 可见化替代);跨轮廓导出省略 `pin_device`(backlog——v1 以「有意可见+明示」替代)。

## 11. 实施触点(文件级)

| 文件 | 变更 |
|---|---|
| `internal/mcpserver/serve.go` | 路径分发 + `/pin-hostkey` 分支(同一 `cacheAuth` 包裹);`handlePinHostkey`(§1.2 守卫序,含 JSON 400、解析规范化、500 分支、事务内 equal、每次请求 stderr 行) |
| `internal/store/store.go` | `host_keys` 三列迁移(`pin_format`/`pin_source`/`pin_device`) |
| `internal/store/hostkeys.go` | **新导出组合式 API `InsertForwardedPin`**(单事务:insert-only 落锚 + 同事务审计 + 事务内 equal 判定);`GetHostKey` 返回锚结构(字节+格式);`SaveHostKey` 保持(UPSERT,vault 首次信任路径);`ApplyForwardedHostKey`(只读态窄缝,双模等值放行,写当代,元数据与 broker 一致) |
| `internal/store/export.go` | `SnapshotHostKey` 三新字段;`ListHostKeys`/两条导出路径/`ImportSnapshot` 携带 + 空新列归一 |
| `internal/sshbroker/hostkey.go` | `HostKeyStore.GetHostKey` 返回锚结构;`HostKeyTOFU` 回调双模比较;`ErrHostKeyMismatch` 文本带双指纹 |
| `internal/mcpserver/run.go` | `RunStdioCache` **加参**(转发构造器,由 cli 传入;凭据缺失 → 无转发能力);构造转发式实现(holder.Current 实时解析),注入 `NewServerFromSource` |
| `internal/cli/mcp.go` | `--cache` 路径构造转发构造器(`--instance` 解析处;禁止 run.go 内默认实例兜底) |
| `internal/mcpserver/server.go` | `NewServerFromSource` 增加主机密钥存储提供者参数或等价注入缝 |
| `internal/mcpserver/{core,bgtools,context,relay}.go` | 9 处生产 `HostKeyTOFU` 调用改走提供者(穿参改造,受控;relay.go 两处**含 Plan 47 中继目标侧拨号**,转发锚定对中继同样生效) |
| `internal/clientops/`(新文件) | 转发 HTTP 客户端(复用 `pinningTransport`/`SplitTokenPin`/`CacheCred`/实例路径);§2.1 分支映射与全部文案 |
| `internal/cli/pinhostkey.go`(新) | `servers pin-hostkey` 六形态(显示/`--list`/`--fingerprint`/`--from-keyscan`/`--force`/`--clear` + `--hostport`)、指纹 32 字节校验、单事务 owner 审计、受影响条目清单、无锚幂等 |
| `internal/cli/servers.go` | 注册子命令 |
| `internal/cli/doctor.go` | rider 1:免迁移裸连接 checkpoint(永不拷 sidecar) |
| 测试 | T1–T12 各就各位(归属见 §8);接口假体与 store 侧直呼更新(conformance `differential_test.go`、sshbroker hostkey 系、tunnels_control_test、`store/hostkeys_test.go`、`store/export_test.go:125`、`cli/gc_test.go:128`) |
| `docs/`(缓存/配对/README) | rider 3(含清毒完整时序、备份时序、混布假警报、锚残留、equal 自动闭合说明) |

## 12. 真机验收(发版后)

NUC10(升级 v0.15.0,自更新通道)+ 笔记本(同步)+ 生化高速工控板(192.168.1.108,直连网卡):

1. 反馈 §二 三步原样复跑:① 笔记本 `exec_command` 一次成功;② NUC10 `sshmgr audit` 可见 `pin-forward` 行(设备名+指纹+affects 清单);③ `servers pin-hostkey 生化高速工控板` 显示已锚指纹一致、来源=forward、设备=笔记本。
2. 拔 VLAN(真离线)对**另一**新目标首连 → §4 新文案逐字(含呈现指纹);`ssh-keyscan` → NUC10 带外锚定 → 回网 `cache pull` → 连接成功。
3. 跨会话复连:笔记本重启 MCP 进程(缓存仍新鲜)再连工控板 → 应经 409 `equal=true` 自动放行,零用户动作。
4. 混布抽查:一台仍跑 v0.14 的客户端连接指纹锚目标 → 观察「possible MITM」假警报形态(留档即升级)。
5. NUC10 上 `doctor` 计数与 `servers ls` 口径一致(rider 1 真机面)。
6. (可选演练)revoke 笔记本设备码 → `--list` 找其转发锚 → `--clear`(核对受影响条目清单输出)→ 合法重锚 → pull 收敛,验证清毒闭环。

## 13. 参照

- 反馈文件(场景与证据):`C:\WorkSpace\urit_things\sshmgr-feedback-cache-client-toufu-hostkey.md`
- ADR 0002(边缘首信取舍,随 rev3 同步更新);ADR 0001(格式参照)
- Plan 12 设计 §3 决策 8(「离线未知锚 → fail-closed… a refresh resolves it」——本设计修订其「刷新可解」假设,修订理由即 §0 死锁)
- Plan 34(401 隔离语义,§1.2 的不触发约定);Plan 31(ServerInfo 打码/无端口——守卫 ③ 下沉 store 层的原因);Plan 39(轮廓范围快照);Plan 40/46(多实例——「读实例永远显式」铁律见 §2.1);Plan 45(配对)
- `internal/conformance/knownhosts.go`(`--from-keyscan` 解析复用)

---

## 附录:rev 历史变更记录(自 rev0)

- **rev1**(第一轮盲评三路 31 条):落锚原子 insert-only(并发覆盖竞态)+ 同事务审计;锚来源元数据 + `--clear` 清毒原语;owner `--force`/`--clear` 落审计;砍 409 自动重拉;409 不回显指纹;服务端解析规范化(畸形 400);客户端 401/403 映射;`HostKeyTOFU` 回调确认要改;调用点修正生产 10 处点名 relay.go;doctor rider 病因改写 WAL 少计;ADR 补预置投毒增量如实承认。
- **rev2**(第二轮盲评两路 22 条):409 响应加 equal 布尔(等值自动放行:闭合跨会话重复转发+并发等值竞态);ApplyForwardedHostKey 等值统一为全系统双模语义;哨兵文本不动、新文案由转发包装层组装;转发构造参数从 cli/mcp.go(`--instance` 掌握方)传入;`--clear` 受影响条目清单;导出组合式 InsertForwardedPin;GetCacheToken 故障回 500;server_name 多候选确定性;明文拒绝文案逐字;ADR 投毒范围改「入口受轮廓约束、效果全局」;测试矩阵补 T2b/T5b/T8b。
- **rev3**(第三轮盲评两路 17 条):equal 零泄露论证锚点改「⊆ 快照可算」;`/pin-hostkey` 每次请求 stderr 行(探测不零痕迹);equal 判定移入事务;`--list` 全量枚举含孤儿 + `--clear --hostport` 直达;`--force` 单事务读旧→写→审计;`--clear` 无锚幂等不落审计;`--fingerprint` 32 字节解码校验;pin-forward 审计附 affects 清单;pin_device 措辞改跨轮廓可见;ImportSnapshot 空新列归一;守卫 ① JSON 400;doctor 修法钉死;rider 3 清毒时序与备份时序。
