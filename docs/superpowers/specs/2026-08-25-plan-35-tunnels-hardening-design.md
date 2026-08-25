# Plan 35 · tunnels 硬化（急停 + 活动感知回收 + listen_host 白名单）设计 spec

- 日期：2026-08-25 · 状态：定稿候选（brainstorm 过审，待 xcheck）
- backlog：P1 #15（吸收老 #1/#2/#4；「不做」清单原文沿用：持久化、自动重连、命名隧道）
- Owner 拍板记录（2026-08-25）：
  1. **急停通道 = vault DB kill 单**（备选 serve loopback admin 端点 / 双轨均否）——复用 master-key 既有通道，零新网络面，serve + 在线 stdio 统一覆盖；
  2. **revoke 级联只拆隧道**——后台任务契约不动（Plan 32 钉住：孤儿任务无读者、24h 封顶 runoff）；
  3. **listen_host 白名单 = vault DB 表 + owner CLI**（备选 serve 启动 flag / 砍掉均否）——per-call 读，改白名单免重启即生效。
- 设计全稿在会话内逐节过审（§0-§8），owner 「继续」放行。

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

> **已建立的 `forward_port` 隧道——revoke/disable 后 ≤ 控制轮询间隔（~15s）内拆除；owner 随时 `tunnels kill <id>` / `tunnels kill --project <name>` 拆除。**

- **翻转声明**：`revoke_semantics_test.go` 中 `TestRevokedProjectKeepsOpenTunnelForwarding` 重写为「revoke 后 ≤tick 拆除 + 端口不可达」（文件头自注"契约变更须经由改测试显式声明"——本 plan 即声明）；`TestServeHTTPRejectsRevokedTokenPerRequest`（逐请求 401）不变；`TestRevokedProjectKeepsBackgroundTaskRunning`（Plan 32 钉子）**原样保持绿**。
- **disable 也拆**（不止 revoke）：disable 语义含「审查中」，威胁面同 revoke；`rotate` 不改 status、不拆。project 行被删（`GetProject` 返回 nil）按非 active 处理、拆。
- **级联触发点**：TunnelManager 控制循环（§4，15s tick）在**本 manager 有活隧道时**复查 project status → 非 active 即 `CloseAll` 自己的隧道。serve 逐请求 401 语义不变（新动作立即断；存量隧道 ≤15s）。
- **serve 的 scopedServer cache 不驱逐**：空 manager 无害，revoked token 下一请求 401；驱逐是多余机器。
- **离线 stdio（`mcp --cache`）永不级联**：其 store 是快照水合的只读临时库，status 定格在拉取时点——离线隧道的拆法仍是四层第 4 层（回连销毁）+ 本机杀进程。登记为预期边界。

## §2 listen_host 白名单（加新能力，非收紧）

### 数据与 CLI

- 新表 `forward_bind_hosts(ip TEXT PRIMARY KEY, created_at INTEGER NOT NULL)`（schemaSQL 追加，`CREATE TABLE IF NOT EXISTS`，纯增量）。
- owner CLI（`serve` 子命令组下）：
  - `ssh-manager serve bind add <ip>` —— 校验：`net.ParseIP` 成功、**非环回**（`IsLoopback` 拒——环回恒允许不需要表项）、**非通配**（`IsUnspecified` 拒，即 `0.0.0.0`/`::`）；错值带原因拒绝。重复 add 幂等（INSERT OR IGNORE + 提示已存在）。
  - `ssh-manager serve bind rm <ip>` / `ssh-manager serve bind ls`
- 白名单条目 = **IP 字面量 only**：hostname/DNS 名、CIDR 网段一律拒（解析漂移与范围失控）。

### forward_port gate 链（`ForwardForProfile`）

新输入参数 `listen_host`（可选，缺省空）：

1. 空 / 缺省 → `127.0.0.1`（现状行为，零破坏）；
2. `net.ParseIP` 失败（hostname、垃圾值）→ 拒；`IsUnspecified`（通配）→ 拒；`IsLoopback` → **恒允许**（不需要表项）；
3. 其他非环回 IP → **per-call 读表**（新 store 方法 `ListForwardBindHosts() ([]string, error)`，与 `VerifyToken`/`ServersForProfile` 同款 per-call 模式）：命中 → 允许；未命中 → 拒。
- 拒绝错误文本：`listen_host %q is not in the owner-approved bind host whitelist` —— **不回白名单内容**，只回 agent 自己提供的输入。
- 拒绝时 audit status 用新值 **`bind_denied`**（区别于 profile 越权的 `denied`，owner 可观测 agent 的 bind 越权尝试）。

### 传导

- `Client.ForwardLocal(localPort int, listenHost, remoteHost string, remotePort int)` —— listenHost 由 gate 链产出的**已验证值**传入（`net.Listen("tcp", net.JoinHostPort(listenHost, port))`）；`localAddr` 如实回报实际绑定地址。
- `ForwardOutput` 加 `listen_host` 字段（回报实际绑定 host）；`ForwardInput` 加 `listen_host`（`omitempty`）。
- **在线 stdio 同一代码路径顺带获得能力**：表项 IP 不在本机 → bind 自然失败（无害错误）。**离线 cache 快照不含此表**：水合库中表恒空 → 离线 stdio 恒 loopback-only——`ImportSnapshot` 不导该表即机制性 fail-closed，零额外代码，写进文档。

## §3 活动感知回收（落实 Touch）

- `sshbroker.Tunnel` 加活动钩子：`ForwardLocal` 构造参数注入 `onActivity func()`，触发点两处：
  1. `serve` 的 `Accept` 成功后（每条新连接 = 活动）；
  2. `handle` 内两个方向 pipe 的**读路径**——io.Copy 包一层计数 reader，每次读到字节即上报。
- **节流**（热路径纪律）：Tunnel 侧 `atomic.Int64` 存上次触发 unixnano，间隔 **≥30s** 才真正调用回调（读路径开销 = 一次原子 load/compare，无锁、无分配）。
- manager 侧接既有 `Touch(id)` → `lastActivity = now`。
- `forwardIdleTimeout` 10 min 语义从「创建后 10 min 必死」变为「**真空闲 10 min**」：持续流量的隧道存活；纯空闲挂连接（如无查询的 SQL 连接）10 min 照收——与现状一致且严格更好。
- in-flight 管道 goroutine 在 listener 关闭后照常 drain 完（现状 `Tunnel.Close` 语义不变）。
- 措辞清理：`tunnels.go` 头注释 "creation-based"、`server.go` forward_port/close_port 工具描述 "~10 minutes after creation, not based on activity"、`agent-tools.md` 同步改为 activity 口径。

## §4 急停：vault DB kill 单

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

### owner CLI

- `ssh-manager tunnels ls` —— 读 `tunnel_registry`（§6）join projects：tunnel_id、project 名、server_id、remote、local_addr、listen_host、opened_at、last_activity。
- `ssh-manager tunnels kill <tunnel_id>` —— **预检**：id 不在 `tunnel_registry` → 快失败 `no open tunnel <id> (recent tunnels may take ~15s to appear in the registry)`，不下单。在 → 下单 → **轮询 ≤20s**：`applied`（成功，回报）/ 仍 pending（如实报：`order pending — no broker applied it within 20s (target may belong to an offline/dead process; expires in 10min)`，**不装成功**）。
- `ssh-manager tunnels kill --project <name|id>` —— 解析 project（`GetProjectByName`/`GetProject`）→ 预检 registry 无该 project 行 → 直接报 `no open tunnels for project X`（不下单）；有 → 下 project 单 → 同款等待语义。
- kill 单 = **surgical**：不动 token，project 仍 active，agent 可重新开隧道（要断根用 `projects disable/revoke` + §1 级联）。

### 执行方（broker 侧控制循环）

每个 `TunnelManager` 的 sweep loop 加 **15s 控制 ticker**（与既有 1min 空闲 sweep 并列，同一 goroutine 两个 ticker 或嵌套 select）：

1. `PendingTunnelOrders()`（读 pending 单）；
2. 本 manager 认领判定：`tunnel_id ∈ 本 registry` 或（project 单且 `project_id == 本 manager 的 projectID`）；
3. **原子认领**：`UPDATE tunnel_orders SET applied_at=?, outcome='applied' WHERE id=? AND applied_at IS NULL` —— `RowsAffected==1` 才算认领（多进程/多 manager 竞争安全；隧道 UUID 全局唯一 + project 单只被同 project manager 认领）；
4. 认领后执行：tunnel 单 → `Close(id)`；project 单 → 本 manager 全拆（等价 `CloseAll` 但不走 quit 通道——sweeper 自身还活着）→ 镜像 DELETE + 日志一行（serve 模式 serve.log；stdio 模式 stderr）。
5. **无主单 GC**：tick 顺带 `UPDATE ... SET outcome='expired' WHERE applied_at IS NULL AND created_at < now-600`（幂等，任何 broker 可执行）。

接线点：`NewServerFromSource`（serve 的 `ServerForProject`→`NewServer` 与 stdio 的 `RunStdio`→`NewServer` 都汇到这）构造 manager 后 `AttachStore(storeFn, projectID)`——manager 持 storeFn（热重载安全，与工具闭包同款 call-time 解析）与 projectID；未 Attach 的 manager（既有单测裸构造）控制循环空转（storeFn nil → 跳过），**既有测试零涟漪**。

离线 stdio：只读水合库中 `tunnel_orders` 恒空（快照不导）→ 轮询零命中、零写入（`ErrReadOnly` 不会出现——没有可写路径被触发）；机制性 fail-safe。

## §5 revoke/disable 级联

同一 15s 控制 tick 内，**本 manager 有活隧道时**：

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
  - `Close`/`SweepIdle`/`CloseAll`/kill/级联 后 DELETE（本 manager 的 ids）；
  - 控制 tick 时对活隧道 `UPDATE last_activity`（Touch 语义落库）。
- **GC**：控制 tick 顺带 `DELETE WHERE last_activity < now-1800`（进程崩溃残留行，幂等，任何 broker 可执行）。
- 陈旧度契约：正常 ≤15s；崩溃残留 ≤30min 后清。`ls` 输出带 last_activity，owner 自行判断。
- 多进程同 project（serve + 在线 stdio 同 project）不冲突：行按 tunnel_id 键控，各管各的行。
- 只读 store 下所有写路径静默跳过（`ErrReadOnly` 容忍 + debug 级日志）。

## §7 审计与日志

- `forward` audit 行 `Command` 字段追加 tunnel id：`host:port id=<tunnelID>`——owner 从审计直达可 kill 的 id（close-forward 行本就是纯 id，闭环对齐）。
- 白名单拒绝 → audit `status="bind_denied"`（§2）。
- kill 应用/级联发生/expired GC：serve.log（serve）或 stderr（stdio）各一行；**owner 动作记录 = `tunnel_orders` 行自身**（created_at/target/outcome）——agent 审计表保持 agent-only 原则，owner CLI 动作不写入。

## §8 测试矩阵

| # | 对象 | 断言 |
|---|---|---|
| 1 | 白名单 gate | 空表+非环回→拒；表含 IP+匹配→过；环回恒过（无表也过）；`0.0.0.0`/`::`/hostname/垃圾→拒；错误文本不含白名单内容；audit `bind_denied` |
| 2 | `serve bind` CLI | add 非法值（通配/环回/非 IP/CIDR）拒；add 幂等；rm/ls；add 后 forward_port **即刻**可用（per-call 读，无重启） |
| 3 | Touch | testsshd echo 隧道持续打字节 → lastActivity 前进 + SweepIdle 不收；静默隧道超时收（既有 lastActivity 直改手法）；30s 节流不炸（高频字节只有限次回调） |
| 4 | kill | 单 id 认领→listener 关+client 关+dial 不可达；两 manager（两 project）不串；无主单 10min 过期；CLI 预检 no-target 快失败；kill --project 全拆；CLI 20s 超时 pending 语义 |
| 5 | 级联 | revoke→≤tick 拆+不可达；disable 同；active 不拆；`GetProject` nil 拆；**翻转后 revoke_semantics 隧道测试**；后台任务钉子测试原样绿 |
| 6 | 镜像 | open 后行在；close/sweep/kill/级联后行无；tick 刷 last_activity；30min GC；两进程各行独立 |
| 7 | 离线 | 只读水合库：orders/bind_hosts 恒空、控制 tick 零写入零命中；非环回 listen_host 拒 |
| 8 | 回归 | 全量 `go test ./...`；conformance 补 kill 急停端到端一例；eval 补一例（T 编号顺延，活动感知或急停取一） |

## §9 不做（scope 纪律）

- 隧道持久化 / 自动重连 / 命名隧道（backlog 原文）。
- 后台任务 revoke 级联（Plan 32 契约不动）。
- serve loopback admin 端点（拍板弃）。
- 白名单进离线快照 / 热推送。
- hostname / CIDR / 通配白名单条目。
- TUI 隧道管理页（CLI only；TUI 留待真实需求）。
- kill 单重试 / 完成回调通知（CLI 同步轮询 ≤20s 即止）。

## §10 文档联动

- `docs/agent-access.md`：四层第 3 层重写（§1 文案）+ 应急表「要立刻断正在跑的会话」行更新（`tunnels kill`）+ 排障表「暂停了 agent 还在跑」行。
- `docs/threat-model.md`：(b) 节补急停/白名单现状；「接口级不暴露」注记：listen_host 是 agent 提供的输入、拒绝文本不披露白名单内容（不构成新披露面）。
- `docs/agent-tools.md`：forward_port `listen_host` 参数说明（默认环回；非环回需 owner 白名单）+ 回收口径改 activity。
- `docs/multi-machine.md`：NUC10 VLAN bind 惯用法（`serve bind add <VLAN-IP>` → agent `listen_host`）+ 笔记本侧恒 loopback 说明。
- `docs/compat-matrix.md`：契约变更行（revoke/disable→隧道 ≤15s 拆；forward_port 新参数；新表×3；新 CLI `serve bind`/`tunnels`）——v0.10 系发版编排时并入哪行留 owner 拍板。
- `README.md` 命令清单补两组 CLI。

## §11 任务拆分预览（writing-plans 骨架）

1. store 层：3 表 schemaSQL + CRUD/查询（`ListForwardBindHosts`/bind add-rm-ls、orders 下单/查 pending/原子认领/过期 GC、registry upsert/delete/刷新/GC）+ 单测。
2. sshbroker：`Tunnel` onActivity 钩子（Accept + 双向读路径、30s 原子节流）+ `ForwardLocal` listenHost 参数 + 单测。
3. mcpserver/TunnelManager：`AttachStore` + 15s 控制循环（orders 认领执行/级联/镜像同步/GC）+ 镜像写时机接线 + 单测。
4. mcpserver/core：`ForwardForProfile` listen_host gate 链 + `bind_denied` audit + forward audit 加 id + `ForwardInput/Output` 字段 + 工具描述更新 + 单测。
5. cli：`serve bind add/rm/ls` + `tunnels ls/kill/kill --project`（预检/等待语义）+ 单测。
6. 契约翻转测试重写 + conformance/eval 补例 + 全量文档联动。
