# 从零到跑通（Getting Started）

目标：15 分钟内，让一个 Claude Code agent 通过 ssh-manager-mcp 安全地在你的一台服务器上跑一条命令——**而 agent 全程看不到任何密码或私钥**。

全程分 6 步。每步都给了“它在干嘛”和“预期看到什么”。

---

## 前置条件

- 一台**你自己的机器**（Windows / Linux / macOS）作为操作端——broker 就跑在这里。
- 一台**目标服务器**，开放 SSH（默认 22 端口），你有它的密码或私钥。
- 拿到 `ssh-manager` 二进制（见下一步）。
- （可选）操作端有一个可用的 **OS keychain**：Windows Credential Manager / macOS Keychain / Linux 的 Secret Service（GNOME Keyring / KWallet）。**没有也能用**，走 passphrase 回退（见末尾“无 keychain 环境”）。

> ssh-manager-mcp **不是** LLM 客户端，不调用任何 LLM API。你不需要为它配任何 LLM key——Claude Code 自己的 key 跟本项目无关。

---

## Step 0：拿到 `ssh-manager` 二进制

任选其一：

```bash
# 方式 A：直接下载预编译二进制（最省事，推荐）
#   https://github.com/allanpk716/ssh-manager-mcp/releases
#   选对应平台，解压后把 ssh-manager 放到 PATH 里

# 方式 B：本地编译（需要 Go）
go build -o ssh-manager ./cmd/ssh-manager

# 方式 C：go install（装到 $GOPATH/bin）
go install ./cmd/ssh-manager
```

验证：

```bash
ssh-manager version     # 应打印版本号（本地编译是 dev）
ssh-manager --help      # 列出所有子命令
```

> 后文示例都假设 `ssh-manager` 在 `PATH` 里。如果没有，用绝对路径替换，例如 `./ssh-manager ...`。
> **Windows 用户**：二进制名是 `ssh-manager.exe`；在 `.mcp.json` 里写 `"command": "ssh-manager"` 时，要确保它确实在 PATH，或干脆写绝对路径（见 Step 5 的 Windows 提示）。

---

## Step 1：解锁保险柜（`unlock`）

保险柜（一个本地加密的 SQLite 文件）需要一个 **master key** 才能读写。第一次用，跑一次 `unlock`：

```bash
ssh-manager unlock
```

**会发生什么（按你的环境自动选路径）：**

- **有可用 keychain**（大多数桌面环境）：生成 32 字节随机 master key，**存进 OS keychain**，并打印一行：
  ```
  export SSHMGR_MASTERKEY_HEX=<64 位 hex>
  ```
  这行你**只需在第一次 / 或你想临时用环境变量时** `eval` 一下；常态下 master key 已经在 keychain 里了，MCP server 会自己读，**不需要你每次设环境变量**。

- **没有 keychain**（headless Linux 无 Secret Service）：`unlock` 会进入 **passphrase 回退**——提示你输入一个口令，用 Argon2id + 随机盐派生 master key，并打印同样的 `export SSHMGR_MASTERKEY_HEX=...`。
  > 这种环境下，master key **不落盘**，每次都要重新派生。为了让 MCP server 也能拿到它，你需要把那行 `export` 放进 Claude Code 启动 MCP 子进程时能继承的环境里（见末尾“无 keychain 环境”）。

**保险柜文件在哪？**

默认路径（可以用环境变量 `SSHMGR_STORE` 覆盖）：

| 平台 | 默认路径 |
|---|---|
| Windows | `%APPDATA%\ssh-manager\store.db`（通常 `C:\Users\<你>\AppData\Roaming\ssh-manager\store.db`） |
| Linux | `~/.config/ssh-manager/store.db`（或 `$XDG_CONFIG_HOME/ssh-manager/store.db`） |
| macOS | `~/Library/Application Support/ssh-manager/store.db` |

passphrase 模式还会在同目录生成 `store.db.meta.json`（存盐）。**这两个文件加 keychain 里的 master key，构成你的全部凭据存储——备份就备份这些，但务必和盘上其它东西一样妥善保管。**

---

## Step 2：加第一台服务器

```bash
ssh-manager servers add \
  --name gpu \
  --host 192.0.2.10 \
  --user deploy \
  --password '你的密码'              # 或用密钥：--key ~/.ssh/id_ed25519 [--key-passphrase '...']
  # 可选：
  --sudo-password 'sudo密码'         # 给了之后，agent 才能用 exec_command 的 sudo=true
  --description '8x A100 80GB, CUDA 12, 训练专用'   # 你的备注，**不会**给 agent 看到
  --tags prod,gpu
```

规则（很重要）：

- `--password` 和 `--key` **二选一**，必须恰好给一个。
- `--name` 全局唯一，后续命令都用这个名字（或它的 id）引用这台机器。
- `--description` 是给**你自己**看的硬件/用途备注，agent 的 `list_servers` **看不到**这个字段（防止你在这里写敏感信息泄露给 agent）。
- `--sudo-password` 只是个“额外的密码”，专门喂给 `sudo -S`；它决定了 agent 看到的 `has_sudo=true/false`。

预期输出：`added server gpu id=<uuid>`

验证：`ssh-manager servers ls` 应能看到 `gpu`。

---

## Step 3：建 profile，把服务器 grant 进去

```bash
ssh-manager profiles add team-a
ssh-manager profiles grant team-a gpu          # 可一次 grant 多台：... team-a gpu db web
```

预期输出分别是 `created profile team-a id=...` 和 `granted 1 server(s) to team-a`。

`ssh-manager profiles ls` 能看到每个 profile 里 grant 了几台 server。

> **为什么要有 profile？** 它是“一组服务器”。你给某个 agent 的门禁卡（project）绑一个 profile，它就只能碰这组里的机器。多 agent 隔离靠的就是“不同 agent 绑不同 profile”。详见 [agent-access.md](./agent-access.md)。

---

## Step 4：建 project，拿到一次性 token + `.mcp.json`

```bash
ssh-manager projects add my-agent --profile team-a
```

**token 只显示这一次**，输出形如：

```
Token (shown once): eyJ...（一长串）

.mcp.json snippet:
{"mcpServers":{"ssh":{"command":"ssh-manager","args":["mcp","--token","eyJ..."]}}}
```

- **马上把这段记下来**（密码管理器里）。token 之后再也查不到明文（库里只存它的 hash）。
- 如果你弄丢了，别慌：`ssh-manager projects rotate my-agent` 会换发一个新 token（旧 token 立刻失效），详见 [agent-access.md](./agent-access.md)。

---

## Step 5：把 `.mcp.json` 配进 Claude Code

Claude Code 读项目根目录的 `.mcp.json`。在你的工作目录里建一个 `.mcp.json`：

```json
{
  "mcpServers": {
    "ssh": {
      "command": "ssh-manager",
      "args": ["mcp", "--token", "把上一步的 token 粘到这里"]
    }
  }
}
```

注意事项：

- **Windows**：`ssh-manager` 必须能被 Claude Code 解析到。最稳妥是写**绝对路径**，例如 `"command": "C:\\path\\to\\ssh-manager.exe"`（JSON 里反斜杠要转义成 `\\`）。
- **token 是敏感信息**：`.mcp.json` 别提交进 git（项目 `.gitignore` 通常应忽略它；Claude Code 首次加载项目级 MCP 会弹确认）。
- 也可以用命令行注册到**用户级**（所有项目可见）：
  ```bash
  claude mcp add ssh ssh-manager -- --token <TOKEN>
  ```
- 其它 MCP 客户端（Cursor 等）：把同样的 JSON 按各自的方式填进去即可，格式是 MCP 标准。

详见 [agent-access.md](./agent-access.md)。

---

## Step 6：让 agent 跑第一个任务

重启 Claude Code（让它加载新的 `.mcp.json`），然后随便在一个项目里对它说：

> 用 ssh 工具看看 gpu 这台机器的显卡情况。

agent 会自己：

1. 调 `list_servers` —— 拿到 `gpu` 的 `id` 和 `has_sudo`；
2. 调 `exec_command`（用那个 `id`，命令如 `nvidia-smi`）—— 拿到输出；
3. 把结果讲给你听。

**它全程看不到密码，也看不到 `--description`，更碰不到 `team-a` profile 之外的任何机器。**

---

## 日常：`lock` / `unlock` / 备份

- **`unlock`**：master key 存在 keychain 里的话，**只需第一次跑**（或换了机器后）。常态下不用每天跑。它主要是“把 master key 引出来”或“首次初始化”用的。
- **`lock`**：在你当前 shell 里 `unset SSHMGR_MASTERKEY_HEX`（只是清掉环境变量，**不清 keychain**）。脚本里做完事想收尾时用。
- **备份**：把 `store.db`（+ passphrase 模式下的 `store.db.meta.json`）和 keychain 里的 master key 一起备份。丢了 `store.db` = 丢了所有服务器凭据；丢了 master key（且无 passphrase）= `store.db` 解不开。

---

## 重启 / 关机后，还要做什么吗？（不用——MCP 客户端会自动拉起）

**答：什么都不用做。** ssh-manager-mcp **不是常驻 daemon**——机器重启后，你不需要手动"启动"它。它是 MCP 客户端（Claude Code / Cursor）**按需 spawn 的 stdio 子进程**：重启机器 → 重开 Claude Code → 当你（或 agent）第一次需要用 SSH 工具时，Claude Code 自己会执行 `.mcp.json` 里那条命令把 broker 子进程拉起来。

> 这点和传统的 ssh-agent / vault 不一样——后者是常驻进程，要开机自启（systemd / Windows 服务 / launchd）。本项目**故意不做 daemon**，生命周期完全交给 MCP 客户端管理。所以"重启后要不要启动它"这个问题的答案是：**客户端会替你启动**。

### 重启后会发生什么

```
开机 → 启动 Claude Code（它读 .mcp.json）
     → 你让它用 ssh 工具
     → Claude Code spawn 子进程: ssh-manager mcp --token <TOKEN>
     → broker 在子进程里跑，stdio 通信
     → 关掉 Claude Code → 子进程随之退出
```

重启**不会丢**的东西（都在磁盘 / keychain，不在内存）：

| 东西 | 存哪 | 重启后 |
|---|---|---|
| 服务器凭据 / profile / project / 审计日志 | 加密 SQLite `store.db` | ✅ 还在 |
| 解密 `store.db` 的 master key | OS keychain（Win Credential Manager / Linux Secret Service / macOS Keychain） | ✅ 还在（keychain 本身持久） |
| `.mcp.json` / token 配置 | 项目目录或用户级配置 | ✅ 还在 |

### 三个"零干预"前提（一次配好，重启后永久成立）

1. **`ssh-manager` 二进制在 PATH**，或 `.mcp.json` 里写了**绝对路径**（Windows 强烈建议绝对路径，见 Step 5）。
2. **`.mcp.json` 已配好**（项目级或用户级都行），并且别提交 git。
3. **master key 走 OS keychain**——这是"重启零干预"的根本保证。keychain 模式下 MCP 子进程能自己读到 master key，**你连 `unlock` 都不用再跑**（见上文"日常"：`unlock` 只需第一次跑）。

三点满足后，日常就是：**开机 → 开 Claude Code → 用**。没有任何"启动本程序"这一步。

### 唯一例外：headless / 无 keychain 环境

如果操作端没有 OS keychain，master key 靠 `SSHMGR_MASTERKEY_HEX` 环境变量。重启后若这个变量不在 MCP 子进程能继承的环境里，你会撞到 `vault locked: run ssh-manager unlock`。解决：把 `export SSHMGR_MASTERKEY_HEX=...` 放进 `~/.bashrc`，**或**写死在 `.mcp.json` 的 `env` 字段——任选一种，重启后照样零干预。详见下方"无 keychain 环境"。

### 不用真重启也能验证

模拟一次"刚开机、啥都没起"的场景，确认链路健康：

```bash
ssh-manager servers ls          # 能列出 = store.db + master key 都健康（数据不依赖任何在跑的进程）
ssh-manager projects ls         # status=active
ssh-manager mcp --token <你的token>   # 手动跑客户端会跑的那条命令；不报 vault locked、停在等 stdin = 子进程能独立起来（Ctrl+C 退出）
```

两步都过 = 重启后 Claude Code 拉它起来时也会一样过——因为**子进程跑的就是同一条命令，读的是同一份磁盘文件**。

---

## 无 keychain 环境（headless Linux 等）

如果操作端没有可用的 OS keychain / Secret Service，`unlock` 会走 passphrase 回退。此时：

1. master key 不在 keychain 里，MCP server **无法自己读到**（它不能弹框问你口令）。
2. 你需要让 Claude Code 启动 `ssh-manager mcp` 子进程时，环境里带着 `SSHMGR_MASTERKEY_HEX`。做法：把 `ssh-manager unlock` 打印的那行 `export SSHMGR_MASTERKEY_HEX=...` 放进启动 Claude Code 的那个 shell 的 profile（如 `~/.bashrc`），或者写进 Claude Code 能继承的环境里。
3. `.mcp.json` 可以用 `env` 字段显式传（如果你的 MCP 客户端支持），例如：
   ```json
   { "mcpServers": { "ssh": {
       "command": "ssh-manager",
       "args": ["mcp", "--token", "<TOKEN>"],
       "env": { "SSHMGR_MASTERKEY_HEX": "<unlock 打印的那个 hex>" }
   } } }
   ```
   （把 hex 当作秘密，别提交。）

---

## 常见坑（首跑高频）

| 现象 | 原因 / 处理 |
|---|---|
| `vault locked: run ssh-manager unlock` | 没跑过 `unlock`，或 headless 环境没把 `SSHMGR_MASTERKEY_HEX` 传给 MCP 子进程。见上文“无 keychain 环境”。 |
| `keychain unavailable` | OS keychain 没装 / 没解锁。要么装一个 Secret Service，要么走 passphrase 回退。 |
| agent 报 `server is not in your profile` | 你让它操作的 server 不在它 project 绑定的 profile 里。用 `ssh-manager projects show <name>` 核对。 |
| agent 看不到 sudo / `has_sudo=false` | 加服务器时没给 `--sudo-password`。用 `ssh-manager servers edit <name> --sudo-password '...'` 补上。 |
| 首次连接弹 host key 确认？ | broker 用 **TOFU**（trust on first use）：第一次连一台机器会自动记下它的 host key，之后变了会拒绝（防中间人）。换机器/IP 后需要清旧记录（见 [managing-servers.md](./managing-servers.md#host-key-与-tofu)）。 |

下一步：去 [managing-servers.md](./managing-servers.md) 学完整的增删改查，去 [agent-access.md](./agent-access.md) 学授权与吊销，去 [scenarios.md](./scenarios.md) 看真实场景。
