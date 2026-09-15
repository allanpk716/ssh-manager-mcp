# Plan 42 — 易用性改造:模式缩减 + 发现配对一条龙 + Web 管理 UI 设计 spec · rev2

- 日期：2026-08-28（rev1 同日首审 codex+kimi 20 条闭环；rev2 同日复审：codex DISAGREE 7 条 + kimi SUGGEST_CHANGES 10 条闭环，含 1 处 owner 拍板（透明中继修法档位 = 地址绑定+登记残余）；rev0→rev1 见 §9.1，rev1→rev2 见 §9.2）
- 来源：2026-08-28 grilling 会话（Q1–Q21 逐题 owner 确认）+ 与 Plan 43（doctor 探活，rev2.1）冲突审查闭环。痛点实证：多机 client 冷启动 = broker 控制台人肉 `cache-tokens add` + `projects add` + 跨机手抄三串字符串 + ②a 的 TLS 信任库两坑（NODE_EXTRA_CA_CERTS/SAN）。
- 前置：**v0.10.1 先行发版**（合 `plan-40-batch1-impl` / `plan-40-batch2-impl` / Plan 41 worktree；doctor-multi-instance 就绪则同船）。本 plan 依赖 Plan 40 的 per-instance 构件（`CachePathsFor` / device name 三合一 / MAX_OFFLINE 持久化）。
- 状态：rev2 定稿待 owner 审

## 1. 目标与非目标

**目标**（四核心诉求 → 三个交付）：

1. **模式缩减 4 → 2 + 管理面**：②a 在线 HTTP 直连**移除**（不是降级——serve mux 撤 MCP handler）；②c 附录化；client TUI wizard 删除。新铁律：**多机 agent 只读 + 执行，写操作仅 broker TUI / Web UI**；单机 ① 不变（可写）。手机不跑 agent，只做管理面。
2. **发现 + SAS 配对一条龙**：新工作机从「装好二进制」到「agent 可用」= 一条 `ssh-manager pair`（UDP 发现 → 批准 → 双屏比对 → 四件套凭据自动下发 → 首拉 → 写配置）。
3. **Web 管理 UI（手机优先）**：go:embed 单二进制、零 npm/零 CDN；完整管理 + 配对批准 + 吊销 + 审计。

**非目标**：远程写（RW 代理模式，真有需求另立项）；DNS-01 真证书（后续可选增强，需自有域名）；PAKE 第三方库（owner 拍板：SAS+地址绑定档位，见 §3.3 安全性质）；公网/外网暴露（同 VLAN 定位）；多 broker 联邦；②a 兼容开关（直接移除，不留 zombie flag）。

## 2. 事实基础（实证锚，2026-08-28 核验）

- **F1（源码，serve.go:205-234）**：`HTTPHandler` 根 mux——`/snapshot` → `cacheAuth`；**其余一切路径** → `mcpChain`（`projectAuth`(:221) → `resolveServer`(:159) → SDK `NewStreamableHTTPHandler`(:220)）。:205-209 注释钉住「两闸不相交 keystone」（project token 不入 /snapshot、设备码不驱动 MCP）。②a 移除 = 撤 mcpChain 分支，根路径落 404。
- **F2（源码，cli/serve_service.go:630-642）**：`probeServeHTTP` GET `https://<addr>/`（根路径，InsecureSkipVerify，1s，401/200=活）。②a 移除后根路径 404 → **必须重指向 `/snapshot`**（未带码 GET → cacheAuth 401，语义保真；auth 层先拒故零序列化零 touch）。**与 Plan 43 R7 互为携带义务，任一 plan 先落地都必须带上此 seam 改动**。
- **F3（源码，store）**：`AddCacheToken(name, profileID)`（cachetoken.go:24）铸一次性明文设备码（revoked 后同名可重发，active 撞名报错——cachetoken_test.go:117/146 钉住）；`VerifyToken`（projects.go:40）；`GenerateToken`（token.go:12，base64url 32B）。**pairing 服务端零新 token 类型，全复用既有铸码路径**。
- **F4（源码，clientops/pin.go）**：`pinningTransport(pin)` = TLS1.3 + InsecureSkipVerify + VerifyConnection 常时 SPKI 比；`cache.auth.json = {url, token, pin}`；`DoPull`（clientops.go:356）含 server_anchored 锚与 quarantine 语义。pair 落地复用 `DoPull` 首拉。
- **F5（源码，mcpserver/cert.go:71）**：`LoadOrCreateServeCert`——ed25519 自签、100 年、幂等三态；SPKI pin = sha256。**配对临时公钥可用此长期 ed25519 私钥签名**（把配对 transcript 绑到 TLS 身份）。
- **F6（分支）**：Plan 40 批1+2 构件在 `plan-40-batch1-impl` / `plan-40-batch2-impl`（已推 origin，未合 master）；v0.10.1 合入为前置。
- **F7（依赖）**：`golang.org/x/crypto v0.41.0` 在 go.mod（argon2/hkdf 可用，零新增）；Go 1.25 stdlib `crypto/ecdh`（X25519）、`crypto/cipher`（AES-GCM）；charm TUI v2；仓库零前端工具链（package.json 为空）→ Web UI 必须 go:embed 自包含。
- **F8（现状）**：全仓无 UDP 代码（discovery 为全新面）；serve 无 health/metrics 端点；`serve bind` 子命令（跨机隧道监听白名单）仅服务 ②a 场景。store 为 SQLite（modernc，`MaxOpenConns(1)`+WAL）——**跨进程共享态的既有先例**（TUI/serve/CLI 本就同库并发）。

## 3. 设计 · 批1：模式缩减 + 发现 + SAS 配对

### 3.1 ②a 移除与 ②c/wizard 退役

1. **`HTTPHandler` 重构**：删 `mcpChain`（projectAuth/resolveServer→MCP 的接线）与 serve 侧 `verifyToken` 接线；根 mux 变为：`/snapshot`（不动）+ `/pair/*`（§3.3）+ 其余 **404**。keystone 注释改写：两闸不相交属性降格为「project token 不再是任何远程 MCP 凭据——仅 client 侧 spawn 闸；设备码仍只入 /snapshot」。**project token 本地闸语义（冻结）**：数据源 = 快照内 projects 表（Plan 39 /snapshot 按授权裁剪，same-profile projects 随快照下发）；`mcp --cache` 用 `SSHMGR_TOKEN` 对快照内 projects 校验后放行工具面；吊销生效 = lazy——owner 吊销后，client **下一次 pull**（在线 ≤30min 保鲜）即从快照消失、spawn 拒绝；永离线设备失窃沿用既有登记（最终兜底 = 轮换服务器凭据）。
2. **serve 的 agent 执行面整体退役**：resolveServer 背后的远程 SSH 拨号、serve 进程后台任务表、`serve bind` 隧道白名单随 ②a 消失（① 单机的本地 `tunnels`/后台任务面不受影响）。serve 收窄为四件事：**权威 vault + /snapshot + /pair + （批2）/ui**。
3. **`probeServeHTTP` 重指向 `/snapshot`**（F2）。`serve status` 语义不变。
4. **②c**：仅文档层面移入「应急附录」（代码不删——`mcp` stdio 直开本机 vault 是 ① 的正常形态，②c 只是「在 broker 上这么干」的用法说明）。
5. **client TUI**：connect-form/wizard 页代码删除；client 模式缩为 sync/status/doctor 指引。新机首选路径 = `ssh-manager pair`；**手工路径（`cache pull` + 手写 .mcp.json）保留并文档化**（CI/自动化零交互场景，也是下面的迁移路径）。
6. **存量迁移与升级顺序（冻结，三步）**：`/pair/*` 只存在于新 serve——**serve 升级前不存在 pair 路径**。故：
   - ① **手工桥迁移**：存量 ②a 机器在旧 serve 上按既有手工流程迁到桥姿态（broker 控制台 `cache-tokens add` + `projects add`，client `cache pull`——多机文档既有流程，本 plan 只把它提升为「②a 退役后的唯一存量迁移路径」并写清清单）；
   - ② **升 serve**（v0.11.0）——②a 当刻 404，桥姿态机器无感；
   - ③ **pair 时代**：此后所有新机/重配对一律 `ssh-manager pair`。
   compat-matrix 加 v0.11.0 breaking 行 + 上述三步。
7. **`/pair` 开关**：`serve --pairing`（默认 true）+ env `SSHMGR_SERVE_PAIRING=false`（env seam 纪律）；关闭 → `/pair/*` 一律 404（与 discovery 开关相互独立——`--url` 直连 pair 不依赖 discovery）。**开关优先级（含批2 /ui 与 discovery，统一冻结）**：env > flag > store 设置（Settings 页写 store）；serve 侧评估带 ≤5s 内存缓存——pairing/web 开关在 HTTP 路由逐请求评估即时生效，discovery 开关在 UDP 收包后评估（listener 常开、关闭时不应答）；均不要求重启。
8. **默认契约（措辞冻结，rev2 修正）**：未使用新功能时**既有客户端工作流零变化**（`/snapshot`/审计/CLI/探活行为不变）。同时**明示新增网络面**：discovery = 默认开启的新 UDP 应答面（Q1 半可信 LAN 已接受，`--discovery=false` 关）；pairing = 未认证 HTTP 端点（限速+人闸+开关）；web = 批2 管理面（认证+开关）。「零变化」不覆盖这些显式新增面。

### 3.2 UDP 发现（discovery）

- **端口 UDP 7878**（与 TCP serve 同号）；serve `--discovery`（默认 true）+ env `SSHMGR_SERVE_DISCOVERY=false` 隐身开关；监听 0.0.0.0:7878/udp（listener 常开，开关逐包评估，§3.1-7）。
- **报文**：首行魔数 `sshmgr-disc-v1`，次行 JSON，整体 ≤512B。probe：`{"t":"probe"}`；offer（**只单播回请求源**）：`{"t":"offer","name":<显示名>,"spki":<sha256:...>,"tcp":<TCP端口>}`。`name` = 服务器显示名（TUI/Web 可设，缺省 hostname）；**零敏感字段**；server 永不主动广播。未知字段双方忽略（前瞻兼容）；魔数不符静默丢弃。
- **client 侧**（pair 内置）：枚举全部非环回 IPv4 接口，**逐接口** broadcast（覆盖多宿主 + 换网两情形）；1.5s 收集窗；按 spki 去重；多结果列表供选，单结果直入配对。
- **兜底（不依赖 discovery）**：`ssh-manager pair --url https://host:7878 [--pin sha256:...]`（跳过发现；pin 缺省走 TOFU+SAS 终闸）——防火墙/路由异常时的逃生门，也是 CI 姿态。**client 最终写入的 url 恒为其自己的连接地址**（discovery 响应源地址或 `--url`），构造上即正确——serve 不下发 url（§3.3-6）。

### 3.3 SAS 配对协议（/pair/*，冻结 rev2）

角色：client（新工作机，`ssh-manager pair`）/ server（serve）/ 批准者（broker TUI 批1、Web UI 批2、`serve pair` CLI 兜底）。**pending 队列 = store 新表 `pairing_pending`**（跨进程共享，F8 先例；serve 重启行见步骤 2）。

**transcript 与密钥派生（冻结 v2）**：

- 规范 transcript 字节串 = 依序拼接，每段前加 4-byte 小端长度前缀：`"sshmgr-pair-v2" ‖ id ‖ name ‖ target_url ‖ client_pub ‖ cnonce ‖ server_pub ‖ snonce`；`T = SHA256(transcript)`。**`target_url` = client 实际连接的地址**（discovery 源或 `--url`，代码注入非人工输入）——把「client 连的是谁」绑进人可核对的比对面（见安全性质）。
- `K_master = HKDF-SHA256(ikm = X25519(client_priv, server_pub), salt = T, info = "sshmgr-pair-v2", L = 64)`；`K_ack = K_master[0:32]`（finish 的 HMAC-SHA256 键）；`K_creds = K_master[32:64]`（凭据 AEAD 键）。两键物理分离，不复用。
- **SAS 推导（无偏，冻结）**：`R = SHA256("sshmgr-sas-v2" ‖ transcript)`；取 R 的 32-bit 大端块自左向右扫描，首个 `< 4,294,000,000` 的块 v → `sas = fmt.Sprintf("%06d", v % 1_000_000)`；**回退（冻结）**：8 块全拒（概率 ~10⁻²⁹）→ `R₁ = SHA256(R ‖ "again")` 继续同规则扫描，仍全拒则 `R₂ = SHA256(R₁ ‖ "again")`……确定性递推，两侧实现不许可自行裁量。
- **双屏显示（冻结）**：两侧各显示**同一行三件套**：`<name> @ <target_url> SAS <6位>`——owner 肉眼核对**实例名 + 目标地址 + SAS 码**三项全部一致才继续（地址比对 = 透明中继的人防，见安全性质）。

**流程**：

1. **enroll（server 侧此刻生成全部密钥态，冻结 rev2）**：client 生成 X25519 临时密钥对 + 32B 随机 id，`POST /pair/enroll` `{id, name, target_url, client_pub, cnonce, profile_hint?}`（编码契约见步骤 0′）。`name` = instance 名（**Plan 40 白名单校验**）；`profile_hint` 为**展示性字段**（批准界面展示，不参与任何分支；经 InsecureSkipVerify 信道可被篡改——§3.4 登记）。serve 收到合法 enroll 后**立即**：①生成自己的 X25519 临时对 + snonce ②用长期 ed25519 证书私钥对 transcript 签 `sig`（此时 name/target_url/pub/nonce 全部已知）③`pairing_pending` 表落**公值**（id、name、target_url、client_pub、cnonce、server_pub、snonce、sig、state=pending、双窗口时间戳）——**server 临时私钥只留 serve 进程内存**（id 索引，TTL 跟随 pending），任何私钥不入表。**serve 重启**：内存私钥丢失 → 启动时将 state=pending/approved 的 in-flight 行标 expired（client poll 得 410「broker restarted, re-pair」）——与「队列持久化」不矛盾：表是决策记录，密钥态是进程态。
   - **撞名活性规则（冻结）**：`name` 撞 active cache-token → serve 查该 token `last_pull`：**为零（从未拉过）→ 自动 revoke + 放行重配对**（audit 记 auto-revoke；覆盖「finish 铸码成功但 client 落地失败」的孤码场景）；**非零（在用）→ 拒绝**（419），提示换名或 owner 先 `cache-tokens revoke`。
   - 限速：per-IP enroll 5/min、全局 30/min、per-IP pending ≤2、全局 pending ≤32（超限 429/409）。
2. **双窗口（冻结）**：`enroll → 批准` 窗口 **10 分钟**；`批准 → finish` 窗口 **120 秒**；起算点 = 各自事件完成时刻，落库时间戳为准；过期 lazy 清理。
3. **批准（任何批准面 = 只写一条 profile 决策，冻结 rev2）**：批准界面（TUI/Web/CLI 三面）列 pending `{name, target_url, 来源IP, profile_hint, 剩余秒}`；批准者选 profile（vault 设置 `pair.default_profile` 预选）后执行**单条 CAS UPDATE**：`UPDATE pairing_pending SET state='approved', profile=?, approved_at=? WHERE id=? AND state='pending'`——败者（并发）409。**批准面不生成/不接触任何密钥态**（server 临时对在 enroll 时已由 serve 生成，§步骤 1）；密钥态唯一持有者 = serve 进程。`pair-<name>` project 复用规则（冻结）：仅当 (a) 该 project 带 `pair_generated` 标记（models 新增布尔）且 (b) 其 profile 与本次批准一致 → 允许复用，且 finish 时吊销该项目全部旧 token（rotate 语义）；同名非 pair project 或 profile 不一致 → 批准界面拒绝并提示换名/owner 处置。
4. **poll（POST，冻结 rev2）**：client 每 2s `POST /pair/poll` `{id}`（id 不走 GET query——避免进访问/代理日志）；未批准 → 202 `{"t":"pending"}`；已批准 → 200 `{"t":"approved","server_pub","sig","snonce"}`（自表直读公值）；过期/拒绝/作废 → 410/403。client 收 approved 后用 discovery pin **机会性**验签 `sig`（失败 WARN 不阻断——SAS+地址比对才是人闸）。
5. **SAS 三件套双屏比对**：两侧按冻结公式算 6 位；client 终端显示 `name @ target_url SAS xxxxxx` 并要求确认；server 侧批准界面/`serve pair approve` 输出**同一行三件套**（CLI 批准路径同样必须打印，冻结）。**用户肉眼三项全对才继续**。
6. **finish（铸码 + 幂等，冻结 rev2）**：client `POST /pair/finish` `{id, ack}`；`ack = HMAC-SHA256(K_ack, "finish" ‖ id)`。serve 验过后**在单个 SQLite 事务内**完成全部铸码：`AddCacheToken(name, profile)` + project 复用（含吊销旧 token）或新建（`pair-<name>`，置 `pair_generated=true`）绑同 profile + project token 签发；事务提交后以 **AEAD（AES-256-GCM，键 K_creds，nonce 随密文）** 封装 `{spki, profile, device_code, project_token, max_offline}`——**信封含 profile 名**，client 解密后回显「已授权 profile: X」供 owner 与批准界面双向核对。事务失败 → pending 置 failed（零残留——单事务回滚保证）。**幂等（冻结）**：成功响应的 AEAD 密文缓存于 pending 行（state=delivered）；finish 重发/响应丢失 → 重放**同一密文**，不重铸码。**不含 url**——client 恒用自身连接地址（§3.2）。`max_offline` 取 vault 设置 `pair.default_max_offline`（缺省 `24h`）。未批准/窗口外/ack 错 → 409/410/403。
7. **client 落地**：解密凭据 → 写 `cache.auth.json{url: 连接地址, token: 设备码, pin: spki}`（per-instance 目录）→ 写 `cache.config.json{"max_offline": <下发值>}` → 调既有 `DoPull` **首拉**（server_anchored 锚随之落盘；**若首拉失败**：凭据已落盘，重试 = `cache pull --instance <name>`，无需重跑 pair——撞名规则已覆盖「落地失败后仍想重来」的例外）→ 打印 .mcp.json 片段（token 以 `<project-token>` **占位符**呈现；真值仅 `--write-mcp <path>` 落盘；凭据前 8 字符回显供人工核对；**终端零完整凭据**）。
8. **审计**：pairing 事件（enroll/auto-revoke/批准/拒绝/finish/过期/failed/delivered-replay）+ 批2 起 Web 全部写操作（§4.2 枚举）全落 vault audit。
9. **CLI 面**：`ssh-manager pair [--url] [--pin] [--profile-hint <名>] [--write-mcp <path>]`；自动化免比对 = env `SSHMGR_PAIR_ASSUME_SAS=1`（显式 env 门槛 + STUB 大字警告；无 CLI flag）；server 侧 `serve pair ls/approve/reject`（approve 输出三件套行）。

**编码契约（冻结 rev2）**：`id` = 32B hex（64 字符）；`client_pub`/`server_pub` = X25519 raw 32B base64url；`cnonce`/`snonce` = 16B base64url；`sig` = ed25519 64B base64url；`ack` = HMAC-SHA256 32B hex。**畸形处理（冻结）**：重复 `id` → 409；公钥/nonce 解码失败或长度错 → 400；JSON 畸形 → 400；CAS 败 → 409。

**安全性质（rev2 重写，修正 rev1 过度断言）**：

- **换钥型 MITM**（TLS 终结假扮 broker / 换 server_pub）：transcript 变 → **双屏三件套不一致**，SAS 与地址同时暴露——防住。
- **透明中继**（假 offer 引 client 连攻击者，字节原样转发真 broker）：transcript/SAS **保持一致**——SAS 防不了；防线 = **target_url 双屏比对**（client 屏显示的是攻击者地址，owner 与 broker 真实地址一眼可辨）。**凭据不泄**：中继读不到 TLS 内文与 AEAD 信封；若攻击者转为终结 TLS 假扮，后续 `DoPull` 的 `pinningTransport` SPKI 常时比对在握手阶段即断，**设备码不上行**。残余 = **中继位置攻击**（DoS / url 被钉在攻击者地址 / 流量分析）——**owner 拍板登记接受**（半可信 LAN 威胁模型，Q1；PAKE 同样不防中继位置，不引入）。
- 重放 → nonce + 双窗口 + 一次性码；穷举 → 批准人审 + 限速；失窃 → `cache-tokens revoke` + `projects revoke` 双吊销（lazy ≤30min 生效，§3.1-1）。

### 3.4 残余风险登记

- **人因（唯一人闸）**：三项比对（名/地址/SAS）任一被忽略不看的残余——文案明示「任一不一致 = 攻击，立即中止」。
- **discovery 伪造 + 透明中继位置攻击**：见 §3.3 安全性质——登记接受（owner 拍板 2026-08-28）。
- **profile_hint 经未验信道可被篡改**（展示性字段）：真闸 = 批准者自己选 profile + finish 信封内 profile 双向核对；hint 仅参考——登记。
- **LAN 可见性**（offer 暴露「此处有 broker」）：Q1 已接受（零敏感字段 + 单播响应）。
- **配对面 DoS**：per-IP pending ≤2 / 全局 ≤32 下多 IP 恶意 enroll 仍可短暂占满全局配额——半可信 LAN 定位登记接受；限速参数可用 env 调（`SSHMGR_SERVE_PAIR_RATE_*` 实现裁量）。

## 4. 设计 · 批2：Web 管理 UI

### 4.1 架构

- `/ui` 路由组挂 serve mux；**go:embed** 静态资源（CSS 内嵌编译期）+ SSR（html/template）；**零 npm / 零 CDN / 单二进制 / 离线 VLAN 可用**；手机优先响应式（单列 → 桌面双列）。网络定位：同 VLAN（复用 serve `--addr` 既有语义与自签 TLS）。
- **`/ui` 开关（rev2 补）**：`serve --web`（默认 true）+ env `SSHMGR_SERVE_WEB=false` → `/ui` 一律 404（与 pairing/discovery 同一套优先级与热生效语义，§3.1-7）。
- 页面清单：登录 / 总览 / Servers（CRUD + 凭据表单）/ Profiles（grant）/ Projects（签发·轮换·吊销；一次性 token 仅签发响应出现一次）/ Instances（cache-tokens 管理 + 一键双吊销）/ **Pairing（两段式：列表页 `<meta http-equiv="refresh" content="5">` 5s 自刷新感知新 pending；批准详情页（读三件套/选 profile/提交）无刷新防打断表单，提交后跳回列表）** / Audit（分页过滤）/ Settings（admin 密码、`pair.default_profile`、`pair.default_max_offline`、discovery/pairing/web 开关）。

### 4.2 认证、会话与审计

- **首启**：`ssh-manager serve admin set`（或 TUI）设管理员账密；密码 **argon2id**（m=64MB, t=3, p=4，参数随行存储）入 vault 新表 `admin_users`；TUI 可重置（重置即全量 session 失效，见下）。
- **session = store 新表 `admin_sessions`（rev2 修法）**：存 session id 的 SHA-256（原始 id 只在 cookie）、绝对过期、滑动过期、创建者 IP——**跨进程即时生效**（TUI/CLI 改密或重置 → 直接 DELETE 表行，serve 无需广播）。登录限速：失败 5 次/15min 锁定（IP + 账号双维度）。cookie `HttpOnly + Secure + SameSite=Strict`；服务端 TTL 12h 滑动 + **绝对上限 7d**；改密/管理员重置 → **全量 session 失效**。CSRF：每会话 token，所有 POST 表单内嵌校验。安全头：`Content-Security-Policy: default-src 'none'; style-src 'self'`、全部响应 `Cache-Control: no-store`。
- **审计覆盖（枚举，冻结）**：登录成功/失败、Servers/Profiles/Projects/Instances 的一切写操作、token 签发/轮换/吊销、pairing 全事件（§3.3-8）、admin 密码与 settings 修改——全部落 vault audit（Web 面无审计盲区）。
- 自签证书在手机浏览器的告警：一次性接受例外（登记 habituation 风险 R5；DNS-01 为远期解）。

### 4.3 配对批准（Web）

= §3.3 步骤 3 的第二 UI：读同一 `pairing_pending` 表、写同一条 CAS UPDATE——**密钥态唯一持有者恒为 serve 进程**（enroll 时生成，§3.3-1），批准面零密钥接触；三件套（name/target_url/SAS）在批准详情页展示。

## 5. 测试策略

**批1**：
- discovery：回环定向 UDP 双 socket——probe/offer 收发、魔数不符静默、畸形 JSON 静默、offer **单播**断言、多接口枚举（stub 接口表）、--discovery=false 逐包评估后零应答。
- pairing（httptest TLS + 真 handler + 真 store，零模拟 HTTP 层）：
  - 编码契约：重复 id 409 / 非法公钥 400 / nonce 长度错 400 / JSON 畸形 400。
  - enroll 白名单拒 / active 撞名两分支（last_pull 零 → auto-revoke + 放行 + audit；非零 → 419）/ 限速与 pending 配额 / 双窗口 TTL（410）。
  - 密钥态：server 临时对 enroll 时生成断言（表内无私钥、serve 内存有）；serve 重启模拟 → in-flight 行 expired、poll 410。
  - CAS 并发批准（双写一败 409）。
  - poll POST（query 零 id 断言）/ 状态机 202→200 / sig 验签。
  - **SAS/HKDF 确定性向量**：固定密钥 → 固定三件套；**全块拒采回退向量**（构造 R 前 8 块全 ≥ 阈值的锚定向量 → R₁ 递推）；K_ack/K_creds 分离断言。
  - finish：ack 错拒 / 铸码**单事务**断言（注入中途失败 → 回滚零残留）/ **幂等**（重发 finish → 重放同密文、store 零新 token）/ `pair-<name>` 复用四分支（pair_generated×profile 一致与否）/ **信封含 profile** + client 回显断言。
  - **MITM 腿**：换 server_pub → 双屏三件套不一致必现；**透明中继腿**（纯管道转发 → SAS 一致断言 + client 屏 target_url=中继地址断言 + 后续 DoPull 到中继终结 TLS 被 SPKI pin 拒断、码不上行断言）。
  - 审计行断言（含 auto-revoke/delivered-replay）。
- ②a 移除契约：根路径与任意 MCP 路径 → 404；`/snapshot` 未带码 → 401；`--pairing=false` → `/pair/*` 404；`probeServeHTTP` 打真 serve → true。
- pair e2e（临时目录 + env `SSHMGR_PAIR_ASSUME_SAS=1`）：cache.auth.json（url=连接地址）/ cache.config.json / cache.bin + 锚 / 片段占位符 / `--write-mcp` 真值落盘 / **首拉失败路径**（凭据落盘 + `cache pull` 重试成功）。
- pending 队列跨进程：serve 写 → TUI/CLI 读同表；TUI 重置 admin 密码 → serve 侧 session 即刻失效（批2 前置断言可先以 store 直查形式钉）。
- 全仓绿；默认契约：既有工作流零变化（§3.1-8 措辞）。

**批2**：
- argon2id / 登录限速 / session 表（绝对上限 7d、滑动窗口、改密 DELETE 全量、跨进程即时）/ CSRF 拒 / CSP + no-store 头 / `--web=false` → 404。
- 页面 smoke：每页 200；Pairing 两段式（列表刷新/详情不刷新 meta 断言）；一次性 token 仅签发响应出现一次。
- Web 批准与 TUI 同表同 CAS；`/ui` 不影响 `/snapshot`、`/pair`；audit 只读；Web 写操作全覆盖 audit 断言（§4.2 枚举逐项）。

## 6. 文档联动

deployment-modes.md **重写**（4→2+管理面；「怎么选」改写：桌面多机默认 = pair 一条龙、手机 = Web 管理、②a 移除三步迁移节）；multi-machine.md（删 Step3 TLS 两坑节、新增 pair 流程、手工桥迁移路径提升为存量迁移官方路径）；quickstart-multi-machine.md 重写为 pair 版；broker-host-agent.md（姿势 A 删除；②c 移附录）；agent-tools.md（多机只读铁律）；compat-matrix.md（v0.11.0 breaking：②a 移除 + 三步迁移 + 升级顺序铁律）；threat-model.md（discovery 零敏感面 / SAS+地址绑定配对与密钥派生 / **中继位置攻击登记与不泄凭据论证** / pairing audit / Web 认证与会话面）；README 索引；backlog 相应销项。

## 7. 验收清单

**批1**：
1. 全仓测试绿；②a 移除契约测试绿（404/401 不变/开关/probe PASS）。
2. pair e2e（§5）绿（含首拉失败重试路径）；MITM 双腿绿（换钥不一致 + 中继不泄码）；HKDF/SAS 确定性向量（含回退）绿。
3. **真机 gate（owner）**：NUC10 升 serve（discovery + pairing 开）；先按三步迁移把笔记本存量 ②a 配置迁桥；干净目录真跑 `ssh-manager pair` → TUI 批准 → **三件套双屏比对** → agent 在线/断网各验一次；`cache-tokens ls` 与 audit 可见配对事件。
4. ②a 负面验收：旧 ②a `.mcp.json` → agent 连接得 404 明确失败。
**批2**：
5. 手机（同 VLAN）登录 Web → 批准一次真配对（两段式页面）→ 全流程闭环；一键双吊销生效。
6. 认证/CSRF/限速/安全头/会话表/审计覆盖测试绿；全仓绿。
7. **v0.11.0 发布**（goreleaser 既有管线；compat-matrix breaking 行上线）。

## 8. 风险与备选

- **R1 存量迁移破坏面**：②a client 在 serve 升级瞬间断供——三步迁移路径（§3.1-6）文档化 + 验收 3 演练；不做自动迁移工具（用户 = owner 本人，清单化）。
- **R2 配对人因**：三件套比对（名/地址/SAS）任一被忽略即人闸失效——文案强警示 + 免比对仅 env `SSHMGR_PAIR_ASSUME_SAS=1`（STUB 大字警告，无 CLI flag）。
- **R3 client 防火墙拦广播回包**：真机验收覆盖 Windows 笔记本；失败即走 `--url` 兜底。
- **R4 server 防火墙拦 UDP 7878**：安装文档一句（如 `ufw allow 7878/udp`）；`--url` 兜底。
- **R5 自签证书手机警告 habituation**：登记；DNS-01 真证书为远期可选。
- **R6 Web 面扩大攻击面**：同 VLAN + argon2id + 限速 + CSRF + CSP + no-store + 全量审计 + `/ui` 可关；无公网暴露。
- **R7 probeServeHTTP 共享 seam**：与 Plan 43 R7 双登记，任一 plan 先落地必须携带。
- **R8 pending = store 表 + 密钥态 = serve 内存（rev2 定稿）**：决策记录持久、密钥态进程态——serve 重启丢 in-flight（自动 expired，client 重跑 pair）；表内零私钥；`admin_sessions` 同理跨进程共享。
- **R9（备选记录）**：poll 轮询若评审嫌糙 → 长轮询/SSE；不取（复杂度不值）。
- **R10（owner 拍板登记，2026-08-28）**：透明中继位置攻击（DoS/url 钉死/流量分析，凭据不泄）在半可信 LAN 威胁模型下接受；防线 = 三件套双屏比对；PAKE 不引入（同样不防中继位置，徒增依赖）。

## 9. 变更记录

### 9.1 rev0 → rev1（首审 codex+kimi 异构盲评 20 条闭环全吸收；无驳回、无降级、无 owner 拍板项）

（18 组修法明细同 rev1 原文，此处不赘——升级三步迁移 / 铸码移 finish / pending 落 store 表 / HKDF+transcript 冻结 / SAS 无偏推导 / 删 url 下发 / profile_hint 通路 / Web 审计枚举 / session 上限+改密失效 / 终端零凭据 / 双窗口 / 本地闸语义 / assume-sas 改 env / 限速配额 / max_offline 来源 / meta refresh。）

### 9.2 rev1 → rev2（复审轮：codex DISAGREE 7 条 + kimi SUGGEST_CHANGES 10 条，17 条闭环，含 1 处 owner 拍板）

1. **批准架构修正（codex#2，高）**：server 临时密钥对改 **enroll 时由 serve 进程生成并签名**（私钥仅 serve 内存，表内零私钥）；三面批准 = 单条 CAS UPDATE 写 profile 决策，零密钥接触；serve 重启 → in-flight 自动 expired（client 重跑）。消除 rev1「单写入点=serve vs TUI/CLI 写表」自相矛盾。
2. **透明中继修正（codex#1+#4，高/中；owner 拍板档位 A）**：transcript v2 纳入 `name` + `target_url`；**双屏显示三件套**（实例名@目标地址+SAS）；安全性质段重写——SAS 防换钥型 MITM，透明中继防线下沉为地址比对；**核验结论入 spec**：中继不泄凭据（TLS 内文不可读 + AEAD 信封 + DoPull SPKI pin 握手期拒断）；残余 = 位置攻击，R10 登记 owner 拍板接受；profile 绑定 = finish 信封内含 profile + client 回显双向核对（取代「profile 入 transcript」——sig 必须在 enroll 时签，profile 彼时未定）。
3. **finish 事务与幂等（codex#3，中）**：全部铸码单 SQLite 事务；成功密文缓存 pending 行，重试重放不重铸。
4. **pair-\<name\> 复用规则四分支冻结（codex#5，中）**：`pair_generated` 标记 + profile 一致才复用 + 吊销旧 token；否则拒绝。
5. **协议编码契约冻结（codex#6，中）**：id/pub/nonce/sig/ack 编码与长度、重复 id 409、非法公钥 400、CAS 409、poll 改 POST（兼 kimi#8——id 不入 GET query/日志）。
6. **默认契约措辞（codex#7 + kimi#4，低）**：改「既有客户端工作流零变化」+ 明示三个新增网络面（discovery UDP 应答/pairing 未认证端点/web）。
7. **`/ui` 开关 + env seam（kimi#1，中）**：`--web`/`SSHMGR_SERVE_WEB`，404 可关，与 §3.1-7 统一优先级/热生效。
8. **session 落 store 表 `admin_sessions`（kimi#2，中）**：跨进程改密/重置即时失效（存 id 哈希非原文）。
9. **enroll 撞名活性规则（kimi#3，中）**：last_pull 零 → auto-revoke 放行重配对；非零 → 419 指引；client 首拉失败改走 `cache pull` 重试（凭据已在盘）。
10. **CLI 批准必须打印三件套（kimi#5，低）**。
11. **SAS 全块拒采回退（kimi#6，低）**：SHA256 递推 R₁/R₂…，确定性冻结。
12. **Pairing 页两段式（kimi#7，低）**：列表刷新/详情停刷新。
13. **Settings 开关热生效语义（kimi#9，低）**：env > flag > store；路由逐请求评估 + ≤5s 缓存；listener 常开。
14. **profile_hint 可篡改登记（kimi#10，低）**：§3.4 一句。
