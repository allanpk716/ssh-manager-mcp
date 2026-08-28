# 多机共享：桥姿态（一台服务器常驻权威 vault，多台机器共用）

> **适用场景**：你在**多台电脑**上开发/办公（同一个内网或虚拟局域网 VLAN），想让所有机器上的 AI agent 共用**同一份** SSH 服务器清单——凭据只存在一台权威 broker 上，工作机只持本地只读缓存。
>
> **单台机器不需要本篇**——直接用默认的 stdio 模式（见 [getting-started.md](./getting-started.md)）。多机形态是给"多机共用"这个场景的可选项。
>
> **Plan 42 批1 起的形态**（随下个发版）：serve 收窄为**权威 vault + `/snapshot` 拉取 + `/pair` 配对**（+批2 `/ui` 管理）——远程 MCP-over-HTTP（旧 ②a）已**移除**（根路径 404）。工作机 agent 一律走**本地只读缓存**（`mcp --cache`，只读 + 执行）；新机入网 = `ssh-manager pair` 一条龙。多机 agent **只读 + 执行**，写操作只在管理面（broker TUI / `serve pair` CLI / 批2 Web UI）。

---

## 该用哪种模式？（先看这张表）

| | **stdio（默认 · 单机）** | **多机桥姿态（可选 · 多机）** |
|---|---|---|
| broker 跑在哪 | Claude Code **按需 spawn** 的本地子进程 | 你**手动启动并常驻**的一台 VLAN 服务器 |
| agent 怎么连 | 本地 stdio | **pair 一条龙入网 → 本地只读缓存**（`mcp --cache`）；无远程 MCP 面 |
| 凭据放在哪 | 本机（自包含） | **只在服务器上**；工作机仅加密只读快照（cache.bin+DEK） |
| agent 可写吗 | ✅（本机 vault） | ❌ **只读快照**（写操作 `ErrReadOnly`——写只在管理面） |
| 离线能用吗 | ✅ 是（本机自包含） | ✅ 是（缓存本就为断网兜底而设计；保鲜 ≤30min 需在线） |
| 重启后要管吗 | 不用（客户端自动拉起） | 要（你得让 serve 常驻 / 开机自启） |
| 适合 | 单台机器 | 多台机器共用一份清单 |
| 配置复杂度 | 最低 | 中（要常驻服务；工作机侧 = 一条 `pair` 命令） |

**默认选 stdio。** 只有"多台机器要共用同一份服务器清单"时才上多机形态。

> 一句话分辨：**broker 是"按需拉起的子进程"（stdio）还是"你常驻的服务"（多机桥）。** 这是两种模式最根本的运营差异，下面的架构会展开。

---

## 架构

```
   多机（桥姿态）                         单机（stdio · 默认）

 ┌──工作机 A──┐  ┌──工作机 B──┐         ┌───你的机器───┐
 │  Claude    │  │  Claude    │         │  Claude      │
 │  Code      │  │  Code      │         │  Code        │
 │ (mcp --cache)│ │ (mcp --cache)│      │ （spawn 子进程）│
 └─────┬──────┘  └─────┬──────┘         └───────┬───────┘
       │ pair 入网 + 保鲜拉取（TLS+指纹钉死）      │ stdio
       │ 命令执行不走这条线 ▼                    ▼
      ┌──────────────────┐            ┌──────────────────┐
      │ VLAN 服务器       │            │ 本机              │
      │ ssh-manager serve │            │ ssh-manager mcp   │
      │  （常驻进程）      │            │  （按需子进程）     │
      │  权威 vault+/snapshot │        │  ┌────────────┐  │
      │  +/pair（+批2 /ui）│           │  │ vault+DEK  │  │
      │  ┌────────────┐  │            │  └────────────┘  │
      │  │ vault+DEK  │  │            └──────────────────┘
      │  └────────────┘  │                     │
      └──────────────────┘                     ▼ SSH
               （serve 不在命令路径上——               ▼
                 两形态都由 agent 侧直拨）        目标服务器
                   ▼ SSH（工作机直拨）
              目标服务器们
```

**本质区别：**

- **stdio（单机）**：Claude Code 读 `.mcp.json` 里的 `command`，**自己 spawn** `ssh-manager mcp` 子进程，broker 和 Claude Code 之间走 stdio。broker 的生死 Claude Code 管；机器自包含（vault 在本机）。详见 [getting-started.md 的"重启/关机后"](./getting-started.md#重启--关机后还要做什么吗不用mcp-客户端会自动拉起)。
- **多机桥姿态**：你在 VLAN 一台服务器上**常驻** `ssh-manager serve`（权威 vault）。各工作机经 **pair 一条龙**入网（SAS 人闸 → 凭据加密下发），持一份**本地加密只读快照**，agent 的子进程 `mcp --cache` 用它干活。**凭据只在服务器上**，工作机上只有只读快照；**命令从工作机直拨目标服务器**，serve 不在命令路径上。

**鉴权（两道独立的闸，永不互通）**：

- **设备码 → `/snapshot`**：拉取该设备绑定 profile 的授权裁剪快照（Plan 39），只进 `/snapshot` 这一条 HTTP 路由；吊销后 pinned 401 触发本地缓存销毁。
- **project token → 本地 spawn 闸**：`mcp --cache` 用 `SSHMGR_TOKEN` 对**快照内随行的 projects 表**校验后放行工具面——**它不再是任何远程 MCP 凭据**（Plan 42 批1 起 serve 无 MCP 面），也不进任何 HTTP 头。
- **铁律**（每条命令前重检 `serverID ∈ profileID`）和 stdio 完全一致——多机形态没有新增任何工具、没有动 agent 表面，只是把数据源换成本地只读快照。

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
# → ssh-manager serve: discovery: udp/7878 (on)
```

| 选项 | 说明 |
|---|---|
| `--addr` | 监听地址。默认 `127.0.0.1:7878`（**只本机**——远程用不了）。多机场景写 `0.0.0.0:7878` 或服务器的 VLAN IP。 |
| `--tls-cert` / `--tls-key` | **可选**。不挂时（默认）serve 首次启动**自动生成一张自签 ed25519 证书**，落 vault 固定目录（`serve-cert.pem` / `serve-key.pem`，ACL 与 `master.key.plain` 同级）。要用自己的证书才挂这两个 flag。 |
| `--pairing` / `--discovery` | **可选**。SAS 配对面（`/pair/*`）与 UDP 发现（udp/7878）的三态开关：显式置位才参与裁决，优先级 **显式 env（`SSHMGR_SERVE_PAIRING`/`SSHMGR_SERVE_DISCOVERY`）> 显式 flag > store 设置 > 缺省 true**；store 变更 ≤5s 生效，env/flag 变更需重启 serve。 |

**自签证书 + 指纹钉死 = 零证书分发。** 自签证书首次生成时，serve 把它的 **SPKI 指纹**（`sha256:...`）打印到启动日志（`client pin:` 那行）。客户端（`pair` / `cache pull` / 工作机）用这个指纹**钉死**对端 —— 连接时校验服务器证书公钥 == 钉死的指纹，不等即拒，**首次连接即校验（零 MITM 窗口）**。无需在每台客户端装根证书。

**指纹怎么交给工作机**：pair 时代它**自动交付**——discovery 的 offer 报文自带指纹、pair 信封内也封入 spki（client 钉的正是它配对的这把 key）。手工路径（`cache-tokens add`）仍会把指纹一并打印（默认编进 `cache pull` 示例命令，形态 `<设备码>:<指纹>`）。也可用 `ssh-manager serve cert-info` 随时查当前指纹。

> ⚠️ **客户端不带指纹 = 默认拒连（hard-fail）**：`cache pull` 在没拿到指纹（env / `--pin` / token 内嵌三处都没有）时，**默认拒绝拉取**（不再静默明文）——明文是 fail-open 隐患，已改为默认安全。pair 侧同理且更紧：`--url` 直连又不带 `--pin` 时**默认拒绝**（需显式 `--allow-tofu`，见 threat-model R12）。若确需明文（连旧明文 serve 调试），显式加 `--allow-plaintext` opt-in。详见下「离线只读缓存」节。

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

### Step 3（每台工作机）：`ssh-manager pair` 一条龙入网

> 🧭 全流程细节（发现/SAS/机械地址校验/产物落盘）见下[「配对入网：`ssh-manager pair`（Plan 42）」](#配对入网ssh-manager-pairplan-42)节；本步是最短路径。

工作机装好 `ssh-manager` 后：

```bash
ssh-manager pair --instance laptop
# → 发现（或已提示用 --url 直指）→ 屏显三件套：laptop @ https://192.0.2.5:7878 SAS 482913
# → owner 在 broker 机 TUI Pairing 页（或 serve pair approve laptop --profile team-a）
#   对照屏上「name @ url」与 client 屏 SAS 一致后批准
# → 批准后 120 秒内 client 完成 finish → 凭据加密下发 → 首拉落盘
# → 产物 pair.laptop.mcp.json（含真值 project token，0600）
```

把产物里的片段抄进该机的 `.mcp.json`（或 pair 时用 `--write-mcp <path>` 直接落位）：

```json
{
  "mcpServers": {
    "ssh": {
      "command": "ssh-manager",
      "args": ["mcp", "--cache"],
      "env": { "SSHMGR_TOKEN": "<项目token>" }
    }
  }
}
```

⚠️ `.mcp.json` 含 project token——**别提交 git**（和 stdio 一样）。机器失窃 = 设备码 + token 双吊销（见下「吊销三路径」）。

重启 Claude Code → 该机的 agent 就能用 SSH 工具了，范围 = 配对时绑定的 profile（**只读 + 执行**；加改删服务器去管理面）。

### Step 4：网络

服务器的 **TCP 7878**（`/snapshot` + `/pair/*`）与 **UDP 7878**（discovery 应答）只对**可信机器**开放。VLAN 内通常天然隔离；跨网段记得 ACL。serve 不内置 IP 白名单——网络层隔离 + TLS 指纹钉死 + SAS 人闸三层够了。

---

## 使用场景

### 场景 A：单人多机 / 家用 VLAN（典型）

一台常开的家用服务器 / NUC / 软路由 + 笔记本 + 台式机，都在同一个 VLAN。

- **服务器**：常驻 `ssh-manager serve`（systemd 托管 + 自动 TLS）。所有服务器清单建在它上面。
- **笔记本 / 台式机**：各跑一条 `ssh-manager pair --instance <名>`，owner 在 broker TUI 批准即入网。任意一台上的 agent 都能用**同一份**清单的**自己的只读快照**。
- **授权范围（一机一码一 profile）**：pair 批准时 owner 选 profile——该机拉到的就是、且只是这个 profile 授权的服务器。要让某台机只看部分服务器，建一个专用 profile 授权那几台，批准时选它。

> serve 的授权是**按设备码 → profile 裁剪快照**（Plan 39）+ **project token → 本地 spawn 闸**两道独立闸路由的：一台 serve 同时服务多台工作机，互不串扰。

### 场景 B：单机（不需要本篇）

只用一台机器 → 用 stdio（[getting-started.md](./getting-started.md)）。serve 对你是多余开销（要常驻一个服务、要管 TLS、要在线）。

---

## 隧道与 owner 急停（Plan 42 后口径）

多机 agent 全部跑在各自工作机的本地 broker 子进程里——`forward_port` 的监听**恒在 agent 所在机器的环回地址**（缺省 `127.0.0.1`），随 ②a 移除，"隧道开在 serve 主机 + `serve bind` 白名单跨机共享"的拓扑已一并退役（`serve bind` 子命令已删除）。非环回监听仍被 fail-closed 拒绝（白名单表不再有管理入口，恒为空 = 环回 only）——被劫持 agent 无法把隧道 bind 到 VLAN 面。

- **owner 急停**：在权威 vault 所在机器（如 NUC10）上 `ssh-manager tunnels ls` 看**共享该 vault 的 broker**（本机 stdio broker 等）的在线隧道（registry 镜像，≤45s 新鲜度），`tunnels kill <tunnel_id>` / `tunnels kill --project <name>` 拆（≤~15s 生效）；revoke/disable 亦级联拆除。**各工作机 cache 模式 client 的隧道不在此域**——不进 registry、不受 kill 单/级联管辖（机制性恒环回）；那台机离线时要拆隧道，去那台机上杀进程（或等它回连触发 cache 销毁，见「吊销」节）。
- **跨机用隧道**：笔记本想用某机开的端口 → 在那台机上直接用（环回），或让该机的 agent `exec_command` 起目标服务——不再有 serve 主机中转监听。

**审计取证去哪台机跑（Plan 36）**：`ssh-manager audit` 读的是**本机** vault——serve 拓扑下 agent 的动作审计行落在**各自 client 机的 `cache-audit.log`**（本地 JSONL，不回传）；权威 vault 的 `audit_log` 记录的是**管理面动作**（发码/批准/吊销等）与共享该 vault 的 broker 行为。要看某台工作机 agent 的历史，去那台机上收 `cache-audit.log`。

---

## 限制（如实，必读）

1. **多机 agent 只读**：agent 的活动面 = 本地只读快照——能 exec / 传输 / 转发，**任何写都被拒**（`ErrReadOnly`）。加改删服务器 / 发码 / 批准配对去**管理面**（broker TUI / `serve pair` / 批2 Web UI）。没有"在线可写"的多机 agent 模式（②a 已移除，Plan 42 批1 起根路径 404）。
2. **保鲜需在线**：缓存自动保鲜（≤30min TTL）要能连上 serve——服务器挂了 / VLAN 断了 / 笔记本带出门 = 停在旧快照上继续干活（功能不受影响，新授权/改动看不到）；重新连上后下次懒检查自动追平。
3. **服务器是单点**：服务器挂了 = 没人能入网/保鲜，存量缓存照常干活，直到它恢复。**自动备份 / 灾难恢复已落地**——[Plan 13](./backup-restore.md#plan-13--nas-定时明文备份backup-create--verify)（NAS 定时明文备份）+ [export/import](./backup-restore.md)（Plan 11，便携加密备份）。恢复手段：从 NAS 拷最新快照或 export 文件，在新机 `ssh-manager import`（[见 backup-restore 的灾难恢复](./backup-restore.md#场景-③-灾难恢复)）。
4. **单 owner 设计**：多个人共用同一个 vault、按人隔离访问——**不在范围**。本方案是"一个人、多台机"。多人场景需要 per-user ACL + 审计隔离，是另一个量级的功能。
5. **两把钥匙都要管**：设备码（拉取权，可吊销、pinned 401 即销毁本机缓存）+ project token（spawn 闸，轮换/吊销见 [agent-access.md](./agent-access.md)）。都进密码管理器/0600 文件、别进 git。吊销生效语义见下「吊销」节与 [agent-tools.md](./agent-tools.md) 的「吊销三路径」。

---

## 后续路线

多机支持历经：**Phase 1**（在线 live 远程访问，Plan 10）→ Plan 12 离线只读缓存 → **Plan 42 起收敛为桥姿态**（远程 MCP 面移除，缓存形态成为唯一多机姿势）。**export/import（Plan 11，便携加密备份 / 迁移）已落地**（见 [backup-restore.md](./backup-restore.md)）。

| 计划 | 解决什么 | 状态 |
|---|---|---|
| Plan 11 · export/import | 整个 vault 口令加密便携文件：备份 / 迁移 / 灾难恢复 | ✅ 已做（[backup-restore.md](./backup-restore.md)） |
| Plan 12 · 离线只读缓存 | 工作机本地缓存加密 vault，断网时只读用、自动保鲜（内置，无需 OS 调度器） | ✅ 已做（本节[「离线只读缓存」](#离线只读缓存plan-12)） |
| Plan 13 · 群晖自动备份 | 服务器定时出明文快照到 NAS，灾难恢复 | ✅ 已做（[backup-restore.md Plan 13](./backup-restore.md#plan-13--nas-定时明文备份backup-create--verify)） |
| Plan 14/15 · Windows 生产部署 | DPAPI master key + `serve install` Task Scheduler | ⚠️ 已 Superseded by Plan 16 |
| Plan 16 · 固定路径 + FileKeyProvider | 三平台固定路径 + 裸文件 master key（L1+）+ kardianos 跨平台 `serve install` + `migrate-path` | ✅ 已做（本篇 + [threat-model.md](./threat-model.md) + [getting-started 第三方服务包](./getting-started.md#第三方服务包可选给不想用内置-install-的进阶用户)） |
| Plan 40 · 多实例（批1 + 批2） | 同机 N agent 各授权各 profile 的独立 cache 实例（目录 + per-instance DEK + `--instance` + MAX_OFFLINE 持久化）；批2 = 首次 enroll **自动归位** + TUI `[i]` 实例切换 / 向导接入卡 + `cache config` 子命令 | ✅ 已做（本篇[「多实例（同机多 agent）」](#多实例同机多-agent-plan-40-第一批)节；doctor 感知命名实例跟随 Plan 38） |
| Plan 42 · 模式缩减 + 发现配对 | 4→2 模式收敛（②a 移除）；UDP 发现 + SAS 配对一条龙（`ssh-manager pair`）；批2 = Web 管理 UI（手机优先，`/ui`） | ✅ 批1 已做（本篇）· 🔜 批2 |

**现在：多机 = 桥姿态（权威 vault 常驻 serve + 工作机 pair 一条龙入网 + 本地只读缓存干活）；备份 / 迁移已可（export/import + Plan 13 NAS + Plan 16 `migrate-path`）；写操作收敛到管理面（broker TUI / `serve pair`，批2 上手机 Web）。**

---

## 离线只读缓存（Plan 12）

> **一句话**：每台工作机在本地持有一份**加密的只读** vault 快照，连不上 serve 服务器时 agent 照常跑（exec / download / upload / 转发），但**任何写都被拒**（`ErrReadOnly`）。

### 它解决什么

把**该设备绑定 profile 的授权集**加密拉到工作机本地（Plan 39 起按授权裁剪），agent 用这份缓存干活（只读 + 执行）——断网/服务器重启**照常工作**（Plan 42 起这就是多机的唯一工作方式，不再有"在线远程 MCP + 离线兜底"两态）。**不是双写、不是同步**——缓存是单向、只读、零合并的快照。

### 模型（两道独立的闸门）

```
 ┌──serve 服务器（owner 在这）─────────────────┐
 │  vault + master key                         │
 │  pair 批准铸发（或 cache-tokens add）        │       ① 发码：每台机一个、可吊销、
 │       --name laptop --profile team-a ──┐    │          绑定一个 profile（Plan 39）
 │                                     │       │
 │  GET /snapshot                      │       │   ② 拉取：设备授权码鉴权
 │   Authorization: Bearer <设备码> ◀──┼─拉─────┤   （和 project token 是
 │   → 该 profile 授权集的 Snapshot     │       │    两套不同的 verifier）
 │     JSON（授权服务器+其凭据）        │       │
 └─────────────────────────────────────┼───────┘
                                       │
 ┌──工作机（laptop）───────────────────▼────────┐
 │  pair 首拉 / cache pull                ──────►  DEK 加密落盘
 │   ↓                                            cache.bin (0600)
 │   cache-dek.key 裸文件（固定路径）       ──────►  cache.meta.json
 │                                                (url + pulled_at)
 │  mcp --cache 进程内（spawn 惰性拉取 + 每 30min    ③ 自动保鲜
 │   会话内拉取 + 热加载，无需 OS 调度器）          （进程内）
 │
 │  .mcp.json → mcp --cache + env SSHMGR_TOKEN（同一个 project token）
 │   ↓                                            ④ 断网照常
 │   读 cache.bin → 验 project token（铁律不变）→ broker 只读跑
 └────────────────────────────────────────────────┘
```

关键：**两道闸门，永不桥接**；拉取范围 = 设备码绑定的 profile 的授权集（Plan 39 起）——未授权的服务器及其凭据不出服务器。

| 闸门 | 鉴什么 | 进哪 |
|---|---|---|
| project token（pair 下发 / `projects add` 发的） | MCP 工具调用（exec / download / upload / forward） | **本地 spawn 闸**——`mcp --cache` 对快照内 projects 校验后放行（不再是任何远程 HTTP 凭据） |
| 设备授权码（pair 铸发 / `cache-tokens add` 发的，**绑定一个 profile**） | 拉取该 profile 授权集的 `/snapshot` | 只进 `/snapshot` |

一个 project token **不能** 拉 `/snapshot`（被 verifier 拒）；一个设备码**不能**驱动 MCP 工具；设备码拉到的也**只有它绑定 profile 的授权集**（Plan 39）。三套边界独立、从不互通——这是整个设计的**基石**（已被测试钉住：project token 打 `/snapshot` 必拒，设备码打 MCP 必拒，裁剪快照不含授权外服务器/凭据/audit）。

---

## 配对入网：`ssh-manager pair`（Plan 42）

> **一句话**：新工作机从「装好二进制」到「agent 可用」= 一条 `ssh-manager pair --instance <名>`——LAN 广播发现 broker → SAS 三件套人闸比对 → owner 批准 → 设备码 + project token + 指纹 + 时效上限**自动加密下发** → 首拉落盘 → `.mcp.json` 产物落盘。不再跨机手抄三串字符串。

### 全流程

1. **发现**（可跳过）：client 对本机所有非环回 IPv4 接口广播 UDP 7878 probe；serve **只单播回请求源**一条 offer（`name` + SPKI 指纹 + TCP 端口——零敏感字段）。多台 serve 同时在网时列清单供选（含 name@addr:port 与指纹前 16 字符）。拿不到 offer（防火墙挡 UDP/跨网段）就走 `--url` 直指。
2. **连接（pin 分级）**：pin 已知（discovery offer 自带 / `--pin` 显式）→ 全程 TLS 层 SPKI 硬校验（不匹配即中止，主防线）；`--url` 直连且无 `--pin` → **默认拒绝**，显式 `--allow-tofu` 才接受无锚通道（TOFU 逃生门，见 [threat-model.md](./threat-model.md) R12）。
3. **enroll → SAS 三件套**：client 生成临时密钥对 + 随机 id，`POST /pair/enroll`；serve 应答后 client **立即算出并在本屏显示**同一行三件套：`<name> @ <target_url> SAS <6位数字>`。**SAS 绑定整条 transcript 与密钥材料**（换钥型 MITM 会令 SAS 对不上）；pending 队列存 store 表（跨进程共享，serve 重启即作废 in-flight）。
4. **批准（人闸 + 机械校验）**：owner 在 **broker TUI 的 Pairing 页**（或 **`serve pair ls / approve / reject`** CLI 兜底）看到待批准行：`<name> @ <target_url> · 来源IP · hint · 剩余秒`。**批准面显示的是 name@url 两件 + 「SAS 码见 client 屏幕」提示**（SAS 派生需要 serve 进程内存里的密钥态，TUI/CLI 批准进程物理不可算，也不该伪造）——批准者对照 **client 屏的 SAS** 与 **批准行的 name@url** 三项一致后才批准。**机械地址校验**：serve 核对 client 声明的 `target_url` 是否为本机地址（非环回 IP 集 + hostname）——不符（疑似中继/假 discovery/错误网络）→ 大字 ⚠ 且拒绝常规批准，仅显式覆盖可用（CLI `serve pair approve --allow-foreign-url`；TUI 键入大写 `OVERRIDE`）。owner 选 profile（`pair.default_profile` 预选）→ CAS 批准，开 **120 秒** finish 窗口（enroll 后 **10 分钟**内不批准即过期作废）。
5. **finish（凭据自动下发）**：client 2s 轮询到 approved 后确认 finish（120s 内）；serve 在**单个事务**里铸设备码 + 建/复用 project（`pair-<名>`）+ 签 token + 落审计行，以 AES-256-GCM 信封返回 `{spki, profile, device_code, project_token, max_offline}`。
6. **先落盘，后首拉**：client 把 `cache.auth.json`（url+设备码+pin）+ `cache.config.json`（`max_offline`，缺省 24h）+ **`pair.<名>.mcp.json`**（完整 `.mcp.json` 片段，env.SSHMGR_TOKEN = 真值，0600）全部落盘**之后**才首拉——**首拉失败零丢失**：修复后重跑 `cache pull --instance <名>`，`.mcp.json` 从产物文件抄（或当初用 `--write-mcp <path>` 已直落）。终端**零完整凭据**（打印片段用 `<project-token>` 占位符，真值只在产物文件里）。
7. **收尾**：产物片段抄进 agent 的 `.mcp.json`（形态 = 下面「手工 enroll」Step 2 的 cache 形态；`--write-mcp` 则已就位），重启 Claude Code 即用。

### 命令速查

```bash
# 工作机（默认发现；或 --url 直指）：
ssh-manager pair --instance laptop                                   # LAN 广播发现
ssh-manager pair --instance laptop --url https://192.0.2.5:7878 --pin sha256:abcd...   # 直指 + 显式 pin
ssh-manager pair --instance laptop --write-mcp /path/to/.mcp.json    # 产物片段直落 agent 配置
ssh-manager pair --instance laptop --force                           # 同名重配对（清 auth/bin/meta/quarantine，保留 config——Plan 40 换码口径）

# broker 机（批准）：
serve pair ls                                   # 待批准队列（name/@url/来源IP/hint/窗口/⚠标记）
serve pair approve laptop --profile team-a      # 批准（输出 '<name> @ <url> (对照 client 屏 SAS 后批准)'）
serve pair approve laptop --profile team-a --allow-foreign-url   # 机械校验 ⚠ 时的显式覆盖
serve pair reject laptop                        # 拒绝（终态，该请求永远无法再 enroll）
```

- **`--instance` 必填** = 设备名 = 本地实例槽（Plan 40 三位一体：设备码 name = 实例名 = profile 授权单元）；命名纪律建议 `机器-实例`。
- **自动化免比对**：env `SSHMGR_PAIR_ASSUME_SAS=1` 跳过终端 SAS 确认（**STUB 大字警告**——无人值守 CI 专用，放弃人闸；机械地址校验与 TLS pin 仍在）。
- **同名覆盖**：目标实例已有 `cache.auth.json` → 默认拒绝；`--force` 按 Plan 40 换码 runbook 清理后重写（保留 `cache.config.json`）。
- 全部 flags 以 `ssh-manager pair --help` 为准；审计（enroll/批准/finish/拒绝）与状态变更**同事务**落权威 vault 的 `audit_log`，字段走脱敏白名单（永不落凭据值/token/设备码/pin/SAS/密文）。

---

## 手工 enroll（存量迁移官方路径 + CI 场景）

> **何时走手工**：① **存量 ②a 机器迁移**（serve 升 Plan 42 版本前的过渡，见 [compat-matrix.md](./compat-matrix.md) 三步迁移——旧 serve 上没有 `/pair`，只能手工）；② **CI / 无人值守自动化**（要把 enroll 做成可脚本化的两步，而非交互式 SAS 比对）。日常新机一律 `ssh-manager pair`。
>
> 🧭 各页签 / 设备码 / token / 指纹谁是谁，一页图解见 [concepts.md](./concepts.md)（概念模型：仓库 · 货架 · 装箱单 · 钥匙 · 水管 · 防伪封条）。

### Step 1（服务器侧，一次性）：发一个绑定 profile 的设备授权码

在 serve 服务器上（同一台常驻 broker 的机器）：

```bash
ssh-manager profiles grant team-a gpu          # 先配好该设备的授权集（装箱单）
ssh-manager cache-tokens add --name laptop --profile team-a
# Authorization code for "laptop" (shown once): <一长串设备码>
# Server fingerprint (serve cert SPKI): sha256:abcd1234...
#
# On the work machine:
#   ssh-manager cache pull --url https://192.0.2.5:7878 --token '<设备码>:sha256:abcd1234...'
#   # (or) set SSHMGR_SERVE_PIN=sha256:abcd1234... and pass --token <设备码>
```

> 也可在 broker 上用 `ssh-manager tui` 的「设备码」页签发——表单里选绑定 profile，设备码 + 指纹 + `cache pull` 示例命令一次性全屏显示（见 [README 的 TUI 主控台](../README.md#tui-主控台ssh-manager-tui)）。

- **`--profile` 必填（Plan 39）**：设备码绑定一个 profile，**该设备拉到的就是、且只是这个 profile 授权的服务器（含凭据）**——未授权服务器及其凭据不出服务器。一台机 = 一个码 = 一个 profile；要让某台机只看部分服务器，建一个专用 profile 授权那几台再绑它。
- **存量未绑码**（Plan 39 之前签发的）：拉取被拒（**403，不毁本地缓存**），owner 跑 `ssh-manager cache-tokens bind <name> <profile>` 原地补绑（保留名字/状态/拉取历史）即可恢复。
- `--name` **必填**且在 **active** 码中唯一（比如 `laptop` / `desktop-2`）；**revoke 后可重发同名**（旧的 revoked 行会被自动清理），后续吊销靠它。
- 设备码**只显示一次**——当场拉、或记进密码管理器。
- **指纹是自动加密的关键**：设备码旁那行 `Server fingerprint` 是 serve 自签证书的 SPKI 指纹。`cache pull` 拿到它（任一形式：token 内嵌 `<码>:<指纹>`、`--pin`、或 `SSHMGR_SERVE_PIN`）就用 TLS + 指纹钉死连 serve；**拿不到则默认拒连**（hard-fail，需显式 `--allow-plaintext` 才明文）。指纹可随时用 `ssh-manager serve cert-info` 重查。另：有 pin 时 URL 必须是 `https://`（否则 hard-fail —— http 不协商 TLS 会让 pin 静默失效）。
- 其他管理命令：
  ```bash
  ssh-manager cache-tokens ls          # name / id / prefix / status / profile / last_pull（不显示码）
  ssh-manager cache-tokens bind laptop team-a   # 未绑码补绑（Plan 39 存量修复）
  ssh-manager cache-tokens revoke laptop   # 位置参数，吊销（断拉新 + 回连销毁，见下「吊销」节）
  ```

### Step 2（工作机）：第一次拉缓存 + 配 `.mcp.json`

在工作机装好 `ssh-manager` 后：

```bash
# 第一次拉（设备码 + 指纹一起给；之后由 `mcp --cache` 自动保鲜）
ssh-manager cache pull --url https://192.0.2.5:7878 --token '<设备码>:sha256:abcd1234...'
# → pulled N servers / M credentials into <UserConfigDir>/ssh-manager/cache.bin

# 看缓存状态
ssh-manager cache status
# cache:    <UserConfigDir>/ssh-manager/cache.bin
# age:      12m3s
# servers:  N
# creds:    M
# scope:    team-a        ← Plan 39 拉取的裁剪快照;未 re-pull 的旧快照显示 unverified
# source:   https://192.0.2.5:7878
```

| 选项 / 环境变量 | 说明 |
|---|---|
| `--url` / `SSHMGR_CACHE_URL` | serve broker 的 URL（`https://host:7878`）。必填 |
| `--token` / `SSHMGR_CACHE_TOKEN` | 设备授权码（`cache-tokens add` 发的那个）。可写成 `<设备码>:<指纹>` 把指纹一起带上。必填 |
| `--pin` / `SSHMGR_SERVE_PIN` | serve 证书 SPKI 指纹（`sha256:...`）。**任一处给了就走 TLS + 指纹钉死**；优先级 `SSHMGR_SERVE_PIN` > `--pin` > token 内嵌；三者都无 → **默认 hard-fail**（需 `--allow-plaintext` 才明文）。格式非法（给了但非 `sha256:<64hex>`）→ hard-fail（防打错别字静默降级）。`cache-tokens add` 默认把指纹打进输出。 |
| `--allow-plaintext` | 显式 opt-in 明文拉取（无 pin 时）。**不安全**，仅用于连旧明文 serve 调试/过渡。默认关。 |
| `SSHMGR_CACHE_DIR` | 缓存目录覆盖（默认 `UserConfigDir/ssh-manager`） |

> **缓存目录**：`cache.bin` / `cache.meta.json` / `cache-audit.log` 进 `SSHMGR_CACHE_DIR`（默认 `os.UserConfigDir()/ssh-manager/`，即 Linux `~/.config/ssh-manager/`、macOS `~/Library/Application Support/ssh-manager/`、Windows `%AppData%\ssh-manager\`）。**DEK** 存在 vault 固定路径下的 `cache-dek.key` 裸文件（Win `C:\ProgramData\ssh-manager\cache-dek.key` / Unix `/var/lib/ssh-manager/cache-dek.key`，Plan 16 T4 从 OS keychain/DPAPI 迁来）。
>
> 💡 工作机上也可用 `ssh-manager tui --mode client` 打开 client 面板：查看连接摘要 / 缓存年龄 / 实例切换（`[i]`）并手动触发同步（`[s]`）；连接编辑已退役——新机入网/换码走 `ssh-manager pair`（见 [tui-multi-machine.md](./tui-multi-machine.md)）。
>
> ⚠️ **已知不一致**（Plan 16 T4 只迁了 DEK，未迁 `cache.bin` 路径）：`cache.bin` 在 `UserConfigDir`、`cache-dek.key` 在 vault 固定路径——两份不在同一目录。功能正常（DEK 文件能读、cache 能解），但离线拷盘需同时拿到两处。后续清理工作会收敛到同一目录。**威胁模型**：cache.bin + cache-dek.key 同机不同目录 → 同盘 → 离线拷盘可解 cache；cache 是只读快照非完整凭据，与 master.key 同等级（L1+，见 [threat-model.md](./threat-model.md)）。

`.mcp.json` 怎么配？只有一种形态——**本地缓存**（`mcp --cache`）；project token 走 `env`（pair 下发的与 `projects add` 发的是**同一个**东西）：

```json
{
  "mcpServers": {
    "ssh": {
      "command": "ssh-manager",
      "args": ["mcp", "--cache"],
      "env": { "SSHMGR_TOKEN": "<项目token>" }
    }
  }
}
```

> 多机 agent 只读 + 执行；写操作去管理面（broker TUI / `serve pair` / 批2 Web UI）。旧的「在线为主（`.mcp.json` 指 serve URL + Bearer）」形态已随 ②a 移除（Plan 42 批1 起根路径 404）——存量 `"type": "http"` 配置请按 [compat-matrix.md](./compat-matrix.md) 三步迁移改写。
>
> 片段权威源 = 代码渲染器 + golden 测试（internal/tui/wizardsteps*.go）；文档片段如与之不符以代码为准。TUI 操作教程见 [tui-multi-machine.md](./tui-multi-machine.md)。

### Step 3（工作机）：缓存自动保鲜（内置，默认无需 OS 调度器）

缓存现在**自己保鲜**——`mcp --cache` **进程内置**了整套拉取逻辑，默认无需配任何系统定时器：

- **spawn 惰性拉取**：Claude Code 启动 `mcp --cache` 时，若缓存超过 **30 分钟**（`--cache-max-age` 可调，`0` 关闭）且本机存过拉取凭据，会自动拉一次新缓存；失败静默用旧缓存。
- **会话内懒检查 + 热加载**：运行中的会话在**每次工具调用前**懒检查缓存是否超过 TTL（默认 30 分钟），过期才自动拉一次（空闲会话不刷新；同一 TTL 窗口内失败只试一次，退避到下个窗口）；新快照落盘后**下一次工具调用即生效**（hash 变化即换，未变不动）——无需重启 Claude Code。
- **凭据持久化**：首次 `cache pull` 成功后，拉取凭据（url + 设备码 + 归一后 pin）自动写入本机 `cache.auth.json`（0600，Windows 另加 ACL）；之后的自动拉取全靠它。
- 首次 `cache pull` 仍需手动（在线）执行一次。

##### 可选：系统定时器（给非 Claude 的消费方）—— legacy

> ⚠️ **legacy（v0.5.0+ 起基本用不上）**：`mcp --cache` 已**进程内自动保鲜**（spawn 惰性拉取 + 会话内按 TTL 懒检查 + 热加载，见上一节），Claude Code 一类经 MCP 的消费方**无需任何 OS 定时器**。下面三份模板只服务"别的程序直接读 `cache.bin`"的非 MCP 消费方。另外：Windows 下若你早年按本节配过计划任务 `ssh-manager-cache-refresh`，`ssh-manager clear`（client 角色）会**顺带删除**它；Unix 的自建 unit 不由程序删，需自行清理。

若这台机上还有**别的程序直接读 `cache.bin`**（不经 `mcp --cache`，比如脚本自己解快照），它们享受不到上述进程内自动保鲜——可照旧配 OS 定时器跑 `cache pull`。建议 **30 min**（按你 vault 的变动频率调）。环境变量走 unit 的 `Environment=` 或独立配置文件（**0600 权限**，里面有设备码）。

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

⚠️ **设备码 = 钥匙**：任何机器拿到 `<设备码>` + 能连 serve = 能拉整份 vault 快照。所以：serve 默认**自签 TLS + 指纹钉死**（指纹 = serve 证书公钥，`cache pull` 钉死它防 MITM）；设备码进 0600 配置文件 / 密码管理器，**别进 git**；机器失窃 → 立刻 `cache-tokens revoke`（见下）。设备码持久化在 `cache.auth.json`（0600，Windows 另加 ACL）；证书轮换后手动带新 `--pin` 重拉一次即可覆盖。

> **指纹失配 ≠ 设备码泄露**。指纹失配意味着你连到的服务器公钥变了（可能是 serve 重装重生证书 = 正常；也可能是中间人 = 异常）。serve 重生证书（如重装、迁移到新机）后，用 `ssh-manager serve cert-info` 拿新指纹，更新各客户端的 `SSHMGR_SERVE_PIN`。这是**指纹钉死**的预期代价：换 key 必须重新交接信任。

### 多机 agent 能做什么 / 不能做什么（cache 形态 = 唯一形态）

| | 多机 agent（`mcp --cache`，唯一形态） |
|---|---|
| `exec_command`（含 `sudo=true`） | ✅ 凭据从缓存取，本地 broker 直拨目标机 SSH |
| `download_file` / `upload_file` | ✅ 同上 |
| `forward_port`（`-L`） | ✅ 同上（监听恒本机环回） |
| `list_servers` | ✅（列出缓存里 profile 范围内的） |
| 加 / 改 / 删 server / profile / project / 凭据 | ❌ `ErrReadOnly`——写只在**管理面**（broker TUI / `serve pair` / 批2 Web UI） |
| 未知目标机 host key | ❌ **fail-closed**（不写 `known_hosts`） |

**铁律 + profile scoping 不变**：project token 的鉴权（验 token → 解析 project → profile → 只放行 `serverID ∈ profileID` 的命令）对快照内数据与对真实 vault 是**同一套**。多机形态只是把数据源换成本地只读副本，agent 的活动范围（profile）和能做的操作（只读 + 已授权的 exec / 传输 / 转发）与单机一致，唯独写操作被拒。

### 审计：本地 JSONL 边车，不回传、不合并

离线模式下，broker 的每次调用（exec / download / upload / forward / 被拒的写）都写进本机的 `cache-audit.log`（JSONL，每行一条）。**单向、零合并**——这份日志**不会**回传 serve 服务器，**不会**并进服务器的审计表，永远只在本机。

- 路径：`<UserConfigDir>/ssh-manager/cache-audit.log`（和 `cache.bin` 同目录；`cache-dek.key` 在 vault 固定路径——见上"已知不一致"）。
- 用途：操作者本机自查（谁在什么时候、用哪个 project / server、干了什么、成功没）。
- 如需集中审计：手工把各机的 `cache-audit.log` 收拢到你的日志系统（程序不代劳）。

### 吊销（机器失窃 / 设备码泄露）

**吊销生效三路径（owner 侧吊销后 client 侧何时失效，取决于吊销对象与设备在线状态）**：

| 吊销对象 | client 侧失效路径 | 时效 |
|---|---|---|
| **project token**（设备码仍活） | 下一次保鲜拉到的新快照已无该 project → 本地 spawn 闸拒绝 | 在线 ≤30min（保鲜 TTL） |
| **设备码** | 下一次 pull 收到 pinned 401 ⇒ **quarantine**（本地缓存四件销毁，见下） | 回连即断供（在线 ≤30min；期间旧快照里 project token 若未吊销仍可用——所以**失窃 = 双吊销**） |
| **永离线设备**（不 pull） | 旧快照 + 本地 project token 的可用窗口 = **`max_offline` 硬上限**（per-instance，pair 下发默认 24h；到期 `LoadCacheSnapshot` 拒载） | **不是 30 分钟**；窗口内失窃设备的最终兜底 = 轮换服务器凭据（见 §3.6 登记） |

设备失窃 / 设备码泄露 → 在服务器上：

```bash
ssh-manager cache-tokens revoke laptop
# → revoked cache token laptop (status=revoked)
# → reminder: also revoke project tokens issued to that device if it may be compromised
```

**语义 = 断拉新 + 回连销毁**（Plan 34 起；此前只断"拉新"）。该码**下次 `cache pull`**（手动，或 `mcp --cache` 的自动保鲜——spawn 惰性 + 会话内懒检查，**≤30min** 内撞上）直接被拒（`status != active`）；且这次拒绝不再只是拉新失败——**pinned 401 = 通过指纹验证的权威服务器明确拒绝**，客户端随之把本地 cache 侧**四件销毁**：

1. **DEK**（`cache-dek.key`；`SSHMGR_CACHE_DEK` seam 路径优先）**物理删除**——钥匙先死：任何一步后崩溃，最坏状态 = 密文留在原地但 DEK 已亡（不可解），crash-safe；
2. `cache.auth.json`（设备码明文）**物理删除**；
3. `cache.bin` → `quarantine/cache.bin.quarantined-<unix秒>`（rename 隔离，单份保留）；
4. `cache.meta.json` **物理删除**。

销毁后该机 spawn `mcp --cache` / `cache status` 报**明确的 quarantined 归因报文**（不再静默失败），无凭据不再自动拉取；恢复 = 重新发码（`cache-tokens add`）+ `cache pull` 重新 enroll（全量重建）。**明文 pull 的 401、网络错误 / TLS 失败、非 401 状态码永不触发销毁**——只有 pinned 连接上的 401 才是权威判定（明文 HTTP 劫持可伪造 401，不采信）。

**DEGRADED / manifest 语义**：DEK / auth / bin 三步是关键步——任一出错（目标**已不存在**除外，那算幂等成功）→ 返回值、stderr 与 manifest 如实记 **`DEGRADED + 失败步骤清单`**（单步失败不阻断其余步骤，尽力而为但如实汇报；meta 删除为非关键步，失败仅日志）。`quarantine/manifest.json` 本身是 **best-effort 记录**（quarantine 目录建不了/磁盘满/权限 → 只记日志并继续，**绝不构成销毁的前置条件**）。spawn/status 的归因报文按**三级降级链**判定：manifest 可读且新鲜（时间戳晚于 `cache.meta.json` 的 pulled_at）→ 完整归因（`done` / `done+DEGRADED` / 中断态 `started`）；manifest 不可读但 `quarantine/` 目录存在 → 无细节归因；目录也不存在 → 维持通用 missing/decrypt 错误（不做 quarantine 归因，防误报）。时间约束同时是归因重置的崩溃安全：重新 enroll 成功 pull 后，旧 manifest 即使残留（重置失败）也因时间戳早于新 meta 而**永不误报**。

**quarantine 痕迹口径**：隔离目录里的 `cache.bin` 密文**不可解密**（DEK 已删）——保留价值是**痕迹/审计，不是数据恢复路径**；误隔离的恢复 = 设备码仍活则重新 `cache pull` 即全量重建。

> **双失败窗口（登记，保守残余）**：manifest 写失败与 `cache.meta.json` 删除失败**叠加**时，真实发生的隔离可能因 meta 仍在记录而被归因链保守判为通用错误（漏归因方向——只会少说、不会误报）。处置指引不变：尝试重新 pull，或直接查看 `quarantine/` 目录。

**换码打错的预期形态（fail-closed 代价）**：pinned 401 **不区分**"已 revoke"与"码本身不对"（服务端 401 的 reason 字段 revoked/unknown 纯供 owner 日志排查，客户端判定不依赖）——换新码时打错/用过期码 = 现有 cache 被销毁 + 用正确码重新 pull 恢复；非攻击场景（服务端数据丢失/重建）同样触发。安全优先的取舍。

**失窃处置 = 两个 token 都 revoke**：cache token（本条命令）+ 该设备上的 project token（`projects revoke` / `rotate`）。销毁清单**不含** project token——`.claude.json` 是用户自己的 agent 配置，客户端程序不改写它；`cache-tokens revoke` 的输出会附一行 reminder 提示。

> ⚠️ **永离线的失窃机，唯一根治仍是轮换服务器凭据**：销毁要"回连"才兑现——永不离线的机器持有"密文 + DEK + 二进制"三件套，没有任何服务端机制能远程废掉其本地解密能力。**视敏感度轮换该机接触过的服务器凭据**（`servers edit <name> --password/--key`），必要时 `projects rotate` 换 project token。详见 [threat-model.md §3.6](./threat-model.md)。

**Lazy 生效，运行中会话不断**：已水合的 store 在内存继续服务至进程退出；隔离在 **spawn 边界**生效（与 revoke 懒语义一致）。

### 离线缓存到龄自废（SSHMGR_CACHE_MAX_OFFLINE）

笔记本侧设 `SSHMGR_CACHE_MAX_OFFLINE=168h`（Go duration 文法，**下限 1h**；unset/`0` = 关）即启用：
超龄缓存在**下次 load/spawn 边界**销毁（DEK/设备码删除、密文进 `quarantine/`），重新 `cache pull` 即恢复（设备码未被 revoke 就仍有效）。

> **持久化（v0.11 / Plan 40 起）**：该上限可持久化进实例目录的 `cache.config.json`（`cache pull --max-offline 24h`），优先级 env > config > 关——env 铺不满所有进程正是锚抹除 bug 的根因，config 是机器/实例属性。详见下「多实例（同机多 agent）」节的「MAX_OFFLINE 持久化」。

运维前提与语义边界：

- **首次启用需联网**：provenance 闸会拒绝一切非服务器锚的旧缓存（含本特性之前拉的）——开 B 后第一次使用前先 `cache pull` 建立服务器锚。
- **两端时钟需基本同步（NTP）**：pull 时 `|server Date − 本地钟| > 1h` 拒拉（skew 闸）；错钟**前跳**超过上限则触发销毁，恢复 = 联网 re-pull。
- **生产建议 ≥24h**：1h 是测试下限；server 钟落后接近 1h 时小上限的缓存可用期趋零（fail-closed 方向，宁可早废重拉）。
- **销毁只在下次运行本客户端时发生**：关机失窃的机器不会自动擦盘——盘上材料保留至下次运行（threat-model 残余清单）。

### 与 export/import 的关系

两套不同的工具，**别混**：

| | export / import（[Plan 11](./backup-restore.md)） | cache（Plan 12，本节） |
|---|---|---|
| 目的 | 便携**口令加密**备份 / 迁移 / 灾难恢复 | 工作机**只读缓存**，断网兜底 |
| 鉴权 | 你的**口令**（KeePass 式） | 设备授权码（owner 发、可吊销） |
| 落地的 vault 可写吗 | ✅ import 进一个**可写** vault | ❌ 只读（`ErrReadOnly`） |
| 怎么触发 | 手动 `export` / `import` | 设备码 + `mcp --cache` 内置自动拉取（spawn 惰性 + 会话内按 TTL 懒检查） |
| 格式 | `SSHMGRV1` 信封（Argon2id + AES-GCM）封 `Snapshot` JSON | 原始 key AES-GCM 封**同一份** `Snapshot` JSON |

两者**复用同一份 `store.Snapshot`**（Plan 11 打的地基）——序列化格式一致，加密信封不同（export 用口令派生 key，cache 用本机固定路径 `cache-dek.key` 裸文件的 DEK）。

### 限制（如实）

- **缓存只读**：离线能 exec / 传输 / 转发，但**任何写都被拒**（`ErrReadOnly`）。要加改删得连上 serve。
- **快照范围 = 绑定 profile 的授权集**（Plan 39）：client 机的 `cache.bin` 只含该设备绑定 profile 授权的服务器与凭据；owner 改动授权（增删 grant）后，**下次拉取生效**（TTL ≤30min 或手动 `[s]`）。一台机**一份缓存**绑定多个 profile 不支持——一个设备码一个 profile，要不同范围就发不同码：不同机器，或**同机多实例**（Plan 40，见下「多实例（同机多 agent）」节——每实例一个码一个 profile，目录/DEK/时效各自独立）。
- **授权边界是"服务器行"粒度，不是"凭据行"粒度**（如实）：若一台**未授权**服务器与已授权服务器**共用同一凭据**（如共享的 bastion/sudo 密码——`servers.go` 一等支持的概念），裁剪快照仍会携带该凭据（已授权服务器登录需要它），而它能登那台未授权服务器。要在凭据层面隔离授权，就别跨授权边界共享凭据——这是 owner 侧的建模决定，机制无法代劳。
- **bind 错配 footgun**：把设备码 bind 到一个**不含该机 project** 的 profile（`cache-tokens bind` 支持 rebind）→ pull 照常 200、在线照常，但离线栈搁浅：运行中的 `mcp --cache` 热加载验证 token 失败后**静默保留旧快照**，新 spawn 直接报 token 无效——错误不会指向错配本身。处置：把设备码 bind 回该机 project 所在的 profile，或在该 profile 下建 project 并换发 token。bind 前核对 `profiles ls`（授权集）与该机 `.mcp.json` 用的 project。
- **未绑码（Plan 39 前签发）拉取被拒 403**：本地缓存不毁，owner 跑 `cache-tokens bind` 补绑后即恢复。
- **自动保鲜是 `mcp --cache` 进程内置的**（spawn 惰性拉取 + 会话内按 TTL 懒检查 + 热加载）——不是常驻 daemon，也无需 OS 调度器。
- **运行中的 `mcp --cache` 会热加载新缓存**（hash 变化即换）——拉取成功后下一次工具调用即生效，无需重启 Claude Code。在线的 serve 是每请求实时鉴权，没有这个问题。
- **离线审计分散在各机本地**：`cache-audit.log` 不回传、不合并——要集中视图得自己收。服务器的 `audit_log`（命令历史）**不进快照**，永远只在 server 侧。
- **首次 `cache pull` 必须在线**——缓存还没拉下来之前，`mcp --cache` 跑不起来（会报 `cache DEK not found` / `no such file`）（凭据文件 `cache.auth.json` 由首次成功 pull 自动写入）。
- **永离线的物理失窃 = 远程吊销解决不了**：见上"吊销"——revoke 的销毁要**回连**才兑现（≤30min lazy cadence，默认 `--cache-max-age`；`0` 关闭自动拉取，销毁则只发生在手动 pull）；永不离线的失窃机上"密文 + DEK + 二进制"三件仍在手，唯一根治 = 轮换服务器凭据。

### 自动 TLS 迁移 Runbook（从旧版明文 / 外部证书升级）

新版 serve **默认强制 TLS**（无 `--tls-cert` 时自签）。已部署的明文或外部证书部署切到指纹钉死。⚠️ **顺序铁律：先升全部工作机并配 pin，最后才升 serve**（升 serve 瞬间其变 TLS-only，旧明文 client 直连会断）：

1. **先升级【所有工作机】二进制**到含自动 TLS 的新版（serve 暂不动）。
2. **拿指纹**：在 serve 机跑 `ssh-manager serve cert-info` —— 打印当前（或首次生成）的 SPKI 指纹 `sha256:...`。幂等。
3. **各工作机把指纹配上**（任一形式）：
   - 重新 `cache-tokens add`（默认把指纹打进设备码输出，形态 `<码>:<指纹>`）；或
   - 在调度器配置（systemd unit / 任务计划 / launchd plist）的 `Environment` / `EnvironmentVariables` 里加 `SSHMGR_SERVE_PIN=sha256:<指纹>`。
4. **【最后】重启 serve** → 从此强制 TLS，启动日志打印 `client pin: <指纹>`。
5. **各工作机手动 `cache pull` 带新 `--pin` 重拉一次**：迁移机**缺 cred（还没有 `cache.auth.json`）或仍持旧 pin** 的，都需要这一次手动重拉——自动拉取依赖已持久化的凭据（缺 cred 不会触发；持旧 pin 会因指纹失配而失败），替代不了这步收尾（也可走调度器：env 配好 `SSHMGR_SERVE_PIN` 后定时 `cache pull`）。成功后写入/更新 `cache.auth.json`（含新 pin）→ 走 TLS + 指纹钉死成功，自动拉取自此恢复正常。

> ⚠️ **新策略（默认安全）**：新 client **无 pin 默认 hard-fail**（拒连），不再静默明文回退。明文拉取需显式 `--allow-plaintext` opt-in（仅调试/连旧明文 serve 用）。所以迁移必须先把 pin 分发到所有工作机（第 3 步），不能依赖"自动回退"。

> ⚠️ **旧二进制 client（完全没有 pin 逻辑）对新版 serve（强制 TLS）**：旧 client 用明文 `http.DefaultClient` 打 TLS-only 的 serve，握手必败。**对策就是第 1 步：把每台工作机的二进制先升上来**。所以顺序铁律是 load-bearing 的 —— 别先升 serve。

### serve 证书密钥轮换 Runbook（私钥疑似泄露 / 迁移到新机）

serve 自签证书长生（不靠过期驱动轮换），但若私钥疑似泄露或迁移到新机，需重签 + 全量重新交接指纹：

1. 在 serve 机**同时删** cert + key + init-marker：
   `rm "<VaultDir>/serve-cert.pem" "<VaultDir>/serve-key.pem" "<VaultDir>/.serve-cert-initialized"`
   （⚠️ 必须三个一起删。只删 cert/key 而 marker 还在 → serve 拒启动，防误删静默重生。）
2. 重启 serve → 生成全新 ed25519 key + 新自签证书 + 新 marker。
3. `ssh-manager serve cert-info` → 拿**新** SPKI 指纹。
4. **全量重新 enroll** 所有工作机：重新 `cache-tokens add` 发带新指纹的设备码，或更新各机 `SSHMGR_SERVE_PIN=<新指纹>`。旧 pin 全部失配（看起来像 MITM，属预期）。
5. 各工作机手动 `cache pull` 带新 `--pin`（会同时更新 `cache.auth.json` 里的 pin）→ 走新指纹成功；之后的自动拉取恢复正常。

> 注：重签 = 所有客户端 pin 失效（硬失败，不是静默泄露）。这是指纹钉死的预期代价：信任根是公钥，换 key 必须重新交接。

---

## 多实例（同机多 agent Plan 40 第一批）

> **一句话**：同一台工作机上 N 个 agent（各持不同 project token / 设备码 / profile）各自拥有**独立的离线 cache 实例**——独立目录、独立 DEK、独立审计、独立 MAX_OFFLINE 时效，互不串扰、泄露不连坐。
>
> 实例 = 设备码 name = profile 授权单元（**三位一体**）：`cache-tokens add --name laptop-agentA` 发的名字就是实例名，该实例的拉取范围就是这个码绑定的 profile。命名纪律建议 `机器-实例`（如 `laptop-agentA`），与运维台账一一对应（不强制）。
>
> **空机器首次 enroll 自动归位（批2）**：真空机上裸 `cache pull` 连 `--instance` 都不用带——材料按响应头 name 直落 `instances/<name>/`。详见下「首次 enroll 自动归位」；收尾只差一步：手工 `.mcp.json` 里给 `mcp --cache` 补上 `--instance <name>`。

### 它解决什么

上面的单实例形态（Plan 12）一台工作机只有一份 cache——同机第二个 agent（不同 profile 的 project token）在 `mcp --cache` 的 token 验证处 spawn 即失败：fail-closed 不越权，但第二个 agent **没有离线能力**。Plan 40 把多实例做成一等公民：N 个 agent = N 个实例 = N 份独立 cache/DEK/设备码/审计/时效策略，谁的授权进谁的目录。

### 目录布局

```
<UserConfigDir>/ssh-manager/                    ← 默认实例（存量零变化）
├── cache.bin / cache.meta.json / cache.auth.json
├── cache-audit.log / cache.config.json / quarantine/
└── instances/<name>/                           ← 命名实例（每实例同构一套）
    ├── cache.bin / cache.meta.json / cache.auth.json
    ├── cache-audit.log / cache.config.json / quarantine/

<VaultDir>/                                     ← DEK 布局（保持与 cache 目录分离）
├── cache-dek.key                               ← 默认实例 DEK（现状）
└── cache-dek-<name>.key                        ← 命名实例 DEK（每实例一份）
```

- 实例目录：`UserConfigDir()/ssh-manager/instances/<name>/`（Windows `%AppData%\ssh-manager\instances\<name>\`）。
- **每实例独立 DEK**：`<VaultDir>/cache-dek-<name>.key`——A 实例的目录连同其 DEK 单独泄露，**解不开 B 实例的 cache.bin**（泄露不连坐）。`SSHMGR_CACHE_DEK_DIR` env 可整体重定位 DEK 目录（测试/迁移 seam，与 `--instance` 可共存）。
- **name 纪律**（owner 起名时即生效，双端校验）：`^[A-Za-z0-9]([A-Za-z0-9._-]{0,62}[A-Za-z0-9])?$`（1-64 字符，首尾必须字母数字）+ 首个 `.` 前的首段不得为 DOS 保留名（`CON`/`PRN`/`AUX`/`NUL`/`COM1-9`/`LPT1-9`，casefold 比对）+ **大小写变体终身唯一**（NTFS 目录名大小写不敏感，`agentA` 与 `AGENTA` 视为同名；revoke 后重发变体同样拒）。存量库若已有碰撞/非法 name，serve 升级后**拒绝启动**（见 [compat-matrix.md](./compat-matrix.md)）。

### enroll 双 agent 流程（例：agentA / agentB 同机）

**Step 1（服务器侧）**：发两个设备码，各绑各 profile——

```bash
ssh-manager cache-tokens add --name laptop-agentA --profile team-a
# Authorization code for "laptop-agentA" (shown once): <设备码A>
# On the work machine:
#   ssh-manager cache pull --url https://192.0.2.5:7878 --token '<设备码A>:<指纹>' --instance laptop-agentA

ssh-manager cache-tokens add --name laptop-agentB --profile team-b   # 同上，得 <设备码B>
```

**Step 2（工作机）**：两次拉取，各进各实例——

```bash
ssh-manager cache pull --url https://192.0.2.5:7878 --token '<设备码A>:<指纹>' --instance laptop-agentA
# → pulled N servers / M credentials into .../instances/laptop-agentA/cache.bin
ssh-manager cache pull --url https://192.0.2.5:7878 --token '<设备码B>:<指纹>' --instance laptop-agentB
```

**Step 3（工作机）**：两个 agent 各配各的 `.mcp.json` 条目（stdio 形态）——每条 = 上面 Plan 12 的离线 `mcp --cache` 形态，`args` 里多一个 `--instance <name>`：

```json
"ssh-agentA": {
  "command": "ssh-manager",
  "args": ["mcp", "--cache", "--instance", "laptop-agentA"],
  "env": { "SSHMGR_TOKEN": "<agentA 的项目token>" }
},
"ssh-agentB": {
  "command": "ssh-manager",
  "args": ["mcp", "--cache", "--instance", "laptop-agentB"],
  "env": { "SSHMGR_TOKEN": "<agentB 的项目token>" }
}
```

（两条放同一份 `.mcp.json` 的 `mcpServers` 对象下、键名不同即可；单 agent 机器就一条。命令行直接跑 = `ssh-manager mcp --cache --instance laptop-agentA`；token 也可 `--token` 传，`.mcp.json` 推荐 env 形态，理由同 [agent-access.md](./agent-access.md)——消除 argv/ps 暴露面。）

> **真空工作机的简化形态**（批2 起）：空机上第一次拉取的码**可以省掉 `--instance`**——裸 pull 自动归位进同名实例（见下节）；第二枚码再裸拉同样归到它自己的实例目录（归位后默认槽仍真空，逐码各自归位、互不干扰）。显式 `--instance` 永远可用，语义更直白。

### 首次 enroll 自动归位（批2）

**一句话**：满足全部条件的裸 `cache pull` / 向导首拉，把整套 cache 材料（bin/meta/audit/quarantine 解析 + auth 连动落槽）直接放进 `instances/<响应头 name>/`——"新机开箱即实例形态"，不再有"先落默认槽再手动搬"的中间窗口。

**触发条件（"真空 v4"，四条同时成立才归位）**：

| # | 条件 | 反例 |
|---|---|---|
| 1 | 无显式路由（不带 `--instance`；TUI 向导首拉同享此路径） | 显式路由已是命名实例 |
| 2 | pinned TLS 且 serve 下发设备码 name 响应头（`X-Sshmgr-Device-Name`，serve ≥v0.11.0）且 name 过白名单校验（非法 → **拒写盘**，owner 改名重发） | plaintext 与老 serve 无头 |
| 3 | 默认槽 `cache.bin`、`cache.auth.json`、`cache.meta.json`、`cache.config.json` **四个文件均不存在** | meta/config 任一在场 = 该槽"曾有材料/曾配置"的**意图标记** |
| 4 | `SSHMGR_CACHE_DIR` 与 `SSHMGR_CACHE_DEK` 均**未设置** | 任一在场 = 单槽完全覆盖语义 |

条件不全即走老路径写默认目录——**不归位七态**一览：

| 不归位态 | 行为 |
|---|---|
| 老 serve（响应头缺失） | 默认目录 + 升级 WARNING |
| plaintext（`--allow-plaintext`，无头） | 默认目录 |
| auth 在而 bin 无（半写态恢复期） | 默认目录 + 门禁补记 |
| `cache.meta.json` 在场 | 默认目录（存量机器零迁移的根基——只要成功 pull 过一次就有 meta） |
| `cache.config.json` 在场 | 默认目录（MAX_OFFLINE 策略就地生效） |
| `SSHMGR_CACHE_DIR` 在场 | 写 override 目录 |
| `SSHMGR_CACHE_DEK` 在场 | 默认目录、材料用 env DEK |

（`SSHMGR_CACHE_DEK_DIR` 只整体搬 DEK 根目录，不在七态之列——归位照常，实例 DEK 落 env 目录。）

补充语义：

- **幂等**：归位后再裸 pull 同一码 → 认出同身份实例、原槽覆写放行，无 flag 刷新不需要任何 flag。
- **门禁照常**：目标实例目录已持他人身份（exact 比对）→ 拒且**零写盘、零新增目录、零新增 DEK**；目标目录在而无 bin（auth-only/空目录）→ 放行（fresh-slot，面板新实例 enroll 的闭合通路）。
- **CLI 归位提示行**（pull 输出末尾认这一行就知道发生了什么）：

  ```
  first enroll located to instance laptop-agentA — mcp --cache needs --instance laptop-agentA in .mcp.json (bare cache pull re-locates idempotently; only the agent's cache-mode launch is affected)
  ```

- **CLI-first 收尾一步（必读）**：CLI 路径没有向导接入卡——**手工 `.mcp.json` 必须自己补 `"args": ["mcp", "--cache", "--instance", "<name>"]`**。提示行的两层含义：继续裸 `cache pull` 刷新不受影响（幂等再归位），真正受影响的只是 agent 的 cache-mode 启动那条链。

### enroll 双 agent 全程形态（批2 picker · Plan 42 后口径）

同机双 agent 的 TUI 少走命令形态（批2 的 `[i]` picker 保留；Plan 42 起 client 向导/连接表单已退役——入网一律 `ssh-manager pair`）：

1. **agentA**：`ssh-manager pair --instance laptop-agentA`（批准时选 profile-a）→ 首拉自动归位进 `instances/laptop-agentA/`；产物 `pair.laptop-agentA.mcp.json` 的 `args` 自动带 `"--instance", "laptop-agentA"` 及注释行（`本机 cache 位于实例槽 instances/laptop-agentA/——args 必须带 --instance laptop-agentA。`），照抄即可。
2. **agentB**：再跑一条 `ssh-manager pair --instance laptop-agentB`（批准时选 profile-b）→ 归位进自己的实例槽、产物各带各的 `--instance`。
3. `ssh-manager tui`（client 面板）`[i]` 打开实例 picker 可随时在两实例间切换查看——会话内有效，不跨进程记忆；`[s]` 同步只作用于当前选中槽。

### `--instance` 用法一览

| 命令 | 形态 |
|---|---|
| 拉取 | `ssh-manager cache pull --url ... --token ... [--pin ...] --instance <name>` |
| 状态 | `ssh-manager cache status --instance <name>`（单实例详情）；**无 flag = 列全部**（默认槽一行 + 每实例一行；单实例加载失败渲染为该行错误，不中断列表） |
| MCP | `ssh-manager mcp --cache --instance <name>`（`.mcp.json` stdio 形态见上） |
| 配置时效 | `ssh-manager cache config [--instance <name>] [--max-offline 24h]`（省略 `--max-offline` = 只读显示当前 cap 与来源；详见下「`cache config` 子命令」） |

- **env × flag 互斥**：`SSHMGR_CACHE_DIR` 或 `SSHMGR_CACHE_DEK` 显式设置**且**带 `--instance` → CLI 层报错（这两个 env 是单槽完全覆盖，混用会静默路由错实例 / 令多实例共享同一 DEK）。`SSHMGR_CACHE_DEK_DIR`（目录级 DEK seam）与 `--instance` **可共存**。
- **`mcp --cache` 无 flag 且默认目录无 cache 而 `instances/` 下有实例 → 报错列出实例清单**并指引 `--instance <name>`——读到哪个实例必须显式，不自动猜。`cache status` 不受限（列表命令，恒列全部）。
- **`--instance` 强一致校验**：serve 在 `/snapshot` 响应下发设备码 name（`X-Sshmgr-Device-Name` 头，pinned TLS 防篡改）；pull 时头 name ≠ flag name → **写盘前拒**（防 owner 发码张冠李戴——实例目录与授权错位）。**`--instance` 需要 serve ≥v0.11.0**：老 serve 无此头 → 拒 + 提示升级（"upgrade the serve, or drop --instance"）。
- **默认实例身份门禁**（无 flag 的 pull，写盘前生效）：默认目录已有 `cache.bin` 时，serve 下发的 name 与 meta 记录的 `device_name` 比对——**异码 → 拒**（三选一指引：这是第二台设备的码就改用 `--instance`；要换默认实例的码就走下方换码 runbook；owner 用 `cache-tokens ls` 核对发码）；**存量机器 meta 无 `device_name`**（字段随本设计新增）→ 放行 + 本次 pull 补记（零迁移零感知）；**meta 缺失/损坏但 bin 在 → 拒**（真异常态）。这关掉的是"异码静默覆盖"的**现状敞口**（该覆盖行为在旧版本即存在）。老 serve 拓扑（无头）下门禁跳过 + WARNING 提示升级。**残余（规格登记）**：降级拉取（明文 `--allow-plaintext`，或 Plan 40 前的老 serve 无 `X-Sshmgr-Device-Name` 头）不携带可信身份——门禁跳过、照常落盘，且本次写盘会把 `cache.meta.json` 的 `device_name` 重写为**空**，已登记身份即被抹除、跨码窗口重新打开，直到下一次 pinned pull 重新补记为止（前置条件是拿到本机 CLI 控制权，属同机威胁，见 [threat-model.md §1.1](./threat-model.md)）。

### 边界（如实·批2 更新）

- **TUI 多实例现状（批2 落地 · Plan 42 收窄）**：`[i]` 实例 picker 会话内切换、单槽 override env 互斥（禁用而非适配）保留；client 向导与连接表单随 ②a 退役删除（Plan 42 批1）——入网/换码 = `ssh-manager pair`（`--force` 承接换码清理语义）。无人值守的批量刷新仍推荐计划任务 wrapper：每实例一条任务 + 各自的 env 文件（设备码是 per-instance 的；TUI 面板 `[s]` 只管当前选中槽）。
- **自动归位只作用于真空机首次 enroll**：存量默认槽机器**永不自动迁移**（意图标记 meta/config 在场即不归位）——要进实例形态显式 `--instance` 重新 enroll，或按下方 runbook v2 清三件套后裸拉归位。
- **doctor 暂不感知命名实例**（批2 后维持）：只有命名实例的机器，doctor 的 client-cache 检查会报"cache 缺失"（roles 判定已修为 client；不静默但属误报）——doctor 感知命名实例跟随 Plan 38 体系解决。
- 存量单实例机器**零迁移**：无 flag 的 pull/mcp/status 行为与旧版一致（门禁对存量空 `device_name` 走补记分支）。

### 失窃响应（多实例口径）

- **吊销设备码**（`cache-tokens revoke laptop-agentA`）= 切断未来 pull + **销毁本机该实例材料**：该实例下次 pull（手动或自动保鲜 ≤30min）收到 pinned 401 → 四件销毁（DEK / `cache.auth.json` / `cache.bin`→隔离 / `cache.meta.json`）——**只毁这一个实例**，同机其他实例不受影响（销毁粒度 = 实例，见上「吊销」节的销毁语义）。
- **已可能外泄的凭据必须轮换**（server 端 re-credential，受影响 profile 的**全部**凭据）——吊销销毁的是"本机这份副本 + 未来的拉取权"，**不消除已发生的外泄**；永不离线的机器持有"密文 + DEK + 二进制"三件套，唯一根治仍是轮换服务器凭据（见上「吊销」节与 [threat-model.md §3.6](./threat-model.md)）。
- **吊销纪律（快速断 agent 的顺序）**：先吊 **device code**（该实例下次 pull 即销毁 cache，切断离线能力），再吊 **project token**（下次保鲜拉到的新快照已无该 project → 本地 spawn 闸拒绝；≤30min）。两个都吊 = 双保险——完整三路径见上「吊销」节。

### 默认实例换码 runbook（v2）

更换默认实例的设备码 = 清除默认目录 cache 材料**三件套**后重新 enroll：

```bash
# 在默认 cache 目录（<UserConfigDir>/ssh-manager/）删除三件：
#   cache.auth.json + cache.bin + quarantine/（整目录）
# ⚠️ cache.meta.json 与 cache.config.json 千万保留（见下）
ssh-manager cache pull --url https://192.0.2.5:7878 --token '<新码>:<指纹>'
```

- **meta/config 是默认槽的意图标记，删了重 enroll 会被归位走**：两者任一在场 = "这个槽有主"，重 enroll 按老路径写回默认目录；两个都删（或 `rm -rf` 整目录）= **机器重置语义**——下次裸 pull 触发[自动归位](#首次-enroll-自动归位批2)，材料落 `instances/<响应头name>/`，手工 `.mcp.json` 的 `--instance <name>` 也得跟着改。日常换码**不要**这么干；要彻底重置时这反而顺手。
- 身份门禁拒绝文案同口径（三选一里的选项 2 原文）：清三件套重 enroll——"KEEP cache.meta.json and cache.config.json — they mark this as the DEFAULT slot; deleting them re-routes the re-enroll into instances/"。
- **保留的 meta 还带着旧 `device_name` 是特性不是残留**：bin 已删后门禁对该槽不生效，下次成功 pull 时 meta 随写盘覆盖刷新——无害痕迹，不必手工清理。
- **config 保留 = MAX_OFFLINE 策略原地继承**（时效是目录/槽位属性，不随设备码变化）；想连策略一起换用 `cache config --max-offline`（见下节）。
- 清三件套的语义 = 按目录/槽位：旧身份的隔离材料（`quarantine/`）一并清除，不留。
- 命名实例换码 = revoke 旧码 + `cache-tokens add` 同名（或新名）新码 + 该实例重新 `cache pull --instance <name>`（`--instance` 门禁保证同目录同身份；要彻底重来删该实例目录再 enroll 亦可——注意此时裸拉也会归位回同名实例）。

### MAX_OFFLINE 持久化（cache.config.json）

MAX_OFFLINE（到龄自废上限，见上「离线缓存到龄自废」节）从**进程 env** 升级为可持久化的 **per-instance 配置文件**——env 是进程属性（"把 env 铺满所有进程"正是历史锚抹除 bug 的根因），config 是机器/实例属性：

```bash
ssh-manager cache pull --url ... --token ... --max-offline 24h
# pinned pull 成功后写入该实例目录的 cache.config.json：{"max_offline":"24h"}
```

- **优先级：env `SSHMGR_CACHE_MAX_OFFLINE` > `cache.config.json` > 关**（env 保留为应急/测试 override）；env 在场时跑 `pull --max-offline` → 输出 WARNING（config 在 env 清除前不生效，防止误以为持久化已生效）。
- 校验与 env 完全同构（Go duration 文法，≥1h，非法 fail-closed）；明文存储（是策略不是凭据）；原子写。**明文 pull 不持久化该 flag**（明文拉不出时间锚，带了上限反而载不动）——给 WARNING 不静默。
- 不传 `--max-offline` 的 pull **不动**现有 config。config 进**每个实例自己的目录**（默认实例在默认目录、命名实例在 `instances/<name>/`）——时效 per-instance。独立查看/写入见下「`cache config` 子命令」。

### `cache config` 子命令

```bash
ssh-manager cache config                                # 只读显示默认槽 cap + 来源
ssh-manager cache config --instance laptop-agentA --max-offline 24h   # 给命名实例持久化上限
ssh-manager cache config --max-offline 168h             # 给默认槽持久化上限
```

- **只读显示形态**：`instance: laptop-agentA (<目录>)` + `cap: 24h0m0s (source: file)`（Go duration 文法渲染）；来源三态 `env > file > off`，无上限渲染为 `cap: off (no offline limit)`。
- **仅对已存在实例可读可写**：目标实例目录不存在 → 报错含 enroll 指引（提示 `cache pull --instance <name>`），**不预配置、不预建目录**——config 永远落在真实材料旁边。
- **没有 `off` 开关**：撤销上限 = 手动删该实例目录下的 `cache.config.json`。⚠️ **默认槽的 config 别顺手删**——它和 `cache.meta.json` 一起构成默认槽意图标记，删了会改变重 enroll 的归位语义（见上[换码 runbook v2](#默认实例换码-runbookv2)）。
- 写入时 `SSHMGR_CACHE_MAX_OFFLINE` env 在场 → WARNING 提示"env 清除前持久化不生效"（既有语义）；`--instance` 与两个 override env 互斥；纯配置命令——不 pull、不触发归位、无 plaintext 语义。

### 过渡期纪律（直到双端都 ≥v0.11.0）

本批同船的 P0 锚修复（pinned pull 恒记服务器锚——无 env 的 pull 进程不再抹掉 `server_anchored`，TUI `[s]` 同步致 `mcp --cache` 瘫痪的 bug 根治）**部署完之前**（两端任一还是 v0.10.0 时）：

- 恢复只跑**带 env 的** pull 通道（计划任务 wrapper / 显式设了 `SSHMGR_CACHE_MAX_OFFLINE` 的 shell）；
- **禁用 TUI `[s]` 同步与裸 CLI pull**（无 env 的 pull 会再抹一次锚 → `mcp --cache` 的 provenance 闸拒载 → 该机 agent 的 SSH 工具全消失）。

双端都升到 ≥v0.11.0 后此纪律作废（任何 pinned pull 都记锚）。

---

## 相关文档

- [deployment-modes.md](./deployment-modes.md)——部署形态全景（选型总览：① 单机 / ② 多机桥姿态 + 管理面）。
- [quickstart-multi-machine.md](./quickstart-multi-machine.md)——多机速通（pair 一条龙版）。
- [getting-started.md](./getting-started.md)——单机 stdio 从零到跑通（**默认模式**，第一次用先看这篇）。
- [agent-access.md](./agent-access.md)——project token 生命周期；断连语义（stdio spawn 边界 / 离线缓存保鲜 / 到龄自废，见「断连语义」一节）。token 管理在同一台服务器上做。
- [managing-servers.md](./managing-servers.md)——服务器增删改查（在 serve 那台**服务器**上操作）。
- [broker-host-agent.md](./broker-host-agent.md)——broker 主机上自己跑 agent 的姿势（零距离 client + 应急附录）。
- [scenarios.md](./scenarios.md)——应用场景示例（GPU 巡检、部署、端口转发……，两种模式都适用）。
- 仓库根 [README 的 "Multi-machine"](../README.md#multi-machine-bridge-posture-on-a-vlan) 节（英文概览）。
- [compat-matrix.md](./compat-matrix.md)——client↔serve 版本兼容矩阵（升级任何一端之前先看；含 Plan 42 三步迁移）。
