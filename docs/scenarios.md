# 应用场景与示例

这篇用一组真实场景说明：**接好之后，你可以让 AI agent 帮你做什么**。每个场景给出“你想要什么 → 你怎么对 agent 说 → agent 会怎么用工具 → 要点”。

> 前提：你已经按 [getting-started.md](./getting-started.md) 装好、解锁、加了服务器、建了 profile + project，并把 `.mcp.json` 配进了 Claude Code。

---

## 怎么读这些示例

- agent 拿到的 10 个工具是：`list_servers` / `exec_command` / `download_file` / `upload_file` / `upload_content` / `forward_port` / `close_port` / `exec_background` / `exec_output` / `exec_stop`（长活命令走后台三件套；详见根 [README](../README.md#what-the-agent-gets-the-mcp-tools)）。
- **你不需要记工具名**。你用自然语言说目标，agent 自己会先 `list_servers` 拿到真实的 server `id`，再用 `id` 调后续工具。下面“agent 会怎么用”只是让你知道它背后在干嘛。
- 所有示例都假设 agent 绑定的 profile 里有名为 `gpu` / `db` / `web` 等的服务器——把名字换成你自己的。

---

## 场景 1：GPU 机器巡检 + 装包

**你想要**：上班第一件事，让 agent 告诉你 GPU 机器的健康状况，顺手装个 `htop`。

**你怎么说**：
> 看看 gpu 这台机器的显卡利用率、磁盘和内存，然后装一下 htop。

**agent 会怎么用**：
1. `list_servers` → 拿到 `gpu` 的 id 和 `has_sudo`。
2. `exec_command`（id=…，command=`nvidia-smi; df -h; free -h`）→ 一把抓状态（多条命令用 `;` / `&&` 串）。
3. 装包需要 root：`exec_command`（command=`apt-get install -y htop`，**sudo=true**）→ broker 用 `sudo -S` 跑。

**要点**：
- 一次想跑多步？让命令自己串（`cmd1 && cmd2 && cmd3`）。**不支持交互式 shell**（没有 `ssh -t`），所以 `apt install` 这类会问交互问题的命令，要让它走非交互（`-y`）或 sudo。
- agent **不会**自己拼 `sudo` 前缀——它传 `sudo=true`，broker 处理。你提醒它“用 sudo”即可。
- 前提是加这台 server 时给了 `--sudo-password`（否则 `has_sudo=false`，sudo 调用会失败）。

---

## 场景 2：读 root-only 的日志（排障）

**你想要**：服务报错，日志在 `/var/log/myapp/app.log`，只有 root 能读。让 agent 把最近的报错捞出来分析。

**你怎么说**：
> gpu 上 `/var/log/myapp/app.log` 是 root 才能读的，帮我看最近 200 行里有哪些 ERROR，给结论。

**agent 会怎么用**：
- `exec_command`（command=`tail -n 200 /var/log/myapp/app.log | grep ERROR`，**sudo=true**）。

**要点**：
- root 文件靠 `sudo=true` 读；broker 用你存的 sudo 密码走 `sudo -S`。
- 大日志别一次全拉：输出每通道 1 MiB 封顶。让 agent **先 `grep` / `tail` / `head` 精确切片**再读。如果某次返回带 `truncated=true`，agent 会看到真实字节数（`stdout_bytes`），应换个更窄的命令重跑，而不是傻乎乎再拉一遍全部。
- 要看完整大文件 → 见场景 5。

---

## 场景 3：批量部署 / 推配置（上传）

**你想要**：本地改好了一组配置文件和部署脚本，推到 web 机器的 `/etc/myapp/`。

**你怎么说**：
> 把我本地 `./deploy/myapp/` 这个目录推到 web 的 `/etc/myapp/`，然后跑里面的 `install.sh`。

**agent 会怎么用**：
1. `upload_file`（local_path=`/abs/path/deploy/myapp/`，remote_path=`/etc/myapp/`）→ 整个目录递归上传（SFTP，保留相对路径，父目录不存在会自动建）。
2. `exec_command`（command=`bash /etc/myapp/install.sh`，sudo=true 如果需要）。

**要点**：
- `upload_file` 的方向：**LocalPath 在你机器上，RemotePath 在服务器上**——别反了。
- 上传是 SFTP，**不走 sudo**（SFTP 协议层没有 sudo 概念）。要写到 root 才能写的路径，先上传到一个可写目录（如 `/tmp/myapp-deploy/`），再用 `exec_command` + `sudo=true` 把它 `mv` / `install` 到目标位置。
- 上传 cap：**单个文件超过 1 MiB 会在传输前被直接拒绝**（错误里带文件名/实际大小/上限，零字节传输；目录上传时此前已完成的文件照常保留）；多个文件累计超过 1 MiB 时已完成的保留并如实标 `truncated=true`（其后的文件不再上传）→ 拆小批次重传。
- 符号链接三态（传目录时）：**根**是符号链接/junction → 跟链解析成目标目录再传；**嵌套的 symlink→目录**（含 Windows junction）→ **显式拒绝**，错误形如 `symlinked directory not uploaded: <路径> — upload the target directory directly (following directory links recursively is not supported)`（要传就直接传目标目录，不递归跟链——环/重复访问风险）；**嵌套的 symlink→文件** → 跟链上传目标内容（cap 按目标大小判，Plan 24）。
- 上传的是你本机（broker 所在机器）上的文件——agent 在你机器上读文件再推过去。
- **内容在 agent 手里、不在任何机器磁盘上？用 `upload_content`**：内容直接内联进工具参数（JSON 入参）写成远程单文件（≤8 MiB，父目录自动建、已存在即覆盖；二进制走 base64）。这是**跨机形态的关键路径**：远程 serve 拓扑（笔记本 agent → serve 主机 broker → 目标机）下，`upload_file` 读的是 **serve 主机**的文件系统，笔记本上的文件它够不到——agent 自己生成的配置/脚本/小产物直接 `upload_content` 推过去（agent 侧详见 [agent-tools.md](./agent-tools.md) upload_content 节）。单文件小配置首选它；整目录、大文件仍走 `upload_file`。

---

## 场景 4：端口转发——本地连服务器上的数据库 / 内网服务

**你想要**：db 机器上跑着 PostgreSQL（`127.0.0.1:5432`），只对本机开放。你想用本地的 DBeaver / `psql` 连它，但不想开外网端口。

**你怎么说**：
> 给我开个本地端口，转发到 db 上 `127.0.0.1:5432` 的 Postgres，告诉我用哪个端口连。

**agent 会怎么用**：
1. `forward_port`（server=db 的 id，remote_host=`127.0.0.1`，remote_port=`5432`）→ 返回 `tunnel_id` 和一个本地 `local_port`（你不指定就自动选空闲端口）。
2. 告诉你：“在 `127.0.0.1:<local_port>` 连即可。”

然后你本地：
```bash
psql -h 127.0.0.1 -p <local_port> -U myuser mydb
```
或把 DBeaver 指向 `127.0.0.1:<local_port>`。

**用完关掉**（**你应该让 agent 主动关，别干等**）：
> 用完了，把刚才那个转发关掉。
- agent 调 `close_port`（tunnel_id=…）→ 关掉本地监听 + 背后的 SSH 连接。

**要点**：
- `remote_host` 是**从服务器视角看**的目标——通常就是 `127.0.0.1`（服务器自己的回环上的服务）。也能转发到服务器**能访问到的**内网其它机器（`remote_host` 填那台的内网 IP）。
- `forward_port` = `ssh -L`（本地转发）。**不支持** `-R`（远程）/ `-D`（动态 SOCKS）。
- 隧道会占住一条 SSH 连接，**创建约 10 分钟后被后台自动回收**（按创建时间计，**不看活动量**——持续有流量也会回收）；你或 agent 也可以随时 `close_port` 主动关。
- 隧道状态在 broker 进程内——agent / Claude Code 重启后隧道就没了，需要重开。

---

## 场景 5：拉日志 / 大文件回来看

**你想要**：把 web 上的访问日志拉下来本地分析，但文件几百 MB。

**你怎么说**：
> web 上 `/var/log/nginx/access.log` 很大，不用全拉，给我最近 1000 行里 5xx 的记录。

**agent 会怎么用**（两种武器，按场景选）：
- **精确切片优先**：`exec_command`（command=`tail -n 1000 /var/log/nginx/access.log | grep ' 5'`，sudo=true 如果需要）。
- **要整段小文件**：`download_file`（path=`/var/log/nginx/access.log`）→ 返回内容（≤1 MiB 直接给全；超过则 `truncated=true` + 真实 `bytes`，提示 agent 换策略）。

**要点**：
- `download_file` 是**单文件**，**不支持递归下目录**。要拉一整个目录树：让它先 `exec_command` 在远端 `tar czf /tmp/x.tgz <dir>`，再 `download_file` 那个 tar。
- 大文件的标准套路：`download_file` 适合“小且要看全貌”；大文件用 `exec_command` 的 `tail/head/grep/wc` 切片更省 token 和带宽。
- `download_file` 返回的是**文本内容**（给 agent 读 / 分析的），不是落到你磁盘上的文件。要让文件落盘，用 `exec_command` + `cat` 重定向不行（那是远端）；落盘得在 agent 所在的本机做（agent 用它本机的写文件能力）。

---

## 场景 6：多环境隔离（dev / staging / prod）

**你想要**：一个 agent 管 dev（随便折腾），另一个 agent 管 prod（只读巡检），互不串台。

**怎么做**（编排见 [agent-access.md](./agent-access.md#典型编排)）：
1. 建三个 profile，各自 grant 对应环境的 server：
   ```bash
   ssh-manager profiles add dev  && ssh-manager profiles grant dev dev-web dev-db
   ssh-manager profiles add prod && ssh-manager profiles grant prod prod-web prod-db
   ```
2. 建**两个** project，各绑一个 profile，得到**两个不同** token：
   ```bash
   ssh-manager projects add dev-agent  --profile dev
   ssh-manager projects add prod-agent --profile prod
   ```
3. 两份 `.mcp.json`（分别给两个 Claude Code 工作区 / 两个客户端）。
4. prod 想“只读”？在 prod 工作区的 `CLAUDE.md` / 系统提示里写明“只允许 `exec_command` 跑只读命令（如 `cat / ls / ps / df / nvidia-smi`），禁止任何写操作”，配合**不给 sudo 密码**（`has_sudo=false`）来收敛破坏面。

**要点**：隔离是**靠 profile**，不是靠 prompt。即使 prod agent 被提示注入，它物理上也碰不到 dev profile 之外的机器（跨 profile 访问被 broker 拒绝，已对抗测试）。prompt 约束是第二道防线，profile 隔离是第一道。

---

## 场景 7：token 泄露 / 离职处置

**情况 A：你不小心把 `.mcp.json`（含 token）提交到了公开仓库。**

```bash
ssh-manager projects rotate dev-agent      # 旧 token 立刻失效，打印新 token + 新 .mcp.json
# 1. 把新 .mcp.json 配回客户端
# 2. 从 git 历史里清掉旧 token（git filter-repo / BFG），强制推送
# 3. （可选）审计：这段时间这台机器有没有异常操作
```
`rotate` 保持 project id 和 profile 不变，**只换 token**——服务器和授权关系都不用动。

**情况 B：某 agent 用完了 / 实习结束了，彻底收回。**

```bash
ssh-manager projects revoke intern-agent   # token 永久失效；默认从 ls 隐藏
# 审计记录保留（软删除）。
# serve 模式下一请求即拒；stdio 会话重启客户端；隧道见 agent-access「断连语义（四层）」。
```

**情况 C：临时暂停（放假 / 审查）。**

```bash
ssh-manager projects disable contractor-agent   # token 被拒
# ... 审查完毕 ...
ssh-manager projects enable  contractor-agent   # 恢复，同一张 token 重新有效
```

> 断连语义分四层（stdio=下次重连；serve=逐请求即拒；既有隧道不受 revoke 影响且只能重启 broker/等创建后 ~10 分钟回收；离线 cache 须轮换凭据），详见 [agent-access.md](./agent-access.md) 的「断连语义（四层）」一节。

---

## 场景 8：你自己直连（不走 agent）

有时候你不想经过 agent，想直接在服务器上跑命令。owner CLI 提供了**不受 profile 限制、输出不封顶**的直达通道：

```bash
ssh-manager ssh gpu nvidia-smi          # 在 gpu 上跑一条命令，输出原样回来
```

**要点**：
- `ssh-manager ssh <name> <command...>` = 用库里存的凭据，直接在命名机器上跑命令，**不受任何 profile 限制**（你是 owner，全权）。输出不封顶（和 agent 路径的 1 MiB 封顶不同）。单命令（连接+执行共享 120s 超时）。
- 这条命令**也不是交互式 shell**：后面的 `<command...>` 是要跑的命令（空格分隔会被拼成一行；**不带命令 / 空命令会显式报错**）。它解决的是“owner 用 broker 里存的凭据直接跑一条命令”，不是给你开个 `ssh -t` 终端。要交互式终端，用你自己的 ssh 客户端（凭据需自行已有或另行配置——它们可能只存在本 vault 里）。
- 连接+执行**共享 120 秒超时**；输出不封顶；**远端非零退出会让本命令以非零码退出**（码值不透传，见 stderr 错误消息；脚本里判断非零即可）。
- 这条路同样写审计（`action=exec`）。

> **权威机实测记录**（2026-08-17，NUC10 @ v0.7.3，目标 `ai_runner`）：`ssh ai_runner echo owner-smoke-ok-20260817` → exit 0、stdout 正确——单命令路径端到端可用。同日负例：远端 `false`（退出码 1）与**无命令**形态在该版本下均 exit 0 静默通过（退出码被吞 / 空命令照跑——两者均已在 master 修复、待发布；升级后按上文要点复测）。
>
> **复测记录**（2026-08-17，NUC10 升级 v0.8.0 后，目标同）：echo → exit 0 ✓；远端 `false` → **CLI exit 1 + stderr `remote command exited with code 1`** ✓；无命令 → **exit 1 + `no command given` 显式报错** ✓——v0.7.3 记录的两个缺陷在 v0.8.0 上全部兑现修复。

---

## 能力边界（故意不做）

| 想做 | 现状 | 替代 |
|---|---|---|
| 交互式 shell（`ssh -t`） | ❌ 不支持 | 用你自己的 ssh 客户端；agent 这边用 `&&` / `;` 串命令 |
| 递归下载整个目录 | ❌ `download_file` 只单文件 | 远端 `tar` 后下载 tar |
| 远程转发 `-R` / 动态 `-D` | ❌ 只支持本地 `-L` | — |
| 跑超 5 分钟的长命令 | ✅ 前台 `exec_command` 5min 硬顶；长活走 `exec_background`（24h 上限）+ `exec_output` 增量轮询 + `exec_stop` | 仍可用 `ssh-manager ssh`（120s）+ `nohup` |
| 单次输出 > 1 MiB | ⚠️ 截断（标 `truncated`） | 切片（`head/tail/grep`）分多次 |

---

## 一句话总结

- **跑命令** → `exec_command`（要 root 就 `sudo=true`，别自己拼 sudo）。
- **看文件** → 小文件 `download_file`，大文件 / 目录用 `exec_command` 切片或 `tar`。
- **推文件** → `upload_file`（目录递归；写 root 路径先传 `/tmp` 再 sudo 移）。
- **连内网服务** → `forward_port` 拿本地端口，用完 `close_port`。
- **隔离多 agent** → 不同 profile + 不同 project。
- **出事了** → rotate（换卡）/ disable（暂停）/ revoke（吊销）——serve 模式下一请求即拒；stdio 会话重启客户端接管；离线缓存场景须轮换服务器凭据（见 agent-access「断连语义（四层）」）。
- **你自己用** → `ssh-manager ssh <name> <cmd>`，全权直达。

需要更细的命令参数？看 [managing-servers.md](./managing-servers.md)。授权细节？看 [agent-access.md](./agent-access.md)。
