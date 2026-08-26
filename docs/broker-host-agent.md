# 在 broker 主机上使用 AI agent

这篇讲：serve 机（broker 主机，比如 VLAN 里那台常驻 `ssh-manager serve` 的服务器）**自己也想跑 agent**（Claude Code / Cursor / 任何 MCP 客户端）时，agent 怎么接入 SSH MCP——三种姿势、各自的 TLS 与吊销语义、以及怎么选。

> 前置阅读：[multi-machine.md](./multi-machine.md)（serve 模式架构）+ [agent-access.md](./agent-access.md)（project token / profile 授权模型）。

---

## 一句话定位

broker 主机上的 agent 就是**一台零距离的 client**——和任何工作机走同一套授权（project token + profile scoping），没有任何"本机特权"。唯一区别是它离 serve 只有 127.0.0.1 一跳。

| 姿势 | 接入方式 | 定位 |
|---|---|---|
| [A](#姿势-ahttp-直连本机-serve) | agent 原生 HTTP 直连本机 serve（在线模式） | ✅ 语义最正统 |
| [B](#姿势-bmcp--cachebroker-机自己也当离线-client) | `mcp --cache`（离线缓存模式，和笔记本一样） | ✅ 最省心 |
| [C](#姿势-c不推荐stdio-直开本地-vault) | `mcp` stdio 直开本地 vault（单机模式） | ⚠️ 不推荐，应急用 |

无论哪种姿势，**给这台机上的 agent 单独发一个 project**，不要复用某台工作机的 token——独立吊销域，一台泄露 `revoke` 不殃及另一台：

```bash
ssh-manager projects add <如 server-local-agent> --profile <profile名>
# Token (shown once): eyJ... —— 立刻存密码管理器
```

---

## 姿势 A：HTTP 直连本机 serve

与工作机的在线姿势完全同构（见 [multi-machine.md](./multi-machine.md) 「Step 3(每台工作机):Claude Code 连远程」一节），只是 URL 指向本机。

### 1. 配 agent

`.mcp.json`（http 型）：

```json
{
  "mcpServers": {
    "ssh": {
      "type": "http",
      "url": "https://<broker主机名或VLAN IP>:7878/",
      "headers": { "Authorization": "Bearer <项目token>" }
    }
  }
}
```

### 2. TLS：两个必须处理的点

serve 的自签证书 + 指纹钉死（`SSHMGR_SERVE_PIN`）是给**自家** `cache pull` 客户端设计的；第三方 agent 的原生 HTTP 客户端（如 Claude Code 的 Node TLS）**不认识 pin**，自签证书默认会被拒。需要一次性配置：

**① 让 agent 的 HTTP 客户端信任这张证书。**

- 对 Node 系客户端（Claude Code）：设 `NODE_EXTRA_CA_CERTS=<serve证书路径>`。
  ⚠️ 它必须设在 **Claude Code 自己的进程环境**里（shell profile / 登录会话 / 服务单元）——`.mcp.json` 的 `env` 字段只注入 stdio 子进程，对 http 型连接**无效**（http 连接由 agent 主进程发起）。
  证书路径用 `ssh-manager serve cert-info` 查。
- 对走系统 TLS 栈的客户端（curl 等）：把证书装进系统信任库（Debian/Ubuntu：`/usr/local/share/ca-certificates/` + `update-ca-certificates`；RHEL/Fedora：`/etc/pki/ca-trust/source/anchors/` + `update-ca-trust`）。注意 Node 默认**不**读系统信任库——Node 系客户端仍需 `NODE_EXTRA_CA_CERTS`。

**② URL 主机名必须匹配证书 SAN。**

自签证书的 SAN = 生成时的 `os.Hostname()` + 全部**非环回**本机 IP——**不含 `127.0.0.1` / `localhost`**。连 `https://127.0.0.1:7878` 会过不了主机名校验；要用 broker 主机名（`hostname` 的输出）或它的 VLAN IP。

如果证书生成后主机名改过、对不上：需要重生成证书——删掉证书和 init marker **两个文件**再启动 serve（路径见 `serve cert-info` / serve 启动报错提示），代价是新指纹、所有 pin 的 client 重新交接（见 [multi-machine.md 密钥轮换](./multi-machine.md)）。也可以改用 `--tls-cert` 传自己的证书（SAN 自己控制）。

### 3. 姿势 A 的语义

- **每请求实时鉴权**：`projects revoke` / `disable` 后，该 project 的下一个请求立即 401（无需重启任何东西）。
- **隧道与后台任务归属 serve 进程**：`forward_port` 的监听开在 serve 主机（= 本机，agent 直接 `curl 127.0.0.1:<port>` 可达）；`tunnels ls` / `tunnels kill` 全局可管；`exec_background` 任务表在 serve 进程内（serve 重启才丢）。
- **依赖 serve 活着**：serve 重启 / 升级窗口内工具不可用，重启后自动恢复（agent 侧无需动作）。

---

## 姿势 B：`mcp --cache`（broker 机自己也当离线 client）

和笔记本的离线姿势一模一样——本地拉一份只读缓存，agent 的 stdio 子进程用它干活。**命令从本机直拨目标服务器，不在 serve 路径上**——所以 serve 挂了 / 重启中，agent 照常工作。

### 1. 给自己发设备码 + 拉缓存

```bash
ssh-manager cache-tokens add --name <如 server-local> --profile <profile名>
# Authorization code (shown once): <设备码>
# Server fingerprint (serve cert SPKI): sha256:...

ssh-manager cache pull --url https://127.0.0.1:7878 --token '<设备码>:sha256:...'
# → pulled N servers / M credentials into cache.bin
```

自家客户端的 TLS 校验是**纯 SPKI 指纹钉死**（只比对 pin，不校验主机名）——所以 `127.0.0.1` 随便用，**不需要任何信任配置**，这是姿势 B 比 A 省事的核心。

### 2. 配 agent

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

（Windows 写绝对路径；token 走 `env` 不走 argv——理由同 [agent-access.md](./agent-access.md)。）

### 3. 姿势 B 的语义

- **只读快照**：你新 grant / 新加的 server，要等缓存保鲜（默认 ≤30min 自动，或手动 `cache pull`）才对 agent 可见。
- **吊销是两道闸**：`cache-tokens revoke` → 该机下次拉取收到 pinned 401，本地 cache 四件销毁（回连即生效）；`projects revoke` → 下次 spawn 拒（stdio 是 lazy 生效）。**断干净要两样都 revoke**——和失窃工作机的处置口径一致（见 [agent-access.md 第 4 层语义](./agent-access.md)）。
- **后台任务表在 agent 子进程内存**：会话 / MCP 子进程重启即全死（agent 侧把在跑的活当全死重新安排）；serve 重启无影响。
- broker 机上多一份 cache 副本（`cache.bin` + DEK）——同机威胁模型下无增量（vault 的 master key 文件本来就在这台机）。

---

## 怎么选

| | A：HTTP 直连 | B：cache 模式 |
|---|---|---|
| serve 挂了 / 重启 | 工具断，恢复即回来 | **照常用** |
| TLS 配置 | 要（信任 + 主机名匹配 SAN） | **零** |
| `revoke` 生效 | 下一请求即拒 | 回连拉取时 / 下次 spawn |
| 新 grant 的 server | **立即可见** | ≤30min 保鲜延迟 |
| 后台任务 / 隧道归属 | serve 进程（owner 可 kill，serve 重启才丢） | agent 子进程内存（会话重启即丢） |

- 图省事、要"serve 重启不断供" → **B**。
- 要实时吊销语义、owner 统一可管的隧道 / 后台任务 → **A**。
- 两种都配也行（改 `.mcp.json` 切换 + 重启 agent 客户端）——vault 内容、project token、profile scoping 完全一致，见 [multi-machine.md 两种接入的切换](./multi-machine.md)。

---

## 姿势 C（不推荐）：stdio 直开本地 vault

`mcp` 不带 `--cache` 即单机模式——直接以 read-write 打开本地 vault。broker 机上 vault 和 unlock 过的 master key 文件都在，技术上能跑，但：

- **与常驻 serve 并发打开同一个 vault**，破坏多机架构"serve 是唯一写者"的纪律（stdio broker 会写 audit 进 vault DB；SQLite 锁会串行化，功能上大概率没事，架构上是负债）。
- 相比 A / B 没有任何额外能力（agent 拿到的都是同一套 broker 工具）。

留作 serve 彻底没起时的应急即可。

---

## `upload_file` / `forward_port` 的空间语义

broker 机上用，姿势 A / B 都**自洽**——不像远程工作机那样有"文件在笔记本、broker 在服务器"的错位：

- 姿势 A：`upload_file` 的 `LocalPath` 读 **serve 主机**的文件（= agent 所在机）；`forward_port` 监听开在 serve 主机（= 本机）。
- 姿势 B：`LocalPath` / 监听都是 **agent 子进程所在机**（= 本机）。

agent、文件、监听端口全在同一台机，无需跨机搬运。

---

## 排错速查

| 现象 | 处理 |
|---|---|
| 姿势 A 连不上，报证书 / TLS 错误 | ① 没设 `NODE_EXTRA_CA_CERTS`（且设错位置——要在 agent 主进程环境，不是 `.mcp.json` 的 `env`）；② URL 用了 `127.0.0.1`/`localhost`（不在证书 SAN 里）——换主机名或 VLAN IP。 |
| 姿势 A 报 401 | token 错 / 已 rotate / project 被 disable / revoke。`projects ls` 核对。 |
| 姿势 B spawn 报 `cache DEK not found` | 还没拉过缓存——先在线跑一次 `cache pull`（见上）。 |
| 姿势 B 里 agent 看不到新加的 server | 缓存还没保鲜——手动 `cache pull`，或等 ≤30min 自动。 |
| serve 重启后姿势 A 短暂全红 | 预期行为——serve 活过来即恢复；等不了就切姿势 B。 |

---

下一步：[agent-tools.md](./agent-tools.md)（贴给 agent 的规则模板）· [multi-machine.md](./multi-machine.md)（serve 模式全量细节）· [scenarios.md](./scenarios.md)（真实任务示例）。
