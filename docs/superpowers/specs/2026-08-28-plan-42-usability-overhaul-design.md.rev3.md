# Plan 42 — 易用性改造:模式缩减 + 发现配对一条龙 + Web 管理 UI 设计 spec · rev3

- 日期：2026-08-28（rev1 首审 20 条、rev2 复审 17 条闭环；rev3 同日第三轮（闭环验证轮）：codex SUGGEST_CHANGES 10 条 + kimi SUGGEST_CHANGES 6 条，去重 13 条闭环——两家均确认前轮修法为真闭环、方案方向可实施；rev0→rev1 见 §9.1，rev1→rev2 见 §9.2，rev2→rev3 见 §9.3）
- 来源：2026-08-28 grilling 会话（Q1–Q21 逐题 owner 确认）+ 与 Plan 43（doctor 探活，rev2.1）冲突审查闭环。痛点实证：多机 client 冷启动 = broker 控制台人肉 `cache-tokens add` + `projects add` + 跨机手抄三串字符串 + ②a 的 TLS 信任库两坑（NODE_EXTRA_CA_CERTS/SAN）。
- 前置：**v0.10.1 先行发版**（合 `plan-40-batch1-impl` / `plan-40-batch2-impl` / Plan 41 worktree；doctor-multi-instance 就绪则同船）。本 plan 依赖 Plan 40 的 per-instance 构件（`CachePathsFor` / device name 三合一 / MAX_OFFLINE 持久化）。
- 状态：rev3 定稿待 owner 审

## 1. 目标与非目标

**目标**（四核心诉求 → 三个交付）：

1. **模式缩减 4 → 2 + 管理面**：②a 在线 HTTP 直连**移除**（不是降级——serve mux 撤 MCP handler）；②c 附录化；client TUI wizard 删除。新铁律：**多机 agent 只读 + 执行，写操作仅 broker TUI / Web UI**；单机 ① 不变（可写）。手机不跑 agent，只做管理面。
2. **发现 + SAS 配对一条龙**：新工作机从「装好二进制」到「agent 可用」= 一条 `ssh-manager pair`（UDP 发现 → 批准 → 双屏比对 → 凭据自动下发 → 首拉 → 写配置）。
3. **Web 管理 UI（手机优先）**：go:embed 单二进制、零 npm/零 CDN；完整管理 + 配对批准 +吊销 + 审计。

**非目标**：远程写（RW 代理模式，真有需求另立项）；DNS-01 真证书（后续可选增强，需自有域名）；PAKE 第三方库（owner 拍板：SAS+地址绑定档位，见 §3.3 安全性质）；公网/外网暴露（同 VLAN 定位）；多 broker 联邦；②a 兼容开关（直接移除，不留 zombie flag）。

## 2. 事实基础（实证锚，2026-08-28 核验）

- **F1（源码，serve.go:205-234）**：`HTTPHandler` 根 mux——`/snapshot` → `cacheAuth`；**其余一切路径** → `mcpChain`（`projectAuth`(:221) → `resolveServer`(:159) → SDK `NewStreamableHTTPHandler`(:220)）。:205-209 注释钉住「两闸不相交 keystone」（project token 不入 /snapshot、设备码不驱动 MCP）。②a 移除 = 撤 mcpChain 分支，根路径落 404。
- **F2（源码，cli/serve_service.go:630-642）**：`probeServeHTTP` GET `https://<addr>/`（根路径，InsecureSkipVerify，1s，401/200=活）。②a 移除后根路径 404 → **必须重指向 `/snapshot`**（未带码 GET → cacheAuth 401，语义保真；auth 层先拒故零序列化零 touch）。**与 Plan 43 R7 互为携带义务，任一 plan 先落地都必须带上此 seam 改动**。
- **F3（源码，store）**：`AddCacheToken(name, profileID)`（cachetoken.go:24）铸一次性明文设备码（revoked 后同名可重发，active 撞名报错——cachetoken_test.go:117/146 钉住）；`VerifyToken`（projects.go:40）；`GenerateToken`（token.go:12，base64url 32B）。**pairing 服务端零新 token 类型，全复用既有铸码路径**。
- **F4（源码，clientops/pin.go）**：`pinningTransport(pin)` = TLS1.3 + InsecureSkipVerify + VerifyConnection 常时 SPKI 比（**握手期拒断，凭据不上行**）；pinned pull 401 ⇒ **quarantine**（销毁 DEK/auth、bin 入隔离目录）；`cache.auth.json = {url, token, pin}`；`DoPull` 含 server_anchored 锚。pair 落地复用 `DoPull` 首拉。
- **F5（源码，mcpserver/cert.go:71）**：`LoadOrCreateServeCert`——ed25519 自签、100 年、幂等三态；SPKI pin = sha256。**配对临时公钥可用此长期 ed25519 私钥签名**（把配对 transcript 绑到 TLS 身份）。
- **F6（分支）**：Plan 40 批1+2 构件在 `plan-40-batch1-impl` / `plan-40-batch2-impl`（已推 origin，未合 master）；v0.10.1 合入为前置。
- **F7（依赖）**：`golang.org/x/crypto v0.41.0` 在 go.mod（argon2/hkdf 可用，零新增）；Go 1.25 stdlib `crypto/ecdh`（X25519）、`crypto/cipher`（AES-GCM）；charm TUI v2；仓库零前端工具链（package.json 为空）→ Web UI 必须 go:embed 自包含。
- **F8（现状）**：全仓无 UDP 代码（discovery 为全新面）；serve 无 health/metrics 端点；`serve bind` 子命令（跨机隧道监听白名单）仅服务 ②a 场景。store 为 SQLite（modernc，`MaxOpenConns(1)`+WAL）——**跨进程共享态的既有先例**（TUI/serve/CLI 本就同库并发），也是**同事务写 audit 的物理基础**。

## 3. 设计 · 批1：模式缩减 + 发现 + SAS 配对

### 3.1 ②a 移除与 ②c/wizard 退役

1. **`HTTPHandler` 重构**：删 `mcpChain`（projectAuth/resolveServer→MCP 的接线）与 serve 侧 `verifyToken` 接线；根 mux 变为：`/snapshot`（不动）+ `/pair/*`（§3.3）+ 其余 **404**。keystone 注释改写：两闸不相交属性降格为「project token 不再是任何远程 MCP 凭据——仅 client 侧 spawn 闸；设备码仍只入 /snapshot」。**project token 本地闸语义（冻结）**：数据源 = 快照内 projects 表（Plan 39 /snapshot 按授权裁剪，same-profile projects 随快照下发）；`mcp --cache` 用 `SSHMGR_TOKEN` 对快照内 projects 校验后放行工具面。
2. **吊销生效语义（rev3 重写，三路径冻结）**——owner 侧吊销（单吊销或双吊销）后 client 侧失效路径取决于吊销对象与在线状态：
   - **project token 吊销、设备码仍活**：client 下一次 pull（在线懒拉 ≤30min）拉到的新快照已无该 project → 本地 spawn 闸拒绝——**在线 ≤30min 生效**。
   - **设备码吊销**：client 下一次 pull 得 pinned 401 ⇒ **quarantine**（F4 既有语义：本地 DEK/auth 销毁、bin 入隔离）——工具面即刻断供。
   - **永离线设备**（不 pull）：旧快照 + 本地 project token 的可用窗口 = **`max_offline` 硬上限**（per-instance，pair 下发默认 24h；`LoadCacheSnapshot` 既有 expiry gate 到期拒载）——**不是 30 分钟**。`max_offline` 窗口内失窃设备的最终兜底 = 轮换服务器凭据（既有登记）。
3. **serve 的 agent 执行面整体退役**：resolveServer 背后的远程 SSH 拨号、serve 进程后台任务表、`serve bind` 隧道白名单随 ②a 消失（① 单机的本地 `tunnels`/后台任务面不受影响）。serve 收窄为四件事：**权威 vault + /snapshot + /pair + （批2）/ui**。
4. **`probeServeHTTP` 重指向 `/snapshot`**（F2）。`serve status` 语义不变。
5. **②c**：仅文档层面移入「应急附录」（代码不删——`mcp` stdio 直开本机 vault 是 ① 的正常形态，②c 只是「在 broker 上这么干」的用法说明）。
6. **client TUI**：connect-form/wizard 页代码删除；client 模式缩为 sync/status/doctor 指引。新机首选路径 = `ssh-manager pair`；**手工路径（`cache pull` + 手写 .mcp.json）保留并文档化**（CI/自动化零交互场景，也是下面的迁移路径）。
7. **存量迁移与升级顺序（冻结，三步；显式并入 compat-matrix 既有升级顺序铁律）**：`/pair/*` 只存在于新 serve——**serve 升级前不存在 pair 路径**。故：
   - ① **手工桥迁移**：存量 ②a 机器在旧 serve 上按既有手工流程迁到桥姿态（broker 控制台 `cache-tokens add` + `projects add`，client `cache pull`）——**本 plan 明确引用并遵守 compat-matrix「client 先升级、serve 后升级」铁律**；
   - ② **升 serve**（v0.11.0）——**前置检查：全部 client 已在 v0.10.1 二进制桥姿态**；②a 当刻 404，桥姿态机器无感；
   - ③ **pair 时代**：此后所有新机/重配对一律 `ssh-manager pair`。
   compat-matrix 加 v0.11.0 breaking 行 + 上述三步 + 各端最低版本（client ≥ v0.10.1）。
8. **开关体系（三态，rev3 冻结）**：`pairing`/`discovery`/（批2）`web` 三开关统一规则——**env 与 flag 均为三态**：显式置位（含显式 false）才参与裁决，未提供则回落下一级；优先级 **显式 env > 显式 flag > store 设置（Settings 页）> 缺省 true**。Settings 页对被 env/flag 覆盖的开关显示「已被 ENV/FLAG 覆盖」状态。**生效时延**：serve 侧评估带 ≤5s 内存缓存——store 变更后 **≤5s 内生效**（不写「即时」；HTTP 路由逐请求查缓存，UDP listener 常开、逐包评估决定是否应答）；均不要求重启。
9. **默认契约（措辞冻结）**：未使用新功能时**既有客户端工作流零变化**（`/snapshot`/审计/CLI/探活行为不变）。同时**明示新增网络面**：discovery = 默认开启的新 UDP 应答面（Q1 半可信 LAN 已接受，可关）；pairing = 未认证 HTTP 端点（限速+人闸+开关）；web = 批2 管理面（认证+开关）。「零变化」不覆盖这些显式新增面。

### 3.2 UDP 发现（discovery）

- **端口 UDP 7878**（与 TCP serve 同号）；serve `--discovery`（三态，§3.1-8）+ env `SSHMGR_SERVE_DISCOVERY`；监听 0.0.0.0:7878/udp（listener 常开，开关逐包评估）。
- **报文**：首行魔数 `sshmgr-disc-v1`，次行 JSON，整体 ≤512B。probe：`{"t":"probe"}`；offer（**只单播回请求源**）：`{"t":"offer","name":<显示名>,"spki":<sha256:...>,"tcp":<TCP端口>}`。`name` = 服务器显示名（TUI/Web 可设，缺省 hostname）；**零敏感字段**；server 永不主动广播。未知字段双方忽略（前瞻兼容）；魔数不符静默丢弃。
- **client 侧**（pair 内置）：枚举全部非环回 IPv4 接口，**逐接口** broadcast（覆盖多宿主 + 换网两情形）；1.5s 收集窗；按 spki 去重；多结果列表供选，单结果直入配对。
- **兜底与 pin 分级（rev3 冻结）**：`ssh-manager pair --url https://host:7878 [--pin sha256:...]`。**pin 通道分级**：
  - **pin 已知**（discovery offer 自带，或 `--pin` 显式传入）→ pair 的**全部 HTTP 请求走既有 `pinningTransport(pin)`**（TLS 层 SPKI 常时硬校验，不匹配即中止——与 pull 同构，trusted pin 场景绝不降级为提示）；
  - **pin 未知**（`--url` 且无 `--pin`）→ InsecureSkipVerify + sig 机会性验签（失败 WARN）+ 三件套人闸兜底（TOFU 姿态）。
  **client 最终写入的 url 恒为其自己的连接地址**（discovery 响应源地址或 `--url`），构造上即正确——serve 不下发 url。

### 3.3 SAS 配对协议（/pair/*，冻结 rev3）

角色：client（新工作机，`ssh-manager pair`）/ server（serve）/ 批准者（broker TUI 批1、Web UI 批2、`serve pair` CLI 兜底）。**pending 队列 = store 新表 `pairing_pending`**（跨进程共享，F8 先例；serve 重启行见步骤 1）。

**transcript 与密钥派生（冻结 v2）**：

- 规范 transcript 字节串 = 依序拼接，每段前加 4-byte 小端长度前缀：`"sshmgr-pair-v2" ‖ id ‖ name ‖ target_url ‖ client_pub ‖ cnonce ‖ server_pub ‖ snonce`；`T = SHA256(transcript)`。**`target_url` = client 实际连接的地址**（discovery 源或 `--url`，代码注入非人工输入）。
- `K_master = HKDF-SHA256(ikm = X25519(client_priv, server_pub), salt = T, info = "sshmgr-pair-v2", L = 64)`；`K_ack = K_master[0:32]`（finish 的 HMAC-SHA256 键）；`K_creds = K_master[32:64]`（凭据 AEAD 键）。两键物理分离，不复用。
- **SAS 推导（无偏，冻结）**：`R = SHA256("sshmgr-sas-v2" ‖ transcript)`；取 R 的 32-bit 大端块自左向右扫描，首个 `< 4,294,000,000`（= ⌊2³²/10⁶⌋×10⁶）的块 v → `sas = fmt.Sprintf("%06d", v % 1_000_000)`；**回退**：8 块全拒 → `R₁ = SHA256(R ‖ "again")` 继续同规则，递推确定性。
- **三件套双屏显示（冻结）**：两侧各显示同一行：`<name> @ <target_url> SAS <6位>`。

**流程（rev3：密钥态全前移 enroll 响应）**：

1. **enroll**：client 生成 X25519 临时密钥对 + 32B 随机 id，`POST /pair/enroll` `{id, name, target_url, client_pub, cnonce, profile_hint?}`。`name` = instance 名（**Plan 40 白名单校验**）；`profile_hint` **校验（rev3 冻结）**：`^[\p{L}\p{N}][\p{L}\p{N} ._-]{0,31}$`（Unicode 可打印、零控制字符、≤32 rune），不匹配 → 400；**所有展示面（TUI/Web/CLI/终端）渲染前剥离 C0/C1 控制字符**——hint 是未认证任意输入，不得获得攻击 SAS/地址显示的渲染能力。serve 收到合法 enroll 后立即：①生成自己的 X25519 临时对 + snonce ②对 transcript 签 `sig` ③落 `pairing_pending` 行（id、name、target_url、client_pub、cnonce、server_pub、snonce、sig、state=pending、双窗口时间戳）④**enroll 响应直接返回 `{server_pub, snonce, sig}`**——client 此刻即可计算并显示三件套（步骤 5），120s finish 窗口不再覆盖人腿比对。**server 临时私钥只留 serve 进程内存**（id 索引，TTL 跟随 pending）。**serve 重启**：内存私钥丢失 → 启动时将 pending/approved 行标 expired（client poll 得 410「broker restarted, re-pair」）。
   - **撞名规则（rev3 改为只查不改）**：`name` 撞 active cache-token → **enroll 仅记录不改库**——查 `last_pull` 落入 pending 行的标记位：**零（未激活）**→ 批准界面标注「⚠ 将替换既有未激活码（finish 时执行）」；**非零（在用）**→ enroll 即拒（419，提示换名或 owner 先 `cache-tokens revoke`）。**auto-revoke 一律推迟到 finish 单事务内执行（事务内复查 `last_pull` 仍为零才执行——竞态安全）**——未认证的 enroll 永远不产生任何吊销副作用。
   - 限速：per-IP enroll 5/min、全局 30/min、per-IP pending ≤2、全局 pending ≤32（超限 429/409）。
2. **双窗口（冻结；rev3 起第二窗口只需覆盖确认+网络）**：`enroll → 批准` 窗口 **10 分钟**（人赶到界面 + 批准 + 对照 client 屏）；`批准 → finish` 窗口 **120 秒**（client 按 y + 网络往返——三件套比对已前移，见步骤 5）。起算点 = 各自事件完成时刻；过期 lazy 清理。
3. **批准（任何批准面 = 只写一条 profile 决策）**：批准界面（TUI/Web/CLI）列 pending `{name, target_url, 来源IP, profile_hint(已消毒), ⚠未激活码替换标记, 剩余秒}`；批准者选 profile（vault 设置 `pair.default_profile` 预选）后执行**单条 CAS UPDATE**：`UPDATE pairing_pending SET state='approved', profile=?, approved_at=? WHERE id=? AND state='pending'`——败者 409。**批准面不生成/不接触任何密钥态**。`pair-<name>` project 复用规则（冻结）：仅当 (a) 该 project 带 `pair_generated` 标记且 (b) profile 一致 → 允许复用（finish 时吊销其全部旧 token）；同名非 pair project 或 profile 不一致 → 批准界面拒绝并提示换名/owner 处置。
4. **poll（POST）**：client 每 2s `POST /pair/poll` `{id}`；未批准 → 202 `{"t":"pending"}`；已批准 → 200 `{"t":"approved"}`（密钥材料已在 enroll 响应交付，此处仅翻状态）；过期/拒绝/作废 → 410/403。
5. **三件套比对（rev3：比对与批准同刻完成）**：client 自 enroll 响应起即显示 `<name> @ <target_url> SAS xxxxxx`；owner 在批准界面（TUI/Web/CLI **必须同屏显示同一行三件套**）**对照 client 屏**——三项（实例名/目标地址/SAS）全一致才批准；批准后 client 端再按确认（y）进 finish（120s 内）。**人闸 = 批准动作 + 双屏对照，一气呵成，不跨窗口**。
6. **finish（铸码 + 幂等 + 事务，冻结 rev3）**：client `POST /pair/finish` `{id, ack}`；`ack = HMAC-SHA256(K_ack, "finish" ‖ id)`。serve 在**单个 SQLite 事务**内完成（F8 单连接串行）：①（若撞名标记）auto-revoke 未激活旧码（事务内复查 `last_pull` 仍为零）②`AddCacheToken(name, profile)` ③project 复用（吊旧 token）或新建（`pair-<name>`，`pair_generated=true`）绑同 profile ④project token 签发 ⑤**pairing audit 行同事务写入**（见步骤 8）——任一步失败整体回滚（状态不落、audit 不落、零残留）。事务提交后以 **AEAD（AES-256-GCM，键 K_creds）** 封装 `{spki, profile, device_code, project_token, max_offline}` 返回——**信封含 profile 名**，client 解密后回显「已授权 profile: X」供 owner 与批准界面双向核对。`max_offline` 取 vault 设置 `pair.default_max_offline`（缺省 `24h`）。**幂等**：成功密文缓存于 pending 行（state=delivered）；finish 重发 → 重放同一密文不重铸码；**delivered 行保留 5 分钟后 lazy 清理；重放上限 10 次（超出删行，client 410 重跑）；行进入任一终态（delivered/expired/failed/rejected）即从 serve 内存清除对应 ECDH 私钥**。未批准/窗口外/ack 错 → 409/410/403。
7. **client 落地**：解密凭据 → **同名覆盖语义（rev3 冻结）**：per-instance 目录已有 `cache.auth.json` → 默认拒绝并提示（实例已在用；确认重配对需 `--force`）；`--force` = 按 Plan 40 换码 runbook 清 auth+bin+meta+quarantine（**保留 `cache.config.json`**）后重写 → 写 `cache.auth.json{url: 连接地址, token: 设备码, pin: spki}` → 写 `cache.config.json{"max_offline": <下发值>}` → 调既有 `DoPull` **首拉**（锚随盘；首拉失败 = 凭据已在盘，重试 `cache pull --instance <name>`，无需重跑 pair）→ 打印 .mcp.json 片段（token 以 `<project-token>` **占位符**；真值仅 `--write-mcp <path>` 落盘；前 8 字符回显；**终端零完整凭据**）。
8. **审计（rev3：同事务 + 脱敏白名单，冻结）**：pairing 事件（enroll/auto-revoke/批准/拒绝/finish/过期/failed/delivered-replay）**与对应状态变更同 SQLite 事务**（审批 CAS、finish 铸码、auto-revoke 均含；崩溃即整体回滚，安全事件永不缺账）。**audit 字段白名单**：事件类型、实例名、profile 名、来源 IP、时间戳、结果码——**永不落**：任何凭据值/project token/设备码/pin/SAS/密文/ack/sig（Web 访问日志同纪律）。
9. **CLI 面**：`ssh-manager pair [--url] [--pin] [--profile-hint <名>] [--write-mcp <path>] [--force]`；自动化免比对 = env `SSHMGR_PAIR_ASSUME_SAS=1`（显式 env 门槛 + STUB 大字警告；无 CLI flag）；server 侧 `serve pair ls/approve/reject`（approve 输出三件套行）。

**编码契约（冻结）**：`id` = 32B hex（64 字符）；`client_pub`/`server_pub` = X25519 raw 32B base64url；`cnonce`/`snonce` = 16B base64url；`sig` = ed25519 64B base64url；`ack` = HMAC-SHA256 32B hex；`profile_hint` = 见步骤 1 白名单。**畸形处理**：重复 `id` → 409；公钥/nonce 解码失败或长度错 → 400；JSON 畸形 → 400；CAS 败 → 409。

**安全性质**：

- **换钥型 MITM**（终结假扮/换公钥）：transcript 变 → 三件套不一致暴露；pin 已知通道更在 TLS 层直接拒断（§3.2 pin 分级）——双重防线。
- **透明中继**：transcript/SAS 一致——防线 = **target_url 双屏比对**；**凭据不泄**（TLS 内文不可读 + AEAD 信封 + pin 已知通道握手期拒断）；残余 = 位置攻击（DoS/url 钉死/流量分析）——**owner 拍板登记接受**（R10；PAKE 不防中继位置，不引入）。
- 重放 → nonce + 双窗口 + 一次性码 + delivered 重放上限；穷举 → 批准人审 + 限速；失窃 → 双吊销，生效语义见 §3.1-2 三路径。

### 3.4 残余风险登记

- **人因（唯一人闸）**：三件套比对任一被忽略的残余——文案明示「任一不一致 = 攻击，立即中止」。
- **discovery 伪造 + 透明中继位置攻击**：见 §3.3 安全性质——登记接受（owner 拍板 2026-08-28）。
- **profile_hint 可篡改**：白名单 + 控制字符剥离（§3.3-1）后仅剩「文字内容误导」面——真闸 = 批准者选 profile + finish 信封回显；登记。
- **LAN 可见性**（offer 暴露「此处有 broker」）：Q1 已接受。
- **配对面 DoS**：多 IP 恶意 enroll 可短暂占满全局 pending 配额（延迟攻击）——半可信 LAN 登记接受；限速参数 env 可调。

## 4. 设计 · 批2：Web 管理 UI

### 4.1 架构

- `/ui` 路由组挂 serve mux；**go:embed** 静态资源 + SSR（html/template）；**零 npm / 零 CDN / 单二进制 / 离线 VLAN 可用**；手机优先响应式。网络定位：同 VLAN。
- **`/ui` 开关**：`serve --web`（三态）+ env `SSHMGR_SERVE_WEB` → 关闭 → 404（§3.1-8 统一开关体系）。
- **无 admin 用户时的姿态（rev3 冻结，fail-closed）**：`admin_users` 表空 → `/ui` 一切路由（含登录）返回 **403 + 单一提示页「管理员未初始化：在 broker 上运行 `ssh-manager serve admin set`」**——**不提供任何未认证的首次设置端点**（初始化是本地 CLI/TUI 一次性动作）。
- 页面清单：登录 / 总览 / Servers（CRUD + 凭据表单）/ Profiles（grant）/ Projects（签发·轮换·吊销；一次性 token 仅签发响应出现一次）/ Instances（cache-tokens 管理 + 一键双吊销）/ **Pairing（两段式：列表页 5s meta refresh；批准详情页显示三件套 + 选 profile，无刷新，提交后跳回）** / Audit（分页过滤）/ Settings（admin 密码、`pair.default_profile`、`pair.default_max_offline`、discovery/pairing/web 开关——被 env/flag 覆盖时显示覆盖状态）。

### 4.2 认证、会话与审计

- **首启**：`ssh-manager serve admin set`（或 TUI）设管理员账密；密码 **argon2id**（m=64MB, t=3, p=4）入 vault 新表 `admin_users`；TUI 可重置（重置即全量 session 失效）。
- **session = store 表 `admin_sessions`**：存 session id 的 SHA-256（原始 id 只在 cookie）、绝对过期、滑动过期、创建者 IP——跨进程即时生效（TUI/CLI 改密或重置 → DELETE 表行）。登录限速：失败 5 次/15min 锁定（IP + 账号双维度）。cookie `HttpOnly + Secure + SameSite=Strict`；TTL 12h 滑动 + **绝对上限 7d**；改密/重置 → 全量失效。CSRF：每会话 token，所有 POST 校验。安全头：`CSP: default-src 'none'; style-src 'self'`、全部响应 `no-store`。
- **审计覆盖（枚举，冻结）**：登录成功/失败、Servers/Profiles/Projects/Instances 一切写操作、token 签发/轮换/吊销、pairing 全事件、admin 密码与 settings 修改——全部落 vault audit（同事务纪律与脱敏白名单同 §3.3-8）。

### 4.3 配对批准（Web）

= §3.3 步骤 3 的第二 UI：读同一 `pairing_pending` 表、写同一条 CAS UPDATE；批准详情页显示三件套（与 TUI/CLI 同一行格式）。

## 5. 测试策略

**批1**：
- discovery：回环定向 UDP 双 socket——probe/offer 收发、魔数不符静默、畸形 JSON 静默、offer 单播断言、多接口枚举（stub）、开关关闭逐包评估后零应答。
- pairing（httptest TLS + 真 handler + 真 store）：
  - 编码契约：重复 id 409 / 非法公钥 400 / nonce 长度错 400 / JSON 畸形 400 / **hint 白名单（控制字符、>32 rune、空开头 → 400）**。
  - **未认证 enroll 零副作用钉子**：last_pull=0 撞名 → 落 pending（标记位）+ **旧 token 仍 active 断言**（auto-revoke 只在 finish 事务内）；last_pull 非零 → 419。
  - enroll 响应含 server_pub/snonce/sig 断言（前移）；限速与配额；双窗口 TTL（410）；serve 重启模拟 → in-flight expired、内存私钥清除断言。
  - CAS 并发批准（双写一败 409）。
  - poll POST（query 零 id）；状态机 202→200。
  - SAS/HKDF 确定性向量（含全块拒采 R₁ 回退）；K_ack/K_creds 分离。
  - finish：ack 错拒；**单事务原子性**（注入中途失败 → 回滚零残留 **且 audit 零行**）；**事务内 auto-revoke 竞态复查**（标记后手动把 last_pull 推非零 → 不吊销、配对拒绝）；幂等（重放同密文、零新 token）；**delivered TTL（>5min 清理）与重放上限（>10 删行 410）**；`pair-<name>` 复用四分支；信封含 profile + client 回显。
  - **hint 渲染消毒**：含 ANSI 转义/控制字符的 hint 在 TUI/Web/CLI 输出被剥离（golden 断言）。
  - **pin 分级**：pin 已知（discovery/--pin）→ pinningTransport 硬校验、换证即中止（握手期，码不上行）；pin 未知 → WARN 路径。
  - **MITM 双腿**：换 server_pub → 三件套不一致；透明中继 → SAS 一致 + client 屏 target_url=中继地址 + DoPull 到终结型假扮被 pin 拒断。
  - **吊销三路径**（§3.1-2）：project 吊销 → 新快照 spawn 拒；设备码吊销 → pull 401→quarantine；永离线 → max_offline 过期拒载。
  - audit：**同事务断言**（状态变更失败 → audit 无行）；**白名单断言**（全事件行不含任何凭据值/码/pin/SAS/密文/ack/sig）。
- ②a 移除契约：根/MCP 路径 404；`/snapshot` 未带码 401；三态开关矩阵（env 显式/flag 显式/store/缺省 四层优先级 + Settings 覆盖提示）；`probeServeHTTP` PASS。
- pair e2e（`SSHMGR_PAIR_ASSUME_SAS=1`）：url=连接地址 / max_offline 下发 / 锚 / 占位符 / `--write-mcp` / 首拉失败→`cache pull` 重试 / **同名目录默认拒 + `--force` 清四件套保留 config**。
- pending 队列跨进程（serve 写/TUI 读）；全仓绿；默认契约（既有工作流零变化）。

**批2**：
- argon2id / 登录限速 / session 表（绝对上限、滑动、改密 DELETE 全量、跨进程即时）/ CSRF / CSP + no-store / `--web` 三态 404。
- **无 admin → /ui 全路由 403 + 提示页**（含无未认证 setup 端点断言——不存在可 POST 的首设路由）。
- 页面 smoke：每页 200；Pairing 两段式（列表刷新/详情停 + 三件套行格式与 TUI/CLI 一致）；一次性 token 仅签发响应出现一次。
- Web 批准与 TUI 同表同 CAS；`/ui` 不影响 `/snapshot`/`/pair`；audit 只读；Web 写操作全覆盖 audit + 脱敏断言。

## 6. 文档联动

deployment-modes.md **重写**（4→2+管理面；「怎么选」：桌面多机默认 = pair 一条龙、手机 = Web 管理、②a 移除三步迁移节）；multi-machine.md（删 Step3 TLS 两坑节、新增 pair 流程、手工桥迁移 = 存量迁移官方路径）；quickstart-multi-machine.md 重写为 pair 版；broker-host-agent.md（姿势 A 删除；②c 移附录）；agent-tools.md（多机只读铁律 + **吊销三路径语义**）；compat-matrix.md（v0.11.0 breaking：②a 移除 + 三步迁移 + 升级顺序铁律 + 各端最低版本）；threat-model.md（discovery 零敏感面 / SAS+地址绑定配对与密钥派生 / 中继位置攻击登记与不泄凭据论证 / **吊销三路径** / pairing audit 同事务与脱敏 / Web 认证与会话面）；README 索引；backlog 相应销项。

## 7. 验收清单

**批1**：
1. 全仓测试绿；②a 移除契约测试绿（404/401/三态开关/probe PASS）。
2. pair e2e 绿（含首拉失败重试、同名 `--force`）；MITM 双腿绿；HKDF/SAS 向量（含回退）绿；**未认证 enroll 零副作用钉子绿**。
3. **真机 gate（owner）**：NUC10 升 serve（discovery + pairing 开）；先三步迁移笔记本存量 ②a；干净目录真跑 `ssh-manager pair` → TUI 批准（**同屏三件套对照 client**）→ agent 在线/断网各验一次；`cache-tokens ls` 与 audit 可见配对事件。
4. ②a 负面验收：旧 ②a `.mcp.json` → 404 明确失败。
**批2**：
5. 手机（同 VLAN）登录 Web → 批准一次真配对（两段式）→ 闭环；一键双吊销生效。
6. 认证/CSRF/限速/安全头/会话表/审计覆盖与脱敏测试绿；全仓绿。
7. **v0.11.0 发布**（goreleaser 既有管线；compat-matrix breaking 行上线）。

## 8. 风险与备选

- **R1 存量迁移破坏面**：三步迁移 + compat-matrix 铁律引用 + 验收 3 演练；不做自动迁移工具。
- **R2 配对人因**：三件套比对（名/地址/SAS）——批准动作与双屏对照一气呵成（§3.3-5）；免比对仅 env `SSHMGR_PAIR_ASSUME_SAS=1`。
- **R3 client 防火墙拦广播回包**：真机验收覆盖；`--url` 兜底。
- **R4 server 防火墙拦 UDP 7878**：安装文档一句；`--url` 兜底。
- **R5 自签证书手机警告 habituation**：登记；DNS-01 远期。
- **R6 Web 面扩大攻击面**：同 VLAN + argon2id + 限速 + CSRF + CSP + no-store + 全量审计 + 三态开关 + 无 admin 全 403（无未认证 setup 面）；无公网。
- **R7 probeServeHTTP 共享 seam**：与 Plan 43 R7 双登记。
- **R8 pending = store 表 + 密钥态 = serve 内存**：决策持久、密钥进程态；serve 重启丢 in-flight（自动 expired）；表内零私钥；delivered 密文 5min TTL + 重放上限 10 + 终态即清内存私钥。
- **R9（备选记录）**：poll 轮询嫌糙 → 长轮询/SSE；不取。
- **R10（owner 拍板登记）**：透明中继位置攻击接受；防线 = 三件套双屏 + pin 分级硬校验；PAKE 不引入。
- **R11（rev3 登记）**：hint 白名单外的「合法字符文字误导」面（钓鱼式提示词）——真闸 = 批准者选 profile + 信封回显；接受。

## 9. 变更记录

### 9.1 rev0 → rev1（首审 codex+kimi 20 条闭环全吸收）

（18 组修法明细同 rev1 原文——升级三步迁移 / 铸码移 finish / pending 落 store 表 / HKDF+transcript 冻结 / SAS 无偏推导 / 删 url 下发 / profile_hint 通路 / Web 审计枚举 / session 上限+改密失效 / 终端零凭据 / 双窗口 / 本地闸语义 / assume-sas 改 env / 限速配额 / max_offline 来源 / meta refresh。）

### 9.2 rev1 → rev2（复审轮 17 条闭环，含 1 处 owner 拍板）

（明细同 rev2 原文——批准架构重构（enroll 时 serve 生成密钥态/CAS 批准/重启作废）/ 透明中继三件套（拍板档位 A）/ finish 事务幂等 / 复用规则 / 编码契约 / 默认契约措辞 / /ui 开关 / session 表 / 撞名活性 / CLI 三件套 / SAS 回退 / 两段式页面 / 热生效 / hint 登记。）

### 9.3 rev2 → rev3（第三轮闭环验证：codex 10 + kimi 6，去重 13 条闭环；两家均确认前轮真闭环、方向可实施）

1. **enroll 撞名 auto-revoke 改「只查不改」（codex#1 高 + kimi#1 中，两案合并）**：rev2 把 auto-revoke 放在 enroll（未认证）路径 = 未认证远程吊销破坏面；改为 pending 行只记标记位、批准界面标注「⚠ 将替换未激活码」、**auto-revoke 移入 finish 单事务（事务内复查 last_pull 竞态安全）**。
2. **吊销生效语义三路径重写（codex#2 高）**：rev2「lazy ≤30min」混淆了三种路径——冻结为：project 吊销+码活 = 新快照 spawn 拒（在线 ≤30min）；码吊销 = pull 401→quarantine（F4）；永离线 = **max_offline 硬上限**（默认 24h）到期拒载，非 30 分钟（§3.1-2）。
3. **profile_hint 注入防护（codex#3 高）**：白名单正则（Unicode 可打印、≤32 rune、零控制字符）→ 400；全展示面剥离 C0/C1 再渲染——hint 不得获得覆盖 SAS/地址显示的渲染能力；R11 登记残余（文字误导面）。
4. **pin 分级硬校验（codex#4 中）**：pin 已知（discovery/--pin）→ pair 全程 `pinningTransport`（TLS 层硬校验，trusted pin 绝不降级为 WARN）；pin 未知才 InsecureSkipVerify+WARN+人闸（§3.2）。
5. **三态开关（codex#5 + kimi#3）**：env/flag 显式置位才参与裁决（显式 env > 显式 flag > store > 缺省 true）；Settings 显示覆盖状态；措辞改「≤5s 内生效」（消除 rev2「即时 vs ≤5s 缓存」自相矛盾，kimi#4）。
6. **/ui 无 admin 全 403（codex#6 + kimi#5）**：fail-closed，无任何未认证首设端点；提示页指向 `serve admin set`。
7. **delivered 密文生命周期（codex#7）**：TTL 5min lazy 清理 + 重放上限 10 次 + 终态即清内存 ECDH 私钥。
8. **audit 同事务（codex#8）**：审批 CAS/finish 铸码/auto-revoke 的 audit 行与状态变更同一 SQLite 事务——崩溃整体回滚，安全事件永不缺账。
9. **audit 脱敏白名单（codex#9）**：字段白名单（事件/实例名/profile/IP/时间/结果码）；凭据值/token/码/pin/SAS/密文/ack/sig 永不入 audit 与日志。
10. **升级铁律显式引用（codex#10）**：三步迁移并入 compat-matrix「client 先升 serve 后升」铁律 + 各端最低版本（client ≥ v0.10.1）。
11. **密钥材料前移 enroll 响应（kimi#2）**：server_pub/snonce/sig 在 enroll 即返回——client 批准前就显示三件套，**人闸 = 批准动作 + 双屏对照一气呵成**，120s 窗口只覆盖确认+网络（消除 rev2 跨机器人腿超窗重跑问题）；poll 仅翻状态。
12. **client 同名覆盖语义（kimi#6）**：默认拒绝 + `--force` 按 Plan 40 换码 runbook 清四件套保留 config。
