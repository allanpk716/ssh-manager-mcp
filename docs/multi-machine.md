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
ssh-manager unlock                                  # master key → 固定路径裸文件 (master.key.plain)
ssh-manager servers add --name gpu --host 192.0.2.10 --user deploy --password '...'
ssh-manager profiles add team-a && ssh-manager profiles grant team-a gpu
ssh-manager projects add my-agent --profile team-a  # 打印一次性 token（工作机要用，记下来）
```

### Step 2（服务器侧）：启动常驻 broker

```bash
ssh-manager serve --addr 0.0.0.0:7878
# → ssh-manager serve: listening on 0.0.0.0:7878 (tls=auto)
# → auto-TLS cert (self-signed). client pin: sha256:abcd1234...
```

| 选项 | 说明 |
|---|---|
| `--addr` | 监听地址。默认 `127.0.0.1:7878`（**只本机**——远程用不了）。多机场景写 `0.0.0.0:7878` 或服务器的 VLAN IP。 |
| `--tls-cert` / `--tls-key` | **可选**。不挂时（默认）serve 首次启动**自动生成一张自签 ed25519 证书**，落 vault 固定目录（`serve-cert.pem` / `serve-key.pem`，ACL 与 `master.key.plain` 同级）。要用自己的证书才挂这两个 flag。 |

**自签证书 + 指纹钉死 = 零证书分发。** 自签证书首次生成时，serve 把它的 **SPKI 指纹**（`sha256:...`）打印到启动日志（`client pin:` 那行）。客户端（`cache pull` / 工作机）用这个指纹**钉死**对端 —— 连接时校验服务器证书公钥 == 钉死的指纹，不等即拒，**首次连接即校验（零 MITM 窗口）**。无需在每台客户端装根证书。

**指纹怎么交给工作机**：`cache-tokens add` 签发设备码时，会把当前 serve 指纹**一并打印**（默认编进 `cache pull` 示例命令，形态 `<设备码>:<指纹>`）。详见下面「离线只读缓存」Step 1。也可用 `ssh-manager serve cert-info` 随时查当前指纹。

> ⚠️ **客户端不带指纹 = 明文回退**：`cache pull` 在没拿到指纹（env / `--pin` / token 内嵌三处都没有）时，会退回明文 HTTP 并打一行 STDERR 警告 —— 这是为了让**旧客户端平滑升级**（不会硬断），但意味着升级窗口里客户端可能还在明文拉。迁移时请尽快把指纹配上（`SSHMGR_SERVE_PIN=...`）。

**让它常驻 + 开机自启**（serve 是个长驻进程，别在前台手跑就完事）：

- **Windows / Linux / macOS**：跑 `ssh-manager serve install`——程序用 [`github.com/kardianos/service`](https://github.com/kardianos/service) 自己注册系统服务（Win=Windows Service、Linux=systemd unit、macOS=launchd plist），三平台一条命令，无需手写 XML / unit / plist。详见下面「`serve install` 三平台一条龙」小节。进阶用户若偏好第三方包（NSSM / 手写 systemd / 手写 launchd），见 [getting-started 的第三方服务包小节](./getting-started.md#第三方服务包可选给不想用内置-install-的进阶用户)。

#### `serve install` 三平台一条龙（Plan 16，kardianos）

```bash
# 在已经跑过 unlock（master.key.plain 已生成）的机器上（Windows 需 admin / Linux·macOS 需 sudo）：
ssh-manager serve install --addr 0.0.0.0:7878
```

（`--tls-cert/--tls-key` 可选；不挂则服务自签证书，同 Step 2。）

程序会：

1. **precheck master.key**：`master.key.plain` 存在且可读。不存在就报错让你先 `unlock`（Plan 16：master.key 是裸文件 + ACL，service 账户需能读——Windows 默认 `LocalSystem` / Linux·macOS 默认 root，目录 ACL 已含这两个）。
2. **解析二进制**：`os.Executable` 取当前 ssh-manager 路径 → service 配置里写"跑这个二进制 + `serve --addr ...` 参数"。**service 用的是同一份代码同一个二进制**。
3. **加固 vault 目录 ACL**（Windows，best-effort）：`master.key.plain` 的文件 ACL 已由 `unlock` 设好（`SYSTEM` + `Administrators` + 当前用户，移除 `Users`/`Authenticated Users`/`Everyone`，禁用继承）；这一步对**目录**再做一遍 defense-in-depth。
4. **注册 + 立即启动**：kardianos 调用各平台原生 service manager（Windows SCM / systemd / launchd），`RestartOnFailure` 用各平台原生概念表达（Win `OnFailure=restart`、Linux `Restart=on-failure`、macOS `KeepAlive=true`）。重装是**幂等**的（先 best-effort 注销旧的，再装新的——支持"升级二进制后重装"的常见流程）。

配套命令：

```bash
ssh-manager serve status      # 四信号：service / process / http / vault
ssh-manager serve uninstall   # 停 service + 注销（不删 vault 数据）
```

`serve status` 四路独立检查：

```
service:   Running (kardianos svc.Status() —— byte 枚举，非本地化文本)
process:   running
http:      responding (401/200 = auth working)
vault:     ok
overall:   HEALTHY
```

- **service**：kardianos `svc.Status()`（Running / Stopped / Unknown / NOT INSTALLED）。**locale-independent**（Plan 15 FINDING E 的修复沿用：旧的 PowerShell `Get-ScheduledTask.State` 文本解析在 zh-CN 下挂掉，byte 枚举无此问题）。
- **process**：是否有 ssh-manager 进程在跑（Win `tasklist` / POSIX 扫 `/proc/comm`）。
- **http**：bound addr 是否响应（401/200 都算活——auth 闸在工作）。
- **vault**：`master.key.plain` 是否**存在 + 可读 + 是合法的 32 字节 key**（直接文件 probe，不扫日志——catch 到缺 key / 损坏 / 长度错的 key，那种"进程在跑但 boot 时会 crash-loop"的失败模式）。

`overall` 仅在四路全过时 `HEALTHY`——任一退化都有具体哪一路的提示。

#### service 账户 + 威胁模型（重要）

service 默认账户：

| 平台 | 默认账户 | 含义 |
|---|---|---|
| Windows | `LocalSystem` | 最高权限，能读 `master.key.plain` |
| Linux | root | 同上 |
| macOS | root（sudo 跑时） | 同上 |

> ⚠️ **R3 风险**：service 账户能读 master.key = service 账户 compromise 等同于 admin compromise（R1）。这是 L1+ 模型接受的代价。完整威胁模型（R1/R2/R3 + 适用前提 + 升级路径 U1/U2/U3）见 [threat-model.md](./threat-model.md)。

#### upgrade（从 Plan 14/15 升级到 Plan 16）

已有 Plan 14（user-scope DPAPI）或 Plan 15（machine-scope DPAPI）vault 的机器升级到 Plan 16（FileKeyProvider）——**旧 master.key 是 DPAPI blob，新版本读不了**。流程见 [backup-restore.md 的 Plan 16 迁移 Runbook](./backup-restore.md)。核心是两条路二选一：

- **migrate-path**（若旧 master.key 在当前 session 可解）：`ssh-manager migrate-path --from <旧路径>`，自动搬 `store.db` + `master.key.plain` 到新固定路径 + N/N 自检 + 删旧。
- **export + import**（若旧 master.key 在当前 session 读不出——NUC10 的 sshd 现状）：在 RDP / 交互 session 跑 `export` 到 `.sme` → 新版本 `unlock` 建 new key → `import --passphrase-file` 导入新固定路径。

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
5. **bearer token = 钥匙**：谁拿到某项目的 token + 能连到服务器 = 拿到那个项目 profile 里的**所有服务器**。所以：serve 默认**自签 TLS + 指纹钉死**防嗅探/防 MITM；用 [`projects rotate`](./agent-access.md)（换发）/ [`revoke`](./agent-access.md)（吊销）管 token 生命周期；token 进密码管理器、别进 git。

---

## 后续路线

serve 模式是多机支持的**第一期（Phase 1）= 在线 live 远程访问**。**export/import（Plan 11，便携加密备份 / 迁移）已落地**（见 [backup-restore.md](./backup-restore.md)）。规划中的多机后续：

| 计划 | 解决什么 | 状态 |
|---|---|---|
| Plan 11 · export/import | 整个 vault 口令加密便携文件：备份 / 迁移 / 灾难恢复 | ✅ 已做（[backup-restore.md](./backup-restore.md)） |
| Plan 12 · 离线只读缓存 | 工作机本地缓存加密 vault，断网时只读用、自动刷新 | ✅ 已做（本节[「离线只读缓存」](#离线只读缓存plan-12)） |
| Plan 13 · 群晖自动备份 | 服务器定时出明文快照到 NAS，灾难恢复 | ✅ 已做（[backup-restore.md Plan 13](./backup-restore.md#plan-13--nas-定时明文备份backup-create--verify)） |
| Plan 14/15 · Windows 生产部署 | DPAPI master key + `serve install` Task Scheduler | ⚠️ 已 Superseded by Plan 16 |
| Plan 16 · 固定路径 + FileKeyProvider | 三平台固定路径 + 裸文件 master key（L1+）+ kardianos 跨平台 `serve install` + `migrate-path` | ✅ 已做（本篇 + [threat-model.md](./threat-model.md) + [getting-started 第三方服务包](./getting-started.md#第三方服务包可选给不想用内置-install-的进阶用户)） |

**现在：serve = 在线 live（**三平台一条龙 `serve install`**，kardianos 收敛 Windows Service / systemd / launchd）；备份 / 迁移已可（export/import + Plan 13 NAS + Plan 16 `migrate-path`）；离线只读缓存已落地（Plan 12，cache DEK = 固定路径裸文件）。**

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
 │   cache-dek.key 裸文件（固定路径）       ──────►  cache.meta.json
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
# Server fingerprint (serve cert SPKI): sha256:abcd1234...
#
# On the work machine:
#   ssh-manager cache pull --url https://192.0.2.5:7878 --token '<设备码>:sha256:abcd1234...'
#   # (or) set SSHMGR_SERVE_PIN=sha256:abcd1234... and pass --token <设备码>
```

- `--name` **必填**且唯一（比如 `laptop` / `desktop-2`），后续吊销靠它。
- 设备码**只显示一次**——当场拉、或记进密码管理器。
- **指纹是自动加密的关键**：设备码旁那行 `Server fingerprint` 是 serve 自签证书的 SPKI 指纹。`cache pull` 拿到它（任一形式：token 内嵌 `<码>:<指纹>`、`--pin`、或 `SSHMGR_SERVE_PIN`）就用 TLS + 指纹钉死连 serve；拿不到就退回明文（仅升级窗口用）。指纹可随时用 `ssh-manager serve cert-info` 重查。
- 其他管理命令：
  ```bash
  ssh-manager cache-tokens ls          # name / id / prefix / status / last_pull（不显示码）
  ssh-manager cache-tokens revoke laptop   # 位置参数，吊销（Lazy，下次 pull 被拒）
  ```

#### Step 2（工作机）：第一次拉缓存 + 配 `.mcp.json`

在工作机装好 `ssh-manager` 后：

```bash
# 第一次拉（设备码 + 指纹一起给；之后会被调度器自动重拉）
ssh-manager cache pull --url https://192.0.2.5:7878 --token '<设备码>:sha256:abcd1234...'
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
| `--token` / `SSHMGR_CACHE_TOKEN` | 设备授权码（`cache-tokens add` 发的那个）。可写成 `<设备码>:<指纹>` 把指纹一起带上。必填 |
| `--pin` / `SSHMGR_SERVE_PIN` | serve 证书 SPKI 指纹（`sha256:...`）。**任一处给了就走 TLS + 指纹钉死**；优先级 `SSHMGR_SERVE_PIN` > `--pin` > token 内嵌；三者都无 → 明文回退（打 STDERR 警告）。`cache-tokens add` 默认把指纹打进输出。 |
| `SSHMGR_CACHE_DIR` | 缓存目录覆盖（默认 `UserConfigDir/ssh-manager`） |

> **缓存目录**：`cache.bin` / `cache.meta.json` / `cache-audit.log` 进 `SSHMGR_CACHE_DIR`（默认 `os.UserConfigDir()/ssh-manager/`，即 Linux `~/.config/ssh-manager/`、macOS `~/Library/Application Support/ssh-manager/`、Windows `%AppData%\ssh-manager\`）。**DEK** 存在 vault 固定路径下的 `cache-dek.key` 裸文件（Win `C:\ProgramData\ssh-manager\cache-dek.key` / Unix `/var/lib/ssh-manager/cache-dek.key`，Plan 16 T4 从 OS keychain/DPAPI 迁来）。
>
> ⚠️ **已知不一致**（Plan 16 T4 只迁了 DEK，未迁 `cache.bin` 路径）：`cache.bin` 在 `UserConfigDir`、`cache-dek.key` 在 vault 固定路径——两份不在同一目录。功能正常（DEK 文件能读、cache 能解），但离线拷盘需同时拿到两处。后续清理工作会收敛到同一目录。**威胁模型**：cache.bin + cache-dek.key 同机不同目录 → 同盘 → 离线拷盘可解 cache；cache 是只读快照非完整凭据，与 master.key 同等级（L1+，见 [threat-model.md](./threat-model.md)）。

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
Environment=SSHMGR_SERVE_PIN=sha256:<指纹>   # 从 `serve cert-info` 或 `cache-tokens add` 输出取
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
# 0600 配置文件存 URL + 设备码 + 指纹
@"
SSHMGR_CACHE_URL=https://192.0.2.5:7878
SSHMGR_CACHE_TOKEN=<设备码>
SSHMGR_SERVE_PIN=sha256:<指纹>
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
        <key>SSHMGR_SERVE_PIN</key>
        <string>sha256:&lt;指纹&gt;</string>
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

⚠️ **设备码 = 钥匙**：任何机器拿到 `<设备码>` + 能连 serve = 能拉整份 vault 快照。所以：serve 默认**自签 TLS + 指纹钉死**（指纹 = serve 证书公钥，`cache pull` 钉死它防 MITM）；设备码进 0600 配置文件 / 密码管理器，**别进 git**；机器失窃 → 立刻 `cache-tokens revoke`（见下）。

> **指纹失配 ≠ 设备码泄露**。指纹失配意味着你连到的服务器公钥变了（可能是 serve 重装重生证书 = 正常；也可能是中间人 = 异常）。serve 重生证书（如重装、迁移到新机）后，用 `ssh-manager serve cert-info` 拿新指纹，更新各客户端的 `SSHMGR_SERVE_PIN`。这是**指纹钉死**的预期代价：换 key 必须重新交接信任。

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

- 路径：`<UserConfigDir>/ssh-manager/cache-audit.log`（和 `cache.bin` 同目录；`cache-dek.key` 在 vault 固定路径——见上"已知不一致"）。
- 用途：操作者本机自查（谁在什么时候、用哪个 project / server、干了什么、成功没）。
- 如需集中审计：手工把各机的 `cache-audit.log` 收拢到你的日志系统（程序不代劳）。

### 吊销（机器失窃 / 设备码泄露）

设备失窃 / 设备码泄露 → 在服务器上：

```bash
ssh-manager cache-tokens revoke laptop
# → revoked cache token laptop (status=revoked)
```

**Lazy 生效**：该码**下次 `cache pull`** 直接被拒（`status != active`），那台机再也拉不到新缓存。已经在跑的 `mcp --cache` 会继续到本次 spawn 结束（下次重启 Claude Code 拉新缓存失败 → 该机离线路径断了）。

> ⚠️ **已拉下的 `cache.bin` 仍能被那台机的 DEK（`cache-dek.key`）解密**——吊销**只断"拉新"**，不擦"已拉"。这和失窃的 `store.db` 一样处置：**吊销 + 视敏感度轮换相关凭据**（`servers edit --password` / `--key` 换那台机接触过的服务器凭据，`projects rotate` 换 project token）。物理拿到机器的人 + 本机 `cache-dek.key` = 能离线爆破那份当时的 vault 快照——这等同于"物理拿到一台配了 stdio vault 的机器"，不在本方案的威胁模型内（host-compromise = out of scope，见 [threat-model.md](./threat-model.md)）。

### 与 export/import 的关系

两套不同的工具，**别混**：

| | export / import（[Plan 11](./backup-restore.md)） | cache（Plan 12，本节） |
|---|---|---|
| 目的 | 便携**口令加密**备份 / 迁移 / 灾难恢复 | 工作机**只读缓存**，断网兜底 |
| 鉴权 | 你的**口令**（KeePass 式） | 设备授权码（owner 发、可吊销） |
| 落地的 vault 可写吗 | ✅ import 进一个**可写** vault | ❌ 只读（`ErrReadOnly`） |
| 怎么触发 | 手动 `export` / `import` | 设备码 + OS 调度器自动 `cache pull` |
| 格式 | `SSHMGRV1` 信封（Argon2id + AES-GCM）封 `Snapshot` JSON | 原始 key AES-GCM 封**同一份** `Snapshot` JSON |

两者**复用同一份 `store.Snapshot`**（Plan 11 打的地基）——序列化格式一致，加密信封不同（export 用口令派生 key，cache 用本机固定路径 `cache-dek.key` 裸文件的 DEK）。

### 限制（如实）

- **缓存只读**：离线能 exec / 传输 / 转发，但**任何写都被拒**（`ErrReadOnly`）。要加改删得连上 serve。
- **自动刷新靠 OS 调度器**（systemd timer / 任务计划 / launchd），**不是**进程常驻的 daemon——`ssh-manager` 本身没有内置调度器。
- **运行中的 `mcp --cache` 不会热加载新缓存**——下次 spawn（Claude Code 重启 MCP 子进程）才看到新快照。在线的 serve 是每请求实时鉴权，没有这个问题。
- **离线审计分散在各机本地**：`cache-audit.log` 不回传、不合并——要集中视图得自己收。
- **首次 `cache pull` 必须在线**——缓存还没拉下来之前，`mcp --cache` 跑不起来（会报 `cache DEK not found` / `no such file`）。
- **物理失窃 ≠ 远程吊销能解决**：见上"吊销"——已拉下的缓存被本机 DEK 守着，吊销只断"拉新"。

### 自动 TLS 迁移 Runbook（从旧版明文 / 外部证书升级）

新版 serve **默认强制 TLS**（无 `--tls-cert` 时自签）。已部署的明文或外部证书部署**无需停服、无需清数据**即可平滑切到指纹钉死：

1. **升级服务器二进制**到含自动 TLS 的新版（serve 暂不重启）。
2. **拿指纹**：`ssh-manager serve cert-info` —— 打印当前（或首次生成）的 SPKI 指纹 `sha256:...`。幂等，不会破坏现有 flag。
3. **重启 serve** → 从此强制 TLS，启动日志打印 `client pin: <指纹>`。
4. **升级各工作机二进制**，把指纹配上（任一形式）：
   - 重新 `cache-tokens add`（默认把指纹打进设备码输出）；或
   - 在调度器配置（systemd unit / 任务计划 / launchd plist）的 `Environment` / `EnvironmentVariables` 里加 `SSHMGR_SERVE_PIN=sha256:<指纹>`。
5. **下一次定时 `cache pull`** → 走 TLS + 指纹钉死成功，迁移完成。

**迁移窗口不断链保证**：新 `cache pull` 没拿到指纹时**退回明文 + STDERR 警告**（不硬断），所以"serve 已切 TLS、某工作机还没配指纹"的窗口里，那台机不会硬失败，只是继续明文拉直到指纹配上。**指纹配上后才真正加密**——所以迁移要尽快把指纹分发到所有工作机。

> ⚠️ **唯一硬失败组合：旧二进制 client（完全没有 pin 逻辑）对新版 serve（强制 TLS）。** 这种 client 用明文 `http.DefaultClient` 打一个现已 TLS-only 的 serve，握手必败——这不是 bug，是"明文 client 没法跟 TLS server 说话"。**对策就是第 4 步本身：把那台机的二进制升级到新版**（升级后它就有 pin 逻辑 + 明文回退能力）。所以迁移别只升 server 就放着——务必把每台工作机的二进制也升上来，第 4 步是 load-bearing 的。

---

## 相关文档

- [getting-started.md](./getting-started.md)——单机 stdio 从零到跑通（**默认模式**，第一次用先看这篇）。
- [agent-access.md](./agent-access.md)——project token 生命周期（`rotate` / `disable` / `revoke` 的 Lazy 语义）；**serve 模式完全适用**，token 管理在同一台服务器上做。
- [managing-servers.md](./managing-servers.md)——服务器增删改查（在 serve 那台**服务器**上操作）。
- [scenarios.md](./scenarios.md)——应用场景示例（GPU 巡检、部署、端口转发……，两种模式都适用）。
- 仓库根 [README 的 "Multi-machine: serve mode"](../README.md#multi-machine-serve-mode-remote-agents-on-a-vlan) 节（英文概览）。
