# Plan 42 — 易用性改造:模式缩减 + 发现配对一条龙 + Web 管理 UI 设计 spec · rev4

- 日期：2026-08-28（四轮盲评：rev1 首审 20 条 / rev2 复审 17 条 / rev3 闭环 13 条 / rev4 收敛验证 11 条——含 2 处 owner 拍板（SAS 研磨修法全包、Web TLS 面缓解档位）；变更记录见 §9.1–§9.4）
- 来源：2026-08-28 grilling 会话（Q1–Q21 逐题 owner 确认）+ 与 Plan 43（doctor 探活，rev2.1）冲突审查闭环。痛点实证：多机 client 冷启动 = broker 控制台人肉 `cache-tokens add` + `projects add` + 跨机手抄三串字符串 + ②a 的 TLS 信任库两坑（NODE_EXTRA_CA_CERTS/SAN）。
- 前置：**v0.10.1 先行发版**（合 `plan-40-batch1-impl` / `plan-40-batch2-impl` / Plan 41 worktree；doctor-multi-instance 就绪则同船）。本 plan 依赖 Plan 40 的 per-instance 构件（`CachePathsFor` / device name 三合一 / MAX_OFFLINE 持久化）。
- 状态：rev4 **定稿**（owner 终审通过，2026-08-28——四轮异构盲评 61 条反馈闭环、4 处 owner 拍板全记录于 §9 与 .xcheck 各轮 SUMMARY；进入 implementation plan 阶段）

## 1. 目标与非目标

**目标**（四核心诉求 → 三个交付）：

1. **模式缩减 4 → 2 + 管理面**：②a 在线 HTTP 直连**移除**（不是降级——serve mux 撤 MCP handler）；②c 附录化；client TUI wizard 删除。新铁律：**多机 agent 只读 + 执行，写操作仅 broker TUI / Web UI**；单机 ① 不变（可写）。手机不跑 agent，只做管理面。
2. **发现 + SAS 配对一条龙**：新工作机从「装好二进制」到「agent 可用」= 一条 `ssh-manager pair`（UDP 发现 → 批准 → 双屏比对 → 凭据自动下发 → 首拉 → 写配置）。
3. **Web 管理 UI（手机优先）**：go:embed 单二进制、零 npm/零 CDN；完整管理 + 配对批准 + 吊销 + 审计。

**非目标**：远程写（RW 代理模式，真有需求另立项）；DNS-01 真证书（后续可选增强，需自有域名）；PAKE 第三方库（owner 拍板：SAS+地址绑定+机械校验档位，见 §3.3 安全性质）；公网/外网暴露（同 VLAN 定位）；多 broker 联邦；②a 兼容开关（直接移除，不留 zombie flag）。

## 2. 事实基础（实证锚，2026-08-28 核验）

- **F1（源码，serve.go:205-234）**：`HTTPHandler` 根 mux——`/snapshot` → `cacheAuth`；**其余一切路径** → `mcpChain`（`projectAuth`(:221) → `resolveServer`(:159) → SDK `NewStreamableHTTPHandler`(:220)）。:205-209 注释钉住「两闸不相交 keystone」（project token 不入 /snapshot、设备码不驱动 MCP）。②a 移除 = 撤 mcpChain 分支，根路径落 404。
- **F2（源码，cli/serve_service.go:630-642）**：`probeServeHTTP` GET `https://<addr>/`（根路径，InsecureSkipVerify，1s，401/200=活）。②a 移除后根路径 404 → **必须重指向 `/snapshot`**（未带码 GET → cacheAuth 401，语义保真；auth 层先拒故零序列化零 touch）。**与 Plan 43 R7 互为携带义务，任一 plan 先落地都必须带上此 seam 改动**。
- **F3（源码，store）**：`AddCacheToken(name, profileID)`（cachetoken.go:24）铸一次性明文设备码（revoked 后同名可重发，active 撞名报错——cachetoken_test.go:117/146 钉住）；`VerifyToken`（projects.go:40）；`GenerateToken`（token.go:12，base64url 32B）。**pairing 服务端零新 token 类型，全复用既有铸码路径**。
- **F4（源码，clientops/pin.go）**：`pinningTransport(pin)` = TLS1.3 + InsecureSkipVerify + VerifyConnection 常时 SPKI 比（**握手期拒断，凭据不上行**）；pinned pull 401 ⇒ **quarantine**（销毁 DEK/auth、bin 入隔离目录）；`cache.auth.json = {url, token, pin}`；`DoPull` 含 server_anchored 锚。pair 落地复用 `DoPull` 首拉。
- **F5（源码，mcpserver/cert.go:71）**：`LoadOrCreateServeCert`——ed25519 自签、100 年、幂等三态；SPKI pin = sha256；SAN = `os.Hostname()` + **`LocalNonLoopbackIPs()`**（cert.go:222-235）——**serve 对「自己有哪些地址」有既有权威函数**，机械地址校验（§3.3-3）复用之。
- **F6（分支）**：Plan 40 批1+2 构件在 `plan-40-batch1-impl` / `plan-40-batch2-impl`（已推 origin，未合 master）；v0.10.1 合入为前置。
- **F7（依赖）**：`golang.org/x/crypto v0.41.0` 在 go.mod（argon2/hkdf 可用，零新增）；Go 1.25 stdlib `crypto/ecdh`（X25519）、`crypto/cipher`（AES-GCM）；charm TUI v2；仓库零前端工具链（package.json 为空）→ Web UI 必须 go:embed 自包含。
- **F8（现状）**：全仓无 UDP 代码（discovery 为全新面）；serve 无 health/metrics 端点；`serve bind` 仅服务 ②a。store 为 SQLite（modernc，`MaxOpenConns(1)`+WAL）——跨进程共享态与同事务写 audit 的物理基础。

## 3. 设计 · 批1：模式缩减 + 发现 + SAS 配对

### 3.1 ②a 移除与 ②c/wizard 退役

1. **`HTTPHandler` 重构**：删 `mcpChain`（projectAuth/resolveServer→MCP 的接线）与 serve 侧 `verifyToken` 接线；根 mux 变为：`/snapshot`（不动）+ `/pair/*`（§3.3）+ 其余 **404**。keystone 注释改写：两闸不相交属性降格为「project token 不再是任何远程 MCP 凭据——仅 client 侧 spawn 闸；设备码仍只入 /snapshot」。**project token 本地闸语义（冻结）**：数据源 = 快照内 projects 表（Plan 39 /snapshot 按授权裁剪，same-profile projects 随快照下发）；`mcp --cache` 用 `SSHMGR_TOKEN` 对快照内 projects 校验后放行工具面。
2. **吊销生效语义（三路径冻结）**——owner 侧吊销后 client 侧失效路径取决于吊销对象与在线状态：
   - **project token 吊销、设备码仍活**：client 下一次 pull（在线懒拉 ≤30min）拉到的新快照已无该 project → 本地 spawn 闸拒绝——**在线 ≤30min 生效**。
   - **设备码吊销**：client 下一次 pull 得 pinned 401 ⇒ **quarantine**（F4 既有语义）——工具面即刻断供。
   - **永离线设备**（不 pull）：旧快照 + 本地 project token 的可用窗口 = **`max_offline` 硬上限**（per-instance，pair 下发默认 24h；`LoadCacheSnapshot` 既有 expiry gate 到期拒载）——**不是 30 分钟**。窗口内失窃设备的最终兜底 = 轮换服务器凭据（既有登记）。
3. **serve 的 agent 执行面整体退役**：resolveServer 背后的远程 SSH 拨号、serve 进程后台任务表、`serve bind` 隧道白名单随 ②a 消失（① 单机的本地 `tunnels`/后台任务面不受影响）。serve 收窄为四件事：**权威 vault + /snapshot + /pair + （批2）/ui**。
4. **`probeServeHTTP` 重指向 `/snapshot`**（F2）。`serve status` 语义不变。
5. **②c**：仅文档层面移入「应急附录」（代码不删）。
6. **client TUI**：connect-form/wizard 页代码删除；client 模式缩为 sync/status/doctor 指引。新机首选路径 = `ssh-manager pair`；**手工路径（`cache pull` + 手写 .mcp.json）保留并文档化**（CI/自动化场景，也是迁移路径）。
7. **存量迁移与升级顺序（冻结，三步；并入 compat-matrix 既有升级顺序铁律）**：
   - ① **手工桥迁移**：存量 ②a 机器在旧 serve 上按既有手工流程迁到桥姿态（`cache-tokens add` + `projects add` + client `cache pull`）——遵守「client 先升级、serve 后升级」铁律；
   - ② **升 serve**（v0.11.0）——**前置检查：全部 client 已在 v0.10.1 二进制桥姿态**；②a 当刻 404；
   - ③ **pair 时代**：此后所有新机/重配对一律 `ssh-manager pair`。
   compat-matrix 加 v0.11.0 breaking 行 + 三步 + 各端最低版本（client ≥ v0.10.1）。
8. **开关体系（三态，冻结）**：`pairing`/`discovery`/（批2）`web` 三开关——**env 与 flag 均三态**：显式置位才参与裁决；优先级 **显式 env > 显式 flag > store 设置 > 缺省 true**。Settings 页对被覆盖开关显示「已被 ENV/FLAG 覆盖」。**生效时延（rev4 措辞）**：store 变更后 ≤5s 内生效（内存缓存 TTL 5s）；**env/flag 变更需重启 serve**（进程环境语义）。
9. **默认契约（措辞冻结）**：未使用新功能时**既有客户端工作流零变化**（`/snapshot`/审计/CLI/探活）。同时**明示新增网络面**：discovery（默认开的 UDP 应答面，可关）；pairing（未认证 HTTP 端点，限速+人闸+开关）；web（批2，认证+开关）。

### 3.2 UDP 发现（discovery）与 pin 分级

- **端口 UDP 7878**；`--discovery`（三态）+ env `SSHMGR_SERVE_DISCOVERY`；监听 0.0.0.0:7878/udp（listener 常开，逐包评估）。
- **报文**：首行魔数 `sshmgr-disc-v1`，次行 JSON，≤512B。probe：`{"t":"probe"}`；offer（**只单播回请求源**）：`{"t":"offer","name","spki","tcp"}`。**字段消毒（rev4 冻结，codex#4）**——offer 是未认证输入且进入选择界面：`name` = 同 hint 白名单正则（§3.3-1，违者 serve 侧不发送/以 hostname 兜底）；`tcp` ∈ [1,65535]；`spki` 匹配 `^sha256:[0-9a-f]{64}$`；client 侧对三字段再校验，任一不符**丢弃该 offer**；**所有展示面渲染前剥离 C0/C1 控制字符**。零敏感字段；server 永不主动广播；未知字段忽略；魔数不符静默丢弃。
- **client 侧**（pair 内置）：枚举全部非环回 IPv4 接口，逐接口 broadcast；1.5s 收集窗；按 spki 去重；**多结果必列清单供选**（含 name@addr:port 与 spki 前 16 字符——假 offer 无法抑制真 offer 的单播回应，用户可见双条目，选错由三件套+机械地址校验兜底）。
- **兜底与 pin 分级（rev4：TOFU 收紧，owner 拍板）**：`ssh-manager pair --url https://host:7878 [--pin sha256:...]`。`target_url` = 严格 parse 的连接地址（scheme 必须 https、host[:port] 规范化，畸形 → 拒绝并报错）。**pin 通道分级**：
  - **pin 已知**（discovery offer 自带 / `--pin` 显式）→ pair 全部 HTTP 走 `pinningTransport(pin)`（TLS 层 SPKI 常时硬校验，不匹配即中止——trusted pin 绝不降级为提示）；
  - **pin 未知**（`--url` 且无 `--pin`）→ **默认拒绝**，需显式 `--allow-tofu`（TOFU 姿态：InsecureSkipVerify + sig 机会性验签 WARN + 三件套人闸——**明知无完整 MITM 防护的逃生门**，R12 登记研磨残余）。**client 最终写入的 url 恒为其自己的连接地址**——serve 不下发 url。

### 3.3 SAS 配对协议（/pair/*，冻结 rev4）

角色：client（`ssh-manager pair`）/ server（serve）/ 批准者（broker TUI 批1、Web UI 批2、`serve pair` CLI 兜底）。**pending 队列 = store 新表 `pairing_pending`**（跨进程共享，F8）。

**transcript 与密钥派生（冻结 v2）**：

- 规范 transcript = 依序拼接，每段前 4-byte 小端长度前缀：`"sshmgr-pair-v2" ‖ id ‖ name ‖ target_url ‖ client_pub ‖ cnonce ‖ server_pub ‖ snonce`；`T = SHA256(transcript)`。
- `K_master = HKDF-SHA256(ikm = X25519(client_priv, server_pub), salt = T, info = "sshmgr-pair-v2", L = 64)`；`K_ack = K_master[0:32]`；`K_creds = K_master[32:64]`。两键物理分离。
- **SAS 推导（rev4：绑定 DH 结果，owner 拍板全包之一）**：`R = SHA256("sshmgr-sas-v2" ‖ transcript ‖ K_master)`；32-bit 大端块自左扫描，首个 `< 4,294,000,000` 的 v → `sas = "%06d" % (v % 1_000_000)`；8 块全拒 → `R₁ = SHA256(R ‖ "again")` 递推。**注记（诚实声明）**：此绑定消除「transcript 可见即可算 SAS」的弱性，但对**双端换钥的离线研磨者**（其为两条 DH 腿的参与方，双侧 K_master 均可算）**不独立构成防护**——防研磨的实际防线 = §3.2 TOFU 收紧（默认拒绝无 pin 配对）+ §3.3-3 机械地址校验。
- **三件套双屏显示（冻结）**：两侧各显示同一行：`<name> @ <target_url> SAS <6位>`。

**流程**：

1. **enroll**：client 生成 X25519 临时对 + 32B 随机 id，`POST /pair/enroll` `{id, name, target_url, client_pub, cnonce, profile_hint?}`。`name` = instance 名（**Plan 40 白名单**）；`profile_hint` 校验：`^[\p{L}\p{N}][\p{L}\p{N} ._-]{0,31}$`（缺省/空串 = 不提供，放行；**首字符非法（如空格开头）→ 400**）；全展示面渲染前剥离 C0/C1。serve 收到合法 enroll 立即：①生成 X25519 临时对 + snonce ②签 `sig` ③落 `pairing_pending`（含 `enroll_deadline = now+10min`、`approved_deadline = NULL`）④**enroll 响应返回 `{server_pub, snonce, sig}`**——client 此刻即可算 SAS 显示三件套。**server 临时私钥只留 serve 进程内存**（TTL = 至 `approved_deadline` 或行入终态，**poll 不延长**）。**serve 重启** → pending/approved 行标 expired（poll 410）。
   - **撞名规则（只查不改）**：撞 active cache-token → 查 `last_pull` 记入 pending 标记：**零（未激活）**→ 批准界面标注「⚠ 将替换既有未激活码（finish 时执行）」；**非零（在用）**→ enroll 即拒（419）。**auto-revoke 一律推迟到 finish 单事务内（事务内复查 last_pull 竞态安全）**——未认证 enroll 零吊销副作用。
   - **限速与输入面（rev4 冻结，codex#3）**：`/pair/*` 全部端点——请求体 ≤1KiB、读超时 5s、JSON 畸形 400；per-IP：enroll 5/min、poll 30/min、finish 5/min（超限 429）。**限速 env（rev4 冻结，codex#7）**：`SSHMGR_SERVE_PAIR_ENROLL_PER_MIN`（默认 5，clamp [1,60]）/ `SSHMGR_SERVE_PAIR_POLL_PER_MIN`（30，[1,120]）/ `SSHMGR_SERVE_PAIR_FINISH_PER_MIN`（5，[1,30]）/ `SSHMGR_SERVE_PAIR_PENDING_MAX_IP`（2，[1,10]）/ `SSHMGR_SERVE_PAIR_PENDING_MAX_GLOBAL`（32，[1,128]）——env 类，重启生效。
2. **双窗口（时间谓词入事务，rev4 冻结，codex#2）**：`enroll → 批准` **10 分钟**；`批准 → finish` **120 秒**。**窗口强制不依赖 lazy 清理**：批准 CAS 与 finish 事务均显式带时间谓词（见下），过期未清理的行**不可**被批准/finish；lazy 清理仅为表卫生。
3. **批准（= CAS UPDATE + 机械地址校验，rev4 核心，owner 拍板全包之二）**：批准界面（TUI/Web/CLI）列 pending `{name, target_url, 来源IP, profile_hint(消毒), ⚠未激活码替换标记, 剩余秒}`；**机械地址校验（冻结）**：serve 计算 `target_url` 的 host[:port] 是否 ∈ 本机地址集合（`LocalNonLoopbackIPs()` + hostname，F5 同源）——**不符 → 大字 ⚠「配对声明目标 ≠ 本机地址（疑似中继/假 discovery/错误网络）」+ 拒绝常规批准**，仅显式覆盖可用（CLI `serve pair approve --allow-foreign-url`；TUI/Web 二次确认输入大写 `OVERRIDE`）。这是防 SAS 研磨/假 discovery 的**机械化杀招**——攻击者要让 client 物理连到自己，target_url 必然暴露非本机地址。批准者选 profile（`pair.default_profile` 预选）后执行：`UPDATE pairing_pending SET state='approved', profile=?, approved_at=now, approved_deadline=now+120s WHERE id=? AND state='pending' AND enroll_deadline > now`——败者 409（含**过期**）。**批准面不接触任何密钥态**。`pair-<name>` 复用规则：仅 `pair_generated` 标记 + profile 一致 → 复用（finish 吊旧 token）；否则拒绝。
4. **poll（POST）**：client 每 2s `POST /pair/poll` `{id}`；未批准 → 202 `{"t":"pending"}`；已批准且 `approved_deadline > now` → 200 `{"t":"approved"}`；过期/拒绝/作废 → 410/403。
5. **三件套比对（批准与对照一气呵成）**：client 自 enroll 响应起显示 `<name> @ <target_url> SAS xxxxxx`；owner 在批准界面（**同屏显示同一行三件套**）对照 client 屏——三项全一致才批准；批准后 client 按 y 进 finish（120s 内）。**人闸 = 批准动作 + 双屏对照 + serve 侧机械地址校验**。
6. **finish（铸码 + 幂等 + 事务，冻结 rev4）**：client `POST /pair/finish` `{id, ack}`；`ack = HMAC-SHA256(K_ack, "finish" ‖ id)`。serve 在**单个 SQLite 事务**内：①显式校验 `state='approved' AND approved_deadline > now`（过期 → 410，不依赖清理）②（若撞名标记）auto-revoke 未激活旧码（事务内复查 `last_pull`）③`AddCacheToken(name, profile)` ④project 复用（吊旧）或新建（`pair_generated=true`）⑤project token 签发 ⑥**audit 行同事务**——任一步失败整体回滚。提交后以 **AEAD（AES-256-GCM，键 K_creds）** 封装 `{spki, profile, device_code, project_token, max_offline}` 返回（信封含 profile，client 回显「已授权 profile: X」）。`max_offline` = `pair.default_max_offline`（缺省 24h）。**幂等（rev4 冻结，kimi#2）**：成功密文缓存 pending 行（state=delivered）；finish 重发 → **凭 id 直接回吐缓存密文，不再验 ack**（私钥已清；id 32B 不可猜、密文无 K_creds 不可解——安全）；delivered 行 5 分钟后 lazy 清理、**重放上限 10 次**（超出删行 410）；行入任一终态即清内存 ECDH 私钥。未批准/窗口外/ack 错 → 409/410/403。
7. **client 落地（rev4：落盘顺序反转，codex#1）**：解密凭据 → **先全部安全落盘**：`cache.auth.json{url, token:设备码, pin:spki}` + `cache.config.json{"max_offline":...}` + **`pair.<name>.mcp.json`（完整 .mcp.json 片段，env.SSHMGR_TOKEN = project token 真值，0600 权限，落 instance 目录）** → 然后才调 `DoPull` 首拉（锚随盘）。**首拉失败 = 全部凭据已在盘**：重试 `cache pull --instance <name>` + 从产物文件取 .mcp.json（或 `--write-mcp <path>` 从产物复制到目标）——**project token 一次性返回值永不丢失**。终端仍零完整凭据（打印片段用 `<project-token>` 占位符 + 指引产物文件路径；前 8 字符回显）。**同名覆盖语义**：per-instance 目录已有 `cache.auth.json` → 默认拒绝提示；`--force` = 按 Plan 40 换码 runbook 清 auth+bin+meta+quarantine（保留 config）后重写。
8. **审计（同事务 + 脱敏白名单，冻结 rev4）**：pairing 事件与状态变更同 SQLite 事务。**audit 字段白名单（rev4 扩展，codex#6）**：事件类型、实例名、profile 名、**target 标识（server/project/setting 的非敏感 ID）与 action 摘要（做了什么，如 `rotate`/`create`/`delete`，不含值）**、来源 IP、时间戳、结果码——**永不落**：凭据值/token/设备码/pin/SAS/密文/ack/sig（Web 访问日志同纪律）。
9. **CLI 面**：`ssh-manager pair [--url] [--pin] [--allow-tofu] [--profile-hint] [--write-mcp] [--force]`；自动化免比对 = env `SSHMGR_PAIR_ASSUME_SAS=1`（STUB 大字警告）；`serve pair ls/approve [--allow-foreign-url]/reject`（approve 输出三件套行）。

**编码契约（冻结）**：`id` = 32B hex64；`client_pub`/`server_pub` = X25519 raw 32B base64url；`cnonce`/`snonce` = 16B base64url；`sig` = ed25519 64B base64url；`ack` = HMAC-SHA256 32B hex。**畸形**：重复 id 409 / 非法公钥或 nonce 长度错 400 / JSON 畸形 400 / CAS 败 409。

**安全性质（rev4 重写，诚实版）**：

- **pin 已知通道**（discovery 正常路径 / `--pin`）：换钥型 MITM 在 **TLS 层**被 `pinningTransport` 拒断（握手期，凭据不上行）——主防线。
- **机械地址校验**（serve 侧，自动）：任何让 client 物理连到非本机地址的攻击（假 discovery、研磨型换钥 MITM）→ 批准界面 ⚠ + 强制显式覆盖——**不依赖 owner 记 IP**。
- **换钥型 MITM + SAS**：三件套含 target_url 与 SAS（绑 transcript+K_master）——单一防线不叠加声称「必致双屏不一致」（**TOFU 下 SAS 可被双端参与方离线研磨**，见 R12）；防御组合 = TLS pin + 机械校验 + 人闸。
- **透明中继**：SAS 一致——防线 = target_url 双屏比对 + 机械校验（中继者地址 ≠ 本机地址 → ⚠）；**凭据不泄**（TLS 内文不可读 + AEAD 信封）；残余 = 位置攻击（R10 登记）。
- **TOFU（`--allow-tofu`）**：**无完整 MITM 防护**（SAS 可研磨、无 TLS 锚）——R12 登记为逃生门残余，仅限受控环境。
- 重放 → nonce + 双窗口（事务内时间谓词）+ 一次性码 + delivered 重放上限；穷举 → 批准人审 + 限速；失窃 → 双吊销（§3.1-2 三路径）。

### 3.4 残余风险登记

- **人因（唯一人闸）**：三件套比对 + 机械校验 ⚠ 的忽略残余——文案强警示。
- **R10（owner 拍板）**：透明中继位置攻击（DoS/url 钉死/流量分析，凭据不泄）接受。
- **R12（rev4 登记，owner 拍板全包）**：`--allow-tofu` 路径无完整 MITM 防护（双端换钥者的离线 SAS 研磨 ~10⁶ 哈希 + 无 TLS 锚）——默认拒绝、显式 opt-in、仅受控环境；主路径（discovery/--pin）不受影响。
- **R11**：hint 白名单外的文字误导面——真闸 = 批准者选 profile + 信封回显；接受。
- **LAN 可见性**：Q1 接受。
- **配对面 DoS**：enroll/poll/finish 限速 + pending 配额（env 可调）后仍可短暂占满——半可信 LAN 接受。

## 4. 设计 · 批2：Web 管理 UI

### 4.1 架构

- `/ui` 路由组挂 serve mux；**go:embed** + SSR（html/template）；零 npm/CDN/单二进制；手机优先响应式；同 VLAN。
- **`/ui` 开关**：`--web`（三态）+ env `SSHMGR_SERVE_WEB` → 404。
- **无 admin 用户**：全路由 403 + 提示页「在 broker 上运行 `ssh-manager serve admin set`」——**无任何未认证首设端点**。
- 页面清单：登录 / 总览 / Servers / Profiles / Projects / Instances / **Pairing（两段式：列表页 5s refresh；批准详情页三件套 + 机械校验 ⚠ + 选 profile + 强制 OVERRIDE 输入，无刷新）** / Audit / Settings（admin 密码、默认 profile、默认 max_offline、三开关含覆盖状态）。
- **TLS 信任缓解（rev4，owner 拍板 B）**：**全页面脚注常显本 serve 证书 SPKI 指纹**（可随时对照 broker TUI `serve cert-info` 输出）；**Servers 凭据写入表单默认折叠**（「编辑凭据」显式展开才出现输入框——降低凭据经 LAN 传输的暴露频次）；首登引导文案要求「首次登录前在 broker TUI 核对一次指纹」。

### 4.2 认证、会话与审计

- **首启**：`ssh-manager serve admin set`；argon2id（m=64MB, t=3, p=4）入 `admin_users`；TUI 可重置（全量失效）。
- **session = store 表 `admin_sessions`**：存 id 的 SHA-256、绝对/滑动过期、创建 IP——跨进程即时失效。登录限速 5 次/15min（IP+账号）。cookie `HttpOnly+Secure+SameSite=Strict`；12h 滑动 + **绝对上限 7d**；改密/重置全量失效。CSRF 会话 token。`CSP: default-src 'none'; style-src 'self'`；全响应 no-store。
- **审计覆盖**：登录成败、Servers/Profiles/Projects/Instances 一切写、token 生命周期、pairing 全事件、admin/settings 修改——同事务纪律 + 脱敏白名单（§3.3-8）。
- **R5（rev4 升级登记，owner 拍板 B）**：Web 面自签证书**无浏览器端锚定**（无 SPKI pin 机制）——首连/换证时 LAN MITM 可截 admin 密码；缓解 = SPKI 页脚常显 + 首登 TUI 核对指引 + 凭据表单折叠 + 登录限速/审计；**残余接受**（半可信 VLAN 定位；DNS-01 真证书为根本解，远期）。

### 4.3 配对批准（Web）

= §3.3 步骤 3 的第二 UI：同表、同 CAS UPDATE、同机械地址校验（⚠ + OVERRIDE 输入）；三件套与 TUI/CLI 同行格式。

## 5. 测试策略

**批1**：
- discovery：回环 UDP 双 socket——probe/offer、魔数/JSON 畸形静默、**offer 字段消毒（控制字符 name/越界 tcp/畸形 spki → offer 丢弃）**、单播断言、多接口、开关零应答。
- pairing（httptest TLS + 真 handler + 真 store）：
  - 编码契约：重复 id 409 / 非法公钥 400 / nonce 长度 400 / **hint 首字符非法 400、空串放行** / body >1KiB 413 / JSON 畸形 400。
  - **限速**：enroll/poll/finish 三端点 per-IP 限速 429；env 覆盖生效。
  - 未认证 enroll 零副作用钉子（撞名 → 落 pending + 旧 token 仍 active）；last_pull 非零 419。
  - enroll 响应含 server_pub/snonce/sig；serve 重启 → in-flight expired + 私钥清除。
  - **双窗口时间谓词**：构造过期未清理 pending → approve CAS 败（409）、finish 410——**不依赖清理器**。
  - **机械地址校验**：target_url = 非本机 IP → 批准界面 ⚠ 标记 + 常规 approve 拒绝 + `--allow-foreign-url` 覆盖腿；本机 IP → 无 ⚠。
  - CAS 并发 409；poll POST 状态机；SAS/HKDF 确定性向量（**K_master 绑定后双侧一致断言** + R₁ 回退）。
  - finish：ack 错拒；单事务原子性（中途失败 → 回滚 + audit 零行）；事务内 auto-revoke 竞态复查；幂等重放（**不验 ack 直接回吐**断言）；delivered TTL/重放上限；复用四分支；信封含 profile。
  - **TOFU**：`--url` 无 `--pin` → 默认拒绝；`--allow-tofu` 才走 InsecureSkipVerify+WARN 腿。
  - pin 分级：pin 已知 → 换证即中止（握手期）；MITM 双腿（换 server_pub → 三件套不一致；透明中继 → SAS 一致 + client 屏 target_url=中继地址 + 机械校验 ⚠ 断言）。
  - 吊销三路径（§3.1-2）。
  - audit：同事务断言；白名单断言（含 **target ID/action 字段存在**、凭据值零出现）。
- ②a 移除契约：404/401/三态开关矩阵（**store 变更 ≤5s 生效 + env/flag 需重启措辞入帮助文本**）/probe PASS。
- pair e2e（`SSHMGR_PAIR_ASSUME_SAS=1`）：**首拉失败 → 产物 `pair.<name>.mcp.json` 已在盘且含真 token 断言**；`cache pull` 重试成功；url=连接地址；占位符打印；`--write-mcp`；同名默认拒 + `--force`。
- 全仓绿；默认契约（既有工作流零变化）。

**批2**：
- argon2id / 限速 / session 表（上限/滑动/改密全量/跨进程）/ CSRF / CSP+no-store / `--web` 三态。
- 无 admin → 全路由 403（无未认证 setup 端点断言）。
- **SPKI 页脚常显断言**（每页含指纹行）；**凭据表单折叠断言**（默认 GET 无输入框，显式展开才出现）。
- Pairing 两段式 + 三件套 + 机械校验 ⚠ + OVERRIDE 输入；与 TUI 同表同 CAS。
- 页面 smoke；一次性 token 仅签发响应；Web 写操作 audit 全覆盖 + 脱敏。

## 6. 文档联动

deployment-modes.md 重写（4→2+管理面；pair 一条龙；②a 三步迁移）；multi-machine.md（删 TLS 两坑节、pair 流程、手工桥 = 官方迁移路径）；quickstart-multi-machine.md 重写；broker-host-agent.md（姿势 A 删；②c 附录）；agent-tools.md（只读铁律 + 吊销三路径）；compat-matrix.md（v0.11.0 breaking + 三步 + 最低版本）；threat-model.md（discovery 零敏感面 / SAS 绑定与**研磨诚实声明 + TOFU R12** / 机械地址校验 / 中继 R10 / 吊销三路径 / audit 同事务与脱敏 / Web R5 升级与 SPKI 常显）；README 索引；backlog 销项。

## 7. 验收清单

**批1**：
1. 全仓测试绿；②a 移除契约绿。
2. pair e2e 绿（**含首拉失败产物已落盘**、`--force`、TOFU 默认拒）；MITM 双腿绿；机械地址校验腿绿；时间谓词条绿；HKDF/SAS 向量绿；未认证 enroll 零副作用钉子绿。
3. **真机 gate（owner）**：NUC10 升 serve；三步迁移笔记本；干净目录 `ssh-manager pair` → TUI 批准（三件套对照 + 机械校验无 ⚠）→ 在线/断网各验；`cache-tokens ls`/audit 可见。
4. ②a 负面验收：旧 ②a `.mcp.json` → 404。
**批2**：
5. 手机登录 Web → 批准真配对（OVERRIDE 仅在 ⚠ 场景要求）→ 闭环；一键双吊销。
6. 认证/CSRF/限速/会话/审计/SPKI 页脚/表单折叠测试绿；全仓绿。
7. **v0.11.0 发布**。

## 8. 风险与备选

- **R1 存量迁移破坏面**：三步 + 铁律引用 + 验收演练。
- **R2 配对人因**：三件套 + 机械校验 ⚠；免比对仅 env。
- **R3/R4 防火墙**：真机覆盖；`--url`(+`--pin`) 兜底。
- **R5（rev4 升级，owner 拍板 B）**：Web 无浏览器端证书锚定——SPKI 常显/首登核对/表单折叠/限速/审计；残余接受；DNS-01 远期根本解。
- **R6 Web 攻击面**：同 VLAN + 认证纪律 + 三态开关 + 无 admin 全 403。
- **R7 probeServeHTTP seam**：与 Plan 43 R7 双登记。
- **R8 pending = store 表 + 密钥态 = serve 内存**：决策持久/密钥进程态；重启作废 in-flight；表内零私钥；delivered TTL+重放上限。
- **R9（备选）**：poll 嫌糙 → 长轮询/SSE；不取。
- **R10（owner 拍板）**：透明中继位置攻击接受。
- **R11**：hint 文字误导面接受。
- **R12（rev4，owner 拍板 A）**：TOFU（`--allow-tofu`）无完整 MITM 防护（SAS 可离线研磨 + 无 TLS 锚）——默认拒绝、显式 opt-in、受控环境专用；主路径不受影响。

## 9. 变更记录

### 9.1 rev0 → rev1（首审 20 条闭环）
（明细同 rev1 原文。）

### 9.2 rev1 → rev2（复审 17 条 + 1 拍板）
（明细同 rev2 原文。）

### 9.3 rev2 → rev3（闭环验证 13 条）
（明细同 rev3 原文。）

### 9.4 rev3 → rev4（收敛验证轮：codex 7 + kimi 4，11 条闭环，含 2 处 owner 拍板；两家均确认 rev3 十二组修法真闭环）

1. **SAS 绑定 K_master + 研磨诚实声明（kimi#1 高；owner 拍板全包）**：`R = SHA256(label ‖ transcript ‖ K_master)`；同时如实声明该绑定对「双端换钥参与方」不独立防研磨——配套三件：**TOFU 收紧**（`--url` 无 `--pin` 默认拒绝，需 `--allow-tofu`，R12 登记）、**serve 侧机械地址校验**（target_url vs `LocalNonLoopbackIPs()`，不符 → ⚠ + 显式覆盖）、:89 旧断言改写。**修法超出 kimi 提案**：kimi 单行绑定经反驳证不闭合研磨面（研磨者双侧 K_master 可算、离线 ~10⁶），机械校验才是杀招。
2. **project token 先落盘后首拉（codex#1 高）**：落盘顺序反转——`pair.<name>.mcp.json`（含真值，0600）在 `DoPull` 之前写入 instance 目录；首拉失败零丢失；终端仍零完整凭据。
3. **双窗口时间谓词入事务（codex#2 高）**：批准 CAS 带 `enroll_deadline > now`、finish 事务带 `approved_deadline > now`——过期未清理行不可用，窗口强制不依赖 lazy 清理；内存私钥 TTL 以 deadline 为准（poll 不延长）。
4. **poll/finish 限速与输入面（codex#3 中）**：三端点 per-IP 限速 + body ≤1KiB + 读超时 5s；限速 env 五项冻结（默认值 + clamp + 重启生效）。
5. **offer 字段消毒（codex#4 中）**：name/tcp/spki 白名单 + client 侧丢弃畸形 offer + 全展示面剥离控制字符；target_url 严格 parse 规范化。
6. **Web TLS 缓解（codex#5 高；owner 拍板 B）**：SPKI 页脚常显 + 首登 TUI 核对指引 + 凭据表单默认折叠；R5 从 habituation 升级为「无锚定 MITM 面」登记。
7. **audit target/action 字段（codex#6 中）**：白名单扩非敏感 target ID + action 摘要。
8. **限速 env 定义（codex#7 低）**：并入 4。
9. **delivered 重放语义（kimi#2 低）**：凭 id 回吐缓存密文、不验 ack（私钥已清；id 不可猜/密文无钥不可解）。
10. **开关生效时延措辞（kimi#3 低）**：store ≤5s；env/flag 需重启。
11. **hint 空串语义（kimi#4 低）**：缺省放行，首字符非法才 400；测试描述对应修正。
