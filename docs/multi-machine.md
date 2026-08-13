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

- **Windows**（推荐）：跑 `ssh-manager serve install`——程序自己注册 Task Scheduler 任务（at-boot + at-logon + 崩溃自重启），不用你手写 schtasks XML 或 NSSM 包装。详见下面"Windows：`serve install` 一条龙"小节。
- **Linux**：自己写 systemd unit（示例见下）。`ssh-manager serve install` 在 Linux 上**尚未实现**（会报 `not yet supported on linux`，见 [T6 限制](#linuxmacos-尚未支持)）。
- **macOS**：自己写 launchd LaunchAgent。`serve install` 同样**尚未实现**。

#### Windows：`serve install` 一条龙（Plan 14，Plan 15 修正为 machine-scope DPAPI）

```powershell
# 在交互式 session（本地终端 / RDP，不是 ssh）里跑：
ssh-manager serve install --addr 0.0.0.0:7878 --tls-cert cert.pem --tls-key key.pem
```

程序会：

1. **先检查 `master.key` 存在且是 machine-scope**（Plan 15 修正：检查 DPAPI scope，避免 user-scope 的跨 session 失败）——没有就报错让你先 `unlock`（Plan 15 会触发 user→machine 迁移）。
2. 生成 Task Scheduler XML（boot + logon 触发，崩溃自重启 PT1M × 3，stdout/stderr 重定向到 `%LocalAppData%\ssh-manager\serve.log`），通过 PowerShell `Register-ScheduledTask` 注册成任务 `ssh-manager-serve`。
3. **用 PowerShell `secureString` 读 Windows 密码**（Plan 15 修正：不再用 `Get-Credential` 对话框，避免密码进 4688 审计日志；用 `Read-Host -AsSecureString` 转 plain text 再传给 `Register-ScheduledTask`）——任务要 `LogonType=Password` 才能 boot 时就起（无需等人登录）。**密码只活在 PowerShell 进程内存里，不进 ssh-manager.exe argv，不进 4688 审计日志**。
4. 立即 `schtasks /Run` 跑一次验证 + 生成 serve.log。

配套命令：

```powershell
ssh-manager serve status      # 查任务状态 + 进程在不在 + HTTP 活着没 + vault 解锁没
ssh-manager serve uninstall   # 删任务 + 停 serve 进程
```

`serve status` 四路独立检查：任务注册 / 进程在跑 / HTTP 响应（401 或 200 都算活）/ vault 解锁（扫 `serve.log` 里有没有"master key present but unreadable"硬失败标记——进程活着但 master key 解不开时这路会报 `LOCKED`，区分"进程在跑"和"真正可用"）。

⚠️ **账户密码过期会让任务起不来**：Task Scheduler 存的是你**当时的** Windows 密码，密码过期后任务会因凭据失效而起不来。单用户本地账户建议直接禁用密码过期：

```powershell
# 以管理员身份跑（NUC10 这种单 owner 机适用）：
wmic UserAccount where Name='allan716' set PasswordExpires=False
```

> Win11 22H2+ 不装 `wmic`，改用 `Set-LocalUser -Name 'allan716' -PasswordNeverExpires $true`。

> **新机器升级注意（Plan 15 修正为 machine-scope）**：已有 v0.2.0 vault 的机器升级到新版后，`master.key` 还没生成（旧 master key 在 keychain 里，新版从非交互 session 读不出）——必须**先在交互式 session 跑一次 `ssh-manager unlock` 触发迁移**（v0.2.0 → DPAPI + user→machine），再 `serve install`，否则 serve 读不到 master key 会启动失败。完整流程见 [backup-restore.md 的 Plan 14 升级 Runbook](./backup-restore.md#升级-runbookv020--新版windows) 和 [Plan 15 修正：user-scope → machine-scope 迁移](./backup-restore.md#升级-runbookplan-15-修正user-scope--machine-scope-迁移)。

#### Linux：systemd（自建，`serve install` 尚未实现）

`ssh-manager serve install` 在 Linux 上会报 `not yet supported on linux; see docs/multi-machine.md`（计划在后续 plan 实现，spec §3.4 / §9）。在那之前自己写 unit：

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
>
> **注意 Linux 路径上的坑**：systemd unit 以 `User=ssh-manager` 跑时，`os.UserConfigDir()` 解析的是那个用户的家目录，不是 owner 的——别把 vault 装在 owner 家又指望 `User=ssh-manager` 的服务能读到。

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

1. **在线 only（serve 本身）**：serve 的远程 MCP 走在线——工作机连不上服务器（服务器挂了 / VLAN 断了 / 笔记本带出门）= 该机的 agent **走不了远程 MCP**。但本地若有缓存（见下条），agent 可以切到只读的 `mcp --cache` 兜底。
2. **离线缓存：✅ 已实现（Plan 12）**：工作机本地持有一份**加密的只读** vault 快照，连不上服务器时 agent 照常 exec / download / upload / 转发（只读，不能改）。**见本篇[「离线只读缓存（Plan 12）」](#离线只读缓存plan-12)节**——它不是 serve 的"离线模式"，而是一份独立拉取、独立加密、自动刷新的本地缓存。在一台机器上**同时**配 serve（在线）+ cache（离线兜底）也行——两套互不冲突。
3. **服务器是单点**：服务器挂了 = 所有人暂停，直到它恢复。**自动备份 / 灾难恢复已落地**——[Plan 13](./backup-restore.md#plan-13--nas-定时明文备份backup-create--verify)（NAS 定时明文备份）+ [export/import](./backup-restore.md)（Plan 11，便携加密备份）。恢复手段：从 NAS 拷最新快照或 export 文件，在新机 `ssh-manager import`（[见 backup-restore 的灾难恢复](./backup-restore.md#场景-③-灾难恢复)）。
4. **单 owner 设计**：多个人共用同一个 vault、按人隔离访问——**不在范围**。本方案是"一个人、多台机"。多人场景需要 per-user ACL + 审计隔离，是另一个量级的功能。
5. **bearer token = 钥匙**：谁拿到某项目的 token + 能连到服务器 = 拿到那个项目 profile 里的**所有服务器**。所以：用 **TLS** 防嗅探；用 [`projects rotate`](./agent-access.md)（换发）/ [`revoke`](./agent-access.md)（吊销）管 token 生命周期；token 进密码管理器、别进 git。

---

## 后续路线

serve 模式是多机支持的**第一期（Phase 1）= 在线 live 远程访问**。**export/import（Plan 11，便携加密备份 / 迁移）已落地**（见 [backup-restore.md](./backup-restore.md)）。规划中的多机后续：

| 计划 | 解决什么 | 状态 |
|---|---|---|
| Plan 11 · export/import | 整个 vault 口令加密便携文件：备份 / 迁移 / 灾难恢复 | ✅ 已做（[backup-restore.md](./backup-restore.md)） |
| Plan 12 · 离线只读缓存 | 工作机本地缓存加密 vault，断网时只读用、自动刷新 | ✅ 已做（本节[「离线只读缓存」](#离线只读缓存plan-12)） |
| Plan 13 · 群晖自动备份 | 服务器定时出明文快照到 NAS，灾难恢复 | ✅ 已做（[backup-restore.md Plan 13](./backup-restore.md#plan-13--nas-定时明文备份backup-create--verify)） |
| Plan 14 · Windows 生产部署 | DPAPI master key（替代 keychain）+ `serve install` Task Scheduler 常驻（**Plan 15 修正为 machine-scope DPAPI + serve install fix**） | ✅ 已做（[backup-restore.md Plan 14](./backup-restore.md#plan-14--windows-生产部署dpapi-master-key--serve-常驻)） |

**现在：serve = 在线 live（Windows 一条龙 `serve install`，Linux/macOS 自建 systemd/launchd）；备份 / 迁移已可（export/import + Plan 13 NAS）；离线只读缓存已落地（Plan 12）。** Linux/macOS 的 `serve install` 还没实现（[见下](#linuxmacos-尚未支持)）。

#### Linux/macOS 尚未支持

Plan 14 只实现了 **Windows Task Scheduler** 的 `serve install`（spec §3.4 scope 收窄）。Linux systemd --user / macOS launchd 各有平台陷阱（linger 权限、D-Bus session、LaunchAgent 仅 GUI login 后启动），**`ssh-manager serve install` 在这两个平台会报**：

```
serve install/uninstall/status is not yet supported on linux; see docs/multi-machine.md
(tracked for a follow-up plan — Windows Task Scheduler is the only implemented path)
```

在那之前，Linux/macOS 用户按上面"Step 2"里的 systemd unit / launchd 模板自己注册开机自启。

---

## 离线只读缓存（Plan 12）

> **一句话**：每台工作机在本地持有一份**加密的只读** vault 快照，连不上 serve 服务器时 agent 照常跑（exec / download / upload / 转发），但**任何写都被拒**（`ErrReadOnly`）。

### 它解决什么

serve 模式是"在线 only"——服务器挂了 / VLAN 断了 / 笔记本带出门，该机 agent 就断了 SSH 工具。Plan 12 给工作机一份**本地兜底**：把整个 vault 加密拉到本机，断网时 agent 切到这份缓存继续干活（只读）。**不是双写、不是同步**——缓存是单向、只读、零合并的快照。

### 模型（两道独立的闸门）

```
 ┌──serve 服务器（owner 在这）─────────────────┐
 │  vault + master key                         │
 │  cache-tokens add --name laptop   ──┐       │   ① 发码：每台机一个、可吊销
 │                                     │       │
 │  GET /snapshot                      │       │   ② 拉取：设备授权码鉴权
 │   Authorization: Bearer <设备码> ◀──┼─拉─────┤   （和 project token 是
 │   → 整个 vault 的 Snapshot JSON      │       │    两套不同的 verifier）
 └─────────────────────────────────────┼───────┘
                                       │
 ┌──工作机（laptop）───────────────────▼────────┐
 │  cache pull                            ──────►  DEK 加密落盘
 │   ↓                                            cache.bin (0600)
 │   OS keychain slot "cache-dek"          ──────►  cache.meta.json
 │                                                (url + pulled_at)
 │  系统调度器（systemd timer / 任务计划 / launchd）
 │   ↓ 每 ~30 min                                 ③ 自动保鲜
 │   cache pull                                   （进程外，非常驻）
 │
 │  .mcp.json（离线时）→ mcp --cache --token <同一个 project token>
 │   ↓                                            ④ 断网兜底
 │   读 cache.bin → 验 project token（铁律不变）→ broker 只读跑
 └────────────────────────────────────────────────┘
```

关键：**两道闸门，永不桥接**。

| 闸门 | 鉴什么 | 进哪 |
|---|---|---|
| project token（`projects add` 发的） | MCP 工具调用（exec / download / upload / forward） | 在线走 serve 的 MCP 路由；离线走 `mcp --cache` |
| 设备授权码（`cache-tokens add` 发的） | 拉整个 vault 的 `/snapshot` | 只进 `/snapshot` |

一个 project token **不能** dump 整个 vault（被 `/snapshot` 的 verifier 拒）；一个设备码**不能**驱动 MCP 工具。两套独立、从不互通——这是整个设计的**基石**（已被 T5 的 cross-auth 隔离测试证明：project token 打 `/snapshot` 必拒，设备码打 MCP 必拒）。

### enroll 一台新机（3 步）

#### Step 1（服务器侧，一次性）：发一个设备授权码

在 serve 服务器上（同一台常驻 broker 的机器）：

```bash
ssh-manager cache-tokens add --name laptop
# Authorization code for "laptop" (shown once): <一长串设备码>
#
# On the work machine:
#   ssh-manager cache pull --url https://192.0.2.5:7878 --token <设备码>
```

- `--name` **必填**且唯一（比如 `laptop` / `desktop-2`），后续吊销靠它。
- 设备码**只显示一次**——当场拉、或记进密码管理器。
- 其他管理命令：
  ```bash
  ssh-manager cache-tokens ls          # name / id / prefix / status / last_pull（不显示码）
  ssh-manager cache-tokens revoke laptop   # 位置参数，吊销（Lazy，下次 pull 被拒）
  ```

#### Step 2（工作机）：第一次拉缓存 + 配 `.mcp.json`

在工作机装好 `ssh-manager` 后：

```bash
# 第一次拉（用刚发的设备码；之后会被调度器自动重拉）
ssh-manager cache pull --url https://192.0.2.5:7878 --token <设备码>
# → pulled N servers / M credentials into <UserConfigDir>/ssh-manager/cache.bin

# 看缓存状态
ssh-manager cache status
# cache:    <UserConfigDir>/ssh-manager/cache.bin
# age:      12m3s
# servers:  N
# creds:    M
# source:   https://192.0.2.5:7878
```

| 选项 / 环境变量 | 说明 |
|---|---|
| `--url` / `SSHMGR_CACHE_URL` | serve broker 的 URL（`https://host:7878`）。必填 |
| `--token` / `SSHMGR_CACHE_TOKEN` | 设备授权码（`cache-tokens add` 发的那个）。必填 |
| `SSHMGR_CACHE_DIR` | 缓存目录覆盖（默认 `UserConfigDir/ssh-manager`） |

> **缓存目录**：默认在 `os.UserConfigDir()/ssh-manager/`，即 Linux `~/.config/ssh-manager/`、macOS `~/Library/Application Support/ssh-manager/`、Windows `%AppData%\ssh-manager\`。三个文件：`cache.bin`（DEK 加密的 vault 快照，0600）、`cache.meta.json`（URL + 拉取时间）、`cache-audit.log`（离线审计，见下）。DEK 存在本机 OS keychain 的 `cache-dek` 槽——**和 master key 是两把不同的钥匙**。

`.mcp.json` 怎么配？**取决于这台机在线为主还是离线为主**——同一个 project token（和 serve 用的是**同一个**）：

**在线为主（推荐默认）**——`.mcp.json` 指 serve URL，断网就临时切 cache：
```json
{
  "mcpServers": {
    "ssh": {
      "type": "http",
      "url": "https://192.0.2.5:7878/",
      "headers": { "Authorization": "Bearer <项目token>" }
    }
  }
}
```

**离线为主**（笔记本常出门）——`.mcp.json` 指 `mcp --cache`，缓存兜底：
```json
{
  "mcpServers": {
    "ssh": {
      "command": "ssh-manager",
      "args": ["mcp", "--cache", "--token", "<项目token>"]
    }
  }
}
```

> 切两种模式只是改 `.mcp.json` + 重启 Claude Code——vault 内容、project token、profile scoping **完全一样**。在线走远程 MCP（可写），离线走本地缓存（只读）。

#### Step 3（工作机）：设系统定时器自动保鲜

缓存不会自己刷新——**进程外的 OS 调度器**定时跑 `cache pull`。建议 **30 min**（按你 vault 的变动频率调）。环境变量走 unit 的 `Environment=` 或独立配置文件（**0600 权限**，里面有设备码）。

**Linux（systemd timer）**：

```ini
# ~/.config/systemd/user/ssh-manager-cache.service
[Unit]
Description=ssh-manager offline cache refresh

[Service]
Type=oneshot
Environment=SSHMGR_CACHE_URL=https://192.0.2.5:7878
Environment=SSHMGR_CACHE_TOKEN=<设备码>
ExecStart=/usr/local/bin/ssh-manager cache pull
```

```ini
# ~/.config/systemd/user/ssh-manager-cache.timer
[Unit]
Description=Refresh ssh-manager offline cache every 30 min

[Timer]
OnBootSec=2min
OnUnitActiveSec=30min
Persistent=true

[Install]
WantedBy=timers.target
```

```bash
systemctl --user enable --now ssh-manager-cache.timer
```

**Windows（任务计划，PowerShell）**：

```powershell
# 0600 配置文件存 URL + 设备码
@"
SSHMGR_CACHE_URL=https://192.0.2.5:7878
SSHMGR_CACHE_TOKEN=<设备码>
"@ | Set-Content -Path "$env:USERPROFILE\.ssh-manager\cache.env" -Encoding UTF8

$action  = New-ScheduledTaskAction -Execute "ssh-manager.exe" `
            -Argument "cache pull"
$trigger = New-ScheduledTaskTrigger -Once -At (Get-Date) `
            -RepetitionInterval (New-TimeSpan -Minutes 30)
$set     = New-ScheduledTaskSettingsSet -StartWhenAvailable
Register-ScheduledTask -TaskName "ssh-manager-cache-refresh" `
            -Action $action -Trigger $trigger -Settings $set
```

> Windows 任务计划不直接读 `.env`——把环境变量设进任务（`Register-ScheduledTask … -Environment`）或写在机器/用户的系统环境变量里。

**macOS（launchd）**：

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.ssh-manager.cache-refresh</string>
    <key>ProgramArguments</key>
    <array>
        <string>/usr/local/bin/ssh-manager</string>
        <string>cache</string>
        <string>pull</string>
    </array>
    <key>EnvironmentVariables</key>
    <dict>
        <key>SSHMGR_CACHE_URL</key>
        <string>https://192.0.2.5:7878</string>
        <key>SSHMGR_CACHE_TOKEN</key>
        <string>&lt;设备码&gt;</string>
    </dict>
    <key>StartInterval</key>
    <integer>1800</integer>
    <key>RunAtLoad</key>
    <true/>
</dict>
</plist>
```

```bash
launchctl load -w ~/Library/LaunchAgents/com.ssh-manager.cache-refresh.plist
```

⚠️ **设备码 = 钥匙**：任何机器拿到 `<设备码>` + 能连 serve = 能拉整份 vault 快照。所以：用 **TLS** 防嗅探；设备码进 0600 配置文件 / 密码管理器，**别进 git**；机器失窃 → 立刻 `cache-tokens revoke`（见下）。

### 离线能做什么 / 不能做什么

| | 离线（`mcp --cache`） | 在线（serve） |
|---|---|---|
| `exec_command`（含 `sudo=true`） | ✅ 凭据从缓存取，broker 直拨目标机 SSH | ✅ |
| `download_file` / `upload_file` | ✅ 同上 | ✅ |
| `forward_port`（`-L`） | ✅ 同上 | ✅ |
| `list_servers` | ✅（列出缓存里 profile 范围内的） | ✅ |
| 加 / 改 / 删 server / profile / project / 凭据 | ❌ `ErrReadOnly` | ✅ |
| 未知目标机 host key | ❌ **fail-closed**（不写 `known_hosts`） | ❌ fail-closed（同 stdio） |

**铁律 + profile scoping 离线不变**：同一个 project token 在线 / 离线走的是**同一套**鉴权（验 token → 解析 project → profile → 只放行 `serverID ∈ profileID` 的命令）。离线只是把 vault 换成本地只读副本，agent 的活动范围（profile）和能做的操作（只读 + 已授权的 exec / 传输 / 转发）**完全一致**。

### 审计：本地 JSONL 边车，不回传、不合并

离线模式下，broker 的每次调用（exec / download / upload / forward / 被拒的写）都写进本机的 `cache-audit.log`（JSONL，每行一条）。**单向、零合并**——这份日志**不会**回传 serve 服务器，**不会**并进服务器的审计表，永远只在本机。

- 路径：`<UserConfigDir>/ssh-manager/cache-audit.log`（和 `cache.bin` 同目录）。
- 用途：操作者本机自查（谁在什么时候、用哪个 project / server、干了什么、成功没）。
- 如需集中审计：手工把各机的 `cache-audit.log` 收拢到你的日志系统（程序不代劳）。

### 吊销（机器失窃 / 设备码泄露）

设备失窃 / 设备码泄露 → 在服务器上：

```bash
ssh-manager cache-tokens revoke laptop
# → revoked cache token laptop (status=revoked)
```

**Lazy 生效**：该码**下次 `cache pull`** 直接被拒（`status != active`），那台机再也拉不到新缓存。已经在跑的 `mcp --cache` 会继续到本次 spawn 结束（下次重启 Claude Code 拉新缓存失败 → 该机离线路径断了）。

> ⚠️ **已拉下的 `cache.bin` 仍能被那台机的 DEK 解密**——吊销**只断"拉新"**，不擦"已拉"。这和失窃的 `store.db` 一样处置：**吊销 + 视敏感度轮换相关凭据**（`servers edit --password` / `--key` 换那台机接触过的服务器凭据，`projects rotate` 换 project token）。物理拿到机器的人 + 本机 DEK（keychain）= 能离线爆破那份当时的 vault 快照——这等同于"物理拿到一台配了 stdio vault 的机器"，不在本方案的威胁模型内（host-compromise = out of scope）。

### 与 export/import 的关系

两套不同的工具，**别混**：

| | export / import（[Plan 11](./backup-restore.md)） | cache（Plan 12，本节） |
|---|---|---|
| 目的 | 便携**口令加密**备份 / 迁移 / 灾难恢复 | 工作机**只读缓存**，断网兜底 |
| 鉴权 | 你的**口令**（KeePass 式） | 设备授权码（owner 发、可吊销） |
| 落地的 vault 可写吗 | ✅ import 进一个**可写** vault | ❌ 只读（`ErrReadOnly`） |
| 怎么触发 | 手动 `export` / `import` | 设备码 + OS 调度器自动 `cache pull` |
| 格式 | `SSHMGRV1` 信封（Argon2id + AES-GCM）封 `Snapshot` JSON | 原始 key AES-GCM 封**同一份** `Snapshot` JSON |

两者**复用同一份 `store.Snapshot`**（Plan 11 打的地基）——序列化格式一致，加密信封不同（export 用口令派生 key，cache 用本机 keychain 的 DEK）。

### 限制（如实）

- **缓存只读**：离线能 exec / 传输 / 转发，但**任何写都被拒**（`ErrReadOnly`）。要加改删得连上 serve。
- **自动刷新靠 OS 调度器**（systemd timer / 任务计划 / launchd），**不是**进程常驻的 daemon——`ssh-manager` 本身没有内置调度器。
- **运行中的 `mcp --cache` 不会热加载新缓存**——下次 spawn（Claude Code 重启 MCP 子进程）才看到新快照。在线的 serve 是每请求实时鉴权，没有这个问题。
- **离线审计分散在各机本地**：`cache-audit.log` 不回传、不合并——要集中视图得自己收。
- **首次 `cache pull` 必须在线**——缓存还没拉下来之前，`mcp --cache` 跑不起来（会报 `cache DEK not found` / `no such file`）。
- **物理失窃 ≠ 远程吊销能解决**：见上"吊销"——已拉下的缓存被本机 DEK 守着，吊销只断"拉新"。

---

## 相关文档

- [getting-started.md](./getting-started.md)——单机 stdio 从零到跑通（**默认模式**，第一次用先看这篇）。
- [agent-access.md](./agent-access.md)——project token 生命周期（`rotate` / `disable` / `revoke` 的 Lazy 语义）；**serve 模式完全适用**，token 管理在同一台服务器上做。
- [managing-servers.md](./managing-servers.md)——服务器增删改查（在 serve 那台**服务器**上操作）。
- [scenarios.md](./scenarios.md)——应用场景示例（GPU 巡检、部署、端口转发……，两种模式都适用）。
- 仓库根 [README 的 "Multi-machine: serve mode"](../README.md#multi-machine-serve-mode-remote-agents-on-a-vlan) 节（英文概览）。
