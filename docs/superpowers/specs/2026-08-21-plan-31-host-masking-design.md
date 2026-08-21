# Plan 31 设计：list_servers host 掩码 + 错误路径清洗（v0.9 破坏性变更）

> backlog #12 · P0。2026-08-21 grilling 已拍板的设计决策（expose_host 布尔默认 false、connect_error 不带 host:port、承诺边界措辞、v0.9 翻默认值 + compat-matrix 登记）不在本文重议；本文是实现设计。brainstorm 补拍板：掩码形态 = 字面量 `"hidden"`；清洗层级 = 源头清洗（sshbroker.Connect）。
> 本版为第三版（2026-08-21 二次收敛修订）：清洗兜底并入 DNS 类错误直接降级、定向替换加边界规则、验收改 golden 断言为主、承诺措辞统一 opt-in 口径、包装类型委托 net.Error。

## 0. 目标与承诺边界

兑现项目目标「接口级不暴露 IP/端口/凭据」的两个现存违反点：

1. `list_servers` 原样返回明文 host（`internal/mcpserver/core.go:54`；`ServerInfo` 无 Port 字段，端口本就未暴露）。
2. `sshbroker.Connect` 的错误文本含 `host:port`——两层泄露：我们包装里的 `addr`（`internal/sshbroker/client.go:49`）+ 底层 `net.OpError` 文本里的**已解析 IP**（host 为域名时按字面替换拦不住），DNS 失败分支的 `lookup <域名>` 还带 vault host 本身。

承诺边界（写进 threat-model.md / concepts.md，**三处口径统一**）：

- **默认不披露；owner 显式 opt-in 的 per-server host 除外**——接口（list_servers / 工具错误文本 / 工具输出）默认不主动披露 vault 内 host:port 与凭据；owner 可按服务器显式 `expose_host=true` 放开 host。
- 「端口」按 **host:port 组合口径**执行：目标原句（backlog #12「不暴露 IP/端口/凭据」）中的端口单项在本设计中落实为组合口径——孤立端口号（无 host/IP 组合）不构成披露（依据：`ServerInfo` 无 Port 字段，错误文本中不指向主机的孤立数字没有披露价值）。
- agent 主动跑 `ip addr` / `hostname` 等远端命令探出的地址**不算违约**；同理不防的运行时逃逸还有：agent 调本机 ssh-manager owner CLI（`projects.go:166` 明文打印 `user@host:port`）、直接读离线 client 上的 cache.bin（设计上含全部 host 明文）——threat-model 新节一并点名，防承诺被高估。
- **运行时级隐藏**（命令过滤 / 输出脱敏 / 网络盲化）与**服务器出网管控**明确不做。

## 1. 架构总览

两项独立但同层配合的改动，都落在**接口投影层**，哲学一致：vault 数据完整，出接口按策略投影。

- **host 掩码**：`expose_host` 布尔存 vault（per-server），`ListServersForProfile` 投影时决定回明文还是 `"hidden"`。数据层不动——broker 连接、TOFU、owner 面板、快照里 host 始终完整。
- **错误文本源头清洗**：`sshbroker.Connect` 产出的错误文本从源头不带地址；MCP 四个工具（exec/download/upload/forward）的错误分支原样传递即安全。MCP 层加「全分支无 host」回归断言网。

已核（第一轮全量读码）：MCP 工具面除上述两点外**无其他泄露路径**——`vault.AuthForServer`、TOFU、exec/sudo/download/upload、tunnel、serve HTTP 端点错误、cache pull 元数据逐文件全量读过，错误文本均不含 vault host。

## 2. 数据模型与存储

- `models.Server` 增 `ExposeHost bool`（`internal/models/models.go`）。
- DB：`servers` 表新列 `expose_host INTEGER NOT NULL DEFAULT 0`。
  - `migrate()` 用 `addColumnIfMissing`（Plan 8 六个 server 文本列同款惯例），**必须位于 `rebuildServersNullable` 检查之前**（循 Plan 8 六列位置 store.go:204-221——rebuild 的 INSERT..SELECT 若引用新列，源表必须已有它，追加到 migrate 尾部会让旧库 Open 全炸；已用同型 SQL 复刻实验证实该机制：错误顺序即 `no such column` 失败）；`initSchema` 建表语句同步。
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
- `ServerInfo.Host` jsonschema 描述改写：`server host; "hidden" = owner has not exposed it (default) — address the server via its id`。
- 工具描述（server.go:63）同步改写：host 可能是 `"hidden"`（owner 未披露，默认态），用 server_id 寻址。
- types.go 注释留痕：`"hidden"` 是「空串=显式无」惯例的**唯一 deliberate 例外**——host 从来不可为空，`"hidden"` 让 agent 区分「被 owner 藏了」而非「没有」。注释同时留痕**病态碰撞**：owner 若给某台 server 的真实 host 恰好起名 `hidden` 且 expose，投影值与掩码值不可区分、forward 守卫会误拒该机合法转发——owner 侧文档（managing-servers.md）提示避免此命名；结构性消除（换掩码字面量/加前缀）不做。
- `"hidden"` 误用格杀：`ForwardForProfile` 对 remoteHost 与 `"hidden"` 做**大小写不敏感**比较（`strings.EqualFold`）拒绝——DNS 解析大小写不敏感，`"Hidden"`/`"HIDDEN"` 同样能被恶意解析器捕获，精确匹配会漏（agent 把掩码字面量抄进 forward 的 remote_host 是唯一「使用」通道）。exec/download/upload 按 server_id 寻址，broker 不会拨 "hidden"，无需处理。
- `User` 字段不动（grilling 未决定掩码，不扩 scope）；`ServerInfo` 无 Port 字段，本就未暴露。

## 4. owner 编辑面

- CLI `servers add`：`--expose-host` bool flag（默认 false；pflag BoolVar 自带 NoOptDefVal，与既有 `--clear-credential` 先例同型）。
- CLI `servers edit`：`--expose-host`（裸用 = 开，`--expose-host=false` 显式关），`Changed()` 才应用——与现有 edit 惯例一致。
- TUI（可行性已核：`clearCredentialEditField`（editfields.go:145-163）的 huh.NewConfirm + 已勾选/未勾选渲染 + Set("true") 就是现成布尔字段先例）：
  - `editfields.go` 字段选择器加布尔项「暴露 Host 给 agent」；**连带测试锁同步**：`snapshotDraft` 是显式键值 map 且被 `TestEditFieldsKeysMatchSnapshot` 锁「恰好 15 键」（editfields_test.go:97-98），editfields.go:25 与 editfields_test.go:31 的「编辑态 15 项/新增态 14 项」计数注释、editpage_test.go:199 用**下标 8** 定位唯一 Confirm 字段——新布尔项插入位置会移动这些下标/计数，全部同步。
  - **静默复位是本节头号失败模式**：`updateServerTx` 写全行，而 TUI 提交走 `toParts()` 构造**全新** models.Server（仅回填 ID/Tags）——必须同时改 `prefill()`（forms.go:203-209，把 `cur.ExposeHost` 带进 draft）与 `toParts()`（forms.go:216-226，写出），漏 prefill 则 owner 在 TUI 改任何别的字段都会把 expose_host 刷回 false。**该失败模式有专门回归测试锁定（见 §6），不靠实现时记得。**
  - 详情页（`servers.go` Detail / `clientpage.go` clientServerDetail）显示 expose 状态行。
- 导入路径：CLI `servers import` / TUI importflow 零值默认 false 自动成立；importflow 的 `submitSupplement`（importflow.go:321-329）同属全新对象全行重写位点，随手带 `ExposeHost: f.srv.ExposeHost` 防未来同型静默复位。

## 5. 错误清洗实现（sshbroker）

`Connect` 失败分支改为：

```go
if r.err != nil {
    return nil, fmt.Errorf("ssh dial: %w", redactAddr(r.err))
}
```

`redactAddr` 设计（两步：**定向清洗 + 无把握整体丢弃**）：

- 返回私有包装错误类型（形如 `addrRedactedError{msg, err}`）：`Error()` 返回清洗后文本，`Unwrap()` 返回**原错误链**——`core.go` 四处 `errors.Is(cerr, sshbroker.ErrHostKeyMismatch)` 的审计状态分类（hostkey_mismatch vs connect_error）必须继续工作，丢 Unwrap 即破。包装类型同时**委托实现 `net.Error`**（`Timeout()`/`Temporary()` 转发到链上可找到的 net.Error，实验证实委托后 `err.(net.Error)` 断言恢复且 errors.Is 穿透保持）——防任何调用点（含未来新增）对 Connect 错误做 net.Error 类型断言静默失效；当前全仓无现存断言点，属前瞻性加固。
- **类型不变量（注释留痕）**：`Unwrap()` 暴露的原始错误链含 host/IP 明文——**任何日志、审计、持久化路径不得打印 cause 链文本**，只允许 `Error()` 的清洗后文本外流。此不变量写进类型注释。
- **为什么不能只重建叶子结构**：`ssh.Dial` 握手期失败返回 `fmt.Errorf("ssh: handshake failed: %w", opErr)`，而 `fmt.wrapError` 在创建时一次性渲染并缓存文本——事后重建链上 OpError 副本改不了外层已缓存文本，握手期 IO 错误（防火墙 accept 后 RST、对非 SSH 端口拨号 reset）里的已解析 IP 原样存活。故清洗在**最终文本**上做，不做结构手术。
- **为什么逐段擦除也不可靠（实验实证，产物留 `.xcheck` exp/）**：① 朴素正则漏 IPv6 zone 形态（`fe80::1%eth0`）；② token + `net.ParseIP` 漏 zone（stdlib 不解析）且漏带端口整 token；③ 短 host 字面量子串替换绞碎无关文本；④ 剥 IP 后残留孤立 `:22` 而全局剥端口数字必误杀 errno/指纹；⑤ search-domain 形态（host=`foo`、文本 `lookup foo.corp.internal`）两步都洗不净——`\b` 词边界对点分域名不构成边界，子串替换后域后缀仍泄露。结论：**定向规则只用于已知形态，任何残留地址形态触发整体降级，不做逐段兜底擦除。**
- 两步清洗：
  1. **定向替换（边界感知）**：host 的归一形态（大小写不敏感 + 去尾点）与 addr 各形态——**仅当 host 以独立 token 出现**（前后为非主机名字符：空格/括号/引号/串首串尾，且**不以点号与更长名字相连**——`\b` 对点分域名不构成边界，见实验 ⑤）或以 `host:port` / `[host]:port` / `net.JoinHostPort` 完整形态出现时替换为 `[REDACTED]`。**已知副作用留痕**：短 host（如 `db`、`a`）即使边界感知也可能命中无关单词（fail-safe 方向：只损诊断不泄露），实现注释承认该取舍。
  2. **分类兜底（无把握整体丢弃）**：满足任一即整体降级为泛化文本——
     - 定向替换后仍检出地址形态：IPv4 字面量、IPv6 字面量（含带括号、含 zone 后缀）、`host:port` 组合；
     - **DNS 类错误**：`errors.As` 检出 `*net.DNSError`，或文本含 `lookup <name>` 形态——DNS 错误的主机名形态（search domain 追加、resolver 改写）不可枚举，一律不尝试替换，直接整体降级（实验 ⑤ 实证）。
     - 泛化文本形如 `ssh dial: connect failed: <原因短语>`；**原因短语映射表在 plan 阶段冻结为字面量表**（errors.Is 归类 → 固定短语），它是验收断言的直接输入。
- 顺带修存量 bug：client.go:36 的 `fmt.Sprintf("%s:%d", host, port)` 改 `net.JoinHostPort(host, strconv.Itoa(port))`（port 为 int，JoinHostPort 收 string）——IPv6 字面量 host 今天拼出畸形地址根本连不上，且其 `*net.AddrError` 文本直接泄露 host；owner 把 `host:port` 整串误填进 host 字段同理。
- 其余错误（`ssh: handshake failed: ...`、`ErrHostKeyMismatch` 等）统一走同一包装。ctx 取消分支（`ctx.Err()` 直返）不变。
- **验收标准（组合口径）**：清洗后文本不含 host（含大小写/尾点变体）、不含 IP 字面量（IPv4/IPv6/带 zone/带括号）、不含 `host:port` 组合。孤立端口号（无 host/IP 组合）不构成披露面（§0 口径）。

owner CLI `ssh` 子命令错误文本随之丢 host——**可接受**（owner 按 name 选目标、TUI / `servers ls` 可见 host），此处留痕。已核实全仓无测试断言 `"ssh dial"` 文本、conformance connect/cancel 无文本断言，改动爆炸半径小。

## 6. 测试与验收

验收方法学（全表适用）：**构造用例以 golden 输出断言为主**（每个用例钉精确期望文本）——否定正则（不含 host/不含 IP 形态）仅作辅助防漏；兜底降级路径用「输入 → 泛化文本字面量」断言。原因：验收正则与兜底检测器若同一套，检测盲点=泄露盲点（循环验证）；golden 断言使两套检查相互独立。

| 层 | 内容 |
|---|---|
| 单测：expose 两态 | store 造两台 server（true/false），`ListServersForProfile` 断言 host 分别 = 明文 / `"hidden"`。已核现有 mcpserver 投影测试无一断言 Host 明文，默认掩码不破存量 |
| 单测：redactAddr 纯函数 | **八用例（各带 golden 输出断言）**：IP 直连 refused、DNS 失败（构造 DNSError，不跑真 DNS）、TCP 通握手期 RST（拨非 SSH 端口，覆盖 fmt.wrapError 缓存文本形态）、DNS 大小写+尾点形态、DNS search-domain 形态（host=`foo`、文本 `lookup foo.corp.internal` → 断言整体降级，实验 ⑤）、IPv6 zone 形态、畸形 AddrError（`%s:%d` 拼接历史对照，验证 JoinHostPort 修复）、无地址错误原样。另加**短 host 用例**（host=`db` + 文本含无关 "db" → 断言已清洗且无关词未误伤）。断言辅助：输出不含 host 变体/不匹配 IP 字面量/不含 host:port 组合；**包装类型 `err.(net.Error)` 断言成立 + `errors.Is` 穿透 Unwrap 对 ErrHostKeyMismatch/原始错误仍成立** |
| 单测：错误分支无 host（回归网） | 对 4 个 `*ForProfile` 的每个错误分支断言 `err.Error()` **不含 `srv.Host` 子串且不匹配 IP 字面量正则**。connect_error 用 `127.0.0.1:1` 真实触发。分支枚举：denied / not found / no_credential / auth_error / connect 三态（cancelled / hostkey_mismatch / connect_error）+ forward 的 `remoteHost == "hidden"` 拒绝分支（含大小写变体 `"Hidden"`）——注意 hostkey_mismatch 实际经由 connect 错误路径浮现（`HostKeyTOFU` 构造期恒返 nil，core.go 的独立 `herr` 分支是死代码），测试不必覆盖 herr 分支 |
| 单测：TUI 静默复位回归 | `prefill()`/`toParts()` 复制点的自动防线：对 expose_host=true 的 server 走 TUI 编辑**其他字段**并保存，断言 store 中 expose_host 仍为 true（editpage/editfields 既有测试形态上加断言）；`submitSupplement` 同型断言 |
| 单测：migrate 旧库 fixture | 构造 pre-Plan-8/20 形态旧库（无 metadata 列、credential_id NOT NULL）→ `store.Open` → 断言 expose_host 列存在、存量行数据保全、rebuild 路径走通——锁「addColumnIfMissing 先于 rebuild」的顺序声明（机制已实验证实，见 §2/§5） |
| 单测：快照 round-trip | `ExportSnapshot` → `ImportSnapshot`（新→新）断言 `expose_host=true` 与 `false` 两态均不丢——防 SQL 列清单/序列化回归静默破坏 owner 偏好（fail-safe 掩码仍会丢偏好，需自动防线） |
| conformance | 现有 connect/cancel 测试跑绿（无文本断言，预期零改动） |
| eval | `seedBroker` 构造点（broker.go:108-113）加 `ExposeHost: true`——理由是 e2e 覆盖暴露态（`broker_test.go:109` 读的是 owner 侧 `st.ListServers()`，store 层永掩码，该断言本就与 ExposeHost 无关）。`wireBrokerLocked` / `wireBrokerTwoProfile` 不加，走默认掩码（T5/T8 按 id 寻址不受影响） |
| 手工双端 | NUC10 升 v0.9 serve + 笔记本：① 在线 list_servers 掩码台为 `"hidden"` ② 错端口触发 connect_error 文本无 host/IP ③ owner `servers edit <name> --expose-host` 放开一台 → 在线回明文 ④ cache pull 后 `mcp --cache` 离线态与各自 expose 状态一致（未放开台 `"hidden"`、已放开台明文） |

## 7. 文档落点

| 文件 | 改动 |
|---|---|
| `threat-model.md` | 新增一节「agent 可见性承诺边界」：**默认不披露（host:port 组合口径），owner 显式 opt-in 的 per-server host 除外**；agent 主动远端探测不算违约；**本机运行时逃逸明确不防**（owner CLI 明文打印 host、cache.bin 含全部 host）；运行时级隐藏与出网管控明确不做（链 backlog 不做清单） |
| `concepts.md` | 类比表后加一小段「agent 看得见什么」：元数据 + 可选 host（owner 逐台 opt-in，默认掩码）+ 永不含凭据；错误文本不含地址 |
| `compat-matrix.md` | 破坏性变更表新行 v0.9.0。**变更** = 默认掩码（`"hidden"`）+ 错误文本清洗。**影响**：serve 升级瞬间在线 agent 即全 hidden（依赖 host 的流程当场断）；v0.9 serve + 未升级 client 的离线模式仍回明文；**旧 binary 导入新快照丢 expose_host=true 偏好（混合版本窗口，方向 fail-safe：折回掩码）**。**迁移**：升级顺序按铁律惯例 client 先、serve 后（本变更技术上无顺序硬约束——快照双向兼容；该顺序服务于「掩码尽快全生效」：在线掩码随 serve 升级即刻生效、离线掩码需 client 升级）。**依赖 host 明文的 agent 流程唯一补救 = 升级前对相应服务器 `servers edit <name> --expose-host`**。已验证组合表行发版后双端实测再回写（惯例） |
| `agent-tools.md` | 字段表 host 行（:48）改写（`"hidden"` 语义）、字段清单行（**:207**）同步、错误表 connect 错误新形态 |
| `README.md` | "What the agent gets" 表（:42）host 措辞改 `"hidden"` 语义；可循 v0.4.0 callout 先例加 v0.9 破坏性变更提示 |
| `agent-access.md` | 验证节 canonical 表述（:100）同步 host 掩码语义（opt-in 口径） |
| `managing-servers.md` | owner 侧 flag 权威参考：add flag 块（:44-61）+ edit flag 全表（:193-209）补 `--expose-host`，心智模型关键字段（:13）提及；**留痕**：避免给真实主机命名 `hidden`（投影值与掩码值不可区分、forward 守卫误拒） |
| `server.go:63` | 工具描述改写（§3 已列） |

已核其余 docs（getting-started / scenarios / tui-multi-machine / multi-machine / quickstart×2）无 list_servers host 返回表述，不需同步；仓库无 CHANGELOG 惯例（破坏性变更登记职能由 compat-matrix 维护规则承担）。

## 8. 发布与 scope 纪律

- 版本 v0.9.0（tag `v0.9.0`，`buildinfo` ldflags 注入惯例；minor bump 载破坏性变更有 v0.4.0 / v0.7.0 先例）。
- 升级顺序：按铁律惯例 **client 先、serve 后**——本变更技术上无顺序硬约束（快照双向兼容），该顺序服务于「掩码尽快全生效」（在线掩码随 serve 升级即刻生效，离线掩码需 client 升级）。**依赖 host 明文的 agent 流程没有升级顺序解，唯一补救是升级前 `--expose-host` 放开**——此点与快照偏好丢失（§7 影响列）均写进 compat-matrix。
- 明确不做（留痕防 re-litigate）：User 掩码、port 暴露（本就无）、per-profile host 策略、运行时隐藏、出网管控、"hidden" 碰撞的结构性消除——均非本 plan 范围。
