# serve 的 HTTP 服务、`/ui` 预留与 TUI 能力清单调研

- 对应议题：GitHub issue #34（父议题 #31，阻塞 #38）
- 范围：只陈述仓库内事实，附文件路径与行号，不做取舍
- 调研基线：起点提交 `ad4cfde`（2026-10-30），行号以该提交为准
- `docs/superpowers/specs/*.rev4.md` 为 Plan 42 规格的最终修订

---

## 1. 权威端 `sshmgr serve` 现有 HTTP 服务

### 1.1 路由清单（全部在 `ServeRunner.HTTPHandler`，`internal/mcpserver/serve.go:191-219`）

| 路径 | 方法 | 鉴权 | 说明 | 位置 |
|---|---|---|---|---|
| `/snapshot` | GET（其他方法 405） | 设备码（Bearer）。校验函数 `verifyCacheToken`，底层 `store.VerifyCacheToken` | 返回设备码绑定 profile 范围内的授权快照（服务器、凭据、授权、同 profile 项目、主机密钥），不含审计行。设备码未绑定 profile 返回 403（刻意不用 401，因为 401 会触发客户端缓存隔离销毁）。响应头 `X-Sshmgr-Snapshot-Scope`、`X-Sshmgr-Device-Name`，`Cache-Control: no-store, no-cache` | `serve.go:148-185, 198-201, 233-289` |
| `/pin-hostkey` | POST | 同一个设备码鉴权实例 `cacheAuth`（Plan 48） | 设备转发主机密钥锚点；请求体上限 64 KiB；成功 201，已存在 409，目标未授权 403 | `serve.go:194-205, 291-544` |
| `/server-metadata` | POST | 同一个 `cacheAuth`（Plan 51）；另受开关 `serve.metadata_edit` 控制，关闭时拒绝 | 客户端编辑服务器结构化备注字段 | `serve.go:206-212`；`serve_meta.go:82, 161` |
| `/pair/enroll`、`/pair/poll`、`/pair/finish` | POST（请求体上限 1 KiB） | 无 HTTP 层鉴权（未认证面），靠逐 IP 限速、待批队列配额、人工批准（SAS 比对）自我把关 | 配对三步。开关 `pairing` 关闭时整个 `/pair/*` 返回 404 | `serve.go:213-216`；`pairserve.go:149-194` |
| 其余任意路径 | 任意 | 无 | 一律 `http.NotFound`（404） | `serve.go:217` |

补充事实：

- `HTTPHandler` 里没有 `http.ServeMux`，是单个 `http.HandlerFunc` 按 `req.URL.Path` 手工分派（`serve.go:197-218`）。仓库内只有一处构造 `http.Server`（`serve.go:676`）。
- 在非测试 Go 代码中搜索 `HandleFunc`、`NewServeMux`、`ListenAndServe` 均无命中（仅用 `srv.ServeTLS`）。
- Plan 42 批1 删除了旧的 MCP-over-HTTP 路由（旧称 ②a）；README 与 `serve.go` 注释都写明现在只剩 `/snapshot`、`/pair`（`README.md:213, 220`；`serve.go:44-48, 187-190`）。后续 Plan 48、Plan 51 又加了 `/pin-hostkey`、`/server-metadata`，沿用同一个设备码鉴权，代码注释称"零新增认证形式"（`serve.go:194-195, 206-207`）。
- `internal/cli/serve_service.go:99-102` 的服务描述与 `internal/cli/serve.go:35-45` 的帮助文本仍只写 `/snapshot` + `/pair`，未列出 `/pin-hostkey`、`/server-metadata`。
- `ServeRunner` 里还残留一个按项目缓存 MCP server 的结构（`ServerForProject`），源码注释称它已无 HTTP 调用方（`serve.go:44-48, 120-135`）。

### 1.2 监听方式

- 入口 `RunServe`（`serve.go:577-698`）：`net.Listen("tcp", addr)`（`serve.go:620`），随后 `srv.ServeTLS(ln, tlsCert, tlsKey)`（`serve.go:681`）。
- `--addr` 默认 `127.0.0.1:7878`（`internal/cli/serve.go:106`）。多机用法需显式写 `0.0.0.0:7878` 或局域网 IP（同处帮助文本）。
- 同一进程还会启动：
  - UDP 发现应答，端口常量 `discoveryPort = 7878`（`internal/mcpserver/discovery.go:43`；`serve.go:636-655`），受 `discovery` 开关控制。
  - 每分钟一次的心跳日志（`serve.go:657-674`）。
- 进程为前台阻塞运行，直到 `ctx` 取消；`serve install` 通过 kardianos/service 注册为系统服务（Windows 服务 / systemd / launchd），`serve uninstall`、`serve status` 配套（`internal/cli/serve_service.go:239-290, 372, 432`）。

### 1.3 TLS 与证书指纹钉死

- TLS 永远开启，没有明文路径。未传 `--tls-cert` 时调用 `LoadOrCreateServeCert` 自动生成并加载自签证书；失败则 serve 拒绝启动，不降级（`serve.go:597-609`；`internal/mcpserver/cert.go:59-120`）。
- 自动证书：ed25519 自签，有效期 100 年，SAN 含主机名与本机非回环 IP，文件权限 0600（`cert.go:213-251, 269`）。
- 指纹口径是证书公钥信息（SPKI）的 sha256，不是整张证书；重签同一把密钥，指纹不变（`cert.go:27-40`）。格式 `sha256:<64hex>`（`cert.go:40`）。
- 获取指纹：`sshmgr serve cert-info`（首次运行会生成证书）（`internal/cli/serve.go:131-`）。
- 客户端钉死（`internal/clientops/pin.go:50-75`）：`InsecureSkipVerify: true` 跳过证书链校验，再在 `VerifyConnection` 中用 `SPKIFingerprint` 与期望值做常量时间比较；最低 TLS 1.3。指纹来源是 `--pin`、环境变量 `SSHMGR_SERVE_PIN`，或设备码字符串里内嵌的 `<code>:<pin>`（`pin.go:27, 84`）。
- 配对客户端 `internal/clientops/pairsession.go:168` 同样 `InsecureSkipVerify: true`，信任锚来自带外传入的 pin 或发现应答里的 SPKI；`pair --url` 不带 `--pin` 默认拒绝（TOFU），需显式 `--allow-tofu`（`docs/tui-multi-machine.md:271`）。
- 配对签名：serve 证书的 ed25519 私钥签配对转录，SPKI 指纹放进密封信封，客户端钉同一把密钥（`serve.go:67-73, 611-618`）。
- 浏览器端没有任何证书锚定机制。规格 Plan 42 §4.2 R5 已把这点登记为"无锚定的中间人面"，原文缓解措施见 2.2。

### 1.4 三态开关（影响各路由是否可达）

- 已实现的开关：`pairing`、`discovery`、`metadata_edit`。
  - 存储键：`serve.pairing`、`serve.discovery`、`serve.metadata_edit`（`internal/mcpserver/switches.go:25-29`）。
  - 环境变量：`SSHMGR_SERVE_PAIRING`、`SSHMGR_SERVE_DISCOVERY`、`SSHMGR_SERVE_METADATA_EDIT`（`switches.go:37-39`）。
  - 命令行 flag：`--pairing`、`--discovery`、`--metadata-edit`（`internal/cli/serve.go:107-113`）。
  - 优先级：显式环境变量 > 显式 flag > 存储设置 > 默认开；存储变更约 5 秒内生效，环境变量与 flag 变更需重启（`switches.go:144-206`；规格 §1-8）。
- 没有 `web` 开关：代码中不存在 `serve.web`、`SSHMGR_SERVE_WEB`、`--web`。
- 全仓 `SetSetting(` 只在 `internal/store/settings.go:28` 定义，非测试代码里没有调用方；`serve.*` 与 `pair.default_profile` 等设置键目前只有读取方（`switches.go:206`、`internal/tui/app.go:166`），仓库里没有写入它们的命令或界面。

### 1.5 各路由的鉴权方式汇总

| 面 | 凭据 | 备注 |
|---|---|---|
| `/snapshot`、`/pin-hostkey`、`/server-metadata` | 设备码，经 `auth.RequireBearerToken`（MCP SDK）包装 `verifyCacheToken` | `/snapshot` 的 401 触发客户端缓存隔离（Plan 34）；`/pin-hostkey` 的 401 刻意不触发（`serve.go:336-338`）。设备码无真实过期，只靠吊销（`serve.go:30-32, 177-184`） |
| `/pair/*` | 无 | 逐 IP 限速（环境变量 `SSHMGR_SERVE_PAIR_ENROLL_PER_MIN` 等）+ 待批队列配额 + 批准者人工核对短认证串（SAS） |
| `/ui` | 不存在 | 见第 2 节 |

---

## 2. Plan 42 第二批规划的 `/ui`：文档与代码里的预留与约束

### 2.1 代码层：没有任何实现

- 非测试 Go 代码中没有 `/ui` 路由、`admin_users`、`admin_sessions`、`serve admin` 命令、`web` 开关。`HTTPHandler` 对未知路径统一 404（`serve.go:217`）。
- 代码里与第二批相关的只有文字预留：
  - `internal/cli/serve.go:37-41`：帮助文本写"in a later release — the web admin UI"。
  - `README.md:213`：`The serve is narrowed to: authoritative vault + /snapshot + /pair (+ web admin UI in a later release)`。
- `docs/threat-model.md:61`："批1 无 Web 面——`/ui`、admin 认证、浏览器端证书锚定问题（R5）随批2 落地时再补 Web 节"。

### 2.2 规格层：`docs/superpowers/specs/2026-08-28-plan-42-usability-overhaul-design.md.rev4.md`

| 主题 | 规格原文要点 | 行号 |
|---|---|---|
| 目标 | Web 管理 UI（手机优先）：`go:embed` 单二进制、零 npm、零 CDN；完整管理 + 配对批准 + 吊销 + 审计 | 14 |
| 依赖事实 | 仓库零前端工具链（`package.json` 为空）→ Web UI 必须 `go:embed` 自包含 | 26 |
| serve 职责 | serve 收窄为四件事：权威 vault + `/snapshot` + `/pair` + （批2）`/ui` | 36 |
| 三开关 | `pairing` / `discovery` /（批2）`web`，env 与 flag 均三态，优先级显式 env > 显式 flag > store > 默认 true | 47 |
| 默认契约 | web（批2）= 认证 + 开关 | 49 |
| 配对批准者 | 批准者包含 broker TUI（批1）、Web UI（批2）、`serve pair` CLI（兜底） | 61 |
| 架构 | `/ui` 路由组挂 serve mux；`go:embed` + 服务端渲染（`html/template`）；手机优先响应式；同 VLAN | 110 |
| 开关 | `--web`（三态）+ env `SSHMGR_SERVE_WEB`；关闭时 404 | 111 |
| 无 admin 用户 | 全路由 403 + 提示页「在 broker 上运行 `ssh-manager serve admin set`」；无任何未认证首设端点 | 112 |
| 页面清单 | 登录 / 总览 / Servers / Profiles / Projects / Instances / Pairing（列表页 5 秒刷新；批准详情页含三件套 + 机械校验 ⚠ + 选 profile + OVERRIDE 输入）/ Audit / Settings（admin 密码、默认 profile、默认 max_offline、三开关含覆盖状态） | 113 |
| TLS 缓解 | 全页面脚注常显证书 SPKI 指纹；Servers 凭据写入表单默认折叠；首登引导要求先在 broker TUI 核对指纹 | 114 |
| 认证 | `serve admin set` 首设；argon2id（m=64MB, t=3, p=4）存 `admin_users`；TUI 可重置（全量失效） | 118 |
| 会话 | store 表 `admin_sessions`（存 id 的 sha256、绝对/滑动过期、创建 IP）；登录限速 5 次/15 分钟（IP+账号）；cookie `HttpOnly+Secure+SameSite=Strict`；12 小时滑动 + 7 天绝对上限；改密/重置全量失效；CSRF 会话 token；`CSP: default-src 'none'; style-src 'self'`；全响应 no-store | 119 |
| 审计覆盖 | 登录成败、各类写、token 生命周期、配对全事件、admin/settings 修改；同事务纪律 + 脱敏白名单（§3.3-8） | 120 |
| 风险 R5 | Web 面自签证书无浏览器端锚定，首连/换证时局域网中间人可截获 admin 密码；缓解 = 指纹页脚 + 首登 TUI 核对 + 凭据表单折叠 + 登录限速/审计；残余接受；DNS-01 真证书为远期根本解 | 121, 176, 203 |
| 配对批准（Web） | 同表、同 CAS UPDATE、同机械地址校验（⚠ + OVERRIDE）；三件套与 TUI/CLI 同行格式 | 123-125 |
| 勘误 | 批准界面（TUI/CLI/批2 Web）直读 `pairing_pending.sas` 同屏显示 SAS | 69-70 |
| 批2 测试 | argon2id / 限速 / session 表 / CSRF / CSP+no-store / `--web` 三态；页面 smoke；一次性 token 仅签发响应；Web 写操作 audit 全覆盖 + 脱敏 | 148-153 |
| 批2 验收 | 手机登录 Web → 批准真配对 → 闭环；一键双吊销 | 166-167 |
| R6 | Web 攻击面：同 VLAN + 认证纪律 + 三态开关 + 无 admin 全 403 | 177 |

### 2.3 其他文档中的 `/ui` 表述

- `docs/deployment-modes.md:65`（写边界：管理面 = broker TUI / `serve pair` CLI / 批2 Web UI）、`:82`（拓扑图"批2：Web UI，手机可批"）、`:97`（serve 收窄为四件事，含批2 `/ui`）、`:112`（表格行"Web UI（serve `/ui`，go:embed 单二进制，手机优先）… 批2"）、`:149, 157, 160`（手机/平板的角色是批2 Web 管理面；批1 阶段手机想批准只能在 broker 机用 TUI 或 `serve pair`）。
- `docs/multi-machine.md:7, 47, 240, 261, 263, 435, 556`：同口径，进度表标"批1 已做 / 批2 待做"。
- `docs/superpowers/plans/2026-08-28-plan-42-batch1-usability.md:22`：批1 范围明确不做 Web UI（`/ui`、`admin_users`、argon2 均为批2）。
- `docs/adr/0005-audited-metadata-writes.md:10`：Web 管理界面（Plan 42 批2）"被否/延后"，理由是入口在 agent 最彻底，人工路径批2 补 CLI/TUI。
- `docs/threat-model.md:59`：审计白名单写明"Web 访问日志同纪律，批2 生效"。
- `CONTEXT.md` 与 `docs/backlog.md` 中没有检索到与 `/ui` 或 Web UI 直接相关的条目。
- 全仓未发现独立的"Plan 42 批2"实施计划文件（`docs/superpowers/plans/` 下只有 `2026-08-28-plan-42-batch1-usability.md`）。

---

## 3. 单机部署下的常驻进程

事实：

- 单机形态（部署形态 ①）文档明确写"不需要任何常驻服务、不需要网络监听"，broker 在 agent 启动的 MCP server 进程里运行（`docs/quickstart-single-machine.md:11`）。
- 单机的进程形态只有：
  - AI 宿主按需拉起的 stdio MCP 进程（`sshmgr mcp`，`internal/cli/mcp.go`）。多机工作机用 `mcp --cache` 的只读桥，v0.20.0 起支持空闲时热升级换代（`docs/deployment-modes.md` 升级节；`docs/adr/0006-bridge-hot-upgrade-self-demote-pump.md`）。
  - 人工启动的命令行与 TUI（`sshmgr tui` 需要真终端，非 TTY 直接报错，`internal/tui/mode.go:48-51`）。
- 唯一的常驻进程是 `sshmgr serve`（前台运行，或经 `serve install` 注册为系统服务，开机自启、自动重启，`internal/cli/serve_service.go:239-240`）。它属于多机部署的权威端；`docs/quickstart-multi-machine.md:52` 写"想让它开机自启：`sshmgr serve install`"。
- 单机机器可以升级为 server：TUI 的 `[u]`（仅 `RoleStandalone` 响应，`internal/tui/app.go:275-280`）走升级段：选局域网地址、管理员提示、安装服务、探活、配对入网卡（`internal/tui/upgrade.go:1-40`）。该升级不改动已有 vault 数据，只写服务注册、serve 证书和角色文件。
- 不升级为 server 时，单机上没有任何常驻进程可承载 HTTP 页面。`sshmgr serve` 是仓库里唯一的 HTTP 监听进程，且与 CLI/TUI 共用同一个 `store.db`：TUI 配对页的刷新键 `r` 注释写明待批队列由 serve 进程从外部写入（`internal/tui/app.go:487`），`serve pair` CLI 也是"跨进程直连 store"（`internal/cli/serve_pair.go:3`）。
- `serve` 前台启动时用 `store.FileKeyProvider` 打开 vault，并调用 `EnsureSidecarACL`（`internal/cli/serve.go` 前台分支，`:55-71` 一带）。

---

## 4. TUI 能力清单与命令行等价面

### 4.1 约束来源

`docs/adr/0003-tui-cli-parity-agent-testable.md`：任何 TUI 能力落地时必须同时存在命令行等价面；需跳过人工信任动作的自动化，只能走显式环境变量接口（前例 `SSHMGR_PAIR_ASSUME_SAS=1`，`internal/cli/pair.go:30` 一带）。TUI 特有的按键链、overlay 门与观感不在该约束内。

### 4.2 启动分派

- `sshmgr tui [--mode client]`（`internal/cli/tui.go`）。
- 由 `roles.ResolveMode` 决定：首次或未完成设置 → 角色向导；broker（含单机 standalone、server）→ 主控台 `App`（7 页）；client → 客户端面板（`internal/tui/mode.go:28-47`，`launchTarget`）。
- 通用键（主控台）：`Tab` / `Shift+Tab` 切页，切页即异步重读 store；`↑↓` / `j k` 移动；列表内 `/` 过滤；`q` 或 `Ctrl+C` 退出；表单内 `Esc` 取消（`internal/tui/app.go:250-283`，`internal/tui/panels.go:5-24`）。

### 4.3 主控台（broker / standalone / server）逐页能力表

页面常量见 `internal/tui/app.go:21-31`（共 7 页：Servers、Profiles、Projects、设备码、Pairing、备份、命令策略）。

| 页面 | 键 | 动作 | CLI 等价命令 | 备注 / 缺口 |
|---|---|---|---|---|
| 服务器 | `a` | 新增服务器（表单；密码/私钥互斥，可都不填 = 无凭据 ⚠） | `servers add --name --host --port --user [--password \| --key --key-passphrase] [--sudo-password] [--tags --description --location --hardware --services --role --special-handling --expose-host --shell]`（`internal/cli/servers.go:60, 138-154`） | 等价 |
| 服务器 | `e` | 逐字段挑选编辑；可「清除凭据」 | `servers edit [name]` + 同名 flag，另有 `--clear-credential`、`--password`、`--key`、`--sudo-password`（`servers.go:343, 516-533`） | 等价 |
| 服务器 | `d` | 删除（确认弹窗）；级联删除独占凭据 | `servers rm [name-or-id]`（`servers.go:281`，同样走级联删除） | 等价；CLI 无确认弹窗 |
| 服务器 | `i` | 导入 ssh config（路径 → 多选 → 批量导入 → 逐台补全） | `servers import [--file] [--dry-run] [--profile]`（`internal/cli/servers_import.go:32, 64-66`） | CLI 无"逐台补全循环"；补缺口令/sudo 密码需另用 `servers edit` |
| 服务器 | `!` | 只看 ⚠ 待处理机器（无凭据 / 未填角色 / 缺私钥口令） | 无。`servers ls` 不输出 ⚠ 状态，也无过滤参数（`servers.go:236-262`） | 缺口 |
| 服务器 | `p` | 探测当前服务器实际 shell 类型并在状态行报告 | `servers probe-shell [name...]`（`servers.go:169`；`app.go:757` 注释明写其为 CLI 等价面） | 等价 |
| 服务器 | `/` | 列表文本过滤 | 无专用参数；`servers ls` 全量输出 | 视图能力，无命令 |
| Profiles | `a` | 新增 profile | `profiles add [name]`（`internal/cli/profiles.go:77`） | 等价 |
| Profiles | `g` | 授权服务器（多选，预选已授权，提交为全量同步，取消勾选即移除；`store.SyncServers`，`internal/store/profiles.go:162-199`） | `profiles grant [profile] [server...]`（`profiles.go:129`）只调用 `store.GrantServers`（`internal/store/profiles.go:107`），仅插入、不移除 | 缺口：命令行没有撤销授权的子命令（`profiles` 子命令仅 add / ls / grant / remove） |
| Profiles | `d` | 删除（被项目引用时拒绝） | `profiles remove [name]`（`profiles.go:36`） | 等价 |
| Projects | `a` | 新建项目（绑定 profile，token 一次性全屏显示，含可抄 `.mcp.json` 片段） | `projects add [name] --profile`（`internal/cli/projects.go:63, 85`；输出 `Token (shown once)` 与 `.mcp.json snippet`，`projects.go:285-287`） | 等价 |
| Projects | `e` | 轮换 token（确认；旧 token 立即失效） | `projects rotate [name]`（`projects.go:177`） | 等价 |
| Projects | `d` | 吊销（永久） | `projects revoke`（`projects.go:19-23`） | 等价 |
| Projects | `x` | 彻底删除已吊销记录 | `projects remove [name]`（`projects.go:27-58`） | 等价 |
| Projects | 无按键 | TUI 没有暂停/恢复项目 | `projects disable` / `projects enable` / `projects show` / `projects ls --all` | CLI 比 TUI 多 |
| 设备码 | `a` | 签发设备码（命名 + 绑定 profile + 服务地址提示，一次性全屏显示设备码、指纹与 `cache pull` 示例） | `cache-tokens add --name --profile`（`internal/cli/cache_tokens.go:39, 82-84`） | 等价；TUI 先加载 serve 证书指纹再签发（`app.go:447-458`） |
| 设备码 | `d` | 吊销（确认；下次拉取被拒） | `cache-tokens revoke [name]`（`cache_tokens.go:158`） | 等价 |
| 设备码 | 无按键 | TUI 无绑定/重绑 profile | `cache-tokens bind [name] [profile]`、`cache-tokens ls` | CLI 比 TUI 多 |
| Pairing | `r` | 刷新待批队列 | `serve pair ls`（`internal/cli/serve_pair.go:92`） | 等价 |
| Pairing | `a` | 批准（选 profile，默认值 `pair.default_profile`；目标地址不属于本机时需键入 `OVERRIDE`；同屏显示 name、target、SAS、来源 IP、hint、剩余秒） | `serve pair approve <name\|idHex> --profile <p> [--allow-foreign-url]`（`serve_pair.go:142, 183-185`） | 等价（`OVERRIDE` 对应 `--allow-foreign-url`） |
| Pairing | `d` | 拒绝（确认） | `serve pair reject <name\|idHex>`（`serve_pair.go:191`） | 等价 |
| 备份 | 进入即显示 | 状态区：配置摘要、`*.sme` 备份列表（最新在前，25 小时新鲜度警告）、计划任务只读探测（`internal/tui/backuppage.go:1-12`） | 无 `backup ls` / `backup status`（`internal/cli/backup.go` 仅 `create`、`verify`） | 缺口（状态展示） |
| 备份 | `b` | 立即备份 | `backup create [--config] [--dir] [--passphrase-file] [--keep] [--prefix]`（`backup.go:102, 137-141`） | 等价（与 CLI 共用同一条代码链，`backuppage.go:4-9`） |
| 备份 | `v` | 校验最新一份备份 | `backup verify <file>`（`backup.go:37`） | CLI 必须给出文件路径，没有"最新一份"的快捷 |
| 备份 | `e` | 编辑 `backup.json`（`dir` / `keep` / `passphrase_file`） | 无命令；配置是 JSON 文件，`backup create --config` 只读取它（`internal/cli/backupconfig.go:14-30`） | 缺口（写配置） |
| 命令策略 | `a` | 增加自定义规则 | `policy add --id --disposition block\|confirm [--shell ...] [--command ...] [--arg ...] [--path-category] [--regex] --why [--instead]`（`internal/cli/policy.go:271`） | 等价 |
| 命令策略 | `d` | 删除自定义规则（默认规则不可删） | `policy rm <id>`（`policy.go:336`） | 等价 |
| 命令策略 | `s` | 停用 / 恢复默认规则（停用需确认，写审计） | `policy disable <id>` / `policy enable <id>`（`policy.go:358, 384`） | 等价 |
| 命令策略 | `t` | 试算命令（用权威库有效规则集） | `policy test [--shell] [--instance] <command>`（`policy.go:414`） | 等价 |
| 命令策略 | 页面列表 | 显示规则列表 | `policy ls [--instance]`（`policy.go:191`） | 等价 |
| 全局 | `u`（仅 standalone） | 单机升级为 server：地址选择 → 管理员提示 → 安装服务 → 探活 → 配对卡 | `serve install`（`serve_service.go:239`）；角色文件 `role.json` 由 TUI 内 `roles.Save` 写入（`internal/tui/upgrade.go:196`） | 部分：安装服务有命令；写角色文件无命令 |

### 4.4 客户端面板（client 角色）能力表

入口：`internal/tui/clientpage.go:340-382`。面板展示当前实例槽的离线缓存内容（只读列表），无任何写入口。

| 键 | 动作 | CLI 等价命令 | 备注 |
|---|---|---|---|
| `s` | 同步（对当前选中实例槽手动拉取；10 秒超时；失败保留旧缓存；永不走明文拉取） | `cache pull`（`internal/cli/cache.go:44`，支持 `--instance`） | 等价 |
| `i` | 打开实例选择器（单槽覆盖环境变量时禁用） | `cache instances ls`（`internal/cli/cache_instances.go:51`） | 等价（展示四要素状态、缓存年龄） |
| 选择器 `↑↓` / `j k`、`Enter` | 选中并切换当前槽位 | 命令行用 `--instance <name>` 指定实例 | 切换是会话内 UI 状态，无独立命令 |
| 选择器 `p` | 对具名实例强制重新配对（Plan 46 扩到残缺槽，`internal/tui/instancepicker.go:215-225`） | `pair --instance <name> --force`（`internal/cli/pair.go:26`） | 等价 |
| 选择器 `d` | 删除具名实例（确认；删除槽目录与 DEK，`instancepicker.go:226-235`） | `cache instances rm <实例名> [--yes]`（`cache_instances.go:213`） | 等价；默认槽不可在此删除（整机清理用 `clear`） |
| 选择器 `Esc` / `q` | 关闭 | 无 | UI 动作 |
| `c` | 打开 SAS 配对向导（表单 → 发现 → SAS 常显等待 → 批准后完成 → 写入 → 自动换槽） | `pair [--instance] [--url] [--pin] [--allow-tofu] [--force]`（`pair.go:26`）；无人值守用 `SSHMGR_PAIR_ASSUME_SAS=1` | 等价 |
| `p` | 命令策略只读查看器（读当前实例缓存快照，含试算，`clientpage.go:371-376`） | `policy ls --instance <name>`、`policy test --instance <name> <command>`（`policy.go:191, 414`） | 等价 |
| `t` | 仅在状态行提示 TTL 由 `--cache-max-age` 控制，无实际动作（`clientpage.go:377-379`） | `cache config`（显示或设置每实例离线上限，`cache.go:322`） | 提示键 |
| `q` / `Ctrl+C` | 退出 | 无 | UI 动作 |

### 4.5 首次运行向导（`internal/tui/wizard.go`、`wizardsteps.go`、`wizardserve.go`）

向导先让用户选角色，并立即写 `role.json`（`setup_complete:false`，防死态）；中断可续配（`wizard.go:1-9`）。

| 流程 | 步骤 | CLI 等价 |
|---|---|---|
| 单机（standalone） | 初始化 vault → 录入服务器（循环，可跳过）→ profile + 授权 → 项目 → token 一次性屏 → `.mcp.json` 收尾屏 → 进入主控台（`wizard.go:42-71`） | `servers add`、`profiles add` / `profiles grant`、`projects add --profile`；解锁见 `unlock`（`internal/cli/unlock.go:42`）。未找到能写 `role.json` 的命令 |
| server | 同上 ① 至 ④ + 绑定 profile（多 profile 续配）→ 局域网地址选择 → 管理员提示 → 安装服务 → 探活 → 结果横幅 → 配对入网卡 | 同上 + `serve install`、`serve status`、`serve cert-info` |
| client | 静态指引页，指向 `sshmgr pair`（Plan 42 批1 T8 起；连接表单已删除，`wizard.go:15-18`） | `pair` |

### 4.6 缺口汇总（TUI 有、命令行无直接对应）

1. Profiles 页 `g` 的"取消勾选即移除授权"：`profiles grant` 只增不减，命令行没有撤销授权的子命令（`internal/cli/profiles.go` 子命令：add / ls / grant / remove；`internal/store/profiles.go:107` 与 `:168` 对比）。
2. 服务器页 `!`（⚠ 待处理视图）与 `/` 过滤：`servers ls` 不输出 ⚠ 状态，也无过滤参数（`internal/cli/servers.go:236-262`）。
3. 备份页状态区（备份列表、新鲜度警告、计划任务探测）：无 `backup ls` / `backup status`。
4. 备份页 `e`（编辑 `backup.json`）：无写配置的命令。
5. 备份页 `v` 的"校验最新一份"：CLI `backup verify` 必须给文件路径。
6. 导入 ssh config 的"逐台补全循环"：`servers import` 之后需手动对每台 `servers edit`。
7. `role.json` 的写入（向导选角色、单机升级 `[u]`）：`roles.Save` 的调用方只有 `internal/tui/wizard.go:160`、`wizardsteps.go:283`、`upgrade.go:196`，没有命令行调用方。
8. 设置存储（`serve.pairing`、`serve.discovery`、`serve.metadata_edit`、`pair.default_profile` 等）：`SetSetting` 无非测试调用方（见 1.4），TUI 与 CLI 都没有写入入口；`pairing` / `discovery` / `metadata_edit` 只能靠 flag 或环境变量。
9. 反向差异（命令行有、TUI 无）：`audit`（`internal/cli/audit.go:32`，主控台没有审计页；规格 §4.1 的 Web 页面清单包含 Audit）、`projects disable/enable/show`、`cache-tokens bind/ls`。

### 4.7 TUI 之外、仅命令行有的命令（供对照）

`audit`、`export`、`import`、`gc`、`doctor`、`lock`、`unlock`、`update`、`clear`、`migrate-path`、`pin-hostkey`、`tunnels ls/kill`、`ssh <host> [cmd...]`、`cache status`、`cache config`、`serve uninstall`。清单取自 `internal/cli/*.go` 的 `Use:` 字段。

---

## 5. 仓库内嵌入静态资源或模板的先例

- 在全部 `.go` 文件中检索 `go:embed`、`"embed"` 导入、`html/template`、`text/template`：均无命中（`grep -rnE 'go:embed|html/template|text/template|"embed"' --include='*.go' .`）。
- 仅有的文字出现：`docs/deployment-modes.md:112` 和规格 `rev4.md:14, 26, 110` 把 `go:embed` 写成将来的设计要求。
- 版本控制中的非 Go、非 `docs/` 文件：`.github/workflows/*.yml`、`.goreleaser.yml`、`.gitignore`、`.gitattributes`、`.scratch/**/*.md`、`internal/secrethint/testdata/corpus-legal.txt`（测试语料）。没有 `package.json`、`.html`、`.css`、`.js`、`.tmpl`（`git ls-files` 过滤结果）。
- 依赖：界面库全是终端界面栈（`go.mod`：`charm.land/bubbletea/v2`、`bubbles/v2`、`huh/v2`、`lipgloss/v2`）；`go 1.25.8`；命令行框架 `cobra`；服务注册 `kardianos/service`（`go.mod:3-17`）。
- 文本屏由代码拼接字符串生成（非 embed、非 template），例如 TUI 的 `.mcp.json` 片段与设备码展示屏（`internal/tui/tokenview.go`、`wizardsteps.go`）。

---

## 6. 结论摘要

1. serve 现有 HTTP 面：`/snapshot`、`/pin-hostkey`、`/server-metadata` 走设备码 Bearer 鉴权，`/pair/enroll|poll|finish` 无 HTTP 鉴权（限速 + 人工 SAS 批准），其余 404。单个 `net.Listen` + `ServeTLS`，默认 `127.0.0.1:7878`，TLS 永远开启，客户端以证书公钥 sha256 指纹钉死。`/ui` 在代码里完全不存在，只在规格与文档里有预留：`go:embed` + `html/template`、`--web` 三态开关、admin 账号 argon2id、session 表、CSRF/CSP、R5（浏览器端无证书锚定）风险登记。
2. 单机部署没有常驻进程：只有 AI 宿主按需拉起的 stdio 桥与 CLI/TUI。唯一常驻进程是多机用的 `sshmgr serve`（可 `serve install` 注册为系统服务），单机可经 TUI `[u]` 升级成 server。
3. TUI 主控台 7 页 + 客户端面板 + 向导的每个按键已列表。绝大多数有 CLI 等价，缺口见 4.6：撤销授权、服务器 ⚠ 视图/过滤、备份页状态区与配置编辑、"校验最新备份"、导入后逐台补全、`role.json` 写入、设置存储写入；反向地，`audit`、`projects disable/enable`、`cache-tokens bind` 只有命令行。
4. 没有任何 `go:embed` 或模板先例，也没有前端工具链或静态资源文件。
