# 在 broker 主机上使用 AI agent

这篇讲：serve 机（broker 主机，比如 VLAN 里那台常驻 `ssh-manager serve` 的服务器）**自己也想跑 agent**（Claude Code / Cursor / 任何 MCP 客户端）时，agent 怎么接入 SSH MCP——以及与工作机唯一的差异点、应急姿势。

> 前置阅读：[multi-machine.md](./multi-machine.md)（多机桥姿态架构 + pair 一条龙）+ [agent-access.md](./agent-access.md)（project token / profile 授权模型）。

---

## 一句话定位

broker 主机上的 agent 就是**一台零距离的 client**——和任何工作机走同一套授权（设备码拉快照 + project token 本地 spawn 闸），没有任何"本机特权"。Plan 42 批1 起 ②a（HTTP 直连本机 serve）已**移除**，所以它只有一条正经姿势：**走桥**——像笔记本一样 pair（或手工）入网，用本地只读缓存干活。

> 曾经的「姿势 A：HTTP 直连本机 serve」（`.mcp.json` 用 `"type": "http"` + Bearer）随 ②a 一并退役：serve 根路径 404，`NODE_EXTRA_CA_CERTS` / 证书 SAN 两个 TLS 坑也随之消失——自家客户端纯指纹钉死，零信任配置。

| 姿势 | 接入方式 | 定位 |
|---|---|---|
| [走桥：pair 入网](#走桥pair-入网默认) | `ssh-manager pair` → 本地只读缓存 → `mcp --cache` | ✅ 默认，与笔记本同构 |
| [走桥：手工路径](#走桥手工路径迁移--ci) | `cache-tokens add` + `cache pull` + 手写 `.mcp.json` | ✅ 迁移/CI 场景 |
| [应急附录](#应急附录不推荐stdio-直开本地-vault) | `mcp` stdio 直开本地 vault | ⚠️ 不推荐，serve 彻底没起时 |

无论哪种姿势，**给这台机上的 agent 单独发一个 project（pair 会自动建 `pair-<名>`）**，不要复用某台工作机的 token——独立吊销域，一台泄露 `revoke` 不殃及另一台。

---

## 走桥：pair 入网（默认）

全流程与 [multi-machine.md 的 pair 节](./multi-machine.md#配对入网ssh-manager-pairplan-42)一模一样，只有两个零距离差异：

1. **连接地址用本机 hostname 或 VLAN IP**（不是 `127.0.0.1`/`localhost`）——pair 的机械地址校验核对的是 `target_url` 是否属于本机**非环回**地址集合 + hostname（环回不在集合内，用 `https://127.0.0.1:7878` 配对会吃 ⚠ 并要求显式覆盖）。LAN 发现路径天然给你的是 VLAN IP，直接用即可。
2. **批准就在本机**：broker 机自己开着 TUI（Pairing 页）或随手 `serve pair ls / approve`——对照 client 屏 SAS 与批准行 name@url 一致后批准，不用切机器。

```bash
ssh-manager pair --instance server-local          # 发现会找到自己（VLAN IP）
# 或显式：ssh-manager pair --instance server-local --url https://<本机VLAN IP>:7878
# → 批准（本机 TUI / serve pair）→ 首拉 → 产物 pair.server-local.mcp.json
```

`.mcp.json`（cache 形态，与工作机完全同构）：

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

### 走桥的语义

- **TLS 零配置**：自家客户端纯 SPKI 指纹钉死（pair 自动交付 pin）——`127.0.0.1` 都不需要，更不需要信任库/证书分发。
- **只读快照**：新 grant / 新加的 server，要等缓存保鲜（默认 ≤30min 自动，或手动 `cache pull`）才对 agent 可见。**写操作**（加改删服务器、发码、批准配对）走本机的 TUI / owner CLI——这正是管理面所在。
- **吊销是两道闸**：`cache-tokens revoke` → 该机下次拉取收到 pinned 401，本地 cache 四件销毁（回连即生效）；project token 吊销 → 下次保鲜的新快照已无该 project，spawn 拒。**断干净要两样都处理**（吊销三路径见 [agent-tools.md](./agent-tools.md)）。
- **后台任务表在 agent 子进程内存**：会话 / MCP 子进程重启即全死（agent 侧把在跑的活当全死重新安排）；serve 重启无影响。
- broker 机上多一份 cache 副本（`cache.bin` + DEK）——同机威胁模型下无增量（vault 的 master key 文件本来就在这台机）。

---

## 走桥：手工路径（迁移 / CI）

与工作机的手工路径同构（见 [multi-machine.md 手工 enroll](./multi-machine.md#手工-enroll存量迁移官方路径--ci-场景)），零距离差异同上——URL 用本机 VLAN IP：

```bash
ssh-manager cache-tokens add --name server-local --profile <profile名>
# Authorization code (shown once): <设备码>
# Server fingerprint (serve cert SPKI): sha256:...

ssh-manager cache pull --url https://<本机VLAN IP>:7878 --token '<设备码>:sha256:...'
# → pulled N servers / M credentials into cache.bin
```

`.mcp.json` 同上节 cache 形态。

---

## `upload_file` / `forward_port` 的空间语义

broker 机上全**自洽**——不像远程工作机那样有"文件在笔记本、broker 在服务器"的错位：agent、文件、监听端口全在同一台机。

- `upload_file` 的 `LocalPath` 读**本机**文件（cache 模式的本地 broker 就跑在本机）；`forward_port` 监听开在**本机环回**（多机 client 恒环回——②a 的 serve 侧监听随移除消失）。

---

## 应急附录（不推荐）：stdio 直开本地 vault

`mcp` 不带 `--cache` 即单机模式——直接以 read-write 打开本地 vault。broker 机上 vault 和 unlock 过的 master key 文件都在，技术上能跑，但：

- **与常驻 serve 并发打开同一个 vault**，破坏多机架构"serve 是唯一写者"的纪律（stdio broker 会写 audit 进 vault DB；SQLite 锁会串行化，功能上大概率没事，架构上是负债）。
- 相比走桥没有任何额外能力（agent 拿到的都是同一套 broker 工具）。

留作 serve 彻底没起时的应急即可。

---

## 排错速查

| 现象 | 处理 |
|---|---|
| pair 吃 ⚠「配对声明目标 ≠ 本机地址」 | 你用了 `127.0.0.1`/`localhost` 作连接地址——机械校验只认**非环回**地址集 + hostname。换本机 hostname 或 VLAN IP 重新 pair（确属故意中继场景才用 `--allow-foreign-url` 覆盖）。 |
| pair / `cache pull` 连不上 | serve 没起 / 防火墙挡 TCP+UDP 7878。`ssh-manager serve status` 查四项信号。 |
| 报 401 | 设备码/项目 token 错 / 已 rotate / project 被 disable/revoke。`projects ls` / `cache-tokens ls` 核对。 |
| agent 看不到新加的 server | 缓存还没保鲜——手动 `cache pull`，或等 ≤30min 自动。 |
| serve 重启窗口内 | 走桥的 agent **照常工作**（本地缓存，不依赖 serve 存活）；只有保鲜/新设备入网受影响。 |

---

下一步：[agent-tools.md](./agent-tools.md)（贴给 agent 的规则模板）· [multi-machine.md](./multi-machine.md)（多机桥姿态全量细节）· [scenarios.md](./scenarios.md)（真实任务示例）。
