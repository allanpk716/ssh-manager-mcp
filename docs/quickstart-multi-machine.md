# 多机快速上手（Quickstart · pair 一条龙版）

> **场景**：你在**多台机器**（笔记本 / 台式机 / 家用服务器）上工作，想让所有机器上的 AI agent 共用**同一份**服务器清单 —— 但凭据只存在一台权威 broker 上，工作机只持本地只读缓存。
>
> 全文只讲「最少要做什么」。架构 / 配对协议细节 / 多实例 / runbook 看详尽版 [`multi-machine.md`](./multi-machine.md)。
>
> **Plan 42 批1 起**：新机入网 = `ssh-manager pair` 一条命令（不再跨机手抄设备码/token/指纹三串字符串）；agent 一律走本地只读缓存（多机 agent 只读 + 执行，写操作在管理面）。

---

## 架构一句话

一台 **VLAN 服务器**跑 `ssh-manager serve`（常驻，持有权威 vault；只做三件事：**权威 vault + `/snapshot` 拉取 + `/pair` 配对**，批2 再加 `/ui` 管理）；各工作机 `ssh-manager pair` 一条龙入网，之后 agent 用**本地只读缓存**干活。**命令实际从工作机直拨目标服务器**，broker 不在命令路径上。

```
 ┌──工作机（笔记本/台式机）──┐  pair 入网+保鲜拉取 ┌──VLAN 服务器──┐
 │  Claude Code            │ ──TLS+指纹钉死──▶ │  ssh-manager   │
 │  （agent）              │                  │  serve         │ ← 权威 vault
 │  本地只读缓存 cache.bin │ ◀── 凭据自动下发 ──│  （常驻）       │   + /snapshot
 └──────────┬────────────┘   （SAS 人闸后）     └────────────────┘   + /pair
            │ agent 跑命令时直拨
            ▼
        目标服务器们
```

---

## 关键：自动加密（零证书配置）

`serve` **首次启动自动生成一张自签证书**，从此强制 TLS —— 你**不用碰 openssl、不用分发 CA 证书、不用配信任库**。工作机用证书的 **SPKI 指纹**钉死对端（首次连接即校验，防 MITM）；pair 时代指纹**自动交付**（discovery offer 自带、pair 信封内封入），没有任何要手抄的证书材料。

---

## Step 1 — 服务器侧：建清单 + 启动 serve

在将常驻 broker 的那台 VLAN 服务器上，像单机一样把服务器和 profile 建好（命令同 [`quickstart-single-machine.md`](./quickstart-single-machine.md) Step 2-4；**project 不用预建**——pair 批准时自动建 `pair-<设备名>`），然后：

```bash
ssh-manager unlock                       # 一次性（写 master.key.plain）

# 录入目标服务器（凭据只在这台机上）+ 分组授权：
ssh-manager servers add --name gpu --host 192.0.2.10 --user deploy --password '...'
ssh-manager profiles add team-a && ssh-manager profiles grant team-a gpu

# 启动常驻 broker（自动自签证书 + 强制 TLS + UDP 发现 + 配对面，全默认开）：
ssh-manager serve --addr 0.0.0.0:7878
# → listening on 0.0.0.0:7878 (tls=auto)
# → auto-TLS cert (self-signed). client pin: sha256:abcd1234...
# → ssh-manager serve: discovery: udp/7878 (on)
```

想让它开机自启：`ssh-manager serve install`（kardianos 注册系统服务，三平台一条龙）。

---

## Step 2 — 工作机：`ssh-manager pair` 一条龙

每台工作机装好 `ssh-manager` 后（完整流程细节见 [`multi-machine.md`](./multi-machine.md#配对入网ssh-manager-pairplan-42)）：

```bash
ssh-manager pair --instance laptop
# → 同网段自动发现 broker（拿不到 offer 就 --url https://192.0.2.5:7878 直指，
#   跨网段/防火墙挡 UDP 时的兜底；--url 不带 --pin 默认拒连，属预期——见下方安全策略）
# → 屏显三件套：laptop @ https://192.0.2.5:7878 SAS 482913
# → 去 broker 机批准：TUI Pairing 页选 profile（或 serve pair approve laptop --profile team-a）
#   批准行 = name@url 两件 + 「SAS 码见 client 屏幕」——对照本机屏上 SAS 与 name@url 一致再批
# → 批准后 120 秒内自动完成：凭据加密下发 → 首拉落盘 → 产物 pair.laptop.mcp.json
```

收尾一步——把产物里的片段抄进该机 `.mcp.json`（或 pair 时加 `--write-mcp <path>` 直接落位）：

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

> 产物文件 `pair.laptop.mcp.json` 在实例目录里、含真值 token（0600）——终端只显示 `<project-token>` 占位符；首拉即使失败凭据也已在盘，重跑 `cache pull --instance laptop` 即补上缓存。

重启 Claude Code → agent 用本地缓存跑命令（只读 + 已授权的 exec/传输/转发；断网照常，保鲜 ≤30min 需在线）。要加改删服务器（写操作），去 broker 机的 TUI——多机 agent 没有写权限是设计，不是缺陷。

> 把 [agent-tools.md](./agent-tools.md) 的规则模板贴进 CLAUDE.md，agent 会更守规矩（含多机只读铁律与吊销三路径）

---

## Step 3 —（可选）缓存自动保鲜说明

缓存自己保鲜，**默认无需任何系统定时器**：

- **spawn 自动拉**：Claude Code 启动 `mcp --cache` 时，若缓存超过 30 分钟（`--cache-max-age` 可调，`0` 关闭）且本机存过拉取凭据，会自动拉一次新缓存；失败静默用旧缓存。
- **会话内懒检查 + 热加载**：运行中的会话在**每次工具调用前**懒检查缓存是否超过 TTL（默认 30 分钟），过期才自动拉新，下一次工具调用即生效——无需重启 Claude Code。
- pair 首拉已自动持久化拉取凭据（`cache.auth.json`）与离线上限（`cache.config.json`，默认 24h——**永离线设备的缓存到期自毁**，这是吊销第三路径），之后的自动拉取全靠它们。

---

## 安全策略（重要）

- **指纹钉死**：工作机连 serve 时校验证书公钥 == 钉死的指纹，不等即拒。pair 场景指纹自动交付（**首次连接即校验，零 MITM 窗口**）。
- **无锚默认拒连**：`pair --url` 直连又不带 `--pin` → **默认拒绝**（显式 `--allow-tofu` 才接受无锚通道，仅限受控环境——它没有完整 MITM 防护，见 [threat-model.md](./threat-model.md) R12）；`cache pull` 没拿到指纹同样 **hard-fail**。防的是打错别字静默降级。
- **SAS 人闸 + 机械地址校验**：批准前 owner 对照 client 屏 SAS 与批准行 name@url；serve 同时机械核对 client 声明的连接地址是否本机地址（不符 → ⚠ + 强制显式覆盖）——假 discovery / 中继/研磨型 MITM 过不了这两道。
- 详见 [`threat-model.md`](./threat-model.md) 的 pairing 节。

---

## 附录：手工 enroll（存量迁移官方路径 + CI 场景）

> **何时用**：① 存量 ②a 机器在**旧 serve** 上的迁移（旧版没有 `/pair`）；② CI / 无人值守自动化。日常新机一律走上面的 pair。完整版见 [`multi-machine.md` 手工 enroll](./multi-machine.md#手工-enroll存量迁移官方路径--ci-场景)。

```bash
# 服务器侧：发一张绑定 profile 的设备授权码（TUI 设备码页签 [a] 亦可）
ssh-manager cache-tokens add --name laptop --profile team-a
# Authorization code for "laptop" (shown once): <设备码>
# Server fingerprint (serve cert SPKI): sha256:abcd1234...

# 服务器侧：再发 project token（pair 会自动做这一步，手工路径要自己来）
ssh-manager projects add laptop --profile team-a   # Token (shown once): <项目token>

# 工作机：第一次拉缓存（设备码 + 指纹一起给；之后 mcp --cache 自动保鲜）
ssh-manager cache pull --url https://192.0.2.5:7878 --token '<设备码>:sha256:abcd1234...'
# → pulled N servers / M credentials into cache.bin
```

`.mcp.json` 同 Step 2 形态（`mcp --cache` + env token）。多实例机器给每条加 `"--instance", "<名>"`（或裸拉自动归位后按提示补）。

---

## 存量部署升级（破坏性变更，必读）

Plan 42 批1（随下个发版）起 serve **移除远程 MCP 面**——旧 ②a `.mcp.json`（`"type": "http"`）打过去是 **404**。升级已部署的多机环境按**三步迁移**：

> ⚠️ **顺序铁律：先升全部工作机（client ≥ v0.10.1）并迁到桥姿态，最后才升 serve。**

1. **手工桥迁移**：在**旧** serve 上按上面附录的手工流程给每台工作机发码 + 拉缓存 + 配 `.mcp.json`（此时工作机二进制先升到 ≥ v0.10.1）。
2. **升 serve**（含 ②a 移除的 Plan 42 版本，版本号发版拍板）——前置检查：全部 client 已在桥姿态；当刻起 ②a 路径 404。
3. **pair 时代**：此后所有新机/重配对一律 `ssh-manager pair`。

完整迁移 runbook + 密钥轮换 runbook → [`multi-machine.md`](./multi-machine.md) / [`compat-matrix.md`](./compat-matrix.md)。

---

## 接下来

- 架构 / pair 协议细节 / 多实例 / runbook 全部细节 → [`multi-machine.md`](./multi-machine.md)
- cache-tokens 生命周期 / 吊销三路径 → [`multi-machine.md`](./multi-machine.md#吊销机器失窃--设备码泄露)
- 备份 / 迁移整个 vault → [`backup-restore.md`](./backup-restore.md)
- **单机用法** → [`quickstart-single-machine.md`](./quickstart-single-machine.md)
