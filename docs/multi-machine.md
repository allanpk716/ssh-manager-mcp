# 多机共享：serve 模式（一台服务器常驻，多台机器共用）

> **适用场景**：你在**多台电脑**上开发/办公（同一个内网或虚拟局域网 VLAN），想让所有机器上的 AI agent 共用**同一份** SSH 服务器清单。
>
> **单台机器不需要本篇**——直接用默认的 stdio 模式（见 [getting-started.md](./getting-started.md)）。serve 是给"多机共用"这个场景的可选项。

---

## 该用哪种模式？（先看这张表）

| | **stdio（默认 · 单机）** | **serve（可选 · 多机）** |
|---|---|---|
| broker 跑在哪 | Claude Code **按需 spawn** 的本地子进程 | 你**手动启动并常驻**的一台 VLAN 服务器 |
| agent 怎么连 | 本地 stdio | 远程 HTTP（streamable MCP） |
| 凭据放在哪 | 本机（自包含） | **只在服务器上**，工作机零凭据 |
| 离线能用吗 | ✅ 是（本机自包含） | ❌ **否（在线 only）** |
| 重启后要管吗 | 不用（客户端自动拉起） | 要（你得让 serve 常驻 / 开机自启） |
| 适合 | 单台机器 | 多台机器共用一份清单 |
| 配置复杂度 | 最低 | 中（要常驻服务 + 建议 TLS） |

**默认选 stdio。** 只有"多台机器要共用同一份服务器清单"时才上 serve。

> 一句话分辨：**broker 是"按需拉起的子进程"（stdio）还是"你常驻的服务"（serve）。** 这是两种模式最根本的运营差异，下面的架构会展开。

---

## 架构

```
   多机（serve）                          单机（stdio · 默认）

 ┌──工作机 A──┐  ┌──工作机 B──┐         ┌───你的机器───┐
 │  Claude    │  │  Claude    │         │  Claude      │
 │  Code      │  │  Code      │         │  Code        │
 │ （远程 MCP）│  │ （远程 MCP）│         │ （spawn 子进程）│
 └─────┬──────┘  └─────┬──────┘         └───────┬───────┘
       │ HTTPS + token   │                      │ stdio
       └────────┬────────┘                      │
                ▼                               ▼
      ┌──────────────────┐            ┌──────────────────┐
      │ VLAN 服务器       │            │ 本机              │
      │ ssh-manager serve │            │ ssh-manager mcp   │
      │  （常驻进程）      │            │  （按需子进程）     │
      │  ┌────────────┐  │            │  ┌────────────┐  │
      │  │ vault+DEK  │  │            │  │ vault+DEK  │  │
      │  └────────────┘  │            │  └────────────┘  │
      └──────────────────┘            └──────────────────┘
               │                               │
               ▼ SSH                           ▼ SSH
          目标服务器们                      目标服务器
```

**本质区别：**

- **stdio（单机）**：Claude Code 读 `.mcp.json` 里的 `command`，**自己 spawn** `ssh-manager mcp` 子进程，broker 和 Claude Code 之间走 stdio。broker 的生死 Claude Code 管；机器自包含（vault 在本机）。详见 [getting-started.md 的"重启/关机后"](./getting-started.md#重启--关机后还要做什么吗不用mcp-客户端会自动拉起)。
- **serve（多机）**：你在 VLAN 一台服务器上**常驻** `ssh-manager serve`。各工作机的 Claude Code 通过**远程 MCP**（HTTP）连它。**凭据只在服务器上**，工作机上零凭据、零 vault。

**鉴权（和 stdio 同一个闸门）**：每个 HTTP 请求带 `Authorization: Bearer <项目token>`；服务器用同一个 `VerifyToken` 把 token resolve 成项目/profile（只放行 `active` 项目）。**铁律**（每条命令前重检 `serverID ∈ profileID`）和 stdio 完全一致——serve 没有新增任何工具、没有动 agent 表面，只是把同一个 broker 暴露到网络上。

> **额外的一道闸**：SDK 自带的 session-binding 防御已激活——防止"拿 A 项目的 token 重放到 B 项目已建立的 session"这类跨项目越权（→ 403 `session user mismatch`）。

---

## 配置与使用

### Step 1（服务器侧，一次性）：把清单建在这台机上

在 VLAN 那台将常驻 broker 的机器上，像单机一样把服务器/profile/project 建好（命令和 stdio 完全一样，详见 [getting-started.md](./getting-started.md)）：

```bash
ssh-manager unlock                                  # master key → keychain
ssh-manager servers add --name gpu --host 192.0.2.10 --user deploy --password '...'
ssh-manager profiles add team-a && ssh-manager profiles grant team-a gpu
ssh-manager projects add my-agent --profile team-a  # 打印一次性 token（工作机要用，记下来）
```

### Step 2（服务器侧）：启动常驻 broker

```bash
ssh-manager serve --addr 0.0.0.0:7878 --tls-cert cert.pem --tls-key key.pem
# → ssh-manager serve: listening on 0.0.0.0:7878 (tls=true)
```

| 选项 | 说明 |
|---|---|
| `--addr` | 监听地址。默认 `127.0.0.1:7878`（**只本机**——远程用不了）。多机场景写 `0.0.0.0:7878` 或服务器的 VLAN IP。 |
| `--tls-cert` / `--tls-key` | 启用 HTTPS。**强烈建议**——不挂时 bearer token 在网络明文传输。VLAN 内自签证书即可。 |

⚠️ **不挂 TLS + 绑非回环 = token 裸奔**：程序会打一行 STDERR 警告。同 VLAN 也不应假设安全——上 TLS。

**让它常驻 + 开机自启**（serve 是个长驻进程，别在前台手跑就完事）：

- **Linux（systemd）**，示例 unit：
  ```ini
  # /etc/systemd/system/ssh-manager-serve.service
  [Unit]
  Description=ssh-manager MCP server (serve mode)
  After=network.target

  [Service]
  ExecStart=/usr/local/bin/ssh-manager serve --addr 0.0.0.0:7878 \
            --tls-cert /etc/ssh-manager/cert.pem --tls-key /etc/ssh-manager/key.pem
  Environment=SSHMGR_MASTERKEY_HEX=<unlock 打印的 hex>   # 服务器若无 keychain 必填；有 keychain 可省
  Restart=on-failure
  User=ssh-manager

  [Install]
  WantedBy=multi-user.target
  ```
  > **master key**：服务器若是 headless（无 OS keychain），master key 靠 `SSHMGR_MASTERKEY_HEX` 环境变量（同 [getting-started 的无 keychain 小节](./getting-started.md#无-keychain-环境headless-linux-等)）；写在 unit 的 `Environment=` 里。有 keychain 的桌面服务器则让该用户 keychain 持有 master key。
- **Windows**：用 NSSM 或任务计划把 `ssh-manager.exe serve ...` 注册成开机自启服务。
- **macOS**：用 launchd。

### Step 3（每台工作机）：Claude Code 连远程

各工作机的 `.mcp.json`：

```json
{
  "mcpServers": {
    "ssh": {
      "type": "http",
      "url": "https://192.0.2.5:7878/",
      "headers": { "Authorization": "Bearer <Step 1 拿到的项目token>" }
    }
  }
}
```

- `"type": "http"` **必填**——漏了 Claude Code 会当 stdio 处理并拒绝这个条目。
- `url` 指向 serve 那台服务器（挂了 TLS 就用 `https://`）。
- `headers.Authorization` 带项目 token。

⚠️ token 是敏感信息——`.mcp.json` **别提交 git**（和 stdio 一样）。机器失窃 = 该 token 泄露，立刻 `ssh-manager projects rotate <name>`（在服务器上跑）换发。

重启 Claude Code → 该机的 agent 就能用 SSH 工具了，范围 = 这个 token 绑定的 profile。

### Step 4：网络

服务器的 `7878`（或你选的端口）只对**可信机器**开放。VLAN 内通常天然隔离；跨网段记得 ACL。这 Serve 不内置 IP 白名单——网络层隔离 + TLS + token 三道够了；要更细的按机器隔离，见下面"按机器分项目"。

---

## 使用场景

### 场景 A：单人多机 / 家用 VLAN（典型）

一台常开的家用服务器 / NUC / 软路由 + 笔记本 + 台式机，都在同一个 VLAN。

- **服务器**：常驻 `ssh-manager serve`（systemd 托管 + TLS）。所有服务器清单建在它上面。
- **笔记本 / 台式机**：Claude Code `.mcp.json` 连服务器。任意一台上的 agent 都能用**同一份**清单。
- **一个 token 还是多个**：
  - 所有机器看同一份清单 → 用**同一个** project token（最简）。
  - 要按机器隔离（比如笔记本只能看部分服务器）→ 在服务器上建**多个 project**，各绑**不同 profile**，各发**不同 token**；不同机器的 `.mcp.json` 填不同 token。

> serve 的鉴权是**按 token → project → profile** 路由的：一个 serve 进程同时服务多个 project/token，互不串扰（跨 project 重放会被 session-binding 防御挡掉）。

### 场景 B：单机（不需要本篇）

只用一台机器 → 用 stdio（[getting-started.md](./getting-started.md)）。serve 对你是多余开销（要常驻一个服务、要管 TLS、要在线）。

---

## 限制（如实，必读）

1. **在线 only**：工作机连不上服务器（服务器挂了 / VLAN 断了 / 笔记本带出门）= 该机的 agent **用不了** SSH 工具。
2. **无离线缓存**：工作机不持有 vault，连不上就没有本地兜底。除非那台机**另外**配了一份本地 stdio vault——但那是**另一份独立、不同步**的清单，不是 serve 的离线模式。真正的"离线只读缓存"是后续计划，**尚未实现**。
3. **服务器是单点**：服务器挂了 = 所有人暂停，直到它恢复。**自动备份 / 灾难恢复是后续 Plan 13，尚未实现**。目前的备份手段：手动复制服务器上的 `store.db`（恢复时需原机 keychain 的 master key，**不可移植**）；可用 [export/import](./backup-restore.md)（Plan 11，已做）做可移植的加密备份。
4. **单 owner 设计**：多个人共用同一个 vault、按人隔离访问——**不在范围**。本方案是"一个人、多台机"。多人场景需要 per-user ACL + 审计隔离，是另一个量级的功能。
5. **bearer token = 钥匙**：谁拿到某项目的 token + 能连到服务器 = 拿到那个项目 profile 里的**所有服务器**。所以：用 **TLS** 防嗅探；用 [`projects rotate`](./agent-access.md)（换发）/ [`revoke`](./agent-access.md)（吊销）管 token 生命周期；token 进密码管理器、别进 git。

---

## 后续路线

serve 模式是多机支持的**第一期（Phase 1）= 在线 live 远程访问**。**export/import（Plan 11，便携加密备份 / 迁移）已落地**（见 [backup-restore.md](./backup-restore.md)）。规划中的多机后续：

| 计划 | 解决什么 | 状态 |
|---|---|---|
| Plan 11 · export/import | 整个 vault 口令加密便携文件：备份 / 迁移 / 灾难恢复 | ✅ 已做（[backup-restore.md](./backup-restore.md)） |
| Plan 12 · 离线只读缓存 | 工作机本地缓存加密 vault，离线时只读用、不能改 | 未做 |
| Plan 13 · vault 复制 | 服务器 → 工作机的同步机制 | 未做 |
| Plan 14 · 群晖自动备份 | 服务器定时出加密快照到 NAS，灾难恢复 | 未做 |
| Plan 15 · 迁移 + DEK enroll | 新机器加入流程、密钥分发 | 未做 |

**现在：serve = 在线 live；备份 / 迁移已可（export/import，见 [backup-restore.md](./backup-restore.md)）。** 离线缓存 / vault 复制 / 群晖自动备份 / 新机 enroll 还没到位（见上节"限制"）。

---

## 相关文档

- [getting-started.md](./getting-started.md)——单机 stdio 从零到跑通（**默认模式**，第一次用先看这篇）。
- [agent-access.md](./agent-access.md)——project token 生命周期（`rotate` / `disable` / `revoke` 的 Lazy 语义）；**serve 模式完全适用**，token 管理在同一台服务器上做。
- [managing-servers.md](./managing-servers.md)——服务器增删改查（在 serve 那台**服务器**上操作）。
- [scenarios.md](./scenarios.md)——应用场景示例（GPU 巡检、部署、端口转发……，两种模式都适用）。
- 仓库根 [README 的 "Multi-machine: serve mode"](../README.md#multi-machine-serve-mode-remote-agents-on-a-vlan) 节（英文概览）。
