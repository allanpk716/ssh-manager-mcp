# 单机快速上手（Quickstart）

> **场景**：只有一台操作机 + 一台目标服务器，让 Claude Code / Cursor 这类 AI agent 在目标机上跑命令、传文件、转发端口 —— **而 agent 全程看不到你的 SSH 密码或私钥**。
>
> 15 分钟、5 步搞定。全文只讲「最少要做什么」；想了解每一步在干嘛、所有可选项、排错，看详尽版 [`getting-started.md`](./getting-started.md)。

---

## 它是什么

`ssh-manager` 把你的 SSH 凭据锁进一个**本地加密保险柜**。AI agent 不直接拿凭据，而是通过一个 broker 工具去操作服务器 —— broker 在 agent 启动的 MCP server 进程里跑，**单机模式不需要任何常驻服务、不需要网络监听**。凭据永远不进 agent 的上下文。

---

## Step 1 — 拿到二进制

```bash
# 推荐：直接下载预编译二进制
#   https://github.com/allanpk716/ssh-manager-mcp/releases
# 或本地编译（需要 Go）：
go build -o ssh-manager ./cmd/ssh-manager

ssh-manager version      # 验证：打印版本号
```

> Windows：二进制是 `ssh-manager.exe`，放进 PATH 或后续命令用绝对路径。

---

## Step 2 — 解锁保险柜（一次性）

```bash
ssh-manager unlock
```

生成 master key，写入固定路径裸文件（Win `C:\ProgramData\ssh-manager\master.key.plain` / Unix `/var/lib/ssh-manager/`，权限 0600 + ACL 硬化）。**跑一次即可**，之后不用再跑。

> 这是 **L1+ 威胁模型**：防同机非特权进程读，**不**防 admin/root 或离线拷盘。适用于单用户、机器可信的部署。详见 [`threat-model.md`](./threat-model.md)。

---

## Step 3 — 录入一台服务器

> 💡 Step 3-4 也可在 TUI 主控台里点点点完成：`ssh-manager tui`（全键盘点选教程见 [tui-single-machine.md](./tui-single-machine.md)）。

```bash
ssh-manager servers add --name gpu \
    --host 192.0.2.10 --user deploy \
    --password '你的SSH密码'          # 或: --key ~/.ssh/id_ed25519
```

可选：加 sudo 密码让 agent 能跑 `sudo`（`--sudo-password '...'`）。还可以挂结构化备注给 agent 看（`--role` / `--hardware` / `--location` 等，**别放机密**）。

```bash
ssh-manager servers ls                 # 看已录入的服务器
```

---

## Step 4 — 建一个 profile + project（授权 agent）

```bash
# profile = 一组服务器；project = 一张发给 agent 的「通行证」
ssh-manager profiles add team-a
ssh-manager profiles grant team-a gpu

ssh-manager projects add my-agent --profile team-a
```

`projects add` 会**打印一个一次性 token** + 一段 `.mcp.json` 片段，**当场记下来**（token 只显示一次）：

```json
{"mcpServers":{"ssh":{"command":"ssh-manager","args":["mcp"],"env":{"SSHMGR_TOKEN":"<TOKEN>"}}}}
```

---

## Step 5 — 配进 Claude Code

把上面的片段写进 `~/.mcp.json`（或项目级 `.mcp.json`），重启 Claude Code。现在 agent 有了 9 个 SSH 工具（`list_servers` / `exec_command` / `download_file` / `upload_file` / `forward_port` / `close_port` / 后台三件套 `exec_background` / `exec_output` / `exec_stop`），范围仅限 `team-a` profile 里你授权的服务器。

对 agent 说「列出可用服务器」→ 它先调 `list_servers` 拿到真实 id → 就能跑命令了。

---

## 你（owner）自己也能用

agent 之外，你本人可以用存储的凭据直接在服务器上跑**单条命令**（非交互；连接+执行共享 120 秒超时，输出不封顶，远端非零退出会使本命令以非零码退出（码值见 stderr 错误消息））：

```bash
ssh-manager ssh gpu nvidia-smi         # 直接跑一条命令（不带命令会显式报错）
```

> 这条路**不是交互式终端**。要开终端，用你自己的 ssh 客户端（凭据需自行已有或另行配置——它们可能只存在本 vault 里）。

---

## 常用维护命令

日常管理推荐直接开 TUI 主控台 `ssh-manager tui`（增删改查/授权/发码全键盘完成）；命令行 equivalents：

```bash
ssh-manager servers edit gpu --hardware "8x A100"   # 改服务器信息（id/profile 绑定不变）
ssh-manager projects rotate my-agent                # 换 token（旧的立即失效）
ssh-manager projects revoke my-agent                # 永久吊销
ssh-manager export                                  # 备份整个 vault（口令加密文件）
ssh-manager lock                                    # 锁回（删内存里的 key；要再跑 unlock）
```

---

## 接下来

- 完整流程 / 所有可选项 / 排错 → [`getting-started.md`](./getting-started.md)
- 服务器增删改查 → [`managing-servers.md`](./managing-servers.md)
- token 生命周期（轮换 / 吊销）→ [`agent-access.md`](./agent-access.md)
- 应用场景示例 → [`scenarios.md`](./scenarios.md)
- TUI 全程点选教程 → [`tui-single-machine.md`](./tui-single-machine.md)；把 [agent-tools.md](./agent-tools.md) 的规则模板贴进 CLAUDE.md，agent 会更守规矩
- **多台机器共用一份 vault** → [`quickstart-multi-machine.md`](./quickstart-multi-machine.md)
