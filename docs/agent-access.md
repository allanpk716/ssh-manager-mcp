# 授权 AI agent 使用你的服务器

这篇讲：怎么给一个 AI agent（Claude Code / Cursor / 任何 MCP 客户端）发“门禁卡”，让它能、且仅能操作你指定的一组服务器；以及怎么轮换、暂停、吊销这张卡。

> 还没建好服务器和 profile？先看 [getting-started.md](./getting-started.md) 和 [managing-servers.md](./managing-servers.md)。

---

## 授权的本质：三步

```
Server（机器+凭据）  ──grant──▶  Profile（分组）  ◀──bind──  Project（token）  ──▶  Agent
```

1. （已有）把 server grant 进一个 profile。
2. **建 project，绑定到那个 profile** → 得到一次性 **token**。
3. **把 token 写进 agent 的 MCP 配置**（`.mcp.json`）。

agent 拿着 token 启动 `ssh-manager mcp`（token 经 `--token` 或 env `SSHMGR_TOKEN` 传入，`.mcp.json` 推荐 env 形态），broker 校验 token → 只放行它绑定的 profile 里的 server。**跨 profile 访问一律拒绝。**

---

## 1. 建 project 拿 token

```bash
ssh-manager projects add <project名> --profile <profile名>
```

输出（**token 只显示这一次，立刻存密码管理器**）：

```
Token (shown once): eyJ...（一长串）

.mcp.json snippet:
{"mcpServers":{"ssh":{"command":"ssh-manager","args":["mcp"],"env":{"SSHMGR_TOKEN":"eyJ..."}}}
```

库里只存 token 的 **hash + salt**（像密码哈希），所以：
- token 丢了 → 查不回明文 → 用 `rotate` 换发（见下）。
- `projects ls` 只显示 token 的**前缀**（方便辨认是哪张卡），不是完整 token。

---

## 2. 配进 Claude Code

Claude Code 读 `.mcp.json`。两种范围：

### A. 项目级（推荐起步用）

在你的工作目录根建 `.mcp.json`：

```json
{
  "mcpServers": {
    "ssh": {
      "command": "ssh-manager",
      "args": ["mcp"],
      "env": { "SSHMGR_TOKEN": "eyJ...你的token..." }
    }
  }
}
```

token 走 `env` 字段（`SSHMGR_TOKEN`）而不是 `args` 里的 `--token`：**消除的是 argv/ps 暴露面**——token 不再出现在子进程命令行里（`ps` / 任务管理器 / `/proc/<pid>/cmdline` 看不到）；env 仍可被同用户/root 经 `/proc/<pid>/environ`（Linux）读到，**不是全部可见性**。（`--token` 仍支持，语义相同。）

- Claude Code 首次加载会**弹确认**让你批准这个项目级 MCP server——批准后该项目的会话就有这 6 个 SSH 工具。
- **别提交 git（公开仓库尤其致命）**：`.mcp.json` 含**活 token**，必须加进 `.gitignore`，绝不能提交进 git 仓库。
- **Windows**：写绝对路径最稳，例如 `"command": "C:\\Tools\\ssh-manager.exe"`（JSON 里 `\` 要写成 `\\`）。
- **headless / 无 keychain**：master key 不在 keychain，需要给子进程传环境变量，加进同一个 `env` 字段即可（见 [getting-started.md](./getting-started.md#无-keychain-环境headless-linux-等)）：
  ```json
  { "mcpServers": { "ssh": {
      "command": "ssh-manager",
      "args": ["mcp"],
      "env": { "SSHMGR_TOKEN": "eyJ...", "SSHMGR_MASTERKEY_HEX": "<unlock 打印的 hex>" }
  } } }
  ```

### B. 用户级（所有项目共享）

```bash
claude mcp add ssh ssh-manager -e SSHMGR_TOKEN=<TOKEN> -- mcp
```

（适合你只自己用、不想每个项目都配一遍的情况。）

### 配进其它 MCP 客户端（Cursor 等）

把同样的 JSON 按该客户端的 MCP 配置入口填进去即可——`mcpServers` 这个结构是 [MCP 标准](https://modelcontextprotocol.io)。例如 Cursor 在其设置里的 MCP servers 面板，字段一一对应。

---

## 3. 验证

重启 agent 客户端（让它重新加载配置），让它：

> 用 ssh 工具列出我能用的服务器。

它应调 `list_servers`，返回你 grant 给那个 profile 的 server 列表（`id` / `name` / `host` / `user` / `has_sudo`，加上 owner 提供的上下文：`role` / `services`（部署了什么）/ `location` / `hardware` / `caveats`（特殊处理——动手前先读）/ `tags` / `description`，**无凭据**）。能列出来说明整条链路通了。

---

## Project 生命周期：轮换 / 暂停 / 恢复 / 吊销

⚠️ **关键机制——断连语义按部署模式分四层**（`rotate` / `disable` / `enable` / `revoke` 的生效范围）：

1. **stdio（本机 MCP 子进程）——Lazy 生效**：token 校验只在 `mcp` 子进程**下次启动**时跑（只放行 `status=active`）。正在跑的会话保留访问直到你重启 Claude Code（或它的 MCP 子进程）。**你的机器你做主**：这是有意的设计。
2. **serve（远程 broker）——逐请求即拒**：broker 对**每一个** HTTP 请求都重新验 token，`revoke`/`disable` 后该 project 的**下一个请求立即 401**——不需要等任何重启。
3. **已建立的 `forward_port` 隧道——不受 revoke 影响，且无 owner 急停**：隧道由 broker 进程持有；被吊销的 project 自己调 `close_port` 会先被第 2 层的 401 挡住；其他人的隧道管理器也够不到它——stdio 会话跑在**独立进程**里，同一 serve broker 上的其他 project 则各有**各自独立的隧道管理器实例**（互不相通）。真实选项只有：**重启 broker**（`serve uninstall`→`install` 或重启机器）/ **等隧道创建后 ~10 分钟自动回收**。（owner 侧急停命令已列 backlog，见 docs/backlog.md。）
4. **离线 cache——旧快照不随 revoke 擦除**：`cache-tokens revoke` 只断"拉新"（下次 `cache pull` 被拒）；已落盘的 `cache.bin` 里凭据仍在。**失窃/泄露场景下让已缓存凭据失效的唯一手段是轮换服务器凭据**（`servers edit <name> --password/--key`）。

未实现的拆除手段见 docs/backlog.md。

`rotate` 保持 project id 和 profile 不变，**只换 token**（serve 模式下旧 token 同样逐请求即拒）。

| 命令 | 作用 | token 结果 | `ls` 里是否可见 |
|---|---|---|---|
| `projects rotate <name>` | 原地换发 token（active/disabled 行；revoked 行硬拒，v0.8.9 起——死 token 不再以"成功"面目发出） | 旧 token 立刻失效，打印新 token + 新 `.mcp.json` | 可见（仍 active） |
| `projects disable <name>` | 暂停（可逆） | token 被拒，直到 enable | 可见（status=disabled） |
| `projects enable <name>` | 恢复（仅 disabled 行；revoked 不可复活，v0.8.8 起硬拒） | token 重新有效 | 可见（status=active） |
| `projects revoke <name>` | 永久吊销（软删除，单向门——enable/disable 均无法恢复，需要时新建 project） | token 永久失效 | **默认隐藏**（`--all` 才看得到） |

查询：

```bash
ssh-manager projects ls            # 列出所有 project（不含 revoked），含 status 和 token 前缀
ssh-manager projects ls --all      # 含已 revoke 的
ssh-manager projects show <name>   # 看 这个 agent → profile → 能碰哪些 server（无任何密钥）
```

`projects show` 输出示例：

```
project: my-agent  (status: active)
profile: team-a
servers:
  - gpu              deploy@192.0.2.10:22
  - db               pguser@10.0.0.5:22
```

所有生命周期动作都会写进**审计日志**（`project.rotate` / `project.disable` / …），和每次工具调用一起被记录。

---

## 典型编排

### 单人单 agent

```
ssh-manager profiles add mine
ssh-manager profiles grant mine gpu db web
ssh-manager projects add my-claude --profile mine
# 把打印的 token 配进 .mcp.json
```

### 多 agent / 多人隔离（每个 agent 一张卡，各开各的门）

```
# 运维 agent：只能碰生产 web
ssh-manager profiles add ops-web && ssh-manager profiles grant ops-web prod-web
ssh-manager projects add ops-agent --profile ops-web

# 数据 agent：只能碰数仓
ssh-manager profiles add data     && ssh-manager profiles grant data warehouse
ssh-manager projects add data-agent --profile data

# 实习生 agent：只能碰 dev
ssh-manager profiles add dev      && ssh-manager profiles grant dev dev-web dev-db
ssh-manager projects add intern   --profile dev
```

每个 agent 拿到的是**不同**的 token，绑**不同**的 profile——它们彼此完全看不到对方的机器（跨 profile 访问被 broker 拒绝，已对抗测试）。

### 紧急处置

| 情况 | 动作 |
|---|---|
| 怀疑 token 泄露（`.mcp.json` 被偷看 / 提交到公开仓库了） | `projects rotate <name>` 立刻作废旧 token，换发新的；去客户端更新 `.mcp.json`。 |
| 某个 agent 要临时停（放假 / 审查中） | `projects disable <name>`，事后 `projects enable <name>` 恢复。 |
| 某个 agent 彻底不用了 / 离职 | `projects revoke <name>`；审计记录保留。 |
| 要立刻断正在跑的会话 | 看模式：serve 远程 agent 下一个请求即拒（无需动作）；stdio 本机会话须重启客户端；既有隧道见「断连语义（四层）」第 3 层（只能重启 broker 或等回收）。 |

---

## 安全模型回顾（铁律）

1. **凭据不出 broker。** 密码 / 私钥只在加密保险柜里。agent 用 token 认证 → broker 自己开 SSH → 只返回命令输出 / 文件字节 / 转发端口。任何工具返回里都不会出现凭据字节。
2. **Profile 隔离。** 一个 project 绑一个 profile，只能碰那组 server。跨 profile 被 broker 拒绝（`server is not in your profile`）。
3. **agent 自己登不进去。** 它的 `~/.ssh` / `ssh-agent` 里没有能用的凭据——被强制走 MCP。启动 `mcp` 时，broker 还会扫一遍本机有没有散落的 SSH 凭据文件，发现就**告警到 stderr**（提示“有旁路风险，建议清掉”）。
4. **全程审计。** 每次工具调用（project / server / action / status / 命令 / 耗时）都写进 `audit_log`；生命周期动作也记。
5. **TOFU host key。** 第一次连记录，之后对不上就拒（防中间人）。
6. **服务端封顶。** 单条 `exec_command` 默认 120s、硬上限 5 分钟；输出每通道 1 MiB 封顶（超出标 `truncated`，告诉你真实字节数，让你 refine 命令而不是硬拉）。防止 agent 跑飞把 broker 占死 / 把自己的上下文撑爆。

---

## 隔离与排错

| 现象 | 处理 |
|---|---|
| agent 报 `server is not in your profile` | 你给错了 server（用了名字而非 id，或那台不在它 profile 里）。让它先 `list_servers` 拿 id；或用 `projects show <name>` 核对它到底能碰哪些。 |
| agent 调工具直接报 token 无效 | token 输错了 / 已 rotate / project 被 disable/revoke。`projects ls` 看 status；必要时 `rotate` 换发并更新 `.mcp.json`。 |
| `mcp` 启动时 stderr 有 `WARNING: ssh credential files detected` | 你本机有散落的 SSH 私钥/密码文件，agent 可能绕过 broker 直接读它们。按提示删掉，以保持“强制走 broker”的隔离。 |
| 暂停了 agent 还在跑 | stdio：Lazy，下次重连才接管，重启那个客户端；serve：下一请求即拒，无需动作。详见「断连语义（四层）」。 |
| Windows 下 agent 说找不到 `ssh-manager` | `.mcp.json` 的 `command` 写绝对路径（`C:\\...\\ssh-manager.exe`）。 |

下一步：去 [scenarios.md](./scenarios.md) 看这些授权在真实任务里长什么样。
