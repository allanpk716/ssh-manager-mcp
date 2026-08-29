# 部署形态全景：单机 / 多机桥姿态 + 管理面（选型总览 · Plan 42 起 4→2）

> **场景**：你的目标是"让 AI agent 用 SSH 控制我的服务器"。本页一屏看全本项目共有哪几种部署方案、各自的优劣、怎么选——**第一次选部署方式，或忘了有几种用法时，看这篇**。每种姿势的具体配置步骤链接到对应展开文档。
>
> **Plan 42 批1 起的模式缩减（4 → 2 + 管理面）**：旧的 ②a「在线 HTTP 直连」已**移除**（serve 不再提供任何远程 MCP 面——根路径 404，不是降级）；②c 降为应急附录；客户端 TUI 向导的连接表单退役。多机从此只有一条接入姿势：**桥姿态**（工作机 = 本地只读缓存的零距离 client），配对入口 = `sshmgr pair` 一条龙。

---

## ⭐ v0.13.0 一次性迁移 runbook（唯一总册：批1 ②a 桥迁 + 二进制改名 ssh-manager → sshmgr）

> v0.13.0 = Plan 42 批1（模式缩减 + 发现 + SAS 配对）与 Plan 44（自更新 + 改名）**同批捆发**——一次 breaking，一次迁移。本节是唯一迁移总册（spec §3.2 定稿原文），顺序不可乱。

**顺序不可乱：先迁 client 后升 serve**（批1 移除 ②a——serve 先升会断掉旧 HTTP MCP 客户端）：

```
# ① ②a 存量桥迁(旧 serve 还在跑时完成;= 批1 G2):
#    各 client 机 agent 从 ②a HTTP 直连姿态迁到 stdio 桥(--cache)
# ② client 机改名(笔记本):
#    v0.13.0 资产解压,sshmgr.exe 替换旧 ssh-manager.exe(旧的最后删/改名)
#    .mcp.json 的 command 路径同步改指 sshmgr.exe
# ③ serve 机迁移(最后;NUC10,管理员 shell):
#    读旧服务参数(Windows: sc qc ssh-manager-serve 记下 --addr/--tls-cert/--tls-key)
#    旧 binary: ssh-manager serve uninstall
#    curl -LO <v0.13.0 资产> + curl -LO checksums.txt,SHA256 核验(certutil/sha256sum),解压到位
#    sshmgr serve install <照旧参数(--addr 0.0.0.0:7878 及 TLS flags 若有)>
# ④ 之后:sshmgr update 一条命令自续
```

**旧服务检测（三分法）**：update **无条件**探测新旧两名（kardianos `service.New`→`Status()`），结果分三类处理——

- **已装（任何态：Running/Stopped/failed/Unknown）**：按已装继续（failed 态恰是崩溃循环、最需要 update 的场景，不得封死；Linux systemd 对 failed 态 `Status()` 返回错误——须按「已装」分类而非机制错误）
- **未装**（`ErrNotInstalled`）：放行
- **探测机制错误**（无法判定存在性）：fail-closed 中止

旧名存在（任何态，无论新名状态）→ 打印迁移块并中止（不半更新，防新旧服务并存）；`ErrNoServiceSystemDetected`（容器/CI 无服务管理器）→ 跳过检测直接更新。

## v0.13.0 之后：升级 = `sshmgr update` 一条命令

一次迁移完成后，双端升级不再手工「停服务 → 换二进制 → 重启」：

```
sshmgr update                        # 检查→显示 当前→最新→确认→下载→校验→替换→(服务则重启)
sshmgr update --check                # 干跑：只报 当前/最新/资产名/update base，不改任何东西
sshmgr update --yes                  # 免确认（远程/脚本；非 TTY 必需；服务重启亦视为同意）
sshmgr update --version v0.13.1      # 装指定版（含降级=回滚通道；降级有显式警告）
sshmgr update --file <包> [--sha256 <hex> | --no-verify]   # 本地包模式（离线/内网兜底）
```

- **版本源**：GitHub Releases 直连（`releases/latest`，天然排除 prerelease/draft；未认证 API 限速 60/h/IP——手动更新场景足够）；`--version <tag>` 可钉版/降级。镜像/内网用 `SSHMGR_UPDATE_BASE` env seam 换源（非环回强制 https，证据行醒目显示生效 base）。
- **信任链**：强制 https（仅环回字面量例外）+ 重定向**每一跳**宿主白名单 + 同 release `checksums.txt` SHA256 比对——校验不过即中止，目标文件零触碰；解压只落地根条目精确名 `sshmgr`（Windows `sshmgr.exe`），zip slip 不可能。
- **事务性替换**：临时目录建在 exe 同目录（同卷原子 rename）；替换点之前的任何失败 = 零变更；Windows 走 `.old` 代际名 + 崩溃窗口启动自愈；exe 目录不可写（如 `/usr/local/bin`）→ 明确报错提示提权，**update 自身永不自动提权**。
- **服务重启**：serve 机替换成功后询问重启（LocalSystem 服务非提升会话 `Restart()` 必 Access denied——NUC10 常态，不算更新失败；失败打印手工命令 + 专用退出码「替换成功/重启待手工」，成功后健康回探）。**重启将断开活动隧道、作废进行中的配对请求**。client 机未装服务：新版本下次 agent 会话生效，运行中的桥继续旧版。
- **升级次序铁律不因 update 改变**：多机拓扑仍「先迁 client 后升 serve」；无后台自动检查/自动更新，升级时点由 owner 手动拍板。
- 并发跑两个 update 无锁保护——**不要并发**。

---

## 共同底座（无论哪种姿势都一样）

- **授权模型**：`Server（机器+凭据）→ grant → Profile（分组）← bind ← Project（token）`。agent 拿一个 project token，只能碰它绑定的 profile 里的服务器，跨 profile 一律拒绝（详见 [agent-access.md](./agent-access.md)）。
- **工具面**：同样 10 个 MCP 工具（`list_servers` / `exec_command` / `exec_background` / `exec_output` / `exec_stop` / `download_file` / `upload_file` / `upload_content` / `forward_port` / `close_port`），语义一致（手册：[agent-tools.md](./agent-tools.md)）。
- **铁律**：凭据（密码/私钥）永远不出加密 vault；agent 只拿到命令输出 / 文件字节 / 转发端口；全程审计。
- **写边界（Plan 42 新铁律）**：**多机 agent 只读 + 执行**（离线缓存形态，写操作一律 `ErrReadOnly`）——加改删服务器 / 发码 / 批准配对等一切写操作，只在**管理面**做（broker TUI / `serve pair` CLI / 批2 的 Web UI）。单机 ① 例外，本机 vault 可写。

各姿势的差异只在两件事：**vault 放哪、agent 怎么连**。多机下命令**始终从工作机直拨目标服务器**，broker 不在命令路径上。

---

## 全景图

```
目标: AI agent ──SSH──▶ 目标服务器们
                        ▲
        ┌───────────────┼──────────────────────────────────┐
        │ ① 单机模式     │ ② 多机桥姿态（唯一多机姿势）          │
        │ vault 在本机   │  权威 broker 常驻 serve（vault 集中） │
        │ stdio 直开     │  工作机 = pair 一条龙 → 本地只读缓存  │
        │ （可写）       │  → mcp --cache（只读 + 执行）        │
        │               │  管理面：broker TUI / serve pair CLI │
        │               │        （批2：Web UI，手机可批）      │
        └───────────────┴──────────────────────────────────┘
```

---

## ① 单机模式（stdio 直开本机 vault）

vault、master key、agent 全在**同一台机**：`.mcp.json` 里 `command: sshmgr, args: ["mcp"]` + `env SSHMGR_TOKEN`，MCP 客户端按需 spawn 子进程直开 vault（可写）。

- 入门：[getting-started.md](./getting-started.md) · 全程点选：[tui-single-machine.md](./tui-single-machine.md) · 快速版：[quickstart-single-machine.md](./quickstart-single-machine.md)
- headless / 无 keychain 环境用 `SSHMGR_MASTERKEY_HEX` 等 env 形态（见 getting-started 对应节）。

## ② 多机桥姿态（唯一多机姿势：权威 broker + 本地只读缓存）

一台权威机器常驻 `sshmgr serve`（**权威 vault + `/snapshot` 拉取 + `/pair` 配对**（+批2 `/ui` 管理）——serve 收窄为这四件事，不再承载任何 agent 会话）。每台工作机装 `sshmgr` 二进制，以**零距离 client** 接入：

1. **配对（一条龙）**：`sshmgr pair --instance <名>`——LAN 广播发现 broker（或 `--url` 直指）→ SAS 三件套人闸比对 → owner 在 broker TUI Pairing 页（或 `serve pair approve`）批准 → 凭据自动加密下发 → 首拉落盘 → 产物 `pair.<名>.mcp.json` 抄进 agent 配置（或 `--write-mcp` 直落）。
2. **干活**：`.mcp.json` 用 `args: ["mcp", "--cache"]` + `env SSHMGR_TOKEN`——agent 的本地子进程用本地只读缓存干活，**命令从工作机直拨目标服务器**；缓存自动保鲜（≤30min），断网照常用（只读）。

- 需要**两样凭据**：设备码（管拉取，pair 批准时自动铸发；手工路径用 `cache-tokens add --name <机> --profile <profile>` 签发）+ project token（管 spawn，pair 一并下发；手工路径 `projects add` 签发）。
- TLS **零配置**：自家客户端纯 SPKI 指纹钉死（不校验主机名、不碰系统信任库）——指纹随 discovery offer / pair 信封自动交付，无 ②a 时代的信任库/SAN 两坑。
- 详见 [multi-machine.md](./multi-machine.md) · 快速版：[quickstart-multi-machine.md](./quickstart-multi-machine.md)

### 管理面（写操作唯一入口）

| 面 | 谁用 | 能做什么 | 状态 |
|---|---|---|---|
| **broker TUI**（`sshmgr tui`，Pairing 页 + 四页签） | owner，broker 机上 | 批准/拒绝配对、发/吊销设备码、管 project/profile/server | ✅ 批1 |
| **`serve pair` CLI**（`ls` / `approve` / `reject`） | owner，broker 机上（TUI 的命令行兜底） | 批准（`--allow-foreign-url` 显式覆盖机械地址校验）/拒绝/列队 | ✅ 批1 |
| **Web UI**（serve `/ui`，go:embed 单二进制，手机优先） | owner，手机/任意浏览器 | 完整管理 + 配对批准 + 吊销 + 审计 | 🔜 批2 |

> 吊销也在管理面做——生效路径见下表「设备失窃处置」与 [agent-tools.md](./agent-tools.md) 的「吊销三路径」。

### ②a 在哪了？（移除说明 + 存量迁移）

②a「在线 HTTP 直连」（`.mcp.json` 用 `"type": "http"` + Bearer project token 直连 serve 的远程 MCP）在 Plan 42 批1（随下个发版起）**已移除**：serve 的根 mux 撤掉 MCP-over-HTTP 路由，旧 ②a `.mcp.json` 打过去是 **404**。project token 从此不再是任何远程 MCP 凭据——它只作为 client 侧 spawn 闸（`mcp --cache` 对快照内 projects 校验）。

**存量 ②a 机器的官方迁移 = 三步（含升级顺序铁律）**——完整 runbook 见 [compat-matrix.md](./compat-matrix.md) 与 [multi-machine.md](./multi-machine.md)：

1. **① 手工桥迁移**：在**旧** serve 上按既有手工流程迁到桥姿态（`cache-tokens add` + `projects add` + 工作机 `cache pull` + 手写 `.mcp.json`）——**遵守「client 先升级、serve 后升级」铁律**（client ≥ v0.10.1）；
2. **② 升 serve**（v0.13.0——含 ②a 移除 + 二进制改名 + 服务名变更，见本页置顶 runbook）——前置检查：全部 client 已在桥姿态；当刻起 ②a 路径 404；
3. **③ pair 时代**：此后所有新机/重配对一律 `sshmgr pair`。

### ②c 应急附录（不推荐）

在 serve 主机上裸跑 `mcp` 直开本地 vault：与常驻 serve 并发开同一个 vault，破坏"serve 是唯一写者"的纪律。仅留作 serve 彻底没起时的应急——见 [broker-host-agent.md 应急附录](./broker-host-agent.md)。

---

## 两姿势对比

| 维度 | ① 单机 stdio | ② 多机桥姿态 |
|---|---|---|
| 凭据放哪 | agent 本机（vault+master key） | 只在权威 broker；工作机仅本地加密只读快照（cache.bin+DEK） |
| 客户端要装 | sshmgr + 本机 vault | sshmgr 二进制（pair 一条龙入网） |
| 认证材料 | project token | 设备码 + project token（两道独立闸，永不互通） |
| 新机接入 | `.mcp.json` 即用 | **`sshmgr pair` 一条命令**（发现→批准→凭据下发→首拉→配置产物） |
| broker 挂了/重启 | —（无 broker） | **照常用**（本地缓存兜底；≤30min 保鲜延迟） |
| 吊销生效 | lazy（下次 spawn） | **三路径**：project token 吊销+码活 → 下次保鲜（≤30min）拿到的新快照已无该 project；设备码吊销 → 下次 pull pinned 401 → 本地缓存就地销毁（隔离）；永离线设备 → `max_offline` 硬上限（pair 下发默认 24h）到期拒载。详见 [agent-tools.md](./agent-tools.md) |
| 新 grant 的 server 可见 | 立即 | ≤30min 保鲜延迟（或手动 `cache pull`） |
| 写操作（加改删服务器） | ✅ | ❌ 只读快照（`ErrReadOnly`）——写只在管理面 |
| TLS 配置 | 无 | **零**（SPKI 指纹钉死；指纹经 discovery/pair 自动交付） |
| 谁直拨目标服务器 | 本机 | **工作机自己**（broker 不在命令路径上） |
| `forward_port` 监听开在哪 | 本机 `127.0.0.1` | 工作机 `127.0.0.1`（多机 client 隧道恒环回——②a 的 serve 侧监听随移除消失） |
| 后台任务表 | 子进程内存（会话重启即丢） | 子进程内存（会话重启即丢） |
| 设备失窃处置 | 轮换服务器凭据 | 设备码 + project token 都吊销（双吊销）；永离线失窃仍需轮换凭据 |
| 适用设备 | 桌面 OS（Win/Linux/mac） | 桌面 OS（需本地跑二进制 + spawn 子进程；手机/平板等非桌面设备 → 批2 Web 管理面，agent 不跑在手机上） |

---

## 怎么选

- **一台机、自己用、不要集中管理** → ①。多机形态对你是多余开销（常驻服务 + 缓存层）。
- **桌面多机共用清单 / 凭据必须集中** → **② + `sshmgr pair` 一条龙**（默认路径）：新机从「装好二进制」到「agent 可用」= 一条命令 + owner 在 broker TUI 批一次。
- **手机 / 平板** → agent 不跑在非桌面设备上（②需要本地 spawn 二进制进程）。手机的角色是**批2 的 Web 管理面**：在 serve 的 `/ui` 上批准配对、吊销、管服务器。批1 阶段手机上想批准 → 在 broker 机用 TUI / `serve pair` CLI（或让桌面代批）。
- ②c 只留应急（serve 彻底没起时）；②a 已移除，不要再用 `"type": "http"` 配置——那是 404。

一句话记忆：**①是"凭据跟人走"，②是"凭据留在 broker、agent 只拿只读快照"；多机入网 = pair 一条龙，写操作 = 管理面（TUI / `serve pair`，批2 上手机 Web）。**

---

## 相关

- 升级任何一端（client ↔ serve）前：[compat-matrix.md](./compat-matrix.md)（版本兼容矩阵 + 升级顺序铁律 + Plan 42 三步迁移 + v0.13.0 四项 breaking 面）。存量 v0.12.x 及更早迁移 = 本页置顶的 v0.13.0 runbook（唯一总册）；此后升级一律 `sshmgr update`。
- 各姿势下 agent 的行为纪律（含多机只读铁律与吊销三路径）：[agent-tools.md](./agent-tools.md)（含可贴进 CLAUDE.md 的规则模板）。
- broker 主机上自己跑 agent 的姿势：[broker-host-agent.md](./broker-host-agent.md)。
- 真实任务示例：[scenarios.md](./scenarios.md)。
