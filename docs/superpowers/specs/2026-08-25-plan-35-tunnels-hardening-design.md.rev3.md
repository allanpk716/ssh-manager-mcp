# Plan 35 · tunnels 硬化（急停 + 活动感知回收 + listen_host 白名单）设计 spec · rev3

- 日期：2026-08-25 · 状态：rev3（rev2 第 3 轮复审 8 条全证实后修订，owner 突破 C5；待确认轮或定稿拍板）
- 原稿链：`...design.md`（v1）→ `.rev1.md`（13 条）→ `.rev2.md`（复审 13 条）→ **本稿 rev3**（第 3 轮 8 条，**以删机器/钉现状为主**）：
  1. **删除 `expired` 终态**（kimi 证实其不可达——applied-on-absence 恒先赢 + 目标在不过期）：outcome ∈ {NULL(pending), 'applied'}；清理改 applied 7d + pending 孤儿 7d；
  2. **续约失败计数语义钉死**（tick 放弃计入 + 三条重置规则）；
  3. **零行续约自关闭**（UPDATE 影响 0 行 → 该隧道自关闭）+ tick 卡死残余登记；
  4. **client 独占模型钉死**（每 forward 独立 Connect，core.go 现状）；
  5. **执法读失败有界降级**（复查/级联读失败 >8 tick → 有界关闭；§1 口径诚实化）；
  6. 混合版本 compat 登记；CLI 轮询 45s；IPv6 测试 skip 策略。
- backlog：P1 #15（吸收老 #1/#2/#4；「不做」清单原文沿用：持久化、自动重连、命名隧道）
- Owner 拍板记录（2026-08-25）：
  1. **急停通道 = vault DB kill 单**（备选 serve loopback admin 端点 / 双轨均否）；
  2. **revoke 级联只拆隧道**——后台任务契约不动（Plan 32 钉住）；
  3. **listen_host 白名单 = vault DB 表 + owner CLI**——per-call 读，改白名单免重启即生效。

---

## §0 背景与现状

三个现状缺口（均已在代码/文档中钉住）：

1. **无急停**：隧道由 broker 进程持有（serve 每 project 一个 `TunnelManager`，stdio 独立进程）；revoke 后逐请求 401 挡住新动作，但**已建立的隧道继续转发**（`revoke_semantics_test.go` 钉住，注释明示"owner decision, kill CLI is backlog"）。`docs/agent-access.md` 断连语义第 3 层原文：真实选项只有重启 broker 或等 ~10 分钟回收。
2. **回收按创建时间**：`managedTunnel.lastActivity` 字段与 `TunnelManager.Touch(id)` 已存在但**零生产调用方**（`tunnels.go:10-16` 注释明示）——隧道创建 10 min 后必死，持续流量也救不了。
3. **非环回 bind 不存在**：`Client.ForwardLocal` 硬编码 `127.0.0.1`（`tunnel.go:68`）。NUC10 serve 拓扑下想让 VLAN 内其他机器用隧道（bind 到 VLAN IP）无路径；而直接放开 per-tunnel bind = 威胁模型 (b)（被劫持 agent 开隧道打内网）的攻击面扩张。

目标：给 owner 三件控制力——①急停（kill 单 + revoke/disable 级联）；②活动感知回收；③非环回 bind 的 owner 预批白名单放行。

验收（backlog #15 原文）：白名单外 bind 拒绝；急停后端口不可达（含已建立连接终止，§8）；持续流量下不回收。

## §1 契约翻转（本 plan 的声明性核心）

`docs/agent-access.md` 断连语义第 3 层翻转为：

> **已建立的 `forward_port` 隧道——revoke/disable 后 ≤ 控制轮询间隔（~15s）内拆除；owner 随时 `tunnels kill <id>` / `tunnels kill --project <name>` 拆除；owner 撤回 bind 白名单条目后 ≤ 控制轮询间隔内，绑定该地址的存量隧道关闭。**
> 时效口径（rev3 诚实化）：以上 ≤15s 以 **store 健康**为前提；store 持续读写故障时，lease/执法纪律降级为**有界关闭**（≤ ~2min，§4），不存在「无限期暴露」路径。

- **翻转声明**：`revoke_semantics_test.go` 中 `TestRevokedProjectKeepsOpenTunnelForwarding` 重写为「revoke 后 ≤tick 拆除 + 端口不可达」（文件头自注"契约变更须经由改测试显式声明"——本 plan 即声明）；`TestServeHTTPRejectsRevokedTokenPerRequest`（逐请求 401）不变；`TestRevokedProjectKeepsBackgroundTaskRunning`（Plan 32 钉子）**原样保持绿**。
- **disable 也拆**（不止 revoke）：disable 语义含「审查中」，威胁面同 revoke；`rotate` 不改 status、不拆。project 行被删（`GetProject` 返回 nil）按非 active 处理、拆。
- **级联触发点**：TunnelManager 控制循环（§4，15s tick）在**本 manager 有活隧道时**复查 project status → 非 active 即拆本 manager 全部隧道。serve 逐请求 401 语义不变。
- **serve 的 scopedServer cache 不驱逐**：空 manager 无害。
- **离线 stdio（`mcp --cache`）永不级联、不在 kill/ls 域**：其 store 是快照水合的只读临时库，隧道不进 `tunnel_registry`——离线隧道的拆法 = 四层第 4 层（回连销毁）+ 本机杀进程。CLI 与文档话术点名（§4/§10）。

## §2 listen_host 白名单（加新能力，非收紧）

### 数据与 CLI

- 新表 `forward_bind_hosts(ip TEXT PRIMARY KEY, created_at INTEGER NOT NULL)`（schemaSQL 追加，纯增量）。
- owner CLI（`serve` 子命令组下）：
  - `ssh-manager serve bind add <ip>` —— 校验：`net.ParseIP` 成功、**非环回**（`IsLoopback` 拒）、**非通配**（`IsUnspecified` 拒，即 `0.0.0.0`/`::`）、**无 zone identifier**（带接口后缀的输入 `net.ParseIP` 本就失败，按非法拒）；错值带原因拒绝。重复 add 幂等（INSERT OR IGNORE + 提示已存在）。
  - `ssh-manager serve bind rm <ip>` / `ssh-manager serve bind ls`
- 白名单条目 = **IP 字面量 only**：hostname/DNS 名、CIDR 网段一律拒。

### IP 规范化（三处统一）

**存取比对一律用规范形**：add 存 `net.ParseIP(input).String()`；rm 先规范化再删；gate 用 `net.ParseIP(agent 输入).String()` 对表内规范形。三处共用一个 canonicalize 帮助函数。

### forward_port gate 链（`ForwardForProfile`）

新输入参数 `listen_host`（可选，缺省空）：

1. 空 / 缺省 → `127.0.0.1`（现状行为，零破坏）；
2. `net.ParseIP` 失败（hostname、垃圾值、带 zone）→ 拒；`IsUnspecified` → 拒；`IsLoopback` → **恒允许**；
3. 其他非环回 IP → 规范形 **per-call 读表**（`ListForwardBindHosts() ([]string, error)`）：命中 → 允许；未命中 → 拒。
- **读失败 fail-closed**：非环回且读表失败 → **拒**，audit `status="error"`（区别于 `bind_denied`）；环回不受影响（无需查表）。
- 拒绝错误文本：`listen_host %q is not in the owner-approved bind host whitelist`——不回白名单内容。越权拒绝 audit `status="bind_denied"`。
- **存量收缩**：gate 只管新 Open；`bind rm` 后存量由控制周期复查关闭（§4），撤回 ≤tick 生效（store 健康前提，§1 口径）。

### 传导

- `Client.ForwardLocal(localPort int, listenHost, remoteHost string, remotePort int)` —— listenHost 为 gate 产出的已验证原输入（`net.Listen("tcp", net.JoinHostPort(listenHost, port))`；IPv6 自动方括号）；`localAddr` 如实回报。
- `ForwardOutput` 加 `listen_host`；`ForwardInput` 加 `listen_host`（`omitempty`）。
- **在线 stdio 同路径获得能力**（表项 IP 不在本机 → bind 自然失败）；**离线快照不含此表** → 离线 stdio 恒 loopback-only（`ImportSnapshot` 不导该表即机制性 fail-closed）。

## §3 活动感知回收（落实 Touch）

- `sshbroker.Tunnel` 加活动钩子：`ForwardLocal` 构造参数注入 `onActivity func()`，触发点：① `serve` 的 Accept 成功后；② `handle` 双向 pipe 读路径（io.Copy 包计数 reader）。**节流**：Tunnel 侧 `atomic.Int64`，间隔 ≥30s 才真正回调（读路径零锁零分配）。
- manager 侧接既有 `Touch(id)`。**并发安全钉住**：回调多 goroutine 并发触发，`Touch` 由 `TunnelManager.mu` 既有互斥保证（`tunnels.go:114-123`）。
- `forwardIdleTimeout` 10 min 语义变为「**真空闲 10 min**」：持续流量的隧道存活；纯空闲挂连接照收（与现状一致且严格更好）。
- **SSH client 独占模型（rev3 钉死）**：**每隧道独占一个 `sshbroker.Client`**——`ForwardForProfile` 每次 forward 独立 `sshbroker.Connect` 后交给 manager（`core.go:509-531` 现状即此）；manager 级拆除关闭的是该隧道**独占**的 client，**无任何连带**（kill 一个不影响同 manager 其他隧道；§8 断言）。
- **drain 语义澄清**：`Tunnel.Close`（仅 listener）后 in-flight 管道照常 drain——listener 级语义；**manager 级拆除**（Close/SweepIdle/CloseAll/kill/级联/收缩/renewal 自关闭）同时关闭 owning `ssh.Client` → 其全部通道终止，已 accept 连接本地读端收 EOF（§8 断言）——急停完整。
- 措辞清理：`tunnels.go` 头注释、`server.go` 工具描述、`agent-tools.md` 改 activity 口径。

## §4 急停：vault DB kill 单（rev3：删 expired + 计数语义 + 零行续约）

### 数据

新表 `tunnel_orders`：

```sql
CREATE TABLE IF NOT EXISTS tunnel_orders (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  tunnel_id TEXT,                -- 二填一（应用层 + CHECK 约束）
  project_id TEXT,
  created_at INTEGER NOT NULL,
  applied_at INTEGER,
  outcome TEXT,                  -- NULL=pending; 'applied'（rev3：expired 终态删除——不可达死码）
  CHECK ((tunnel_id IS NULL) <> (project_id IS NULL))
);
```

**状态机（rev3：两终态化一）**——`outcome ∈ {NULL(pending), 'applied'}`：

- pending ⇔ `outcome IS NULL`——**pending 查询与一切标记 UPDATE 一律带 `outcome IS NULL`**。
- **tunnel 单**：拥有者执行、标记后置。**拥有者 = 该 tunnel_id 位于本 manager 内存隧道集的 manager**。路径：①拥有者 `Close(id)`（幂等，含独占 client → in-flight 终止）→ ②镜像 DELETE → ③`UPDATE ... SET applied_at=?, outcome='applied' WHERE id=? AND outcome IS NULL`。崩溃在 ③ 前 → 单仍 pending，下个 tick 幂等重走。
- **tunnel 单的全局缺席即达成**：任何 manager 的 tick 发现 pending tunnel 单的目标在 `tunnel_registry` **无行**（已被 sweep 正常关闭、或进程崩溃残留行已被 GC）→ 标 `applied`（目标不在 = 已达成；无龄条件）。
- **project 单 = 幂等扇出（弱语义）**：**不排他认领**。每个 manager 每 tick：单匹配本 project 且有活隧道 → 拆自己当时全部（幂等）。完成标记与执行解耦：任一 manager 的 tick 发现该 project 行 COUNT==0 → 标 `applied`——仅为完成信号，非杀性承诺。**弱语义**：拆各 manager 处理时刻的存量，**不承诺阻止重开**——agent 持续重开时单**持续 pending 且每 tick 继续拆掉新开的**（急停效果持续），重开停止（或 owner disable/revoke 级联清场）后行清零 → `applied`。**终态恒 applied，不存在 expired**（rev3：原 expired 语义不可达，删除；owner 侧「重开持续」的观测信号 = `tunnels ls` 见新隧道 / audit 见新 forward 行，非订单状态）。要阻止重开用 `projects disable/revoke`。
- **行清理（rev3 合并）**：tick 顺带 `DELETE FROM tunnel_orders WHERE created_at < now-604800 AND (outcome IS NOT NULL OR outcome IS NULL)`——7 天以上**applied 终结行与 pending 孤儿行**一并清理（孤儿 = 全库无可写 broker 消费时兜底；可写 broker 存在时 pending 单必达 applied，见上）。

### owner CLI

- `ssh-manager tunnels ls` —— 读 `tunnel_registry` **LEFT JOIN** projects（级联窗口可见，直显 project_id）：tunnel_id、project（名，可空）+ project_id、server_id、remote、local_addr、listen_host、opened_at、last_activity；last_activity 超 45s 未刷的行标注「疑似残留（进程可能已崩溃，≤30min 自动清）」。
- `ssh-manager tunnels kill <tunnel_id>` —— 预检：id 不在 registry → 快失败 `no open tunnel <id>`（注明本命令只覆盖写权威 vault 的 broker——serve 与在线 stdio；**离线 cache 客户端的隧道不在此域**）。在 → 下单 → **轮询 ≤45s（rev3：3×tick 余量）**：`applied` / 仍 pending（如实报：`order pending — no broker applied it within 45s (target may belong to an offline/dead process; it will complete when a writable broker ticks)`）。
- `ssh-manager tunnels kill --project <name|id>` —— 解析 project → 预检 registry 无该 project 行 → `no open tunnels for project X`（不下单）；有 → 下 project 单 → 同款等待；超时文案附「agent 持续重开会使单保持 pending 并被持续拆除——要阻止重开请 disable/revoke」。
- kill 单 = **surgical**：不动 token。

### 执行方（broker 侧控制循环）

每个 `TunnelManager` 的 sweep loop 加 **15s 控制 ticker**。**控制周期职责清单**：

1. 读 pending 单（`WHERE outcome IS NULL`）→ 按状态机执行/标记；
2. 行清理（7d，上文）；
3. **级联复查**（§5）：本 manager 有活隧道时复查 project status；
4. **白名单存量复查**：本 manager 活隧道中 `listen_host` 非环回者对照当前白名单（规范形）——不在表内 → 关闭（复用 kill 关闭 + 镜像 DELETE + 日志）；
5. **镜像同步**：对活隧道 `UPDATE last_activity`；顺带 registry 30min GC（§6）；**零行续约自关闭（rev3）**：某活隧道的 UPDATE 影响 0 行（行已被 GC）→ 该隧道立即自关闭（脱离 kill 域的隧道不允许存在）。

**瞬时错误语义**：tick 内任何 store 读写失败（可写库瞬时错误）→ 日志一行 + 本 tick 放弃、下个 tick 整体重试；只读 store（离线）跳过语义不变。

**fail-the-renewal（lease 纪律，rev3 计数语义钉死）**：

- **「续约失败 tick」定义**：本 manager **有活隧道** 且该 tick **未完成职责 5 的续约**——含两种情形：续约 UPDATE 尝试后失败，或**整 tick 因 store 故障提前放弃（续约未被尝试）**——两者都推进计数（堵住「恰恰在最需要 lease 纪律的持续故障场景计数永不推进」的缺口）。
- **阈值**：连续失败 >8 tick（~2min）→ **自关闭本 manager 全部隧道**（含独占 client）+ 镜像 best-effort DELETE + 日志一行（`lease renewal failed N ticks — closed M tunnels`）。
- **计数重置规则（rev3）**：任一成功完成续约的 tick → 计数清零；本 manager 无活隧道期间 → 计数**挂起**（不推进不复位——无隧道时续约无从谈起）；进程重启 → 计数归零（内存态，重启 = 全新 lease 周期）。
- **执法读失败有界降级（rev3，同族纪律）**：职责 3（级联 `GetProject`）/ 职责 4（白名单复查读）持续失败连续 >8 tick（~2min）→ 按「无法验证执法条件 = 条件不满足」处理：级联不可读 → 关闭本 manager 全部隧道；白名单复查不可读 → 关闭全部非环回存量隧道 + 日志。§1 的 ≤15s 承诺以 store 健康为前提；持续故障下降级为 ≤2min 有界关闭——**不存在无限期暴露路径**。
- **tick 卡死残余（rev3 登记）**：控制 goroutine 本身卡死（进程级故障，非 DB 故障）时不产生失败 UPDATE、不推进计数——此时 kill 单可能在行被 GC（≤30min）后误标 applied 而数据面仍转发；**恢复后零行续约自关闭兜底**（该场景最终收敛为隧道自灭）。独立 watchdog / GC 区分活 owner 判过度工程不做（§9 登记）。

接线点：`NewServerFromSource` 构造 manager 后 `AttachStore(storeFn, projectID)`；未 Attach 的 manager 控制循环空转（既有单测零涟漪）。离线 stdio：水合库 orders 恒空 → 轮询零命中零写入。

## §5 revoke/disable 级联

同一 15s 控制 tick 内（职责 3），**本 manager 有活隧道时**：

- `GetProject(projectID)` → `nil`（行已删）或 `status != active` → 拆本 manager 全部隧道 + 镜像 DELETE + 日志一行。读失败的有界降级见 §4。
- 只拆隧道；后台任务契约不动（§1）。

## §6 镜像表 tunnel_registry（tunnels ls 数据源）

**地基前提（钉死）**：`tunnel_id = sshbroker.Tunnel.ID = uuid.NewString()`（UUID v4，`tunnel.go:73` 现状）——**跨进程全局唯一**；本表主键、kill 寻址、多进程行隔离三处机制的地基；任何未来改动 id 生成方式必须重新审视本节。

```sql
CREATE TABLE IF NOT EXISTS tunnel_registry (
  tunnel_id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  server_id TEXT NOT NULL,
  remote TEXT NOT NULL,
  local_addr TEXT NOT NULL,
  listen_host TEXT NOT NULL,    -- 存 canonicalize() 规范形
  opened_at INTEGER NOT NULL,
  last_activity INTEGER NOT NULL
);
```

- **写时机**（事件驱动）：`Open` 后 INSERT（manager 侧，`Open` 扩元数据参数）；`Close`/`SweepIdle`/`CloseAll`/kill/级联/收缩/renewal 自关闭后 DELETE（本 manager ids）；控制 tick 对活隧道 `UPDATE last_activity`。
- **fail-the-Open**：可写 store 上 `Open` 的 INSERT 失败 → 立即关闭该隧道 + `forward_port` 报错（audit `status="error"`）——不可 kill 的隧道不允许存在。
- **DELETE/UPDATE 失败重试**：镜像 DELETE 失败 → 每 tick 重试至成功；ghost 行由 30min 龄 GC 自愈；续约 UPDATE 失败走 §4 fail-the-renewal、零行走零行自关闭。
- **GC**：tick 顺带 `DELETE WHERE last_activity < now-1800`（幂等，**任何可写 broker** 可执行）。
- **陈旧度契约**：`last_activity` 正常 **≤45s**（30s 节流 + 15s tick）；崩溃残留 ≤30min 清。`ls` 按 45s 口径标注疑似残留。
- 多进程同 project 不冲突：行按 tunnel_id 键控（全局唯一前提）。只读 store 写路径静默跳过。
- **混合版本边界（rev3 登记）**：kill/ls 域的完整性要求**所有可写 broker（serve + 在线 stdio）都升级到本版**；混合部署期，旧版进程的隧道不进 registry、不受 kill 单/级联管辖，`tunnels ls` 覆盖不完整——compat-matrix 登记（§10），不做版本探测。

## §7 审计与日志

- `forward` audit 行 `Command` 追加 tunnel id：`host:port id=<tunnelID>`。
- 越权拒绝 → `status="bind_denied"`；gate 读失败 → `status="error"`。
- kill 应用/级联/白名单收缩/fail-the-renewal 与执法降级自关闭：serve.log 或 stderr 各一行；owner 动作记录 = `tunnel_orders` 行自身（agent 审计 agent-only）。

## §8 测试矩阵

| # | 对象 | 断言 |
|---|---|---|
| 1 | 白名单 gate | 空表+非环回→拒；表含 IP 匹配→过；环回恒过；通配/hostname/垃圾/带 zone→拒；错误文本不含白名单内容；audit `bind_denied` |
| 2 | `serve bind` CLI | add 非法值拒；add 幂等；rm/ls；add 后 forward_port 即刻可用 |
| 3 | IP 规范化 | add 全写→表存缩写规范形；gate 缩写形式过；rm 任一等价形式命中同一行；registry.listen_host 为规范形 |
| 4 | IPv6 bind | 合法非环回 IPv6 表项 → forward 成功，LocalAddr `[v6]:port` 方括号形式；**测试机无可用非环回 IPv6 地址时 t.Skip（注明原因，rev3）** |
| 5 | gate 读失败 | 非环回+读失败（注入）→ 拒 + audit `error`；环回不受影响 |
| 6 | Touch | 持续打字节 → lastActivity 前进 + SweepIdle 不收；静默超时收；30s 节流不炸；多 goroutine 触发无竞争（-race） |
| 7 | kill·tunnel 单 | 拥有者执行→listener 关+client 关+dial 不可达；已 accept 连接读端收 EOF；标记后置崩溃重走；全局缺席即达成（sweep 后下单 → 下 tick applied）；两 manager（两 project）不串 |
| 8 | kill·project 单 | 多 manager 同 project 全拆（幂等扇出）；完成 = registry 该 project 行清零才 applied；**弱语义（rev3 口径）**：拆后 agent 立即重开 → 单保持 pending 且新隧道下 tick 被拆、重开停止后终态 applied（无 expired）；CLI 预检 0 行快失败；CLI 45s 超时 pending 语义 |
| 9 | 状态机/清理 | pending 查询与标记全带 outcome IS NULL；applied 行与 pending 孤儿行 7d 一并清理；**无 expired 路径（rev3）** |
| 10 | id 全局唯一 | 两 manager 同 project 同开隧道 → id 不冲突、registry 两行并存 |
| 11 | **client 独占（rev3）** | 同 manager 两隧道，kill 一个 → 另一个**存活且继续转发**（独占 client 无连带）；被 kill 隧道的已 accept 连接收 EOF |
| 12 | 级联 | revoke→≤tick 拆+不可达；disable 同；active 不拆；`GetProject` nil 拆；翻转后 revoke_semantics 隧道测试；后台任务钉子原样绿 |
| 13 | 白名单存量收缩 | 非环回隧道开着 → `bind rm` → ≤tick 关闭+镜像删+日志；环回不受影响；表空后非环回存量全关 |
| 14 | 镜像 | open 后行在；各关闭路径后行无；tick 刷 last_activity（45s 口径）；30min GC；两进程各行独立；fail-the-Open（注入）；DELETE 失败（注入）→ 每 tick 重试 + ghost 自愈 |
| 15 | fail-the-renewal | 续约 UPDATE 失败注入 >8 tick → 全部自关闭 + 日志；**整 tick 放弃（注入读失败于职责 1）计入计数（rev3）**；成功续约清零；无活隧道期间挂起；**执法读失败注入 >8 tick：级联不可读→全关、复查不可读→非环回全关（rev3）** |
| 16 | 零行续约自关闭（rev3） | 活隧道的 registry 行被外部删除（模拟 GC 抢先）→ 下个 tick 续约零行 → 该隧道自关闭 |
| 17 | ls | LEFT JOIN 级联窗口可见 + 直显 project_id；last_activity>45s 标注疑似残留 |
| 18 | 离线 | 只读水合库：orders/bind_hosts 恒空、tick 零写入零命中；非环回 listen_host 拒 |
| 19 | 回归 | 全量 `go test ./...`；conformance 补 kill 急停端到端一例；eval 补一例（T 编号顺延） |

## §9 不做（scope 纪律）

- 隧道持久化 / 自动重连 / 命名隧道（backlog 原文）。
- 后台任务 revoke 级联（Plan 32 契约不动）。
- serve loopback admin 端点（拍板弃）。
- 白名单进离线快照 / 热推送。
- hostname / CIDR / 通配白名单条目。
- TUI 隧道管理页（CLI only）。
- kill 单的 CLI 侧完成回调通知 / 持久重试预算（CLI 同步轮询 ≤45s 即止；broker 侧 tick 幂等重走是状态机自然语义）。
- kill 单期间的 forward 新开禁令（弱语义明确不承诺阻止重开）。
- **独立 watchdog / GC 活 owner 探测（rev3 登记）**：tick 卡死为进程级故障，残余 = kill 单误标 applied 窗口 ≤卡死时长+30min、恢复后零行续约自关闭兜底——watchdog 与 pid/owner 探测的复杂度不匹配该残余概率。
- **多隧道共享 SSH client / channel 级关闭 / client 引用计数（rev3 登记）**：现状每隧道独占连接（§3 钉死），共享模型是未来优化空间，非本 plan 范围。

## §10 文档联动

- `docs/agent-access.md`：四层第 3 层重写（§1 文案，含白名单撤回收缩 + 时效口径）+ 应急表（`tunnels kill`；project kill 持续 pending = agent 在持续重开且被持续拆除——防重开用 disable/revoke）+ 排障表。
- `docs/threat-model.md`：(b) 节补急停/白名单现状；「接口级不暴露」注记。
- `docs/agent-tools.md`：forward_port `listen_host` 参数说明 + 回收口径改 activity。
- `docs/multi-machine.md`：NUC10 VLAN bind 惯用法 + 笔记本恒 loopback + **离线客户端隧道不在 kill/ls 域**。
- `docs/compat-matrix.md`：契约变更行（revoke/disable→隧道 ≤15s 拆（store 健康前提）；forward_port 新参数；新表×3；新 CLI `serve bind`/`tunnels`；audit forward 行 Command 格式变更单列；**kill/ls 域完整性要求全部可写 broker 升级（混合版本边界，rev3）**）——v0.10 系发版编排时并入哪行留 owner 拍板。
- `README.md` 命令清单补两组 CLI。

## §11 任务拆分预览（writing-plans 骨架）

1. store 层：3 表 schemaSQL + CRUD/查询（bind add-rm-ls 规范形/`ListForwardBindHosts`、orders 下单/pending 查询/标记 UPDATE（outcome IS NULL）/7d 行清理、registry upsert/delete/刷新/GC）+ 单测。
2. sshbroker：`Tunnel` onActivity 钩子（Accept + 双向读路径、30s 原子节流）+ `ForwardLocal` listenHost 参数 + 单测。
3. mcpserver/TunnelManager：`AttachStore` + 15s 控制循环（五职责 + fail-the-renewal 计数[含 tick 放弃计入] + 执法读失败有界降级 + 零行续约自关闭）+ fail-the-Open + DELETE 重试 + 镜像接线 + 单测。
4. mcpserver/core：listen_host gate 链（规范形 + 读失败 fail-closed）+ audit 两态 + forward audit 加 id + Input/Output 字段 + 工具描述 + 单测。
5. cli：`serve bind`（规范形）+ `tunnels ls`（LEFT JOIN + 残留标注 + 离线域话术）/`kill`（45s 预算）/`kill --project` + 单测。
6. 契约翻转测试重写 + conformance/eval 补例 + 全量文档联动。
