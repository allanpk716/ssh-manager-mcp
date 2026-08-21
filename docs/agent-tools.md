# 给 AI agent 的 SSH 工具使用手册（docs/agent-tools.md）

> **你是谁**：你是一个拿到 ssh MCP 工具的 AI agent（Claude Code / Cursor / 任何
> MCP 客户端）。这份手册教你安全高效地用它们。

## 铁律

1. **MCP 工具是唯一授权入口。** 所有远程操作——执行命令、传文件、端口转发——
   一律走 `mcp__ssh__*` 工具。这是架构强制，不是建议。
2. **禁止裸 `ssh` / `scp` / 从文件系统找私钥。** 那是旁路：本机 `~/.ssh` 里通常没有
   可用凭据，直连必失败。broker 启动时若检测到散落的 SSH 凭据文件，会向 stderr
   打 `WARNING: ssh credential files detected` 告警（提示 owner 清掉，防止绕过）。
3. **你永远拿不到任何密码/私钥字节——这是设计。** 凭据只活在 broker 的加密
   vault 里；broker 自己开 SSH 连接，只把命令输出 / 文件内容 / 转发端口返回给你。
   任何工具结果里都不会出现凭据。不要试图索取、解码或猜测它们。

为什么敢这么设计、owner 侧如何配授权，见 [agent-access.md](./agent-access.md)
「安全模型回顾（铁律）」与 [../README.md](../README.md) "The security model"。

## 标准工作流

1. **先 `list_servers`。** 拿到你能用的服务器清单和**真实 id**——
   **name ≠ id**：`exec_command` / `download_file` / `upload_file` /
   `forward_port` 的 `server_id` 参数一律填 id（工具描述原话："Pass the
   server's id (from list_servers), not its name"）。凭记忆或名字猜 id 会吃到
   `server is not in your profile` 拒绝。
2. **动手前读该机的 owner 上下文。** `list_servers` 的每行带 owner 给的操作
   须知：`caveats`（**特殊处理规则，动手前必读**）、`role`（这台机器是干嘛的）、
   `services`（上面部署了什么）、`location` / `hardware` / `tags` / `description`。
   忽略 caveats 是踩坑第一原因。
3. **核对 `has_sudo` 再提权。** `has_sudo=false` 的机器别尝试提权——
   `sudo=true` 会直接报 `sudo not configured for server ...`。空字段 = 无。
4. **用完有状态的资源要还。** `forward_port` 开的隧道用完 `close_port`；
   长任务别霸占连接（见 exec_command 的超时与截断）。

## 逐工具语义

### list_servers

列出你（这个 project token）被授权用的服务器。**每个会话第一步**。

返回字段全解（`servers` 数组，每行一个服务器）：

| 字段 | 含义 |
|---|---|
| `id` | 稳定 server id——**跨工具引用一律用它**，不是 name |
| `name` | 人类可读名字（仅供你识别） |
| `host` / `user` | SSH 用户；host 默认是 `"hidden"`（owner 未披露，用 id 寻址；owner 逐台放开后才是明文） |
| `has_sudo` | `true` 才支持 `sudo=true`（= owner 给这台机配了 sudo 凭据） |
| `role` | 这台机器的用途（如 "prod pg primary"） |
| `services` | 上面部署/运行着什么 |
| `caveats` | **操作须知 & 特殊处理规则——动手前读**；空 = 无 |
| `location` / `hardware` | 部署位置 / 硬件配置 |
| `tags` / `description` | 自由标签 / owner 补充说明（优先看上面的结构化字段） |

**永不含凭据**——任何字段都不出现密码/私钥。空字符串/空数组的字段表示"没有"，
不是错误。哪些 server 会出现在你的清单里、owner 怎么改，见
[agent-access.md](./agent-access.md)；背后的概念模型见 [concepts.md](./concepts.md)。

### exec_command

在服务器上跑一条 shell 命令。

- **`sudo=true` 让 broker 替你跑 `sudo -S`**——broker 把存储的 sudo 密码写进
  远端 sudo 的 stdin，你**别自己在 command 里拼 `sudo` 前缀**（会变成
  `sudo -S -p '' -- sudo ...`，密码喂不进去）。前提 `has_sudo=true`。
- **超时只作用于命令执行阶段**：`timeout_seconds` 缺省 = **120 秒**；硬上限
  **5 分钟**（超限值被**静默钳到 5 分钟**，不报错）。连接建立不受这个参数
  约束（由 MCP 工具调用本身的上下文管理）。**长任务拆步**（分页/分文件），
  或一条 `nohup ... &` 后台化后用短命令轮询结果。
- **超时不是错误**：到点后命令被杀，你收到正常结果对象，`timed_out=true` +
  已产生的部分输出——据此判断是拆步还是后台化。非零退出码同理（`exit_code`
  承载它，不是工具报错）。
- **输出每通道（stdout / stderr 各自）1 MiB 封顶**。超出时 `truncated=true`，
  你拿到的只是**前缀**；`stdout_bytes` / `stderr_bytes` 是**真实总字节数**。
  此时要 **refine**——改跑 `tail -n 200 <file>`、`head -n`、`grep <pattern>`
  拿你要的那段，**不要**原样重跑硬拉全量（会再吃 1 MiB 前缀，白费）。

### download_file

从服务器**下载单个文件**读内容。参数：`server_id` + 远端**绝对路径**。

- 内容封顶 **1 MiB**：文件更大时 `truncated=true`，`content` 是前缀，`bytes`
  是真实总大小。
- `truncated=true` 时 **refine**：改用 `exec_command` 跑 `head -c` / `tail -c`
  / `sed -n 'N,Mp'` 取你要的字节段，别反复整拉。
- **只收单个文件，不收目录。** 要目录树：先 `exec_command` 打
  `tar czf /tmp/x.tgz <dir>`，再 download 那个 tgz（注意 1 MiB 帽）。
- **禁止编造文件内容**——工具描述原话 "do NOT fabricate file contents"。拿不到
  就说拿不到。

### upload_file

把**本地**文件**或目录**推到服务器（方向 matters：`local_path` 是 **broker 所在
机器**上的绝对路径——stdio 本机部署就是你的机器；远程 serve 部署是 serve 主机。
`remote_path` 是服务器上的绝对 POSIX 路径）。

- 目录递归上传，保留相对路径（`scp -r` 语义）；`remote_path` 的**父目录不存在
  会自动创建**。
- **preflight 拒绝条件（实写）**：
  - **单个文件严格大于 1 MiB（`> cap`，恰好等于允许）→ 传输前直接拒绝**，
    零字节移动、远端不创建文件；错误信息自带 文件路径/实际大小/上限，
    据此拆分或压缩后重试。目录上传中撞上这种文件：之前已完成的文件保留，
    错误在此处中止。
  - **符号链接指向目录 → 拒绝**（错误提示 "upload the target directory
    directly"）；指向文件的链接按目标文件的大小过同样的 per-file 帽。
  - **累计总量跨过 1 MiB（每个文件都在帽内）→ 不报错**，`truncated=true`：
    已完成的文件保留，正在传的文件完整落盘，**之后的文件不再上传**——
    用更小的载荷分批重试。
- SFTP 通道，**sudo 不适用**。

### forward_port

开一条本地端口转发（**只支持 `ssh -L` 语义**——本地监听、经服务器转发；没有
`-R`/`-D`）。用于够到**跑在服务器上（或服务器能访问到）的服务**：数据库、
web UI、metrics 端口。

- 参数：`server_id`（SSH 入口机）+ `remote_host` / `remote_port`——
  **从服务器视角**写转发目标（够服务器自己的回环服务就填 `127.0.0.1` + 该
  服务端口）；`local_port` 可省（broker 挑空闲口）。
- 返回 `tunnel_id` + `local_port`：转发**监听在 broker 所在机器的
  `127.0.0.1:<local_port>`**——stdio 本机部署就是你脚下的机器，直接
  `curl http://127.0.0.1:<local_port>`；远程 serve 部署则是 **serve 主机**上
  的 127.0.0.1，你从别的机器够不到它。
- 这是唯一**有状态**的工具：broker 会为隧道**全程持有一条 SSH 连接**。
  创建后 **~10 分钟自动回收**（按创建时间算、**不看流量**——持续在用的隧道
  到点也收；扫描周期 1 分钟，实际 10–11 分钟内收）。**用完主动 `close_port`**，
  别等回收。

### close_port

按 `tunnel_id` 关闭 forward_port 开的隧道——拆掉本地监听**和**背后那条 SSH
连接（broker 一直替你持有，这是释放资源的唯一途径）。不需要 server_id：
tunnel_id 是绑定在 broker 进程上的不透明句柄。

- 成功返回 `closed`；id 未知（已关过/已被 ~10 分钟回收器收走/从未开过）报
  `no open tunnel with id <id>`——**正常现象**，需要就重开 forward_port。
- **serve 模式下 401 = token 已失效**（被 owner 轮换/禁用/吊销）：HTTP 中间件
  在请求到达工具层之前就拒了，**任何**后续工具调用都会 401——报告 owner，
  **别**重试开新隧道。注意：**已经开着的隧道在 token 吊销后仍会继续转发**
  （broker 不会级联拆它），但你已经管不了它了。

## 错误对照表（每条给"你该做什么"）

| 报错 | 含义 | 你该做 |
|---|---|---|
| `invalid or unknown token`（stdio：broker 起不来）；serve：任意调用 HTTP 401 | token 错了 / 被 owner `rotate` 换发 / project 被 disable/revoke | **报告 owner**；别反复重试。owner 会核 status、必要时换发并更新 `.mcp.json`（见 [agent-access.md](./agent-access.md)「Project 生命周期」） |
| `server is not in your profile — call list_servers ...` | id 不在授权清单（用错 id、拿 name 当 id、或它不在你 profile） | **重新 `list_servers` 核对 id**；还不在就是 owner 没授权，报告 owner，别试别的 id |
| `server has no credential configured (set one with: ...)`（`no_credential`） | owner 建了服务器但没配登录凭据——连接前就被拒 | **报告 owner** 按错误里的提示配凭据；重试无意义 |
| 结果里 `timed_out: true`（不是报错） | 命令超过 timeout（默认 120s）被杀 | **拆小命令**：分页/分文件；真要长跑就 `nohup ... &` 后台化 + 短命令轮询 |
| 结果里 `truncated: true`（不是报错） | 输出超每通道 1 MiB（download 是文件超 1 MiB），你只有前缀 | **refine**：`tail -n` / `head -n` / `grep` 取目标段重跑；看 `*_bytes` 判断真实体量；别硬拉全量 |
| `host key mismatch: possible MITM, connection rejected` | 服务器 host key 和首次记录的不一致——可能是中间人（TOFU fail-closed） | **报告 owner 核实**，**绝对别**尝试绕过（没有任何"跳过检查"参数） |
| `ssh dial: connect failed: connection refused`（等分类短语） | 连不上目标机（地址细节已按可见性边界清洗） | 核对 server_id 是否正确；网络问题报告 owner |
| `sudo not configured for server <name> (call list_servers: has_sudo tells you)` | `sudo=true` 但该机 `has_sudo=false` | 回 `list_servers` 核对；确实需要提权就报告 owner 配 sudo 凭据 |
| `no open tunnel with id <id>` | 隧道已关/已被 ~10 分钟自动回收/从未开过 | 正常现象；需要转发就重开 `forward_port` |
| `file <path> (<N> bytes) exceeds upload cap <cap> — refused before transfer` | 单文件严格大于 1 MiB，传输前被拒（零字节移动） | 按错误里的 size/cap **拆分或压缩**后重传 |
| `store is read-only (offline cache); connect to the server to mutate`（ErrReadOnly） | broker 跑在离线缓存模式（见三态环境） | **报告 owner** 切回在线/本机模式；**别**重试写操作（见下） |

## 三态环境（你通常无需分辨）

broker 有三种部署形态，**工具面完全一致**（同 6 个工具、同 profile 隔离、同审计），
差别只在可写性：

| 形态 | 什么样 | 可写性 |
|---|---|---|
| 单机 / stdio | broker 跑在你脚下的机器（`.mcp.json` 里 `command: ssh-manager`） | **可写** |
| 在线 serve | broker 跑在远程 VLAN 主机（http + token，见 [multi-machine.md](./multi-machine.md)） | **可写** |
| 离线 cache | broker 从本地快照服务（`--cache`；见 [quickstart-multi-machine.md](./quickstart-multi-machine.md)） | **只读** |

在线时你几乎感觉不到差异（forward_port 的 `127.0.0.1:<port>` 落在哪台机器除外，
见上）。离线 cache 模式下**一切写操作被拒**（`ErrReadOnly`）；你最可能撞上的具体
形态是**首次连接一台缓存里没有 host key 记录的服务器**——TOFU 想记录新 key 但
store 只读，拒绝并包着 ErrReadOnly。遇到任何 `read-only` 字样的报错：
**报告 owner 切在线/本机，别重试写操作**——重试一万次也是同样的错。

## 附录：贴进你项目的规则模板（CLAUDE.md / AGENTS.md）

把下面这段贴进你所在项目的 `CLAUDE.md` / `AGENTS.md`，让每个会话的你自动带着
这套纪律：

```text
# SSH 访问铁律
所有远程服务器操作（执行命令/传文件/端口转发）一律用 mcp__ssh__* 工具，
禁止裸 ssh/scp/寻找私钥（本机没有可用凭据，直连必失败）。
- 先 list_servers 拿真实 id（name ≠ id），动手前读目标机的 caveats/role。
- 提权用 sudo=true 参数，不要自己拼 sudo 前缀。
- 工具报错先查 docs/agent-tools.md 错误对照表；read-only 报错=离线缓存，
  报告 owner，不要重试写操作。
（按需替换工具前缀 mcp__ssh__* 为你的客户端实际命名。）
```

## 行为依据（事实核对表）

本手册每条数值/行为断言的代码锚点（以代码为准核对于成文当日；行号为当前
HEAD，后续重构以符号名为准）：

| 断言 | 锚点 |
|---|---|
| exec 默认超时 120s（`defaultTimeout = 120 * time.Second`；schema 提示 "defaults to 120"） | `internal/mcpserver/types.go:103`；`internal/mcpserver/server.go:180` |
| 超时硬上限 5 分钟、超限**静默钳制**不报错（`clampExecTimeout`） | `internal/mcpserver/types.go:113-118`；`internal/mcpserver/core.go:19-27` |
| 超时只作用于**执行**：Connect 用工具调用 ctx（dial 不可中断，取消即弃）；clamp 在 Connect 之后、只传给 Exec/ExecSudo | `internal/mcpserver/core.go:131,146,161,163`；`internal/sshbroker/client.go:15-22` |
| 超时是 result 不是 error（`timed_out=true` + 部分输出）；非零退出码同理 | `internal/sshbroker/exec.go:74-77,81-84` |
| 输出每通道 1 MiB 封顶（`MaxOutputBytes = 1 << 20`）、前缀 + truncated + 真实总字节 | `internal/mcpserver/types.go:105-111,30-32`；`internal/sshbroker/exec.go:26-28` |
| download 同 1 MiB 帽：truncated=true 给前缀、bytes=真实大小；建议 exec head/tail refine | `internal/mcpserver/core.go:272`；`internal/mcpserver/types.go:43-45`；`internal/mcpserver/server.go:97` |
| `sudo=true` → broker 跑 `sudo -S -p '' -- <cmd>`、密码写 stdin；**不要自己拼 sudo 前缀** | `internal/sshbroker/sudo.go:25-30,46`；`internal/mcpserver/server.go:179`；`internal/mcpserver/core.go:70` |
| `has_sudo` = owner 配了 sudo 凭据（`SudoCredentialID != ""`）；false 时 sudo=true 报 `sudo not configured for server ...` | `internal/mcpserver/core.go:56,150-153`；`internal/mcpserver/types.go:14` |
| name ≠ id：工具描述 "Pass the server's id (from list_servers), not its name" | `internal/mcpserver/server.go:78,177` |
| list_servers 字段清单（id/name/host（默认 `"hidden"`，owner 逐台 opt-in）/user/has_sudo/role/services/caveats/location/hardware/tags/description）永不含凭据；caveats "READ BEFORE acting" | `internal/mcpserver/types.go:5-22`；`internal/mcpserver/server.go:63` |
| 空字段=无；tags 空数组非 null（schema 一致性） | `internal/mcpserver/core.go:41-50` |
| out-of-profile 拒绝文案 + 四工具同一道门（连接前先 gate） | `internal/mcpserver/types.go:95`；`internal/mcpserver/core.go:97-101,223-227,332-335,455-458` |
| `no_credential`：连接前拒绝，错误自带配置提示（`vault.ErrNoCredential`） | `internal/vault/vault.go:112-121`；`internal/mcpserver/core.go:112-117` |
| host key 失配 fail-closed（TOFU 首连记录、之后必须一致；错误文案） | `internal/sshbroker/hostkey.go:12-15,32-40` |
| upload 单文件**严格** >1 MiB 传输前拒绝（零字节、错误含 file/size/cap；== cap 放行）；目录内同样 gate | `internal/sshbroker/upload.go:66-72,228-229,95-97`；`internal/mcpserver/core_test.go:580-613` |
| upload 累计跨 cap → truncated=true、已完成保留、in-flight 完整落盘、后续不再传 | `internal/sshbroker/upload.go:38-40,190-191,233-235,270-282`；`internal/mcpserver/types.go:60` |
| upload symlink→目录拒绝（"upload the target directory directly"）；→文件按目标大小过帽 | `internal/sshbroker/upload.go:217-227` |
| upload 目录递归 + 远端父目录自动创建（MkdirAll parent） | `internal/mcpserver/types.go:52-53`；`internal/mcpserver/core.go:381-389` |
| forward 只支持本地 -L 语义；监听 broker 所在机器的 127.0.0.1:local_port；remote_host 从服务器视角 | `internal/mcpserver/types.go:63-84`；`internal/mcpserver/server.go:133` |
| local_port 省略/0 = broker 挑空闲端口 | `internal/mcpserver/types.go:72` |
| 隧道 ~10 分钟自动回收、**按创建时间非流量**（持续在用也收）；扫描周期 1 分钟（实际 10–11 分钟内） | `internal/mcpserver/tunnels.go:10-21`；`internal/mcpserver/types.go:79-80` |
| close_port 拆监听 + 背后 SSH 连接（broker 全程持有）；id 未知报 `no open tunnel with id ...` | `internal/mcpserver/tunnels.go:125-142`；`internal/mcpserver/core.go:537-540`；`internal/mcpserver/server.go:151` |
| serve 模式 401 = token 失效（rotate/disable/revoke），HTTP 中间件在工具层之前拒；**已开隧道吊销后继续转发**（不级联拆） | `internal/mcpserver/serve.go:83-96`；`internal/mcpserver/revoke_semantics_test.go:88-130,26-87` |
| stdio 模式 token 无效 → broker 进程起不来（stderr `invalid or unknown token` 后退出） | `internal/mcpserver/run.go:29-35`；`internal/cli/mcp.go:70-73` |
| 工具报错形态 = IsError=true + 错误文本（非传输层错误） | `internal/mcpserver/server.go:83-89` |
| broker 启动检测散落 SSH 凭据 → stderr `WARNING: ssh credential files detected`（仅本机 stdio 模式） | `internal/cli/mcp.go:63-67`；另见 docs/agent-access.md「隔离与排错」 |
| 三态工具面一致（cache 与在线同 6 工具、同 profile 隔离、同审计；仅写操作被拒 + 审计走 sidecar） | `internal/mcpserver/run.go:223-227,208-213` |
| ErrReadOnly 文案 + SetReadOnly 语义（cache hydrate 后置只读） | `internal/store/store.go:46-54`；`internal/mcpserver/run.go:76` |
| 离线 cache 模式：未知 host key 被拒（TOFU 无法记录，包 ErrReadOnly；已知 key 正常匹配） | `internal/sshbroker/hostkey.go:33-34`；`internal/sshbroker/hostkey_readonly_test.go:33-58`；`internal/mcpserver/run.go:211-213` |
| 凭据字节永不出现在任何工具结果（owner 侧模型） | `internal/mcpserver/types.go:5`；docs/agent-access.md「安全模型回顾（铁律）」；../README.md "The security model" |
| 每次工具调用全程审计（project/server/action/status/命令/耗时） | `internal/mcpserver/core.go:81-89` 等；../README.md "What the agent gets" |
