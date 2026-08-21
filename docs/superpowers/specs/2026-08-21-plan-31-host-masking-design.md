# Plan 31 设计：list_servers host 掩码 + 错误路径清洗（v0.9 破坏性变更）

> backlog #12 · P0。2026-08-21 grilling 已拍板的设计决策（expose_host 布尔默认 false、connect_error 不带 host:port、承诺边界措辞、v0.9 翻默认值 + compat-matrix 登记）不在本文重议；本文是实现设计。brainstorm 补拍板：掩码形态 = 字面量 `"hidden"`；清洗层级 = 源头清洗（sshbroker.Connect）。
> xcheck 三路异构评审（安全 / 可行性 / 文档一致性）2026-08-21 收敛，17 项 findings 全部吸收——§2 位点清单补 getServerTx、§5 清洗机制重设计（fmt.wrapError 缓存文本问题）、§6 断言加宽与用例补 handshake-RST、§7 文档清单补三处、§8 顺序措辞修正。

## 0. 目标与承诺边界

兑现项目目标「接口级不暴露 IP/端口/凭据」的两个现存违反点：

1. `list_servers` 原样返回明文 host（`internal/mcpserver/core.go:54`；`ServerInfo` 无 Port 字段，端口本就未暴露）。
2. `sshbroker.Connect` 的错误文本含 `host:port`——两层泄露：我们包装里的 `addr`（`internal/sshbroker/client.go:49`）+ 底层 `net.OpError` 文本里的**已解析 IP**（host 为域名时按字面替换拦不住），DNS 失败分支的 `lookup <域名>` 还带 vault host 本身。

承诺边界（写进 threat-model.md / concepts.md）：

- **接口级不暴露**是承诺：list_servers / 工具错误文本 / 工具输出不主动披露 vault 内 host:port 与凭据。
- agent 主动跑 `ip addr` / `hostname` 等远端命令探出的地址**不算违约**；同理不防的运行时逃逸还有：agent 调本机 ssh-manager owner CLI（`projects.go:166` 明文打印 `user@host:port`）、直接读离线 client 上的 cache.bin（设计上含全部 host 明文）——threat-model 新节一并点名，防承诺被高估。
- **运行时级隐藏**（命令过滤 / 输出脱敏 / 网络盲化）与**服务器出网管控**明确不做。

## 1. 架构总览

两项独立但同层配合的改动，都落在**接口投影层**，哲学一致：vault 数据完整，出接口按策略投影。

- **host 掩码**：`expose_host` 布尔存 vault（per-server），`ListServersForProfile` 投影时决定回明文还是 `"hidden"`。数据层不动——broker 连接、TOFU、owner 面板、快照里 host 始终完整。
- **错误文本源头清洗**：`sshbroker.Connect` 产出的错误文本从源头不带地址；MCP 四个工具（exec/download/upload/forward）的错误分支原样传递即安全。MCP 层加「全分支无 host」回归断言网。

xcheck 已核（安全维度）：MCP 工具面除上述两点外**无其他泄露路径**——`vault.AuthForServer`、TOFU、exec/sudo/download/upload、tunnel、serve HTTP 端点错误、cache pull 元数据逐文件全量读过，错误文本均不含 vault host。

## 2. 数据模型与存储

- `models.Server` 增 `ExposeHost bool`（`internal/models/models.go`）。
- DB：`servers` 表新列 `expose_host INTEGER NOT NULL DEFAULT 0`。
  - `migrate()` 用 `addColumnIfMissing`（Plan 8 六个 server 文本列同款惯例），**必须位于 `rebuildServersNullable` 检查之前**（循 Plan 8 六列位置 store.go:204-221——rebuild 的 INSERT..SELECT 若引用新列，源表必须已有它，追加到 migrate 尾部会让 pre-Plan-20 旧库 Open 全炸）；`initSchema` 建表语句同步。
  - 存量行迁移后 = 0 = 掩码——**这就是 v0.9 破坏性变更本体**（v0.8 及之前 list_servers 恒回明文）。
- SQL 显式列清单同步位点（以 grep 为准，不设计数；每处漏改都是运行期炸点）：
  1. **`getServerTx`（tx.go:76，by-id SELECT）——最优先**：`scanServer` 加第 18 个目的地而它不加列 → Scan 列数不匹配 → `GetServer` 恒错 → `ListServersForProfile` 静默 continue → **list_servers 静默空列表**，owner edit/级联删除全报错。
  2. `GetServerByName` / `ListServers` 的 SELECT（servers.go:47/:58）。
  3. `scanServer`（servers.go:101）。
  4. `insertServerTx` / `updateServerTx`（tx.go:35/:60；updateServerTx 写全行）。
  5. `ExportSnapshot` SELECT（export.go:119）、`ImportSnapshot` INSERT（export.go:309）。
  6. `rebuildServersNullable`——**两份**列清单（CREATE TABLE servers_new store.go:318-336 + INSERT..SELECT store.go:337-338），漏改会在旧库触发重建时丢列。
- 快照：`SnapshotServer` 增 `ExposeHost bool`（json `"expose_host"`）。**`Snapshot.Version` 不 bump**：旧快照缺字段 → false → 掩码（fail-safe 方向：缺数据落在更严的一侧）；旧 binary 读新快照忽略未知字段（其 INSERT 列清单亦无该列）。双向兼容、零迁移动作；`serve_snapshot_test.go:66` 断言 `Version == 1` 与不 bump 自洽。已核全链路（ImportSnapshot / LoadCacheSnapshot / DoPull / cli import）无 Version 门。
- **掩码执行点 = 运行投影的 binary**：在线模式（agent `.mcp.json` 直连 serve 的 MCP 端点）工具闭包在 serve 进程内跑（`mcpserver/serve.go:54-66`）——**serve 单端升 v0.9 在线即全掩码**；离线模式（`mcp --cache` 对本地 cache.bin）投影在 client 端（`run.go:241`）——client 不升级则离线态仍明文。cache.bin 内 host 明文不变（整仓快照本就含全部数据，掩码只发生在给 agent 的投影）。

## 3. 投影行为（list_servers）

- `ListServersForProfile`（core.go）：`srv.ExposeHost == true` → `Host: srv.Host`；否则 → `Host: "hidden"`。
- `ServerInfo.Host` jsonschema 描述改写：`server host; "hidden" = owner has not exposed the address — address the server via its id`。
- 工具描述（server.go:63）同步改写：host 可能是 `"hidden"`（owner 未披露地址），用 server_id 寻址。
- types.go 注释留痕：`"hidden"` 是「空串=显式无」惯例的**唯一 deliberate 例外**——host 从来不可为空，`"hidden"` 让 agent 区分「被 owner 藏了」而非「没有」。
- `"hidden"` 误用格杀（xcheck L4）：`ForwardForProfile` 对 `remoteHost == "hidden"` 一行拒绝（agent 把掩码字面量抄进 forward 的 remote_host 是唯一「使用」通道——服务器侧解析器若有恶意 "hidden" 记录可捕获误抄流量）。exec/download/upload 按 server_id 寻址，broker 不会拨 "hidden"，无需处理。
- `User` 字段不动（grilling 未决定掩码，不扩 scope）；`ServerInfo` 无 Port 字段，本就未暴露。

## 4. owner 编辑面

- CLI `servers add`：`--expose-host` bool flag（默认 false；pflag BoolVar 自带 NoOptDefVal，与既有 `--clear-credential` 先例同型）。
- CLI `servers edit`：`--expose-host`（裸用 = 开，`--expose-host=false` 显式关），`Changed()` 才应用——与现有 edit 惯例一致。
- TUI（可行性已核：`clearCredentialEditField`（editfields.go:145-163）的 huh.NewConfirm + 已勾选/未勾选渲染 + Set("true") 就是现成布尔字段先例）：
  - `editfields.go` 字段选择器加布尔项「暴露 Host 给 agent」；**连带测试锁同步**：`snapshotDraft` 是显式键值 map 且被 `TestEditFieldsKeysMatchSnapshot` 锁「恰好 15 键」（editfields_test.go:97-98），editfields.go:25 与 editfields_test.go:31 的「编辑态 15 项/新增态 14 项」计数注释、editpage_test.go:199 用**下标 8** 定位唯一 Confirm 字段——新布尔项插入位置会移动这些下标/计数，全部同步。
  - **静默复位是本节头号失败模式**：`updateServerTx` 写全行，而 TUI 提交走 `toParts()` 构造**全新** models.Server（仅回填 ID/Tags）——必须同时改 `prefill()`（forms.go:203-209，把 `cur.ExposeHost` 带进 draft）与 `toParts()`（forms.go:216-226，写出），漏 prefill 则 owner 在 TUI 改任何别的字段都会把 expose_host 刷回 false。
  - 详情页（`servers.go` Detail / `clientpage.go` clientServerDetail）显示 expose 状态行。
- 导入路径：CLI `servers import` / TUI importflow 零值默认 false 自动成立；importflow 的 `submitSupplement`（importflow.go:321-329）同属全新对象全行重写位点，随手带 `ExposeHost: f.srv.ExposeHost` 防未来同型静默复位。

## 5. 错误清洗实现（sshbroker）

`Connect` 失败分支改为：

```go
if r.err != nil {
    return nil, fmt.Errorf("ssh dial: %w", redactAddr(r.err))
}
```

`redactAddr` 设计（xcheck H1 修订：结构重建改为**保留原链的清洗包装**）：

- 返回私有包装错误类型（形如 `addrRedactedError{msg, err}`）：`Error()` 返回清洗后文本，`Unwrap()` 返回**原错误链**——`core.go` 四处 `errors.Is(cerr, sshbroker.ErrHostKeyMismatch)` 的审计状态分类（hostkey_mismatch vs connect_error）必须继续工作，丢 Unwrap 即破。
- **为什么不能只重建叶子结构**：`ssh.Dial` 握手期失败返回 `fmt.Errorf("ssh: handshake failed: %w", opErr)`，而 `fmt.wrapError` 在创建时一次性渲染并缓存文本——事后重建链上 OpError 副本改不了外层已缓存文本，握手期 IO 错误（防火墙 accept 后 RST、对非 SSH 端口拨号 reset）里的已解析 IP 原样存活。故清洗在**最终文本**上做，不做结构手术。
- 文本清洗的确定性输入：`Connect` 自己知道 host 与 addr。清洗集 = host 字面量、addr（含 JoinHostPort 形态）、IPv4 字面量、带括号 IPv6 字面量、网络错误语境中的裸 IPv6。OpError 的 `Source`（本端地址）与 DNSError 的 `Server`（resolver 地址）由 IP 字面量规则一并覆盖。验收标准：清洗后文本不含 host、不含任何 IP 字面量、不含端口。
- 顺带修存量 bug（xcheck M1）：client.go:36 的 `fmt.Sprintf("%s:%d", host, port)` 改 `net.JoinHostPort(host, port)`——IPv6 字面量 host 今天拼出畸形地址根本连不上，且其 `*net.AddrError` 文本（`address 2001:db8::1:22: too many colons...`）直接泄露 host；owner 把 `host:port` 整串误填进 host 字段同理。
- 其余错误（`ssh: handshake failed: ...`、`ErrHostKeyMismatch` 等）本身无地址或被文本规则覆盖，统一走同一包装。ctx 取消分支（`ctx.Err()` 直返）不变。

owner CLI `ssh` 子命令错误文本随之丢 host——**可接受**（owner 按 name 选目标、TUI / `servers ls` 可见 host），此处留痕。已核实全仓无测试断言 `"ssh dial"` 文本、conformance connect/cancel 无文本断言，改动爆炸半径小。

## 6. 测试与验收

| 层 | 内容 |
|---|---|
| 单测：expose 两态 | store 造两台 server（true/false），`ListServersForProfile` 断言 host 分别 = 明文 / `"hidden"`。已核现有 mcpserver 投影测试无一断言 Host 明文，默认掩码不破存量 |
| 单测：redactAddr 纯函数 | 四用例：IP 直连 refused、DNS 失败（构造 DNSError，不跑真 DNS）、**TCP 通握手期 RST（拨非 SSH 端口，覆盖 fmt.wrapError 缓存文本形态）**、无地址错误原样。断言：输出不含 host、不匹配 IPv4/IPv6 字面量、不含端口；`errors.Is` 穿透 `Unwrap` 对 ErrHostKeyMismatch/原始错误仍成立 |
| 单测：错误分支无 host（回归网） | 对 4 个 `*ForProfile` 的每个错误分支断言 `err.Error()` **不含 `srv.Host` 子串且不匹配 IP 字面量正则**（hostname 型 server 的泄露物是解析后 IP，只查子串全盲——xcheck M2）。connect_error 用 `127.0.0.1:1` 真实触发。分支枚举：denied / not found / no_credential / auth_error / connect 三态（cancelled / hostkey_mismatch / connect_error）——注意 hostkey_mismatch 实际经由 connect 错误路径浮现（`HostKeyTOFU` 构造期恒返 nil，core.go 的独立 `herr` 分支是死代码），测试不必覆盖 herr 分支 |
| conformance | 现有 connect/cancel 测试跑绿（无文本断言，预期零改动） |
| eval | `seedBroker` 构造点（broker.go:108-113）加 `ExposeHost: true`——理由是 e2e 覆盖暴露态（`broker_test.go:109` 读的是 owner 侧 `st.ListServers()`，store 层永掩码，该断言本就与 ExposeHost 无关）。`wireBrokerLocked` / `wireBrokerTwoProfile` 不加，走默认掩码（T5/T8 按 id 寻址不受影响） |
| 手工双端 | NUC10 升 v0.9 serve + 笔记本：① 在线 list_servers 掩码台为 `"hidden"` ② 错端口触发 connect_error 文本无 host/IP ③ owner `servers edit <name> --expose-host` 放开一台 → 在线回明文 ④ cache pull 后 `mcp --cache` 离线态与各自 expose 状态一致（未放开台 `"hidden"`、已放开台明文） |

## 7. 文档落点

| 文件 | 改动 |
|---|---|
| `threat-model.md` | 新增一节「agent 可见性承诺边界」：接口级不暴露是承诺；agent 主动远端探测不算违约；**本机运行时逃逸明确不防**（owner CLI 明文打印 host、cache.bin 含全部 host）；运行时级隐藏与出网管控明确不做（链 backlog 不做清单） |
| `concepts.md` | 类比表后加一小段「agent 看得见什么」：元数据 + 可选 host（owner 逐台决定）+ 永不含凭据；错误文本不含地址 |
| `compat-matrix.md` | 破坏性变更表新行 v0.9.0：变更 = 默认掩码（`"hidden"`）+ 错误文本清洗；**影响列**含「serve 升级瞬间在线 agent 即全 hidden；v0.9 serve + 未升级 client 的离线模式仍回明文」；**迁移列给顺序**（该文件维护规则要求）：按铁律惯例 client 先 serve 后，本变更技术上无顺序硬约束（快照双向兼容），依赖 host 明文的 agent 流程先 `--expose-host` 放开或先升 client。已验证组合表行发版后双端实测再回写（惯例） |
| `agent-tools.md` | 字段表 host 行（:48）改写（`"hidden"` 语义）、字段清单行（**:207**）同步、错误表 connect 错误新形态 |
| `README.md` | "What the agent gets" 表（:42）host 措辞改 `"hidden"` 语义；可循 v0.4.0 callout 先例加 v0.9 破坏性变更提示 |
| `agent-access.md` | 验证节 canonical 表述（:100）同步 host 掩码语义 |
| `managing-servers.md` | owner 侧 flag 权威参考：add flag 块（:44-61）+ edit flag 全表（:193-209）补 `--expose-host`，心智模型关键字段（:13）提及 |
| `server.go:63` | 工具描述改写（§3 已列） |

已核其余 docs（getting-started / scenarios / tui-multi-machine / multi-machine / quickstart×2）无 list_servers host 返回表述，不需同步。

## 8. 发布与 scope 纪律

- 版本 v0.9.0（tag `v0.9.0`，`buildinfo` ldflags 注入惯例；minor bump 载破坏性变更有 v0.4.0 / v0.7.0 先例）。
- 升级顺序：**按铁律惯例 client 先、serve 后**。本变更技术上无顺序硬约束（快照双向兼容），但有两点必须写进 compat-matrix（§7）：serve 升级瞬间在线 agent 即全 `"hidden"`（行为悬崖，依赖 host 的流程当场断）；未升级 client 的离线模式仍回明文（掩码执行点语义，§2）。
- 明确不做（留痕防 re-litigate）：User 掩码、port 暴露（本就无）、per-profile host 策略、运行时隐藏、出网管控——均非本 plan 范围。
