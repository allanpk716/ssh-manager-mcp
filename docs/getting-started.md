# 从零到跑通（Getting Started）

目标：15 分钟内，让一个 Claude Code agent 通过 ssh-manager-mcp 安全地在你的一台服务器上跑一条命令——**而 agent 全程看不到任何密码或私钥**。

全程分 6 步。每步都给了“它在干嘛”和“预期看到什么”。

---

## 前置条件

- 一台**你自己的机器**（Windows / Linux / macOS）作为操作端——broker 就跑在这里。
- 一台**目标服务器**，开放 SSH（默认 22 端口），你有它的密码或私钥。
- 拿到 `ssh-manager` 二进制（见下一步）。
- master key 存为一个**裸文件 + ACL**（`master.key.plain`）放在程序固定路径下（Win `C:\ProgramData\ssh-manager\` / Unix `/var/lib/ssh-manager/`）。这是 **L1+ 威胁模型**（见 [threat-model.md](./threat-model.md)）——防同机非特权进程意外读，**不**防 admin/root 或离线拷盘。适用于单用户、机器可信的部署。

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

**会发生什么**：

- `unlock` 生成 32 字节随机 master key，**写入固定路径的裸文件**（`master.key.plain`，0600 权限 / Windows ACL 硬化——`SYSTEM` + `Administrators` + 当前用户，移除 `Users`/`Authenticated Users`/`Everyone`）。
- 不再有 keychain / DPAPI tier（Plan 16 删干净）。**第一次跑 `unlock` 后，常态下不用再跑**——MCP server / serve 都能直接读这个文件。
- 历史升级：从 v0.2.0（keychain）或 Plan 14/15（DPAPI）升级，见 [backup-restore.md 的迁移 Runbook](./backup-restore.md)。

**保险柜文件在哪？**

固定路径（可以用环境变量 `SSHMGR_STORE` / `SSHMGR_FILEKEY_PATH` 覆盖，**仅供测试/迁移/自定义**，生产不建议改）：

| 平台 | store.db / master.key.plain / cache-dek.key / serve.log |
|---|---|
| Windows | `C:\ProgramData\ssh-manager\` |
| Linux | `/var/lib/ssh-manager/` |
| macOS | `/var/lib/ssh-manager/` |

> **首次创建路径需要权限**：Windows 上首次跑 `unlock` / `serve install` 需 admin（建 `C:\ProgramData\ssh-manager\` + 设 ACL）；Linux/macOS 上需 root 或 `serve install` 时建目录 + chown 给 service 账户。非特权进程建不了 `/var/lib/` 下的目录——程序会报错提示先 `serve install`。

> **威胁模型（L1+，必读）**：master.key 是**裸明文**——admin/root 可读、离线拷盘可得。仅适用于"单用户、机器可信"。完整前提 / 残留风险 R1-R3 / 升级路径 U1-U3 见 [threat-model.md](./threat-model.md)。**不要把 `SSHMGR_MASTERKEY_HEX` 环境变量用于生产 boot 自起**（明文会落进 service 配置）。

完整备份 / 迁移 / 灾难恢复见 [backup-restore.md](./backup-restore.md)。

---

## Step 2：加第一台服务器

> 💡 不想背命令行？**Step 2-4（加服务器/建 profile/发 token）都可以在 TUI 主控台里完成**：`ssh-manager tui`（全屏界面，凭据掩码录入、token 一次性展示——见 [README「TUI 主控台」](../README.md#tui-主控台ssh-manager-tui)；概念分不清时翻 [concepts.md](./concepts.md)）。

```bash
ssh-manager servers add \
  --name gpu \
  --host 192.0.2.10 \
  --user deploy \
  --password '你的密码'              # 或用密钥：--key ~/.ssh/id_ed25519 [--key-passphrase '...']
  # 可选：
  --sudo-password 'sudo密码'         # 给了之后，agent 才能用 exec_command 的 sudo=true
  --hardware '8x A100 80GB, CUDA 12'   # 结构化字段，会给 agent 看到
  --tags prod,gpu
```

规则（很重要）：

- `--password` 和 `--key` **互斥**（最多给一个）。也可以**都不给**——先录机器、凭据后补（「无凭据服务器」，见 [managing-servers.md](./managing-servers.md#无凭据服务器)）。
- `--name` 全局唯一，后续命令都用这个名字（或它的 id）引用这台机器。
- `--hardware` / `--role` / `--services` / `--location` / `--special-handling`（Caveats）是结构化字段，`--description` / `--tags` 是补充——**全部会通过 `list_servers` 给 agent 看到**（agent 需要机器全貌才能安全操作）。⚠️ **别在这些字段里放任何敏感信息**（密钥 / token / PII）：它们每次都进入 agent 上下文并上行到 LLM 提供方；机密只走凭据保险柜（`--password` / `--key`）。完整字段表见 [managing-servers.md](./managing-servers.md#structured-server-metadata)。
- `--sudo-password` 只是个“额外的密码”，专门喂给 `sudo -S`；它决定了 agent 看到的 `has_sudo=true/false`。

预期输出：`added server gpu id=<uuid>`

验证：`ssh-manager servers ls` 应能看到 `gpu`。

> 💡 **手上已有一份 `~/.ssh/config`？不用逐台敲。** 一条命令批量导入（TUI 主控台服务器页按 `i` 也一样）：
>
> ```bash
> ssh-manager servers import --dry-run          # 先看会发生什么（什么都不写）
> ssh-manager servers import --profile team-a   # 确认无误再真导入，顺手 grant 进 profile
> ```
>
> `--profile` 要求 profile 已存在（fail-fast）——刚走到这步还没建的话，先不带它导入，下一步建好 profile 再 `profiles grant`。同名 / 同 host:port:user 的机器自动跳过（可反复跑），缺凭据或缺私钥口令的机器以 ⚠ 标出、事后逐台补。完整语义（密钥去重、Match 警示、相对路径规则）见 [managing-servers.md 的「批量导入」](./managing-servers.md#批量导入servers-import)。

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
{"mcpServers":{"ssh":{"command":"ssh-manager","args":["mcp"],"env":{"SSHMGR_TOKEN":"eyJ..."}}}
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
      "args": ["mcp"],
      "env": { "SSHMGR_TOKEN": "把上一步的 token 粘到这里" }
    }
  }
}
```

token 走 `env` 字段（`SSHMGR_TOKEN`）而不是 `args` 里的 `--token`：**消除的是 argv/ps 暴露面**——token 不再出现在子进程命令行里（`ps` / 任务管理器 / `/proc/<pid>/cmdline` 看不到）；env 仍可被同用户/root 经 `/proc/<pid>/environ`（Linux）读到，**不是全部可见性**。（`--token` 仍支持，语义相同。）

注意事项：

- **Windows**：`ssh-manager` 必须能被 Claude Code 解析到。最稳妥是写**绝对路径**，例如 `"command": "C:\\path\\to\\ssh-manager.exe"`（JSON 里反斜杠要转义成 `\\`）。
- **别提交 git（公开仓库尤其致命）**：`.mcp.json` 含**活 token**，必须加进 `.gitignore`，绝不能提交进 git 仓库。（Claude Code 首次加载项目级 MCP 会弹确认。）
- 也可以用命令行注册到**用户级**（所有项目可见）：
  ```bash
  claude mcp add ssh ssh-manager -e SSHMGR_TOKEN=<TOKEN> -- mcp
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

**它全程看不到密码，更碰不到 `team-a` profile 之外的任何机器。**（`--description` 等元数据字段它**看得到**——那是给它的上下文；凭据则永远看不到。）

---

## 日常：`lock` / `unlock` / 备份

- **`unlock`**：master key 写进固定路径的裸文件（`master.key.plain`）。**只需第一次跑**（或换机器后）。常态下不用每天跑。
- **`lock`**：在你当前 shell 里 `unset SSHMGR_MASTERKEY_HEX`（只是清掉环境变量，**不删 master.key 文件**）。脚本里做完事想收尾时用。
- **备份**：`master.key.plain` + `store.db`（+ cache 模式下的 `cache-dek.key`）是全部凭据存储。丢了 `store.db` = 丢了所有服务器凭据；丢了 `master.key.plain` = `store.db` 解不开。**两者都不可移植**（绑本机固定路径 + L1+ 威胁模型），便携备份走 [export/import](./backup-restore.md)。

---

## 重启 / 关机后，还要做什么吗？（不用——MCP 客户端会自动拉起）

**答：什么都不用做。** ssh-manager-mcp **不是常驻 daemon**——机器重启后，你不需要手动"启动"它。它是 MCP 客户端（Claude Code / Cursor）**按需 spawn 的 stdio 子进程**：重启机器 → 重开 Claude Code → 当你（或 agent）第一次需要用 SSH 工具时，Claude Code 自己会执行 `.mcp.json` 里那条命令把 broker 子进程拉起来。

> 这点和传统的 ssh-agent / vault 不一样——后者是常驻进程，要开机自启（systemd / Windows 服务 / launchd）。本项目**故意不做 daemon**，生命周期完全交给 MCP 客户端管理。所以"重启后要不要启动它"这个问题的答案是：**客户端会替你启动**。

### 重启后会发生什么

```
开机 → 启动 Claude Code（它读 .mcp.json）
     → 你让它用 ssh 工具
     → Claude Code spawn 子进程: ssh-manager mcp（token 经 env SSHMGR_TOKEN）
     → broker 在子进程里跑，stdio 通信
     → 关掉 Claude Code → 子进程随之退出
```

重启**不会丢**的东西（都在磁盘，不在内存）：

| 东西 | 存哪 | 重启后 |
|---|---|---|
| 服务器凭据 / profile / project / 审计日志 | 加密 SQLite `store.db`（固定路径，见 Step 1 路径表） | ✅ 还在 |
| 解密 `store.db` 的 master key | 裸文件 `master.key.plain`（固定路径，ACL/0600 保护） | ✅ 还在（文件本身持久） |
| `.mcp.json` / token 配置 | 项目目录或用户级配置 | ✅ 还在 |

### 三个"零干预"前提（一次配好，重启后永久成立）

1. **`ssh-manager` 二进制在 PATH**，或 `.mcp.json` 里写了**绝对路径**（Windows 强烈建议绝对路径，见 Step 5）。
2. **`.mcp.json` 已配好**（项目级或用户级都行），并且别提交 git。
3. **master.key 文件已存在**（跑过一次 `ssh-manager unlock`）——这是"重启零干预"的根本保证。MCP 子进程能自己读到 master.key，**你连 `unlock` 都不用再跑**（见上文"日常"：`unlock` 只需第一次跑）。

三点满足后，日常就是：**开机 → 开 Claude Code → 用**。没有任何"启动本程序"这一步。

### 不用真重启也能验证

模拟一次"刚开机、啥都没起"的场景，确认链路健康：

```bash
ssh-manager servers ls          # 能列出 = store.db + master key 都健康（数据不依赖任何在跑的进程）
ssh-manager projects ls         # status=active
SSHMGR_TOKEN=<你的token> ssh-manager mcp   # 手动跑客户端会跑的那条命令；不报 vault locked、停在等 stdin = 子进程能独立起来（Ctrl+C 退出）
```

两步都过 = 重启后 Claude Code 拉它起来时也会一样过——因为**子进程跑的就是同一条命令，读的是同一份磁盘文件**。

---

## 测试 / 脚本：`SSHMGR_MASTERKEY_HEX` 环境变量

Plan 16 起，master key **不再依赖 OS keychain**——固定路径裸文件就是默认路径，所有平台（含 headless Linux）行为一致。`SSHMGR_MASTERKEY_HEX` 环境变量退化为**仅供测试 / 脚本 / 临时迁移**的注入 tier（`resolveMasterKey` 在文件 tier 之前检查它）。

> **⚠️ 不要用于生产 boot 自起**：把 `SSHMGR_MASTERKEY_HEX` 写进 service 配置（Windows 注册表 / systemd `EnvironmentFile` / launchd plist）= **明文 master key 落进 service 配置文件**，比 0600+ACL 的 `master.key.plain` **更糟**（service 配置常进版本控制 / 备份 / 监控采集）。生产路径只能走 `FileKeyProvider`，详见 [threat-model.md §5](./threat-model.md)。

仅在以下场景用 env tier：

1. **测试**：单测 / 集成测试用 `SSHMGR_MASTERKEY_HEX` 注入 master key，不污染固定路径文件。
2. **临时脚本**：CI / 一次性脚本里不想落盘文件。
3. **手动调试**：临时用别的 master key 解 vault（`SSHMGR_STORE` 指向另一份 `store.db`）。

`.mcp.json` 也可经 `env` 字段传（你的 MCP 客户端支持的话）：
```json
{ "mcpServers": { "ssh": {
    "command": "ssh-manager",
    "args": ["mcp"],
    "env": { "SSHMGR_TOKEN": "<TOKEN>", "SSHMGR_MASTERKEY_HEX": "<unlock 时输出的 hex>" }
} } }
```
（把 hex 当秘密，别提交——但**不推荐**生产用，原因见上。）

---

## serve 常驻 / 开机自起（`serve install`，跨平台）

如果你要**多机共享 vault**（笔记本 + 台式机共用一份清单），需要在 VLAN 一台服务器上常驻 `ssh-manager serve`。Plan 16 起，`serve install` 用 [`github.com/kardianos/service`](https://github.com/kardianos/service) **跨平台注册**系统服务：

| 平台 | 注册成 | 默认 service 账户 |
|---|---|---|
| Windows | Windows Service（`Automatic` 启动 + `OnFailure=restart`） | `LocalSystem` |
| Linux | systemd unit（`Restart=on-failure`，`WantedBy=multi-user.target`） | root |
| macOS | launchd plist（`RunAtLoad` + `KeepAlive`） | root（sudo 跑） |

```bash
# 在已经跑过 unlock 的机器上（admin / root）：
ssh-manager serve install --addr 0.0.0.0:7878 --tls-cert cert.pem --tls-key key.pem
ssh-manager serve status        # 四信号：service / process / http / vault
ssh-manager serve uninstall     # 停 service + 注销（不删 vault 数据）
```

`serve install` 会：

1. **precheck**：`master.key.plain` 存在且可读（service 账户需能读）——不存在就报错让你先 `unlock`。
2. 解析当前二进制路径（`os.Executable`）→ service 配置里写"跑这个二进制 + `serve --addr ...` 参数"。
3. 加固 vault 目录 ACL（Windows，best-effort；文件 ACL 已由 `unlock` 设好）。
4. 注册 + 立即启动。重装是幂等的（先注销旧的再装新的）。

**完整的多机部署流程**（profile / project / TLS / 网络隔离 / 离线缓存）见 [multi-machine.md](./multi-machine.md)。**威胁模型**（service 账户 = root/LocalSystem → R3）见 [threat-model.md](./threat-model.md)。

### 第三方服务包（可选，给不想用内置 install 的进阶用户）

如果你偏好用第三方服务包装器而非内置的 `serve install`（例如已有统一的服务管理工具链），手动包也行——只要包装的是同一条 `ssh-manager serve --addr ...` 命令：

- **Windows（NSSM）**：
  ```powershell
  nssm install ssh-manager-serve "C:\path\to\ssh-manager.exe" "serve" "--addr" "0.0.0.0:7878" "--tls-cert" "cert.pem" "--tls-key" "key.pem"
  nssm set ssh-manager-serve AppDirectory "C:\path\to"
  nssm set ssh-manager-serve AppStdout "C:\ProgramData\ssh-manager\serve.log"
  nssm set ssh-manager-serve AppStderr "C:\ProgramData\ssh-manager\serve.log"
  nssm start ssh-manager-serve
  ```
  注意：NSSM 默认以 `LocalSystem` 跑（与内置 install 一致）；`master.key.plain` 的 ACL 必须让 `LocalSystem` 可读（默认如此）。

- **Linux（systemd 手动 unit）**：
  ```ini
  # /etc/systemd/system/ssh-manager-serve.service
  [Unit]
  Description=ssh-manager MCP server (serve mode)
  After=network.target

  [Service]
  ExecStart=/usr/local/bin/ssh-manager serve --addr 0.0.0.0:7878 \
            --tls-cert /etc/ssh-manager/cert.pem --tls-key /etc/ssh-manager/key.pem
  Restart=on-failure
  User=root   # 或专用 service 账户，但需能读 /var/lib/ssh-manager/master.key.plain

  [Install]
  WantedBy=multi-user.target
  ```
  ```bash
  systemctl daemon-reload && systemctl enable --now ssh-manager-serve
  ```

- **macOS（launchd 手动 plist）**：
  ```xml
  <?xml version="1.0" encoding="UTF-8"?>
  <!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
  <plist version="1.0">
  <dict>
      <key>Label</key>
      <string>com.ssh-manager.serve</string>
      <key>ProgramArguments</key>
      <array>
          <string>/usr/local/bin/ssh-manager</string>
          <string>serve</string>
          <string>--addr</string>
          <string>0.0.0.0:7878</string>
          <string>--tls-cert</string>
          <string>/etc/ssh-manager/cert.pem</string>
          <string>--tls-key</string>
          <string>/etc/ssh-manager/key.pem</string>
      </array>
      <key>RunAtLoad</key>
      <true/>
      <key>KeepAlive</key>
      <true/>
  </dict>
  </plist>
  ```
  ```bash
  sudo launchctl load -w /Library/LaunchDaemons/com.ssh-manager.serve.plist
  ```

> **内置 `serve install` vs 第三方包**：内置路径是可被 CI / 脚本自动驱动、且在本程序掌控内的部署路径（kardianos 收敛三平台）；第三方包留给已有一致运维工具链的进阶用户。两者跑的是同一个二进制同一条命令，没有功能差异。

---

## 常见坑（首跑高频）

| 现象 | 原因 / 处理 |
|---|---|
| `vault locked: run ssh-manager unlock` | 没跑过 `unlock`（`master.key.plain` 还没生成）；或非特权进程建不了固定路径目录（Windows 需 admin / Unix 需 root 先 `serve install` 建目录）。 |
| `harden ACL on master key ...: needs admin`（Windows） | 首次写 `master.key.plain` 时设 ACL 失败——用 admin 跑 `unlock` 或 `serve install`（ACL 是 L1+ 唯一保护层，必须能设）。 |
| `master key not found: run 'ssh-manager unlock' in an interactive session first`（`serve install`） | 还没 `unlock` 就装 service。先 admin 跑 `unlock` 生成 master.key，再 `serve install`。 |
| agent 报 `server is not in your profile` | 你让它操作的 server 不在它 project 绑定的 profile 里。用 `ssh-manager projects show <name>` 核对。 |
| agent 看不到 sudo / `has_sudo=false` | 加服务器时没给 `--sudo-password`。用 `ssh-manager servers edit <name> --sudo-password '...'` 补上。 |
| 首次连接弹 host key 确认？ | broker 用 **TOFU**（trust on first use）：第一次连一台机器会自动记下它的 host key，之后变了会拒绝（防中间人）。换机器/IP 后需要清旧记录（见 [managing-servers.md](./managing-servers.md#host-key-与-tofu)）。 |
| Windows 上想替换 `ssh-manager.exe` 报"文件被占用" | serve 常驻进程锁着二进制。先 `ssh-manager serve uninstall`（停 service + 杀 serve）→ 替换 exe → 再 `serve install`。见 [multi-machine.md](./multi-machine.md)。 |

下一步：去 [managing-servers.md](./managing-servers.md) 学完整的增删改查，去 [agent-access.md](./agent-access.md) 学授权与吊销，去 [scenarios.md](./scenarios.md) 看真实场景。**多机共享 vault / Windows 上 `serve` 常驻开机自起 / 备份迁移**——见 [multi-machine.md](./multi-machine.md) 和 [backup-restore.md](./backup-restore.md)。
