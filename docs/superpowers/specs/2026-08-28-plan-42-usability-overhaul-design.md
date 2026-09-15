# Plan 42 — 易用性改造:模式缩减 + 发现配对一条龙 + Web 管理 UI 设计 spec

- 日期：2026-08-28
- 来源：2026-08-28 grilling 会话（Q1–Q21 逐题 owner 确认）+ 与 Plan 43（doctor 探活，rev2.1）冲突审查闭环。痛点实证：多机 client 冷启动 = broker 控制台人肉 `cache-tokens add` + `projects add` + 跨机手抄三串字符串 + ②a 的 TLS 信任库两坑（NODE_EXTRA_CA_CERTS/SAN）。
- 前置：**v0.10.1 先行发版**（合 `plan-40-batch1-impl` / `plan-40-batch2-impl` / Plan 41 worktree；doctor-multi-instance 就绪则同船）。本 plan 依赖 Plan 40 的 per-instance 构件（`CachePathsFor` / device name 三合一 / MAX_OFFLINE 持久化）。
- 状态：rev0 定稿待 owner 审

## 1. 目标与非目标

**目标**（四核心诉求 → 三个交付）：

1. **模式缩减 4 → 2 + 管理面**：②a 在线 HTTP 直连**移除**（不是降级——serve mux 撤 MCP handler）；②c 附录化；client TUI wizard 删除。新铁律：**多机 agent 只读 + 执行，写操作仅 broker TUI / Web UI**；单机 ① 不变（可写）。手机不跑 agent，只做管理面。
2. **发现 + SAS 配对一条龙**：新工作机从「装好二进制」到「agent 可用」= 一条 `ssh-manager pair`（UDP 发现 → 批准 → 双屏比对 → 四件套凭据自动下发 → 首拉 → 写配置）。
3. **Web 管理 UI（手机优先）**：go:embed 单二进制、零 npm/零 CDN；完整管理 + 配对批准 + 吊销 + 审计。

**非目标**：远程写（RW 代理模式，真有需求另立项）；DNS-01 真证书（后续可选增强，需自有域名）；PAKE 第三方库（SAS 已覆盖）；公网/外网暴露（同 VLAN 定位）；多 broker 联邦；②a 兼容开关（直接移除，不留 zombie flag）；pairing pending 队列持久化（内存态，serve 重启即清）。

## 2. 事实基础（实证锚，2026-08-28 核验）

- **F1（源码，serve.go:205-234）**：`HTTPHandler` 根 mux——`/snapshot` → `cacheAuth`；**其余一切路径** → `mcpChain`（`projectAuth`(:221) → `resolveServer`(:159) → SDK `NewStreamableHTTPHandler`(:220)）。:205-209 注释钉住「两闸不相交 keystone」（project token 不入 /snapshot、设备码不驱动 MCP）。②a 移除 = 撤 mcpChain 分支，根路径落 404。
- **F2（源码，cli/serve_service.go:630-642）**：`probeServeHTTP` GET `https://<addr>/`（根路径，InsecureSkipVerify，1s，401/200=活）。②a 移除后根路径 404 → **必须重指向 `/snapshot`**（未带码 GET → cacheAuth 401，语义保真；auth 层先拒故零序列化零 touch）。**与 Plan 43 R7 互为携带义务，任一 plan 先落地都必须带上此 seam 改动**。
- **F3（源码，store）**：`AddCacheToken(name, profileID)`（cachetoken.go:24）铸一次性明文设备码（revoked 后同名可重发，active 撞名报错——cachetoken_test.go:117/146 钉住）；`VerifyToken`（projects.go:40）；`GenerateToken`（token.go:12，base64url 32B）。**pairing 服务端零新 token 类型，全复用既有铸码路径**。
- **F4（源码，clientops/pin.go）**：`pinningTransport(pin)` = TLS1.3 + InsecureSkipVerify + VerifyConnection 常时 SPKI 比；`cache.auth.json = {url, token, pin}`；`DoPull`（clientops.go:356）含 server_anchored 锚与 quarantine 语义。pair 落地复用 `DoPull` 首拉。
- **F5（源码，mcpserver/cert.go:71）**：`LoadOrCreateServeCert`——ed25519 自签、100 年、幂等三态；SPKI pin = sha256。**配对临时公钥可用此长期 ed25519 私钥签名**（把配对 transcript 绑到 TLS 身份）。
- **F6（分支）**：Plan 40 批1+2 构件在 `plan-40-batch1-impl` / `plan-40-batch2-impl`（已推 origin，未合 master）；v0.10.1 合入为前置。
- **F7（依赖）**：`golang.org/x/crypto v0.41.0` 在 go.mod（argon2/hkdf 可用，零新增）；Go 1.25 stdlib `crypto/ecdh`（X25519）、`crypto/cipher`（AES-GCM）；charm TUI v2；仓库零前端工具链（package.json 为空）→ Web UI 必须 go:embed 自包含。
- **F8（现状）**：全仓无 UDP 代码（discovery 为全新面）；serve 无 health/metrics 端点；`serve bind` 子命令（跨机隧道监听白名单）仅服务 ②a 场景。

## 3. 设计 · 批1：模式缩减 + 发现 + SAS 配对

### 3.1 ②a 移除与 ②c/wizard 退役

1. **`HTTPHandler` 重构**：删 `mcpChain`（projectAuth/resolveServer→MCP 的接线）与 serve 侧 `verifyToken` 接线；根 mux 变为：`/snapshot`（不动）+ `/pair/*`（§3.3）+ 其余 **404**。keystone 注释改写：两闸不相交属性降格为「project token 不再是任何远程 MCP 凭据——仅 client 侧 spawn 闸（cache 模式本地校验）；设备码仍只入 /snapshot」。
2. **serve 的 agent 执行面整体退役**：resolveServer 背后的远程 SSH 拨号、serve 进程后台任务表、`serve bind` 隧道白名单随 ②a 消失（① 单机的本地 `tunnels`/后台任务面不受影响）。serve 收窄为四件事：**权威 vault + /snapshot + /pair + （批2）/ui**。
3. **`probeServeHTTP` 重指向 `/snapshot`**（F2）。`serve status` 语义不变。
4. **②c**：仅文档层面移入「应急附录」（代码不删——`mcp` stdio 直开本机 vault 是 ① 的正常形态，②c 只是「在 broker 上这么干」的用法说明）。
5. **client TUI**：connect-form/wizard 页代码删除；client 模式缩为 sync/status/doctor 指引。新机唯一路径 = `ssh-manager pair`；**手工路径（`cache pull` + 手写 .mcp.json）保留并文档化**（CI/自动化零交互场景）。
6. **升级顺序铁律**：**先迁 client（②a 形态 → pair/桥），后升 serve**——serve 升级瞬间 ②a client 即 404。compat-matrix 加 v0.11.0 breaking 行。

### 3.2 UDP 发现（discovery）

- **端口 UDP 7878**（与 TCP serve 同号）；serve `--discovery`（默认 true）+ env `SSHMGR_SERVE_DISCOVERY=false` 隐身开关；监听 0.0.0.0:7878/udp。
- **报文**：首行魔数 `sshmgr-disc-v1`，次行 JSON，整体 ≤512B。probe：`{"t":"probe"}`；offer（**只单播回请求源**）：`{"t":"offer","name":<显示名>,"spki":<sha256:...>,"tcp":<TCP端口>}`。`name` = 服务器显示名（TUI/Web 可设，缺省 hostname）；**零敏感字段**；server 永不主动广播。未知字段双方忽略（前瞻兼容）；魔数不符静默丢弃。
- **client 侧**（pair 内置）：枚举全部非环回 IPv4 接口，**逐接口** broadcast（覆盖多宿主 + 换网两情形）；1.5s 收集窗；按 spki 去重；多结果列表供选，单结果直入配对。
- **兜底（不依赖 discovery）**：`ssh-manager pair --url https://host:7878 [--pin sha256:...]`（跳过发现；pin 缺省走 TOFU+SAS 终闸）——防火墙/路由异常时的逃生门，也是 CI 姿态。

### 3.3 SAS 配对协议（/pair/*，冻结）

角色：client（新工作机，`ssh-manager pair`）/ server（serve）/ 批准者（broker TUI 批1、Web UI 批2、`serve pair` CLI 兜底——三面同一 pending 队列）。

1. **enroll**：client 生成 X25519 临时密钥对 + 32B 随机 id，`POST /pair/enroll` `{id, name, client_pub, cnonce}`。`name` = instance 名（**Plan 40 白名单校验**；active 撞名即拒，client 提示换名——F3 语义）。传输走 TLS（InsecureSkipVerify——信任由 SAS 人闸兜底，discovery 的 pin 为机会性绑定）。server 限速：per-IP 5/min、全局 30/min、pending 上限 10（超限 429）；pending 项 TTL **120s**，内存态。
2. **批准**：批准界面列出 pending `{name, 来源IP, 剩余秒}`；批准者选 profile（vault 设置 `pair.default_profile` 预选，TUI/Web 可改，未设则列首）。批准瞬间 server：①生成自己的 X25519 临时对 ②`AddCacheToken(name, profile)` 铸一次性设备码 ③自动建 project `pair-<name>` 绑同 profile 并铸一次性 project token ④入待交付态。
3. **poll**：client 每 2s `GET /pair/poll?id=`；未批准 → 202 `{"t":"pending"}`；已批准 → 200 `{"t":"approved","server_pub","sig","snonce"}`。`sig` = serve 长期 ed25519 证书私钥对 `(server_pub‖id‖cnonce‖snonce)` 的签名（F5）——client 用 discovery pin **机会性**验签（失败 WARN 不阻断，SAS 才是终闸）。
4. **SAS 双屏比对**：两侧各算 `sas = 6 位十进制 ← SHA256("sshmgr-sas-v1"‖client_pub‖server_pub‖cnonce‖snonce) 前 20 bit`。client 终端显示并要求确认；server 侧批准界面同步显示。**用户肉眼一致才继续**。任一层 MITM（TLS 终结/中继/换公钥）必致两屏不一致。
5. **finish**：client `POST /pair/finish` `{id, ack}`；`ack = HMAC-SHA256(HKDF-SHA256(ECDH(client,server)‖transcript), "finish")`——同时证明持有临时私钥且过了人闸。server 验过后以 **AEAD（AES-256-GCM，密钥 = 同 HKDF 派生 "creds"）** 返回 `{url, spki, device_code, project_token, max_offline:"24h"}`（nonce 随密文）。窗口外/未批准/ack 错 → 410/409/403。
6. **client 落地**（pair 命令继续执行）：解密四件套 → 写 `cache.auth.json{url, token:设备码, pin:spki}`（per-instance 目录）→ 写 `cache.config.json{"max_offline":"24h"}` → 调既有 `DoPull` **首拉**（server_anchored 锚随之落盘）→ 打印 .mcp.json 片段（`command: ssh-manager, args: [mcp, --cache, --instance, <name>], env.SSHMGR_TOKEN: <project_token>`）；`--write-mcp <path>` 可直接写盘。终端只回显凭据前 8 字符。
7. **审计**：enroll / 批准 / 拒绝 / finish / 过期 全落 vault audit（`pairing` 类目）。
8. **CLI 面**：`ssh-manager pair [--url] [--pin] [--profile-hint <名>] [--write-mcp <path>] [--assume-sas]`（`--assume-sas` 仅供自动化测试，帮助文本大字警告）；server 侧 `serve pair ls/approve/reject`。

**安全性质**：MITM → SAS 双屏不一致（唯一依赖人因，文案强警示）；重放 → nonce + 120s 窗 + 一次性码；穷举 → 批准人审 + 限速；失窃 → `cache-tokens revoke` + `projects revoke` 双吊销（TUI/Web 一键）。

### 3.4 残余风险登记

- **SAS 人因**（用户对不一致仍确认）：全案唯一人因残余，文案明示「不一致 = 攻击，立即中止」。
- **discovery 伪造**（假 server + 假 pin 骗 enroll）：真 broker 屏幕不会出现该 pending → 批准不会发生；SAS 终闸。登记接受。
- **LAN 可见性**（offer 暴露「此处有 broker」）：Q1 已接受（半可信 LAN，加固便宜就加固——零敏感字段 + 单播响应）。

## 4. 设计 · 批2：Web 管理 UI

### 4.1 架构

- `/ui` 路由组挂 serve mux；**go:embed** 静态资源（CSS 内嵌编译期）+ SSR（html/template）；**零 npm / 零 CDN / 单二进制 / 离线 VLAN 可用**；手机优先响应式（单列 → 桌面双列）。网络定位：同 VLAN（复用 serve `--addr` 既有语义与自签 TLS）。
- 页面清单：登录 / 总览（角色、版本、实例数、pending 配对数）/ Servers（CRUD + 凭据表单）/ Profiles（grant）/ Projects（签发·轮换·吊销；一次性 token 仅签发响应出现一次）/ Instances（cache-tokens 管理 + 一键双吊销）/ **Pairing（§3.3 同一队列，含 SAS 展示与 profile 选择）** / Audit（分页过滤）/ Settings（admin 密码、默认 profile、discovery 开关）。

### 4.2 认证与会话

- **首启**：`ssh-manager serve admin set`（或 TUI）设管理员账密；密码 **argon2id**（m=64MB, t=3, p=4，参数随行存储）入 vault 新表 `admin_users`；TUI 可重置。
- 登录限速：失败 5 次/15min 锁定（IP + 账号双维度）。session = 256bit 随机 id，cookie `HttpOnly + Secure + SameSite=Strict`，服务端 TTL 12h 滑动。CSRF：每会话 token，所有 POST 表单内嵌校验。安全头：`Content-Security-Policy: default-src 'none'; style-src 'self'`（CSS 走嵌入文件）、全部响应 `Cache-Control: no-store`。
- 自签证书在手机浏览器的告警：一次性接受例外（登记 habituation 风险 R5；DNS-01 为远期解）。

### 4.3 配对批准（Web）

= §3.3 步骤 2 的第二 UI，与 TUI/CLI 共享同一内存 pending 队列与同一审批状态机（单写入点），SAS 在批准页展示。

## 5. 测试策略

**批1**：
- discovery：回环定向 UDP 双 socket——probe/offer 收发、魔数不符静默、畸形 JSON 静默、offer **单播**断言（无广播回包）、多接口枚举（stub 接口表）、--discovery=false 零响应。
- pairing（httptest TLS + 真 handler，零模拟 HTTP 层）：enroll 白名单拒绝/active 撞名拒/限速 429/pending TTL 过期/poll 202→200 状态机/sig 验签/SAS **确定性向量**（固定密钥 → 固定 6 位）/finish ack 错拒/**MITM 腿**（中间换 server_pub → 两侧 SAS 必不一致）/AEAD 四件套往返/410 过期/audit 行断言。
- ②a 移除契约：根路径与任意 MCP 路径 → **404**；`/snapshot` 未带码 → **401**（既有不变）；`probeServeHTTP` 打真 serve → true。
- pair e2e（临时目录 + `--assume-sas`）：断言 cache.auth.json / cache.config.json(max_offline) / cache.bin + 锚 / 打印与写盘的 .mcp.json golden。
- TUI 批准页：喂 pending → 批准 → 队列消费 + audit。
- 全仓绿；**默认契约**：不启用 discovery/pair 时 serve 行为零变化。

**批2**：
- argon2id 哈希/登录限速/会话 TTL/CSRF 无 token POST 拒/CSP + no-store 头断言。
- 页面 smoke：每页 200 + 关键元素；一次性 token 只在签发响应出现一次。
- Web 批准与 TUI 同队列（一面批准另一面即消失）；`/ui` 不影响 `/snapshot`、`/pair`；audit 页只读。

## 6. 文档联动

deployment-modes.md **重写**（4→2+管理面；「怎么选」改写：桌面多机默认 = pair 一条龙、手机 = Web 管理、②a 移除迁移节）；multi-machine.md（删 Step3 TLS 两坑节、新增 pair 流程、手工路径保留）；quickstart-multi-machine.md 重写为 pair 版；broker-host-agent.md（姿势 A 删除——broker 主机 agent = 零距离 client 走桥姿态；②c 移附录）；agent-tools.md（多机只读铁律）；compat-matrix.md（v0.11.0 breaking：②a 移除 + 升级顺序铁律）；threat-model.md（discovery 零敏感面 / SAS 配对 / pairing audit / Web 认证面）；README 索引；backlog 相应销项。

## 7. 验收清单

**批1**：
1. 全仓测试绿；②a 移除契约测试绿（404/401 不变/probe PASS）。
2. pair e2e（§5）绿：四件套落盘 + 首拉 + 锚 + .mcp.json golden。
3. MITM 腿绿（SAS 不一致必现）。
4. **真机 gate（owner）**：NUC10 升 serve（discovery 开）；笔记本存量 ②a 配置先迁桥；干净目录真跑 `ssh-manager pair` → TUI 批准 → SAS 双屏比对 → agent 在线/断网各验一次；`cache-tokens ls` 与 audit 可见配对事件。
5. ②a 负面验收：旧 ②a `.mcp.json` → agent 连接得 404 明确失败。
**批2**：
6. 手机（同 VLAN）登录 Web → 批准一次真配对 → 全流程闭环；一键双吊销生效。
7. 认证/CSRF/限速/安全头测试绿；全仓绿。
8. **v0.11.0 发布**（goreleaser 既有管线；compat-matrix breaking 行上线）。

## 8. 风险与备选

- **R1 存量迁移破坏面**：②a client 在 serve 升级瞬间断供——升级顺序铁律文档化（验收 4/5 覆盖）；不做自动迁移工具（用户 = owner 本人，清单化）。
- **R2 SAS 人因**：`--assume-sas` 显式标注测试专用；人工路径无条件肉眼比对。
- **R3 client 防火墙拦广播回包**：真机验收覆盖 Windows 笔记本；失败即走 `--url` 兜底（不依赖 discovery）。
- **R4 server 防火墙拦 UDP 7878**：安装文档一句（如 `ufw allow 7878/udp`）；同样有 `--url` 兜底。
- **R5 自签证书手机警告 habituation**：登记；DNS-01 真证书为远期可选。
- **R6 Web 面扩大攻击面**：同 VLAN + argon2id + 限速 + CSRF + CSP + no-store；无公网暴露。
- **R7 probeServeHTTP 共享 seam**：与 Plan 43 R7 双登记，任一 plan 先落地必须携带。
- **R8 pending 队列内存态**：serve 重启丢 pending，client 重跑 pair 即可——简单优先，不做持久化。
- **R9（备选记录）**：poll 轮询若评审嫌糙 → 长轮询/SSE；不取（复杂度不值）。

## 9. 里程碑

0. **v0.10.1**（前置，另行执行）：合 Plan 40 批1+2 + Plan 41 → 发版。
1. **Plan 42 批1**：§3 全部（TUI 批准兜底）。
2. **Plan 42 批2**：§4 全部。
3. 批2 后合发 **v0.11.0**（单次发版；breaking = ②a 移除；「手机授权」核心诉求在批2 闭环，不发半成品）。
