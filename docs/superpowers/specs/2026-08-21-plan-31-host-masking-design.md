# Plan 31 设计：list_servers host 掩码 + 错误路径清洗（v0.9 破坏性变更）

> backlog #12 · P0。2026-08-21 grilling 已拍板的设计决策（expose_host 布尔默认 false、connect_error 不带 host:port、承诺边界措辞、v0.9 翻默认值 + compat-matrix 登记）不在本文重议；本文是实现设计。brainstorm 澄清补拍板：掩码形态 = 字面量 `"hidden"`；清洗层级 = 源头清洗（sshbroker.Connect）。

## 0. 目标与承诺边界

兑现项目目标「接口级不暴露 IP/端口/凭据」的两个现存违反点：

1. `list_servers` 原样返回明文 host（`internal/mcpserver/core.go:54`；`ServerInfo` 无 Port 字段，端口本就未暴露）。
2. `sshbroker.Connect` 的错误文本含 `host:port`——两层泄露：我们包装里的 `addr`（`internal/sshbroker/client.go:49`）+ 底层 `net.OpError` 文本里的**已解析 IP**（host 为域名时按字面替换拦不住），DNS 失败分支的 `lookup <域名>` 还带 vault host 本身。

承诺边界（写进 threat-model.md / concepts.md）：

- **接口级不暴露**是承诺：list_servers / 工具错误文本 / 工具输出不主动披露 vault 内 host:port 与凭据。
- agent 主动跑 `ip addr` / `hostname` 等命令探出的地址**不算违约**。
- **运行时级隐藏**（命令过滤 / 输出脱敏 / 网络盲化）与**服务器出网管控**明确不做。

## 1. 架构总览

两项独立但同层配合的改动，都落在**接口投影层**，哲学一致：vault 数据完整，出接口按策略投影。

- **host 掩码**：`expose_host` 布尔存 vault（per-server），`ListServersForProfile` 投影时决定回明文还是 `"hidden"`。数据层不动——broker 连接、TOFU、owner 面板、快照里 host 始终完整。
- **错误文本源头清洗**：`sshbroker.Connect` 产出的错误文本从源头不带地址；MCP 四个工具（exec/download/upload/forward）的错误分支原样传递即安全。MCP 层加「全分支无 host」回归断言网。

## 2. 数据模型与存储

- `models.Server` 增 `ExposeHost bool`（`internal/models/models.go`）。
- DB：`servers` 表新列 `expose_host INTEGER NOT NULL DEFAULT 0`。
  - `migrate()` 用 `addColumnIfMissing`（Plan 8 六个 server 文本列同款惯例）；`initSchema` 建表语句同步。
  - 存量行迁移后 = 0 = 掩码——**这就是 v0.9 破坏性变更本体**（v0.8 及之前 list_servers 恒回明文）。
- 七处 SQL 列清单同步：`GetServerByName` / `ListServers` 的 SELECT、`scanServer`、`insertServerTx` / `updateServerTx`（tx.go）、`ExportSnapshot` SELECT、`ImportSnapshot` INSERT、`rebuildServersNullable`（Plan 20 的表重建路径带显式列清单——漏改会在旧库触发重建时**丢掉新列**，后续 SELECT 直接失败）。
- 快照：`SnapshotServer` 增 `ExposeHost bool`（json `"expose_host"`）。**`Snapshot.Version` 不 bump**：旧快照缺字段 → false → 掩码（fail-safe 方向：缺数据落在更严的一侧）；旧 binary 读新快照忽略未知字段。双向兼容、零迁移动作。
- **掩码执行点 = 运行投影的 binary**：在线模式（HTTP MCP 打 serve 端点）在 serve 端投影；离线模式（`mcp --cache` 对本地 cache.bin）在 client 端投影。**两端都升 v0.9 才全掩码**——compat-matrix 迁移列写明。cache.bin 内 host 明文不变（整仓快照本就含全部数据，掩码只发生在给 agent 的投影）。

## 3. 投影行为（list_servers）

- `ListServersForProfile`（core.go）：`srv.ExposeHost == true` → `Host: srv.Host`；否则 → `Host: "hidden"`。
- `ServerInfo.Host` jsonschema 描述改写：`server host; "hidden" = owner has not exposed the address — address the server via its id`。
- 工具描述（server.go:63）同步改写：host 可能是 `"hidden"`（owner 未披露地址），用 server_id 寻址。
- types.go 注释留痕：`"hidden"` 是「空串=显式无」惯例的**唯一 deliberate 例外**——host 从来不可为空，`"hidden"` 让 agent 区分「被 owner 藏了」而非「没有」。
- `User` 字段不动（grilling 未决定掩码，不扩 scope）；`ServerInfo` 无 Port 字段，本就未暴露。

## 4. owner 编辑面

- CLI `servers add`：`--expose-host` bool flag（默认 false）。
- CLI `servers edit`：`--expose-host`，`NoOptDefVal="true"`（裸用 = 开，`--expose-host=false` 显式关），`Changed()` 才应用——与现有 edit 惯例一致。
- TUI：`editfields.go` 字段选择器加布尔项「暴露 Host 给 agent」；`forms.go` 的 `serverDraft` 与提交路径带过；详情页（`servers.go` / `clientpage.go`）显示 expose 状态行。
- 导入路径（CLI `servers import` / TUI importflow）：默认 false，不加开关（导入后按需 edit）。

## 5. 错误清洗实现（sshbroker）

`Connect` 失败分支改为：

```go
if r.err != nil {
    return nil, fmt.Errorf("ssh dial: %w", redactAddr(r.err))
}
```

新增私有函数 `redactAddr`，递归处理两类携带地址的标准错误：

- `*net.OpError` → 重建副本 `Addr: nil`（`Error()` 输出变为 `"dial tcp: connect: connection refused"`，已解析 IP 消失）。
- `*net.DNSError`（DNS 失败嵌在 `OpError.Err` 里）→ 去掉 `Name` 重组（如 `"lookup: no such host"`）——`lookup <域名>` 里的域名就是 vault host，光剥 IP 拦不住。验收标准是输出不含域名，具体措辞 plan 定。

其余错误（`ssh: handshake failed: ...`、`ErrHostKeyMismatch` 等）本身无地址，原样传递。ctx 取消分支（`ctx.Err()`）不变。

owner CLI `ssh` 子命令错误文本随之丢 host——**可接受**（owner 按 name 选目标、TUI / `servers ls` 可见 host），此处留痕。

已核实：全仓无测试断言 `"ssh dial"` 文本；conformance 的 connect/cancel 测试无错误文本断言。改动爆炸半径小。

## 6. 测试与验收

| 层 | 内容 |
|---|---|
| 单测：expose 两态 | store 造两台 server（true/false），`ListServersForProfile` 断言 host 分别 = 明文 / `"hidden"` |
| 单测：redactAddr 纯函数 | IP 直连 refused（OpError 重排）、域名 DNS 失败（DNSError.Name 清除）、无地址错误原样——不跑真 DNS |
| 单测：错误分支无 host（回归网） | 对 4 个 `*ForProfile` 的**每个**错误分支（denied / not found / no_credential / auth_error / hostkey_mismatch / connect_error…）断言 `err.Error()` 不含 `srv.Host` 子串——比 grilling 验收的单点 connect_error 更宽，把「等」字钉成结构化承诺。connect_error 用 `127.0.0.1:1` 真实触发 dial refused |
| conformance | 现有 connect/cancel 测试跑绿（无文本断言，预期零改动） |
| eval | `seedBroker` 的 `models.Server` 加 `ExposeHost: true`——`broker_test.go:109` 的 T1 host 可见断言原样保持（顺带 e2e 覆盖 true 态） |
| 手工双端 | NUC10 升 v0.9 serve + 笔记本：① 在线 list_servers 全 `"hidden"` ② 断网/错端口触发 connect_error 文本无 host ③ owner `servers edit <name> --expose-host` 放开一台 → 双端回明文 ④ cache pull 后 `mcp --cache` 离线态同样掩码 |

## 7. 文档落点

| 文件 | 改动 |
|---|---|
| `threat-model.md` | 新增一节「agent 可见性承诺边界」：接口级不暴露是承诺；agent 主动探测不算违约；运行时级隐藏与出网管控明确不做（链 backlog 不做清单） |
| `concepts.md` | 类比表后加一小段「agent 看得见什么」：元数据 + 可选 host（owner 逐台决定）+ 永不含凭据；错误文本不含地址 |
| `compat-matrix.md` | 破坏性变更表新行 v0.9.0（默认掩码 + 错误清洗；影响：依赖 host 明文的 agent 流程；迁移：两端升 v0.9，需要 host 的服务器显式 `--expose-host`）。已验证组合表行发版后双端实测再回写（惯例） |
| `agent-tools.md` | 字段表 host 行改写（`"hidden"` 语义）、:206 字段清单行、错误表 connect 错误新形态 |
| `server.go:63` | 工具描述改写（§3 已列） |

## 8. 发布与 scope 纪律

- 版本 v0.9.0（`buildinfo` ldflags 注入惯例）。升级顺序铁律不变（快照格式兼容，无强制顺序），但掩码全生效需两端 v0.9（§2 语义）。
- 明确不做（留痕防 re-litigate）：User 掩码、port 暴露（本就无）、per-profile host 策略、运行时隐藏、出网管控——均非本 plan 范围。
