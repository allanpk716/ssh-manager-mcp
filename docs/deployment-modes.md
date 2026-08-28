# 部署形态全景：单机 / 多机桥姿态 + 管理面（选型总览 · Plan 42 起 4→2）

> **场景**：你的目标是"让 AI agent 用 SSH 控制我的服务器"。本页一屏看全本项目共有哪几种部署方案、各自的优劣、怎么选——**第一次选部署方式，或忘了有几种用法时，看这篇**。每种姿势的具体配置步骤链接到对应展开文档。
>
> **Plan 42 批1 起的模式缩减（4 → 2 + 管理面）**：旧的 ②a「在线 HTTP 直连」已**移除**（serve 不再提供任何远程 MCP 面——根路径 404，不是降级）；②c 降为应急附录；客户端 TUI 向导的连接表单退役。多机从此只有一条接入姿势：**桥姿态**（工作机 = 本地只读缓存的零距离 client），配对入口 = `ssh-manager pair` 一条龙。

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

vault、master key、agent 全在**同一台机**：`.mcp.json` 里 `command: ssh-manager, args: ["mcp"]` + `env SSHMGR_TOKEN`，MCP 客户端按需 spawn 子进程直开 vault（可写）。

- 入门：[getting-started.md](./getting-started.md) · 全程点选：[tui-single-machine.md](./tui-single-machine.md) · 快速版：[quickstart-single-machine.md](./quickstart-single-machine.md)
- headless / 无 keychain 环境用 `SSHMGR_MASTERKEY_HEX` 等 env 形态（见 getting-started 对应节）。

## ② 多机桥姿态（唯一多机姿势：权威 broker + 本地只读缓存）

一台权威机器常驻 `ssh-manager serve`（**权威 vault + `/snapshot` 拉取 + `/pair` 配对**（+批2 `/ui` 管理）——serve 收窄为这四件事，不再承载任何 agent 会话）。每台工作机装 `ssh-manager` 二进制，以**零距离 client** 接入：

1. **配对（一条龙）**：`ssh-manager pair --instance <名>`——LAN 广播发现 broker（或 `--url` 直指）→ SAS 三件套人闸比对 → owner 在 broker TUI Pairing 页（或 `serve pair approve`）批准 → 凭据自动加密下发 → 首拉落盘 → 产物 `pair.<名>.mcp.json` 抄进 agent 配置（或 `--write-mcp` 直落）。
2. **干活**：`.mcp.json` 用 `args: ["mcp", "--cache"]` + `env SSHMGR_TOKEN`——agent 的本地子进程用本地只读缓存干活，**命令从工作机直拨目标服务器**；缓存自动保鲜（≤30min），断网照常用（只读）。

- 需要**两样凭据**：设备码（管拉取，pair 批准时自动铸发；手工路径用 `cache-tokens add --name <机> --profile <profile>` 签发）+ project token（管 spawn，pair 一并下发；手工路径 `projects add` 签发）。
- TLS **零配置**：自家客户端纯 SPKI 指纹钉死（不校验主机名、不碰系统信任库）——指纹随 discovery offer / pair 信封自动交付，无 ②a 时代的信任库/SAN 两坑。
- 详见 [multi-machine.md](./multi-machine.md) · 快速版：[quickstart-multi-machine.md](./quickstart-multi-machine.md)

### 管理面（写操作唯一入口）

| 面 | 谁用 | 能做什么 | 状态 |
|---|---|---|---|
| **broker TUI**（`ssh-manager tui`，Pairing 页 + 四页签） | owner，broker 机上 | 批准/拒绝配对、发/吊销设备码、管 project/profile/server | ✅ 批1 |
| **`serve pair` CLI**（`ls` / `approve` / `reject`） | owner，broker 机上（TUI 的命令行兜底） | 批准（`--allow-foreign-url` 显式覆盖机械地址校验）/拒绝/列队 | ✅ 批1 |
| **Web UI**（serve `/ui`，go:embed 单二进制，手机优先） | owner，手机/任意浏览器 | 完整管理 + 配对批准 + 吊销 + 审计 | 🔜 批2 |

> 吊销也在管理面做——生效路径见下表「设备失窃处置」与 [agent-tools.md](./agent-tools.md) 的「吊销三路径」。

### ②a 在哪了？（移除说明 + 存量迁移）

②a「在线 HTTP 直连」（`.mcp.json` 用 `"type": "http"` + Bearer project token 直连 serve 的远程 MCP）在 Plan 42 批1（随下个发版起）**已移除**：serve 的根 mux 撤掉 MCP-over-HTTP 路由，旧 ②a `.mcp.json` 打过去是 **404**。project token 从此不再是任何远程 MCP 凭据——它只作为 client 侧 spawn 闸（`mcp --cache` 对快照内 projects 校验）。

**存量 ②a 机器的官方迁移 = 三步（含升级顺序铁律）**——完整 runbook 见 [compat-matrix.md](./compat-matrix.md) 与 [multi-machine.md](./multi-machine.md)：

1. **① 手工桥迁移**：在**旧** serve 上按既有手工流程迁到桥姿态（`cache-tokens add` + `projects add` + 工作机 `cache pull` + 手写 `.mcp.json`）——**遵守「client 先升级、serve 后升级」铁律**（client ≥ v0.10.1）；
2. **② 升 serve**（含 ②a 移除的 Plan 42 版本，版本号发版拍板）——前置检查：全部 client 已在桥姿态；当刻起 ②a 路径 404；
3. **③ pair 时代**：此后所有新机/重配对一律 `ssh-manager pair`。

### ②c 应急附录（不推荐）

在 serve 主机上裸跑 `mcp` 直开本地 vault：与常驻 serve 并发开同一个 vault，破坏"serve 是唯一写者"的纪律。仅留作 serve 彻底没起时的应急——见 [broker-host-agent.md 应急附录](./broker-host-agent.md)。

---

## 两姿势对比

| 维度 | ① 单机 stdio | ② 多机桥姿态 |
|---|---|---|
| 凭据放哪 | agent 本机（vault+master key） | 只在权威 broker；工作机仅本地加密只读快照（cache.bin+DEK） |
| 客户端要装 | ssh-manager + 本机 vault | ssh-manager 二进制（pair 一条龙入网） |
| 认证材料 | project token | 设备码 + project token（两道独立闸，永不互通） |
| 新机接入 | `.mcp.json` 即用 | **`ssh-manager pair` 一条命令**（发现→批准→凭据下发→首拉→配置产物） |
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
- **桌面多机共用清单 / 凭据必须集中** → **② + `ssh-manager pair` 一条龙**（默认路径）：新机从「装好二进制」到「agent 可用」= 一条命令 + owner 在 broker TUI 批一次。
- **手机 / 平板** → agent 不跑在非桌面设备上（②需要本地 spawn 二进制进程）。手机的角色是**批2 的 Web 管理面**：在 serve 的 `/ui` 上批准配对、吊销、管服务器。批1 阶段手机上想批准 → 在 broker 机用 TUI / `serve pair` CLI（或让桌面代批）。
- ②c 只留应急（serve 彻底没起时）；②a 已移除，不要再用 `"type": "http"` 配置——那是 404。

一句话记忆：**①是"凭据跟人走"，②是"凭据留在 broker、agent 只拿只读快照"；多机入网 = pair 一条龙，写操作 = 管理面（TUI / `serve pair`，批2 上手机 Web）。**

---

## 相关

- 升级任何一端（client ↔ serve）前：[compat-matrix.md](./compat-matrix.md)（版本兼容矩阵 + 升级顺序铁律 + Plan 42 三步迁移）。
- 各姿势下 agent 的行为纪律（含多机只读铁律与吊销三路径）：[agent-tools.md](./agent-tools.md)（含可贴进 CLAUDE.md 的规则模板）。
- broker 主机上自己跑 agent 的姿势：[broker-host-agent.md](./broker-host-agent.md)。
- 真实任务示例：[scenarios.md](./scenarios.md)。
