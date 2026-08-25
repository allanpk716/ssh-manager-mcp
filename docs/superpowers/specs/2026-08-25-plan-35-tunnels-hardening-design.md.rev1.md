# Plan 35 · tunnels 硬化（急停 + 活动感知回收 + listen_host 白名单）设计 spec · rev1

- 日期：2026-08-25 · 状态：rev1（v1 原稿评审 13 条全证实后修订；待复审）
- 原稿：`2026-08-25-plan-35-tunnels-hardening-design.md`（不动）。rev1 变更（对应原稿评审 13 条必改）：
  1. **认领协议重构**（§4）：project 单幂等扇出、执行先于标记、outcome 过滤、tunnel 单「全局缺席即达成」、orders 终结行 GC；
  2. **IP 规范化**（§2）：三处统一规范形比对 + zone 拒；
  3. **registry 写失败 = fail-the-Open**（§6）：不可 kill 的隧道不允许存在；
  4. **bind rm 存量复查关闭**（§4 控制周期职责）：撤回 ≤tick 生效；
  5. 陈旧度契约改 ≤45s（§6）、控制周期瞬时错误钉死（§4）、Touch 并发安全显式钉住（§3）、测试矩阵补 7 组场景（§8）、compat 补 audit 字段行（§10）。
- backlog：P1 #15（吸收老 #1/#2/#4；「不做」清单原文沿用：持久化、自动重连、命名隧道）
- Owner 拍板记录（2026-08-25）：
  1. **急停通道 = vault DB kill 单**（备选 serve loopback admin 端点 / 双轨均否）——复用 master-key 既有通道，零新网络面，serve + 在线 stdio 统一覆盖；
  2. **revoke 级联只拆隧道**——后台任务契约不动（Plan 32 钉住：孤儿任务无读者、24h 封顶 runoff）；
  3. **listen_host 白名单 = vault DB 表 + owner CLI**（备选 serve 启动 flag / 砍掉均否）——per-call 读，改白名单免重启即生效。

---

## §0 背景与现状

三个现状缺口（均已在代码/文档中钉住）：

1. **无急停**：隧道由 broker 进程持有（serve 每 project 一个 `TunnelManager`，stdio 独立进程）；revoke 后逐请求 401 挡住新动作，但**已建立的隧道继续转发**（`revoke_semantics_test.go` 钉住，注释明示"owner decision, kill CLI is backlog"）。`docs/agent-access.md` 断连语义第 3 层原文：真实选项只有重启 broker 或等 ~10 分钟回收。
2. **回收按创建时间**：`managedTunnel.lastActivity` 字段与 `TunnelManager.Touch(id)` 已存在但**零生产调用方**（`tunnels.go:10-16` 注释明示）——隧道创建 10 min 后必死，持续流量也救不了。
3. **非环回 bind 不存在**：`Client.ForwardLocal` 硬编码 `127.0.0.1`（`tunnel.go:68`）。NUC10 serve 拓扑下想让 VLAN 内其他机器用隧道（bind 到 VLAN IP）无路径；而直接放开 per-tunnel bind = 威胁模型 (b)（被劫持 agent 开隧道打内网）的攻击面扩张。

目标：给 owner 三件控制力——①急停（kill 单 + revoke/disable 级联）；②活动感知回收；③非环回 bind 的 owner 预批白名单放行。

验收（backlog #15 原文）：白名单外 bind 拒绝；急停后端口不可达；持续流量下不回收。

## §1 契约翻转（本 plan 的声明性核心）

`docs/agent-access.md` 断连语义第 3 层翻转为：

> **已建立的 `forward_port` 隧道——revoke/disable 后 ≤ 控制轮询间隔（~15s）内拆除；owner 随时 `tunnels kill <id>` / `tunnels kill --project <name>` 拆除；owner 撤回 bind 白名单条目后 ≤ 控制轮询间隔内，绑定该地址的存量隧道关闭。**

- **翻转声明**：`revoke_semantics_test.go` 中 `TestRevokedProjectKeepsOpenTunnelForwarding` 重写为「revoke 后 ≤tick 拆除 + 端口不可达」（文件头自注"契约变更须经由改测试显式声明"——本 plan 即声明）；`TestServeHTTPRejectsRevokedTokenPerRequest`（逐请求 401）不变；`TestRevokedProjectKeepsBackgroundTaskRunning`（Plan 32 钉子）**原样保持绿**。
- **disable 也拆**（不止 revoke）：disable 语义含「审查中」，威胁面同 revoke；`rotate` 不改 status、不拆。project 行被删（`GetProject` 返回 nil）按非 active 处理、拆。
- **级联触发点**：TunnelManager 控制循环（§4，15s tick）在**本 manager 有活隧道时**复查 project status → 非 active 即拆本 manager 全部隧道。serve 逐请求 401 语义不变（新动作立即断；存量隧道 ≤15s）。
- **serve 的 scopedServer cache 不驱逐**：空 manager 无害，revoked token 下一请求 401；驱逐是多余机器。
- **离线 stdio（`mcp --cache`）永不级联**：其 store 是快照水合的只读临时库，status 定格在拉取时点——离线隧道的拆法仍是四层第 4 层（回连销毁）+ 本机杀进程。登记为预期边界。

## §2 listen_host 白名单（加新能力，非收紧）

### 数据与 CLI

- 新表 `forward_bind_hosts(ip TEXT PRIMARY KEY, created_at INTEGER NOT NULL)`（schemaSQL 追加，`CREATE TABLE IF NOT EXISTS`，纯增量）。
- owner CLI（`serve` 子命令组下）：
  - `ssh-manager serve bind add <ip>` —— 校验：`net.ParseIP` 成功、**非环回**（`IsLoopback` 拒——环回恒允许不需要表项）、**非通配**（`IsUnspecified` 拒，即 `0.0.0.0`/`::`）、**无 zone identifier**（`fe80::1%eth0` 这类带接口后缀的输入 `net.ParseIP` 本就解析失败，按非法拒）；错值带原因拒绝。重复 add 幂等（INSERT OR IGNORE + 提示已存在）。
  - `ssh-manager serve bind rm <ip>` / `ssh-manager serve bind ls`
- 白名单条目 = **IP 字面量 only**：hostname/DNS 名、CIDR 网段一律拒（解析漂移与范围失控）。

### IP 规范化（rev1：三处统一）

**存取比对一律用规范形**：add 时存 `net.ParseIP(input).String()`（如 `2001:0db8::0001` 存为 `2001:db8::1`）；rm 按同规则先规范化再删（提示按规范形删）；gate 链比对用 `net.ParseIP(agent 输入).String()` 对表内规范形。三处共用一个 canonicalize 帮助函数——IPv6 同地址多文本形式在 add/rm/gate 之间不再产生不一致。

### forward_port gate 链（`ForwardForProfile`）

新输入参数 `listen_host`（可选，缺省空）：

1. 空 / 缺省 → `127.0.0.1`（现状行为，零破坏）；
2. `net.ParseIP` 失败（hostname、垃圾值、带 zone）→ 拒；`IsUnspecified`（通配）→ 拒；`IsLoopback` → **恒允许**（不需要表项）；
3. 其他非环回 IP → 规范形 **per-call 读表**（新 store 方法 `ListForwardBindHosts() ([]string, error)`，与 `VerifyToken`/`ServersForProfile` 同款 per-call 模式）：命中 → 允许；未命中 → 拒。
- 拒绝错误文本：`listen_host %q is not in the owner-approved bind host whitelist` —— **不回白名单内容**，只回 agent 自己提供的输入。
- 拒绝时 audit status 用新值 **`bind_denied`**（区别于 profile 越权的 `denied`，owner 可观测 agent 的 bind 越权尝试）。
- **存量收缩（rev1）**：gate 只管新 Open；owner `bind rm` 后，已绑定该地址的存量隧道由控制周期复查关闭（§4），**撤回 ≤tick 生效**——「改白名单免重启即生效」对新开与存量一致。

### 传导

- `Client.ForwardLocal(localPort int, listenHost, remoteHost string, remotePort int)` —— listenHost 由 gate 链产出的**已验证原输入**传入（`net.Listen("tcp", net.JoinHostPort(listenHost, port))`；IPv6 由 JoinHostPort 自动加方括号）；`localAddr` 如实回报实际绑定地址。
- `ForwardOutput` 加 `listen_host` 字段（回报实际绑定 host）；`ForwardInput` 加 `listen_host`（`omitempty`）。
- **在线 stdio 同一代码路径顺带获得能力**：表项 IP 不在本机 → bind 自然失败（无害错误）。**离线 cache 快照不含此表**：水合库中表恒空 → 离线 stdio 恒 loopback-only——`ImportSnapshot` 不导该表即机制性 fail-closed，零额外代码，写进文档。

## §3 活动感知回收（落实 Touch）

- `sshbroker.Tunnel` 加活动钩子：`ForwardLocal` 构造参数注入 `onActivity func()`，触发点两处：
  1. `serve` 的 `Accept` 成功后（每条新连接 = 活动）；
  2. `handle` 内两个方向 pipe 的**读路径**——io.Copy 包一层计数 reader，每次读到字节即上报。
- **节流**（热路径纪律）：Tunnel 侧 `atomic.Int64` 存上次触发 unixnano，间隔 **≥30s** 才真正调用回调（读路径开销 = 一次原子 load/compare，无锁、无分配）。
- manager 侧接既有 `Touch(id)` → `lastActivity = now`。**并发安全显式钉住（rev1）**：回调来自 Accept 与双向 pipe 多个 goroutine 并发触发，`Touch` 的安全性由 `TunnelManager.mu` 既有互斥保证（现状实现已持锁，`tunnels.go:114-123`；节流本身原子无锁，不放大争用）。
- `forwardIdleTimeout` 10 min 语义从「创建后 10 min 必死」变为「**真空闲 10 min**」：持续流量的隧道存活；纯空闲挂连接（如无查询的 SQL 连接）10 min 照收——与现状一致且严格更好。
- in-flight 管道 goroutine 在 listener 关闭后照常 drain 完（现状 `Tunnel.Close` 语义不变）。
- 措辞清理：`tunnels.go` 头注释 "creation-based"、`server.go` forward_port/close_port 工具描述 "~10 minutes after creation, not based on activity"、`agent-tools.md` 同步改为 activity 口径。

## §4 急停：vault DB kill 单（rev1：认领协议重构）

### 数据

新表 `tunnel_orders`：

```sql
CREATE TABLE IF NOT EXISTS tunnel_orders (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  tunnel_id TEXT,                -- 二填一（应用层 + CHECK 约束）
  project_id TEXT,
  created_at INTEGER NOT NULL,
  applied_at INTEGER,
  outcome TEXT,                  -- NULL=pending; 'applied'; 'expired'
  CHECK ((tunnel_id IS NULL) <> (project_id IS NULL))  -- 恰一非空
);
```

**状态机（rev1 重构核心）**：

- pending ⇔ `outcome IS NULL`——**pending 查询与一切标记 UPDATE 一律带 `outcome IS NULL`**（过期单不可被认领/复活/改写）。
- **tunnel 单**：拥有者执行、标记后置。完成路径：①拥有者（`tunnel_id ∈ 本 registry` 的 manager）`Close(id)`（幂等）→ ②镜像 DELETE 该行（幂等）→ ③`UPDATE ... SET applied_at=?, outcome='applied' WHERE id=? AND outcome IS NULL`。崩溃在 ③ 前 → 单仍 pending，下个 tick 幂等重走（①② 均幂等）——**不存在「标了成功但没执行」**。
- **tunnel 单的全局缺席即达成**：任何 manager 的 tick 发现 pending tunnel 单的目标在 `tunnel_registry` **无行**（含已被 sweep 正常关闭、进程崩溃残留行已被 30min GC 清掉的情形）→ 直接标 `applied`（目标不在 = 已达成）。「registry 无行 ⇔ 无活隧道」由 §6 的 fail-the-Open 保证（活隧道必有行）。
- **project 单 = 幂等扇出**：**不排他认领**。每个 manager 每 tick：单 `project_id == 本 projectID` 且本 manager 有活隧道 → 拆自己的全部（幂等，无隧道时为 no-op）。**完成标记与执行解耦**：单保持 pending，直到任一 manager 的 tick 发现 `SELECT COUNT(*) FROM tunnel_registry WHERE project_id=?` == 0 → 标 `applied`（= 该 project 的隧道在所有进程里都拆完或从未存在）。多 manager（serve + 在线 stdio 同 project）各拆各的，一个不落；死进程的残留行由 30min GC 清后单自然达成。
- **过期（无主单 GC）**：tick 顺带 `UPDATE ... SET outcome='expired' WHERE outcome IS NULL AND created_at < now-600`（幂等，任何 broker 可执行）。
- **终结行清理（rev1）**：tick 顺带 `DELETE FROM tunnel_orders WHERE outcome IS NOT NULL AND created_at < now-604800`（7 天，终结行归档删除）。

### owner CLI

- `ssh-manager tunnels ls` —— 读 `tunnel_registry`（§6）join projects：tunnel_id、project 名、server_id、remote、local_addr、listen_host、opened_at、last_activity。
- `ssh-manager tunnels kill <tunnel_id>` —— **预检**：id 不在 `tunnel_registry` → 快失败 `no open tunnel <id>`，不下单（镜像为事件驱动即时写入，见 §6；无需陈旧性软话术）。在 → 下单 → **轮询 ≤20s**：`applied`（成功，回报）/ 仍 pending（如实报：`order pending — no broker applied it within 20s (target may belong to an offline/dead process; expires in 10min)`，**不装成功**）。
- `ssh-manager tunnels kill --project <name|id>` —— 解析 project（`GetProjectByName`/`GetProject`）→ 预检 registry 无该 project 行 → 直接报 `no open tunnels for project X`（不下单）；有 → 下 project 单 → 同款等待语义。
- kill 单 = **surgical**：不动 token，project 仍 active，agent 可重新开隧道（要断根用 `projects disable/revoke` + §1 级联）。

### 执行方（broker 侧控制循环）

每个 `TunnelManager` 的 sweep loop 加 **15s 控制 ticker**（与既有 1min 空闲 sweep 并列，同一 goroutine 两个 ticker 或嵌套 select）。**控制周期职责清单（rev1 汇总）**：

1. 读 pending 单（`WHERE outcome IS NULL`）→ 按 §4 状态机执行/标记（tunnel 单：拥有者执行 + 全局缺席即达成；project 单：幂等扇出 + registry 清零达成）；
2. 过期 GC + 终结行清理（上文 SQL）；
3. **级联复查**（§5）：本 manager 有活隧道时复查 project status；
4. **白名单存量复查（rev1）**：本 manager 的活隧道中 `listen_host` 非环回者，逐一对照当前白名单（`ListForwardBindHosts`，规范形比对）——已不在表内 → 关闭（复用 kill 的关闭 + 镜像 DELETE + 日志一行）；
5. **镜像同步**：对活隧道 `UPDATE last_activity`；顺带 registry 30min GC（§6）。

**瞬时错误语义（rev1 钉死）**：tick 内任何 store 读写失败（DB locked 等可写库瞬时错误）→ **日志一行 + 本 tick 放弃、下个 tick 整体重试**——pending 单天然持续可见，等价于自动重试；级联/白名单复查同样顺延一个 tick；不做告警升级。只读 store（离线）的跳过语义不变（表恒空，零路径触发）。

接线点：`NewServerFromSource`（serve 的 `ServerForProject`→`NewServer` 与 stdio 的 `RunStdio`→`NewServer` 都汇到这）构造 manager 后 `AttachStore(storeFn, projectID)`——manager 持 storeFn（热重载安全，与工具闭包同款 call-time 解析）与 projectID；未 Attach 的 manager（既有单测裸构造）控制循环空转（storeFn nil → 跳过），**既有测试零涟漪**。

离线 stdio：只读水合库中 `tunnel_orders` 恒空（快照不导）→ 轮询零命中、零写入（`ErrReadOnly` 不会出现——没有可写路径被触发）；机制性 fail-safe。

## §5 revoke/disable 级联

同一 15s 控制 tick 内（职责 3），**本 manager 有活隧道时**：

- `GetProject(projectID)` → `nil`（行已删）或 `status != active`（disabled/revoked）→ 拆本 manager 全部隧道 + 镜像 DELETE + 日志一行（`cascade: project <id> status=<s> — closed N tunnels`）。
- 只拆隧道；后台任务契约不动（§1）。
- 读走 storeFn（热重载安全）。

## §6 镜像表 tunnel_registry（tunnels ls 数据源）

```sql
CREATE TABLE IF NOT EXISTS tunnel_registry (
  tunnel_id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  server_id TEXT NOT NULL,
  remote TEXT NOT NULL,
  local_addr TEXT NOT NULL,
  listen_host TEXT NOT NULL,
  opened_at INTEGER NOT NULL,
  last_activity INTEGER NOT NULL
);
```

- **写时机**（事件驱动，量级：agent 每会话几条，写量可忽略）：
  - `Open` 注册后 INSERT（**manager 侧**：`TunnelManager.Open` 扩元数据参数——project_id/server_id/remote/listen_host；所有镜像写集中在 manager 的变更路径，`ForwardForProfile` 只负责传入元数据）；
  - `Close`/`SweepIdle`/`CloseAll`/kill/级联/白名单收缩 后 DELETE（本 manager 的 ids）；
  - 控制 tick 时对活隧道 `UPDATE last_activity`（Touch 语义落库）。
- **fail-the-Open（rev1）**：可写 store 上 `Open` 的 registry INSERT 失败 → **立即关闭该隧道**（listener + client）+ `forward_port` 返回错误（audit `status="error"`），不注册 manager 表项——**不可 kill 的隧道不允许存在**。瞬时 DB 错误表现为该次 forward 诚实失败，agent 重试即可。只读 store（离线）维持静默跳过（不 INSERT、不 fail——离线隧道本就不在本急停域内，§1 边界）。「registry 有行 ⇔ 隧道活」由此成为 §4 状态机（全局缺席即达成）的地基。
- **GC**：控制 tick 顺带 `DELETE WHERE last_activity < now-1800`（进程崩溃残留行，幂等，任何 broker 可执行）。
- **陈旧度契约（rev1 修正）**：`last_activity` 正常 **≤45s**（Touch 30s 节流 + 15s tick 落库延迟）；崩溃残留 ≤30min 后清。`ls` 输出带 last_activity 并注明该口径，owner 自行判断。
- 多进程同 project（serve + 在线 stdio 同 project）不冲突：行按 tunnel_id 键控，各管各的行。
- 只读 store 下所有写路径静默跳过（`ErrReadOnly` 容忍 + debug 级日志）。

## §7 审计与日志

- `forward` audit 行 `Command` 字段追加 tunnel id：`host:port id=<tunnelID>`——owner 从审计直达可 kill 的 id（close-forward 行本就是纯 id，闭环对齐）。
- 白名单拒绝 → audit `status="bind_denied"`（§2）。
- kill 应用/级联发生/白名单收缩关闭/expired GC：serve.log（serve）或 stderr（stdio）各一行；**owner 动作记录 = `tunnel_orders` 行自身**（created_at/target/outcome）——agent 审计表保持 agent-only 原则，owner CLI 动作不写入。

## §8 测试矩阵

| # | 对象 | 断言 |
|---|---|---|
| 1 | 白名单 gate | 空表+非环回→拒；表含 IP+匹配→过；环回恒过（无表也过）；`0.0.0.0`/`::`/hostname/垃圾/带 zone →拒；错误文本不含白名单内容；audit `bind_denied` |
| 2 | `serve bind` CLI | add 非法值（通配/环回/非 IP/CIDR）拒；add 幂等；rm/ls；add 后 forward_port **即刻**可用（per-call 读，无重启） |
| 3 | **IP 规范化（rev1）** | add `2001:0db8::0001` → 表存 `2001:db8::1`；gate 用缩写形式过；rm 用任一等价形式命中同一行；带 zone 输入拒 |
| 4 | **IPv6 bind（rev1）** | 合法非环回 IPv6 表项 + listen_host → forward 成功，LocalAddr 为 `[v6]:port` 方括号形式（JoinHostPort 实际绑定断言） |
| 5 | Touch | testsshd echo 隧道持续打字节 → lastActivity 前进 + SweepIdle 不收；静默隧道超时收（既有 lastActivity 直改手法）；30s 节流不炸（高频字节只有限次回调）；Accept 与读路径并发触发无竞争（-race） |
| 6 | kill·tunnel 单 | 拥有者认领执行→listener 关+client 关+dial 不可达；**标记后置**：Close 成功但标记前中断（模拟）→ 单仍 pending、下 tick 重走达成；**全局缺席即达成**：目标已被 sweep 后下单 → 下 tick 标 applied（不落 expired）；两 manager（两 project）不串 |
| 7 | kill·project 单（rev1） | **多 manager 同 project：两 manager 各有隧道，单下发后两个都拆空**（幂等扇出）；完成条件 = registry 该 project 行清零才 applied；CLI 预检 0 行直接报 no open tunnels；CLI 20s 超时 pending 语义 |
| 8 | 认领状态机（rev1） | expired 单不可复活（GC 后认领 UPDATE 零行影响）；pending 查询/标记全带 outcome IS NULL；终结行 7d 清理；无主单 10min 过期 |
| 9 | 级联 | revoke→≤tick 拆+不可达；disable 同；active 不拆；`GetProject` nil 拆；**翻转后 revoke_semantics 隧道测试**；后台任务钉子测试原样绿 |
| 10 | 白名单存量收缩（rev1） | 非环回隧道开着 → `bind rm` 该 IP → ≤tick 隧道关闭+镜像删+日志；环回隧道不受 rm 影响；表空后非环回存量全关 |
| 11 | 镜像 | open 后行在；close/sweep/kill/级联/收缩后行无；tick 刷 last_activity（45s 口径）；30min GC；两进程各行独立；**fail-the-Open：可写库 INSERT 失败（注入）→ forward 报错 + 隧道即关 + registry 无行** |
| 12 | 离线 | 只读水合库：orders/bind_hosts 恒空、控制 tick 零写入零命中；非环回 listen_host 拒 |
| 13 | 回归 | 全量 `go test ./...`；conformance 补 kill 急停端到端一例；eval 补一例（T 编号顺延，活动感知或急停取一） |

## §9 不做（scope 纪律）

- 隧道持久化 / 自动重连 / 命名隧道（backlog 原文）。
- 后台任务 revoke 级联（Plan 32 契约不动）。
- serve loopback admin 端点（拍板弃）。
- 白名单进离线快照 / 热推送。
- hostname / CIDR / 通配白名单条目。
- TUI 隧道管理页（CLI only；TUI 留待真实需求）。
- kill 单的 CLI 侧完成回调通知 / 持久重试预算（CLI 同步轮询 ≤20s 即止）。**注**：broker 侧 pending 单在控制 tick 内的幂等重走是状态机的自然语义（执行/删除均幂等），不属「重试机制」——不设重试计数、不设告警升级。

## §10 文档联动

- `docs/agent-access.md`：四层第 3 层重写（§1 文案，含白名单撤回收缩）+ 应急表「要立刻断正在跑的会话」行更新（`tunnels kill`）+ 排障表「暂停了 agent 还在跑」行。
- `docs/threat-model.md`：(b) 节补急停/白名单现状；「接口级不暴露」注记：listen_host 是 agent 提供的输入、拒绝文本不披露白名单内容（不构成新披露面）。
- `docs/agent-tools.md`：forward_port `listen_host` 参数说明（默认环回；非环回需 owner 白名单；rm 后存量 ≤15s 收缩）+ 回收口径改 activity。
- `docs/multi-machine.md`：NUC10 VLAN bind 惯用法（`serve bind add <VLAN-IP>` → agent `listen_host`）+ 笔记本侧恒 loopback 说明。
- `docs/compat-matrix.md`：契约变更行（revoke/disable→隧道 ≤15s 拆；forward_port 新参数；新表×3；新 CLI `serve bind`/`tunnels`）**+ audit `forward` 行 Command 字段格式变更（追加 `id=<tunnelID>`）单列（rev1）**——v0.10 系发版编排时并入哪行留 owner 拍板。
- `README.md` 命令清单补两组 CLI。

## §11 任务拆分预览（writing-plans 骨架）

1. store 层：3 表 schemaSQL + CRUD/查询（bind add-rm-ls（含规范形）/`ListForwardBindHosts`、orders 下单/pending 查询（outcome IS NULL）/标记 UPDATE（outcome IS NULL）/过期 GC/终结行清理、registry upsert/delete/刷新/GC）+ 单测。
2. sshbroker：`Tunnel` onActivity 钩子（Accept + 双向读路径、30s 原子节流）+ `ForwardLocal` listenHost 参数 + 单测。
3. mcpserver/TunnelManager：`AttachStore` + 15s 控制循环（五职责：orders 状态机/双 GC/级联/白名单存量复查/镜像同步）+ fail-the-Open + 镜像写时机接线 + 单测。
4. mcpserver/core：`ForwardForProfile` listen_host gate 链（规范形比对）+ `bind_denied` audit + forward audit 加 id + `ForwardInput/Output` 字段 + 工具描述更新 + 单测。
5. cli：`serve bind add/rm/ls`（规范形）+ `tunnels ls/kill/kill --project`（预检/等待语义）+ 单测。
6. 契约翻转测试重写 + conformance/eval 补例 + 全量文档联动。
