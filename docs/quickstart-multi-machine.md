# 多机快速上手（Quickstart）

> **场景**：你在**多台机器**（笔记本 / 台式机 / 家用服务器）上工作，想让所有机器上的 AI agent 共用**同一份**服务器清单 —— 但凭据只存在一台权威 broker 上，工作机零凭据。
>
> 全文只讲「最少要做什么」。架构 / 离线缓存 / 备份 / 全部细节看详尽版 [`multi-machine.md`](./multi-machine.md)。

---

## 架构一句话

一台 **VLAN 服务器**跑 `ssh-manager serve`（常驻，持有 vault + 凭据，**唯一写者**）；各工作机的 agent 连它（在线可写）或连本地只读缓存（离线兜底）。**命令实际从工作机直拨目标服务器**，broker 不在命令路径上。

```
 ┌──工作机（笔记本/台式机）──┐         ┌──VLAN 服务器──┐
 │  Claude Code            │  HTTPS  │  ssh-manager   │
 │  （agent）              │──指纹──▶│  serve         │ ← 唯一 vault + 凭据
 │  本地只读缓存 cache.bin │  钉死   │  （常驻）       │
 └──────────┬──────────────┘         └────────────────┘
            │ agent 跑命令时直拨
            ▼
        目标服务器们
```

---

## 关键：自动加密（零证书配置）

`serve` **首次启动自动生成一张自签证书**，从此强制 TLS —— 你**不用碰 openssl、不用分发 CA 证书**。工作机用证书的**指纹**钉死对端（首次连接即校验，防 MITM）。指纹随设备码一起交给工作机。

> 这是「路线乙」：agent 平时连本地只读缓存（`mcp --cache`，零跨网络）；只有同步时（`cache pull`）才跨网络碰 serve。所以加密需求塌缩成只剩同步那一跳。

---

## Step 1 — 服务器侧：建清单 + 启动 serve

在将常驻 broker 的那台 VLAN 服务器上，像单机一样把服务器/profile/project 建好（命令同 [`quickstart-single-machine.md`](./quickstart-single-machine.md) Step 2-4），然后：

```bash
ssh-manager unlock                       # 一次性（写 master.key.plain）

# 录入目标服务器（凭据只在这台机上）
ssh-manager servers add --name gpu --host 192.0.2.10 --user deploy --password '...'
ssh-manager profiles add team-a && ssh-manager profiles grant team-a gpu

# 发一张设备授权码给每台工作机（用于拉只读缓存）：
ssh-manager cache-tokens add --name laptop
# Authorization code for "laptop" (shown once): <设备码>
# Server fingerprint (serve cert SPKI): sha256:abcd1234...
# On the work machine:
#   ssh-manager cache pull --url https://<serve>:7878 --token '<设备码>:sha256:abcd1234...'

# 启动常驻 broker（自动自签证书 + 强制 TLS，无需 --tls-cert）：
ssh-manager serve --addr 0.0.0.0:7878
# → listening on 0.0.0.0:7878 (tls=auto)
# → auto-TLS cert (self-signed). client pin: sha256:abcd1234...
```

想让它开机自启：`ssh-manager serve install`（kardianos 注册系统服务，三平台一条龙）。

> 想用自己的证书也行：`serve --tls-cert cert.pem --tls-key key.pem`（向后兼容）。但默认的自签 + 指纹钉死对单 owner 场景已经足够，且零分发。

---

## Step 2 — 工作机：拉缓存 + 配 agent

每台工作机装好 `ssh-manager` 后（连接配置/手动同步也可开可视化面板 `ssh-manager tui --mode client`；概念图解见 [concepts.md](./concepts.md)）：

```bash
# 第一次拉缓存（之后 mcp --cache 会自动保鲜，见 Step 3）
ssh-manager cache pull --url https://192.0.2.5:7878 --token '<设备码>:sha256:abcd1234...'
# → pulled N servers / M credentials into cache.bin

ssh-manager cache status                 # 看缓存状态
```

`.mcp.json` 配 agent（**离线为主**，缓存兜底；同一个 project token）：

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

重启 Claude Code → agent 用本地缓存跑命令（只读 + 已授权的 exec/传输/转发）。要在线写（加改删服务器），用在线模式指向 serve（见详尽版）。

---

## Step 3 —（可选）缓存自动保鲜说明

缓存现在自己保鲜，**默认无需任何系统定时器**：

- **spawn 自动拉**：Claude Code 启动 `mcp --cache` 时，若缓存超过 30 分钟（`--cache-max-age` 可调，`0` 关闭）且本机存过拉取凭据，会自动拉一次新缓存；失败静默用旧缓存。若 `cache.bin` 还不存在，只要有凭据且 `--cache-max-age>0`，会视为无限旧必拉一次；拉失败则按原有「首次 `cache pull` 必须在线手动」规则报错（不会静默降级出缓存）。
- **会话内懒检查 + 热加载**：运行中的会话在**每次工具调用前**懒检查缓存是否超过 TTL（默认 30 分钟），过期才自动拉新（空闲会话不刷新），下一次工具调用即生效——无需重启 Claude Code。
- 首次 `cache pull` 仍需手动（在线）执行一次；成功后凭据自动存入本机 `cache.auth.json`（0600），之后的自动拉取都靠它。

仍想配**可选的**系统定时器（比如给非 Claude 的消费方保鲜）？照旧跑 `cache pull`（**带指纹**，否则默认拒连）即可，模板见详尽版。

---

## 安全策略（重要）

- **指纹钉死**：工作机连 serve 时校验证书公钥 == 钉死的指纹，不等即拒。指纹在 enroll 时随设备码交付（**首次连接即校验，零 MITM 窗口**）。
- **无指纹默认拒连**：`cache pull` 没拿到指纹（env / `--pin` / token 内嵌三处都无）→ **hard-fail**（不再静默明文）。确需明文（连旧明文 serve 调试）显式加 `--allow-plaintext`。
- **enroll 渠道必须可信**：指纹和设备码是 `cache-tokens add` 一起打印的，别在被 MITM 的渠道上做首次 enroll。
- 详见 [`threat-model.md`](./threat-model.md) §1.1。

---

## 迁移 / 升级顺序（破坏性变更，必读）

新 master 的 serve **强制 TLS**。升级已部署的明文 serve 时：

> ⚠️ **顺序铁律：先升全部工作机 + 配 pin，最后才升 serve。** 升 serve 瞬间其变 TLS-only，旧明文 client 直连会断。

1. 先升所有工作机二进制。
2. serve 机跑 `ssh-manager serve cert-info` 拿指纹。
3. 各工作机配 `SSHMGR_SERVE_PIN=<指纹>`（或重发带指纹的设备码）。
4. **最后**重启 serve。

完整迁移 runbook + 密钥轮换 runbook（私钥泄露怎么办）→ [`multi-machine.md`](./multi-machine.md)。

---

## 接下来

- 架构 / 在线 live 模式 / 离线缓存全部细节 → [`multi-machine.md`](./multi-machine.md)
- cache-tokens 生命周期 / 吊销 → [`multi-machine.md`](./multi-machine.md#离线只读缓存plan-12)
- 备份 / 迁移整个 vault → [`backup-restore.md`](./backup-restore.md)
- **单机用法** → [`quickstart-single-machine.md`](./quickstart-single-machine.md)
