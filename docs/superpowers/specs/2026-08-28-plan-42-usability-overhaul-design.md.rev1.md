# Plan 42 — 易用性改造:模式缩减 + 发现配对一条龙 + Web 管理 UI 设计 spec · rev1

- 日期：2026-08-28（rev1 同日：首审 codex+kimi 异构盲评 20 条反馈闭环全吸收——含铸码时点/密钥派生/升级路径三处结构性修法；rev0→rev1 变更见 §9.1）
- 来源：2026-08-28 grilling 会话（Q1–Q21 逐题 owner 确认）+ 与 Plan 43（doctor 探活，rev2.1）冲突审查闭环。痛点实证：多机 client 冷启动 = broker 控制台人肉 `cache-tokens add` + `projects add` + 跨机手抄三串字符串 + ②a 的 TLS 信任库两坑（NODE_EXTRA_CA_CERTS/SAN）。
- 前置：**v0.10.1 先行发版**（合 `plan-40-batch1-impl` / `plan-40-batch2-impl` / Plan 41 worktree；doctor-multi-instance 就绪则同船）。本 plan 依赖 Plan 40 的 per-instance 构件（`CachePathsFor` / device name 三合一 / MAX_OFFLINE 持久化）。
- 状态：rev1 定稿待 owner 审

## 1. 目标与非目标

**目标**（四核心诉求 → 三个交付）：

1. **模式缩减 4 → 2 + 管理面**：②a 在线 HTTP 直连**移除**（不是降级——serve mux 撤 MCP handler）；②c 附录化；client TUI wizard 删除。新铁律：**多机 agent 只读 + 执行，写操作仅 broker TUI / Web UI**；单机 ① 不变（可写）。手机不跑 agent，只做管理面。
2. **发现 + SAS 配对一条龙**：新工作机从「装好二进制」到「agent 可用」= 一条 `ssh-manager pair`（UDP 发现 → 批准 → 双屏比对 → 四件套凭据自动下发 → 首拉 → 写配置）。
3. **Web 管理 UI（手机优先）**：go:embed 单二进制、零 npm/零 CDN；完整管理 + 配对批准 + 吊销 + 审计。

**非目标**：远程写（RW 代理模式，真有需求另立项）；DNS-01 真证书（后续可选增强，需自有域名）；PAKE 第三方库（SAS 已覆盖）；公网/外网暴露（同 VLAN 定位）；多 broker 联邦；②a 兼容开关（直接移除，不留 zombie flag）；pairing pending 队列独立持久化引擎（直接用 store 表，见 §3.3）。

## 2. 事实基础（实证锚，2026-08-28 核验）

- **F1（源码，serve.go:205-234）**：`HTTPHandler` 根 mux——`/snapshot` → `cacheAuth`；**其余一切路径** → `mcpChain`（`projectAuth`(:221) → `resolveServer`(:159) → SDK `NewStreamableHTTPHandler`(:220)）。:205-209 注释钉住「两闸不相交 keystone」（project token 不入 /snapshot、设备码不驱动 MCP）。②a 移除 = 撤 mcpChain 分支，根路径落 404。
- **F2（源码，cli/serve_service.go:630-642）**：`probeServeHTTP` GET `https://<addr>/`（根路径，InsecureSkipVerify，1s，401/200=活）。②a 移除后根路径 404 → **必须重指向 `/snapshot`**（未带码 GET → cacheAuth 401，语义保真；auth 层先拒故零序列化零 touch）。**与 Plan 43 R7 互为携带义务，任一 plan 先落地都必须带上此 seam 改动**。
- **F3（源码，store）**：`AddCacheToken(name, profileID)`（cachetoken.go:24）铸一次性明文设备码（revoked 后同名可重发，active 撞名报错——cachetoken_test.go:117/146 钉住）；`VerifyToken`（projects.go:40）；`GenerateToken`（token.go:12，base64url 32B）。**pairing 服务端零新 token 类型，全复用既有铸码路径**。
- **F4（源码，clientops/pin.go）**：`pinningTransport(pin)` = TLS1.3 + InsecureSkipVerify + VerifyConnection 常时 SPKI 比；`cache.auth.json = {url, token, pin}`；`DoPull`（clientops.go:356）含 server_anchored 锚与 quarantine 语义。pair 落地复用 `DoPull` 首拉。
- **F5（源码，mcpserver/cert.go:71）**：`LoadOrCreateServeCert`——ed25519 自签、100 年、幂等三态；SPKI pin = sha256。**配对临时公钥可用此长期 ed25519 私钥签名**（把配对 transcript 绑到 TLS 身份）。
- **F6（分支）**：Plan 40 批1+2 构件在 `plan-40-batch1-impl` / `plan-40-batch2-impl`（已推 origin，未合 master）；v0.10.1 合入为前置。
- **F7（依赖）**：`golang.org/x/crypto v0.41.0` 在 go.mod（argon2/hkdf 可用，零新增）；Go 1.25 stdlib `crypto/ecdh`（X25519）、`crypto/cipher`（AES-GCM）；charm TUI v2；仓库零前端工具链（package.json 为空）→ Web UI 必须 go:embed 自包含。
- **F8（现状）**：全仓无 UDP 代码（discovery 为全新面）；serve 无 health/metrics 端点；`serve bind` 子命令（跨机隧道监听白名单）仅服务 ②a 场景。store 为 SQLite（modernc，`MaxOpenConConns(1)`+WAL）——**跨进程共享态的既有先例**（TUI/serve/CLI 本就同库并发）。

## 3. 设计 · 批1：模式缩减 + 发现 + SAS 配对

### 3.1 ②a 移除与 ②c/wizard 退役

1. **`HTTPHandler` 重构**：删 `mcpChain`（projectAuth/resolveServer→MCP 的接线）与 serve 侧 `verifyToken` 接线；根 mux 变为：`/snapshot`（不动）+ `/pair/*`（§3.3）+ 其余 **404**。keystone 注释改写：两闸不相交属性降格为「project token 不再是任何远程 MCP 凭据——仅 client 侧 spawn 闸；设备码仍只入 /snapshot」。**project token 本地闸语义（冻结）**：数据源 = 快照内 projects 表（Plan 39 /snapshot 按授权裁剪，same-profile projects 随快照下发）；`mcp --cache` 用 `SSHMGR_TOKEN` 对快照内 projects 校验后放行工具面；吊销生效 = lazy——owner 吊销后，client **下一次 pull**（在线 ≤30min 保鲜）即从快照消失、spawn 拒绝；永离线设备失窃沿用既有登记（最终兜底 = 轮换服务器凭据）。
2. **serve 的 agent 执行面整体退役**：resolveServer 背后的远程 SSH 拨号、serve 进程后台任务表、`serve bind` 隧道白名单随 ②a 消失（① 单机的本地 `tunnels`/后台任务面不受影响）。serve 收窄为四件事：**权威 vault + /snapshot + /pair + （批2）/ui**。
3. **`probeServeHTTP` 重指向 `/snapshot`**（F2）。`serve status` 语义不变。
4. **②c**：仅文档层面移入「应急附录」（代码不删——`mcp` stdio 直开本机 vault 是 ① 的正常形态，②c 只是「在 broker 上这么干」的用法说明）。
5. **client TUI**：connect-form/wizard 页代码删除；client 模式缩为 sync/status/doctor 指引。新机首选路径 = `ssh-manager pair`；**手工路径（`cache pull` + 手写 .mcp.json）保留并文档化**（CI/自动化零交互场景，也是下面的迁移路径）。
6. **存量迁移与升级顺序（冻结，三步）**：`/pair/*` 只存在于新 serve——**serve 升级前不存在 pair 路径**。故：
   - ① **手工桥迁移**：存量 ②a 机器在旧 serve 上按既有手工流程迁到桥姿态（broker 控制台 `cache-tokens add` + `projects add`，client `cache pull`——多机文档既有流程，本 plan 只把它从「三姿势之一」提升为「②a 退役后的唯一存量迁移路径」并写清清单）；
   - ② **升 serve**（v0.11.0）——②a 当刻 404，桥姿态机器无感；
   - ③ **pair 时代**：此后所有新机/重配对一律 `ssh-manager pair`。
   compat-matrix 加 v0.11.0 breaking 行 + 上述三步。
7. **`/pair` 开关**：`serve --pairing`（默认 true）+ env `SSHMGR_SERVE_PAIRING=false`（env seam 纪律：新生产路径必须留 env 覆盖口）；关闭 → `/pair/*` 一律 404（与 discovery 开关相互独立——`--url` 直连 pair 不依赖 discovery）。§5 默认契约相应措辞：**未使用新功能时，既有行为（/snapshot/审计/CLI/探活）零变化**（新端点静默存在不算行为变化）。

### 3.2 UDP 发现（discovery）

- **端口 UDP 7878**（与 TCP serve 同号）；serve `--discovery`（默认 true）+ env `SSHMGR_SERVE_DISCOVERY=false` 隐身开关；监听 0.0.0.0:7878/udp。
- **报文**：首行魔数 `sshmgr-disc-v1`，次行 JSON，整体 ≤512B。probe：`{"t":"probe"}`；offer（**只单播回请求源**）：`{"t":"offer","name":<显示名>,"spki":<sha256:...>,"tcp":<TCP端口>}`。`name` = 服务器显示名（TUI/Web 可设，缺省 hostname）；**零敏感字段**；server 永不主动广播。未知字段双方忽略（前瞻兼容）；魔数不符静默丢弃。
- **client 侧**（pair 内置）：枚举全部非环回 IPv4 接口，**逐接口** broadcast（覆盖多宿主 + 换网两情形）；1.5s 收集窗；按 spki 去重；多结果列表供选，单结果直入配对。
- **兜底（不依赖 discovery）**：`ssh-manager pair --url https://host:7878 [--pin sha256:...]`（跳过发现；pin 缺省走 TOFU+SAS 终闸）——防火墙/路由异常时的逃生门，也是 CI 姿态。**client 最终写入的 url 恒为其自己的连接地址**（discovery 响应源地址或 `--url`），构造上即正确——serve 不下发 url（§3.3-5）。

### 3.3 SAS 配对协议（/pair/*，冻结）

角色：client（新工作机，`ssh-manager pair`）/ server（serve）/ 批准者（broker TUI 批1、Web UI 批2、`serve pair` CLI 兜底）。**pending 队列 = store 新表 `pairing_pending`**（非内存——TUI/CLI 是独立进程，与 serve 共享 SQLite 即共享队列，F8 先例；serve 重启队列保留，过期项 lazy 清理）。

**transcript 与密钥派生（冻结，先于一切步骤）**：

- 规范 transcript 字节串 = 依序拼接，每段前加 4-byte 小端长度前缀：`"sshmgr-pair-v1" ‖ id ‖ client_pub ‖ cnonce ‖ server_pub ‖ snonce`；`T = SHA256(transcript)`。
- `K_master = HKDF-SHA256(ikm = X25519(client_priv, server_pub), salt = T, info = "sshmgr-pair-v1", L = 64)`；`K_ack = K_master[0:32]`（finish 的 HMAC-SHA256 键）；`K_creds = K_master[32:64]`（凭据 AEAD 键）。两键物理分离，不复用。
- `sig`（批准响应内）= serve 长期 ed25519 证书私钥对 transcript 字节串的签名（F5）；client 用 discovery pin **机会性**验签（失败 WARN 不阻断，SAS 才是终闸）。
- **SAS 推导（无偏，冻结）**：`R = SHA256("sshmgr-sas-v1" ‖ transcript)`；取 R 的 32-bit 大端块自左向右扫描，首个 `< 4,294,000,000` 的块 v → `sas = fmt.Sprintf("%06d", v % 1_000_000)`（rejection sampling 消除 mod 偏置；确定性可测）。

**流程**：

1. **enroll**：client 生成 X25519 临时密钥对 + 32B 随机 id，`POST /pair/enroll` `{id, name, client_pub, cnonce, profile_hint?}`。`name` = instance 名（**Plan 40 白名单校验**；active 撞名即拒，client 提示换名——F3 语义）；`profile_hint` 为**展示性字段**（client 传入、批准界面展示，不参与任何分支）。传输走 TLS（InsecureSkipVerify——信任由 SAS 人闸兜底，discovery 的 pin 为机会性绑定）。server 限速：per-IP enroll 5/min、全局 30/min、**per-IP pending ≤2、全局 pending ≤32**（超限 429/409）；pending 项写入 `pairing_pending` 表。
2. **双窗口（冻结）**：`enroll → 批准` 窗口 **10 分钟**（人赶到界面的余量；超时项 lazy 清理，client poll 得 410）；`批准 → finish` 窗口 **120 秒**（SAS 比对 + 确认是快操作）。**TTL 起算点 = 各自事件的完成时刻**，落库时间戳为准。
3. **批准**：批准界面（TUI/Web/CLI 三面同表）列出 pending `{name, 来源IP, profile_hint, 剩余秒}`；批准者选 profile（vault 设置 `pair.default_profile` 预选，TUI/Web 可改，未设则列首）。**批准只记录决策（profile + server 侧 X25519 临时对 + snonce），不铸任何码**——杜绝「已铸未领」残留与回滚问题。`pair-<name>` project 已存在且 active → 批准界面提示「将复用既有 project 签发新 token（rotate 语义）」；撞名检查在批准时展示、finish 时复查。
4. **poll**：client 每 2s `GET /pair/poll?id=`；未批准 → 202 `{"t":"pending"}`；已批准 → 200 `{"t":"approved","server_pub","sig","snonce"}`；过期/拒绝 → 410/403。
5. **SAS 双屏比对**：两侧按上文冻结公式各算 6 位 SAS。client 终端显示并要求确认；server 侧批准界面同步显示。**用户肉眼一致才继续**。任一层 MITM（TLS 终结/中继/换公钥）必致两屏不一致。
6. **finish**：client `POST /pair/finish` `{id, ack}`；`ack = HMAC-SHA256(K_ack, "finish" ‖ id)`——同时证明持有临时私钥且过了人闸。server 验过后**此刻才铸码**：`AddCacheToken(name, profile)` + （复用或新建）project `pair-<name>` 绑同 profile 并铸一次性 project token；任一铸码失败 → pending 置 failed、client 收 5xx 重跑 pair（零残留）。成功 → 以 **AEAD（AES-256-GCM，键 K_creds，nonce 随密文）** 返回 `{spki, device_code, project_token, max_offline}`。**不含 url**——client 恒用自己的连接地址（§3.2 兜底段）。`max_offline` 取 vault 设置 `pair.default_max_offline`（缺省 `24h`；TUI/Web Settings 可改）。未批准/窗口外/ack 错 → 409/410/403。
7. **client 落地**（pair 命令继续执行）：解密凭据 → 写 `cache.auth.json{url: 连接地址, token: 设备码, pin: spki}`（per-instance 目录）→ 写 `cache.config.json{"max_offline": <下发值>}` → 调既有 `DoPull` **首拉**（server_anchored 锚随之落盘）→ 打印 .mcp.json 片段（`command: ssh-manager, args: [mcp, --cache, --instance, <name>], env.SSHMGR_TOKEN: <project-token 占位符>`）——**终端零完整凭据**：token 以占位符呈现，真值仅 `--write-mcp <path>` 直接写盘时落入文件；凭据前 8 字符回显仅用于人工核对。
8. **审计**：pairing 事件（enroll/批准/拒绝/finish/过期/failed）+ 批2 起 Web 全部写操作（§4.2 枚举）全落 vault audit。
9. **CLI 面**：`ssh-manager pair [--url] [--pin] [--profile-hint <名>] [--write-mcp <path>]`；自动化免比对 = env `SSHMGR_PAIR_ASSUME_SAS=1`（显式 env 门槛，生效时终端大字 STUB 警告；不提供 CLI flag，防误用架空人闸）；server 侧 `serve pair ls/approve/reject`（读写 store 表，与 TUI/Web 同队列）。

**安全性质**：MITM → SAS 双屏不一致（唯一依赖人因，文案强警示）；重放 → nonce + 双窗口 + 一次性码；穷举 → 批准人审 + 限速；失窃 → `cache-tokens revoke` + `projects revoke` 双吊销（TUI/Web 一键，lazy ≤30min 生效，§3.1-1）。

### 3.4 残余风险登记

- **SAS 人因**（用户对不一致仍确认）：全案唯一人因残余，文案明示「不一致 = 攻击，立即中止」。
- **discovery 伪造**（假 server + 假 pin 骗 enroll）：真 broker 的批准界面不会出现该 pending → 批准不会发生；SAS 终闸。登记接受。
- **LAN 可见性**（offer 暴露「此处有 broker」）：Q1 已接受（半可信 LAN，零敏感字段 + 单播响应）。
- **配对面 DoS**：per-IP pending ≤2 / 全局 ≤32 下，多 IP 恶意 enroll 仍可短暂占满全局配额、挤出新配对（延迟攻击，非破坏）——半可信 LAN 定位下登记接受；限速参数可用 env 调（`SSHMGR_SERVE_PAIR_RATE_*` 实现裁量）。

## 4. 设计 · 批2：Web 管理 UI

### 4.1 架构

- `/ui` 路由组挂 serve mux；**go:embed** 静态资源（CSS 内嵌编译期）+ SSR（html/template）；**零 npm / 零 CDN / 单二进制 / 离线 VLAN 可用**；手机优先响应式（单列 → 桌面双列）。网络定位：同 VLAN（复用 serve `--addr` 既有语义与自签 TLS）。
- 页面清单：登录 / 总览（角色、版本、实例数、pending 配对数）/ Servers（CRUD + 凭据表单）/ Profiles（grant）/ Projects（签发·轮换·吊销；一次性 token 仅签发响应出现一次）/ Instances（cache-tokens 管理 + 一键双吊销）/ **Pairing（§3.3 同一 store 队列，含 SAS 展示与 profile 选择；页面 `<meta http-equiv="refresh" content="5">` 5s 自刷新——零 JS 纪律下感知新 pending，CSP 不阻导航刷新）** / Audit（分页过滤）/ Settings（admin 密码、`pair.default_profile`、`pair.default_max_offline`、discovery 开关、pairing 开关）。

### 4.2 认证、会话与审计

- **首启**：`ssh-manager serve admin set`（或 TUI）设管理员账密；密码 **argon2id**（m=64MB, t=3, p=4，参数随行存储）入 vault 新表 `admin_users`；TUI 可重置（重置即全量 session 失效）。
- 登录限速：失败 5 次/15min 锁定（IP + 账号双维度）。session = 256bit 随机 id，cookie `HttpOnly + Secure + SameSite=Strict`；服务端 TTL 12h 滑动 + **绝对上限 7d**（过期即失效，无法靠持续使用无限续期）；改密/管理员重置 → **全量 session 失效**。CSRF：每会话 token，所有 POST 表单内嵌校验。安全头：`Content-Security-Policy: default-src 'none'; style-src 'self'`（CSS 走嵌入文件）、全部响应 `Cache-Control: no-store`。
- **审计覆盖（枚举，冻结）**：登录成功/失败、Servers/Profiles/Projects/Instances 的一切写操作、token 签发/轮换/吊销、pairing 全事件（§3.3-8）、admin 密码与 settings 修改——全部落 vault audit（Web 面无审计盲区）。
- 自签证书在手机浏览器的告警：一次性接受例外（登记 habituation 风险 R5；DNS-01 为远期解）。

### 4.3 配对批准（Web）

= §3.3 步骤 3 的第二 UI，与 TUI/CLI 共享同一 `pairing_pending` 表与同一状态机（单写入点 = serve），SAS 在批准页展示。

## 5. 测试策略

**批1**：
- discovery：回环定向 UDP 双 socket——probe/offer 收发、魔数不符静默、畸形 JSON 静默、offer **单播**断言（无广播回包）、多接口枚举（stub 接口表）、--discovery=false 零响应。
- pairing（httptest TLS + 真 handler + 真 store，零模拟 HTTP 层）：enroll 白名单拒绝/active 撞名拒/限速与 pending 配额（per-IP 2、全局 32）/双窗口 TTL（enroll→批准 10min 过期 410；批准→finish 120s 过期 410）/poll 202→200 状态机/sig 验签/**SAS 确定性向量**（固定密钥 → 固定 6 位，含 rejection-sampling 边界块 ≥4,294,000,000 的跳块用例）/**HKDF/transcript 确定性向量**（K_ack/K_creds 分离断言）/finish ack 错拒/**finish 铸码失败 → pending=failed 且 store 零新增 token（零残留断言）**/**MITM 腿**（中间换 server_pub → 两侧 SAS 必不一致）/AEAD 凭据往返/`pair-<name>` 复用 rotate 语义/审计行断言。
- ②a 移除契约：根路径与任意 MCP 路径 → **404**；`/snapshot` 未带码 → **401**（既有不变）；`--pairing=false` → `/pair/*` 404；`probeServeHTTP` 打真 serve → true。
- pair e2e（临时目录 + env `SSHMGR_PAIR_ASSUME_SAS=1`）：断言 cache.auth.json（**url = client 连接地址**）/ cache.config.json(max_offline 来自 serve 设置) / cache.bin + 锚 / 打印片段 **token 为占位符** / `--write-mcp` 落盘含真值。
- pending 队列跨进程：serve 进程写 pending → TUI/CLI 读同表（同库双连接）。
- TUI 批准页：喂 pending → 批准 → 队列状态流转 + audit。
- 全仓绿；**默认契约**：未启用 discovery/pairing 时，既有行为（/snapshot/审计/CLI/探活）零变化。

**批2**：
- argon2id 哈希/登录限速/会话 TTL + **绝对上限 7d**/**改密 → 全量 session 失效**/CSRF 无 token POST 拒/CSP + no-store 头断言/批准页 meta refresh 存在。
- 页面 smoke：每页 200 + 关键元素；一次性 token 只在签发响应出现一次。
- Web 批准与 TUI 同表（一面批准另一面即消失）；`/ui` 不影响 `/snapshot`、`/pair`；audit 页只读；**Web 写操作全覆盖 audit 断言**（§4.2 枚举逐项）。

## 6. 文档联动

deployment-modes.md **重写**（4→2+管理面；「怎么选」改写：桌面多机默认 = pair 一条龙、手机 = Web 管理、**②a 移除三步迁移节**）；multi-machine.md（删 Step3 TLS 两坑节、新增 pair 流程、**手工桥迁移路径提升为存量迁移官方路径**）；quickstart-multi-machine.md 重写为 pair 版；broker-host-agent.md（姿势 A 删除——broker 主机 agent = 零距离 client 走桥姿态；②c 移附录）；agent-tools.md（多机只读铁律）；compat-matrix.md（v0.11.0 breaking：②a 移除 + 三步迁移 + 升级顺序铁律）；threat-model.md（discovery 零敏感面 / SAS 配对与密钥派生 / pairing audit / Web 认证与会话面）；README 索引；backlog 相应销项。

## 7. 验收清单

**批1**：
1. 全仓测试绿；②a 移除契约测试绿（404/401 不变/`--pairing` 开关/probe PASS）。
2. pair e2e（§5）绿：凭据落盘 + 首拉 + 锚 + .mcp.json（占位符/写盘两形态）golden。
3. MITM 腿绿（SAS 不一致必现）；HKDF/SAS 确定性向量绿。
4. **真机 gate（owner）**：NUC10 升 serve（discovery + pairing 开）；**先按三步迁移把笔记本存量 ②a 配置迁桥**；干净目录真跑 `ssh-manager pair` → TUI 批准 → SAS 双屏比对 → agent 在线/断网各验一次；`cache-tokens ls` 与 audit 可见配对事件。
5. ②a 负面验收：旧 ②a `.mcp.json` → agent 连接得 404 明确失败。
**批2**：
6. 手机（同 VLAN）登录 Web → 批准一次真配对 → 全流程闭环；一键双吊销生效。
7. 认证/CSRF/限速/安全头/会话上限/审计覆盖测试绿；全仓绿。
8. **v0.11.0 发布**（goreleaser 既有管线；compat-matrix breaking 行上线）。

## 8. 风险与备选

- **R1 存量迁移破坏面**：②a client 在 serve 升级瞬间断供——三步迁移路径（§3.1-6）文档化 + 验收 4 演练；不做自动迁移工具（用户 = owner 本人，清单化）。
- **R2 SAS 人因**：免比对仅 env `SSHMGR_PAIR_ASSUME_SAS=1`（显式 env + STUB 大字警告，无 CLI flag）；人工路径无条件肉眼比对。
- **R3 client 防火墙拦广播回包**：真机验收覆盖 Windows 笔记本；失败即走 `--url` 兜底（不依赖 discovery）。
- **R4 server 防火墙拦 UDP 7878**：安装文档一句（如 `ufw allow 7878/udp`）；同样有 `--url` 兜底。
- **R5 自签证书手机警告 habituation**：登记；DNS-01 真证书为远期可选。
- **R6 Web 面扩大攻击面**：同 VLAN + argon2id + 限速 + CSRF + CSP + no-store + 全量审计；无公网暴露。
- **R7 probeServeHTTP 共享 seam**：与 Plan 43 R7 双登记，任一 plan 先落地必须携带。
- **R8 pending 队列 = store 表**（rev1 修法）：跨进程共享（TUI/Web/CLI/serve 同库）；serve 重启队列保留，过期项 lazy 清理——较内存态多一项「重启后残留待清理行」的既有 SQLite 语义，无新风险；配对敏感字段（临时公钥/nonce）在表内不留私钥（私钥只在 serve 进程内存）。
- **R9（备选记录）**：poll 轮询若评审嫌糙 → 长轮询/SSE；不取（复杂度不值）。

## 9. 变更记录

### 9.1 rev0 → rev1（首审 codex+kimi 异构盲评 20 条闭环全吸收；无驳回、无降级、无 owner 拍板项）

1. **§3.1-6 重写（codex#2 + kimi#1，高/中）**：升级铁律原文「先迁 client(②a→pair/桥)」不可执行——pair 端点旧 serve 不存在。改为三步：手工桥迁移（既有流程）→ 升 serve → pair 时代。
2. **§3.3 密钥派生冻结（codex#1，高）**：transcript = 长度前缀规范拼接 + SHA256；HKDF salt=transcript、info 固定、K_ack/K_creds 两键分离。
3. **§3.3-6 铸码时点移至 finish（codex#3 + kimi#6，中/低）**：批准只记决策不铸码 → 零「已铸未领」残留/零回滚；`pair-<name>` 已存在 active → 复用 project 签新 token（rotate），批准/finish 双时点查撞名。
4. **pending 队列改 store 表（codex#4，中）**：TUI/Web/CLI 独立进程共享内存队列不可行 → `pairing_pending` 表（SQLite 跨进程先例 F8）；R8 相应改写。
5. **`/pair` 开关 + env seam（codex#5 + kimi#4，中）**：`--pairing`/`SSHMGR_SERVE_PAIRING`，关 → 404；§5 默认契约措辞改「既有行为零变化」。
6. **删除 finish 下发 url（codex#6，中）**：client 恒用自身连接地址（构造上正确），serve 不再下发可能错误的多宿主地址。
7. **profile_hint 通路（codex#7，中）**：enroll 加字段 + 批准界面展示（不分支）。
8. **Web 审计覆盖枚举（codex#8，中）**：登录/全部写操作/token 生命周期/settings 修改全进 audit。
9. **session 绝对上限 7d + 改密全量失效（codex#9，中）**。
10. **终端零完整凭据（codex#10，低）**：打印片段 token 用占位符，真值仅 `--write-mcp` 落盘。
11. **双窗口 TTL（kimi#2，中）**：enroll→批准 10min、批准→finish 120s，起算点冻结。
12. **project token 本地闸语义（kimi#3，中）**：数据源 = 快照内 projects；吊销 lazy ≤30min；永离线失窃既有登记兜底（§3.1-1）。
13. **--assume-sas 改 env（kimi#5，中）**：删 CLI flag → `SSHMGR_PAIR_ASSUME_SAS=1` 显式 env 门槛。
14. **SAS 无偏推导（kimi#7，低）**：32-bit 块 rejection sampling（≥4,294,000,000 跳块）→ mod 10^6。
15. **限速/pending 配额调整（kimi#8，低）**：per-IP pending ≤2、全局 ≤32；DoS 面登记 §3.4。
16. **max_offline 来源（kimi#9，低）**：vault 设置 `pair.default_max_offline`（缺省 24h）。
17. **Web 批准页 meta refresh 5s（kimi#10，低）**。
18. §3.4/§7 相应登记与验收扩展（迁移演练、占位符断言、跨进程队列、审计覆盖断言）。
