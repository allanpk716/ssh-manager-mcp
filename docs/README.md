# ssh-manager-mcp 使用文档（中文）

这是一份面向**操作者（你）**的实操手册：怎么把你的 SSH 服务器接进来、怎么管、怎么授权给 AI agent（Claude Code / Cursor / 任何 MCP 客户端）去安全地操作它们。

> 仓库根目录的 [README](../README.md) 是英文的项目概览（它解决什么、安全模型、能力边界）。本目录是**“我作为人类操作者，一步步怎么做”**的展开。

---

## 两个角色，不要混淆

| 角色 | 是谁 | 用什么 | 能看到凭据吗 |
|---|---|---|---|
| **Owner（你）** | 人类操作者 | `ssh-manager` 命令行（owner CLI） | 能（持有 master key，可解锁保险柜） |
| **Agent（AI）** | Claude Code / Cursor 等 | MCP 工具（`list_servers` / `exec_command` / …） | **永远不能**（只拿到命令输出 / 文件字节 / 转发端口） |

核心安全模型（**铁律**）：凭据（密码 / 私钥）只存在加密保险柜里；agent 用一个 **project token** 认证到 MCP server（即 broker），broker 自己开 SSH 连接，只把**结果**返回给 agent——**凭据字节永远不会出现在任何工具返回里**。Agent 自己的 `ssh`（就算它有 shell）也登不进去，因为 `~/.ssh` 和 `ssh-agent` 里根本没有它能用的凭据。

详细的安全模型与授权机制见 [agent-access.md](./agent-access.md)。

---

## 目录

| 文档 | 解决什么 |
|---|---|
| [getting-started.md](./getting-started.md) | **从零到跑通**：装好 → 解锁 → 加第一台服务器 → 建 profile + project → 配进 Claude Code → 让 agent 跑第一个任务。第一次用先看这篇。 |
| [managing-servers.md](./managing-servers.md) | **新增 / 编辑 / 维护 / 删除服务器**：`servers add` / `edit` / `ls` / `rm` 的全部用法，含换密钥、sudo、tags、备注（description）。 |
| [agent-access.md](./agent-access.md) | **授权 AI agent**：project token 怎么生成、`.mcp.json` 怎么配进 Claude Code / Cursor、token 轮换 / 暂停 / 吊销的 Lazy 语义、多 agent 隔离、紧急处置。 |
| [scenarios.md](./scenarios.md) | **应用场景与示例**：GPU 巡检装包、读 root-only 日志、上传部署、端口转发连数据库、拉日志排查、多环境隔离、token 泄露处置、owner 自己直连。 |
| [multi-machine.md](./multi-machine.md) | **多机共享（serve 模式 · 可选）+ 离线只读缓存（Plan 12）**：多台机器共用一份服务器清单——一台 VLAN 服务器常驻 broker、其他机器的 agent 连远程；或每台工作机持本地加密只读缓存，断网时兜底。架构 / 配置 / 场景 / 限制（含后续路线）。 |
| [backup-restore.md](./backup-restore.md) | **备份与迁移（export / import）**：把整个 vault 导出成口令加密的便携文件（跨机、可恢复）——备份 / 迁移 / 灾难恢复；安全模型（KeePass 式）、限制、与复制 store.db 的对比。 |

---

## 三条心智主线（贯穿所有文档）

1. **Server**：一台被记录的目标机器，**自带它的凭据**（密码或私钥，外加可选的 sudo 密码）。
2. **Profile**：服务器的**分组**。把若干 server “grant” 到一个 profile 里。
3. **Project**：一个**绑定到某个 profile 的 token**。一个 agent 拿一个 project token → 只能看到 / 碰到**那个 profile 里的 server**。跨 profile 访问会被 broker 拒绝（这条已被对抗测试覆盖）。

一句话：**Server 是资产，Profile 是分组，Project 是给某个 agent 的“门禁卡”，卡只能开它绑定的那道门。**

> 注：本目录里的 `superpowers/`、`ssh-conformance/`、`eval/` 子目录是**内部文档**（设计 spec、实现计划、SSH 一致性测试、agent 评测记录），不属于使用教程，日常操作无需阅读。

---

## 需要帮忙？

- 命令记不全：每个子命令都支持 `--help`，例如 `ssh-manager servers add --help`、`ssh-manager projects --help`。
- 报错“vault locked”：回到 [getting-started.md](./getting-started.md) 的“Step 1：解锁保险柜”一节。
- 机器重启后还要做什么吗？：**不用启动任何东西**——它不是 daemon，MCP 客户端（Claude Code / Cursor）会在你需要时自动 spawn 它。见 [getting-started.md](./getting-started.md) 的“重启 / 关机后”一节。
- agent 报“server is not in your profile”：说明你让它操作的 server 不在它 project 绑定的 profile 里，见 [agent-access.md](./agent-access.md) 的“隔离与排错”。
