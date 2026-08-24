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
   `exec_background` 的后台任务用完 `exec_stop`（每 project 只有 32 个槽位）；
   长活命令走后台三件套（见 exec_background），别硬塞前台。

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
  **5 分钟**（超限值被钳到 5 分钟，**生效值以 `effective_timeout_seconds`
  字段回显**——恒存在，读它别猜）。连接建立不受这个参数约束（由 MCP 工具
  调用本身的上下文管理）。**长活别塞前台**：会超 5 分钟的活（编译 / 训练 /
  日志跟踪）用 `exec_background` 后台跑 + `exec_output` 轮询增量；前台留给
  短命令，要缩量时分页/分文件拆步。
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

### upload_content

把**你（agent）自己持有的内容**内联写成一个远程文件——`content` 直接进 JSON
入参，不要求 broker 机器上先有文件。参数：`server_id` + `content` +
`remote_path`（绝对路径：`/` 开头，或 Windows 盘符形 `X:/`）+ 可选
`encoding`（text / base64，缺省 text）。

- **跨机路径（方向 matters）**：`upload_file` 的 `local_path` 是 **broker
  所在机器**上的路径——远程 serve 拓扑（你的笔记本跑 agent → serve 主机跑
  broker → 目标机）下 broker 够不到你机器上的文件。**你自己生成/持有的内容**
  （配置、脚本、小产物）走本工具直推目标机；broker 机器可达的文件/目录仍走
  `upload_file`。
- **`encoding` 两态**（与 exec_output 同构，零新概念）：
  - `text`（默认）：写入的是 **JSON 解码后的字符串**，按 UTF-8 落盘——
    **非字节精确**：客户端送来的非法 UTF-8 字节在 JSON 解码层就被替换为
    U+FFFD（Go encoding/json 公开行为）。普通文本用它。
  - `base64`：解码后落盘，**字节精确**——二进制 / GBK / 任意字节串一律
    走它。**只收单行**、standard 字母表、带 padding：content 含 `\r` 或
    `\n` 直接拒绝（把折行拼成单行再发；MIME 折行不容忍）。
- **上限 = 解码后 8 MiB**（默认值；owner 可用 env `SSHMGR_UPLOAD_CONTENT_MAX`
  调整，fail-closed——env 非法/非正/**>1 GiB** 时 broker 直接拒绝启动；工具
  描述里的 cap 数字随实际生效值如实变化）。超限 = **传输前拒绝**（零字节
  移动、零远程文件创建），错误自带 size/cap 证据；**恰好等于 cap 放行**。
  大文件别硬塞：先放到 broker 可达的位置再 `upload_file`，或让服务器侧
  自己拉取。
- `remote_path` 的**父目录不存在会自动创建**（深层新路径直接写）；目标
  已存在 = **覆盖写**（截断重写，upload_file 同语义）。空内容合法（写出
  0 字节文件）。
- **无 sudo**：SFTP 通道，root 属主路径不可写——写 root 路径先传到可写
  目录再 `exec_command` + `sudo=true` 移过去（upload_file 同套路）。
- **同路径并发写不保证原子性/最终一致性**：两个并发写同一 `remote_path`
  会交错截断/写入（SFTP 无锁语义）——**避免对同路径并发上传**；怕误覆盖
  可先 `exec test -e` 自查。
- **失败留半写**：失败（含取消/超时）时远端可能留下**半写文件/已建的父
  目录**，清理归你自己——失败后自查目标路径，再决定重传或清掉。

#### serve 请求体上限（在线 serve 模式）

HTTP 请求体有中间件收口：上限 = **`cap + cap/3 + 64 KiB`**（覆盖 base64
展开 + JSON 包装 + 头部余量），与内容上限**同源联动**——owner 调大
`SSHMGR_UPLOAD_CONTENT_MAX` 时两个上限一起动，没有独立旋钮（该联动已在
[threat-model.md](./threat-model.md) §6 登记）。两级行为：Content-Length
诚实超限 → 中间件直接 **413**；谎报/无 Content-Length（chunked）→
`http.MaxBytesReader` 兜底，读到一半报错、返回错误响应（攻击者拿不到
工具执行）。stdio 模式无此 cap（对端是本机 agent 进程，非网络面）。

**已知边界（413 早拒）**：text 模式下 JSON 字符串转义使内容在线上膨胀
（`"`/`\` 2×、控制字符 `\uXXXX` 最高 6×/字节）——**线上平均膨胀超过 4/3
的贴上限内容就可能被 413 早拒**，不是只有极端形态才中：以 8 MiB cap 为例，
全 2× 转义内容 >~5.6 MiB、控制字符 6× 内容 >~1.8 MiB 即触发（~48 MiB 只是
6× 全覆盖的形态上界）；被 413 的内容解码后其实可能 ≤ cap。真实配置/脚本的
转义膨胀通常 <1.1，几乎不会命中。**极端转义/二进制/控制字符内容一律走
base64**——base64 字母表无需 JSON 转义，贴 cap 的合法 base64 线上体恒在
限内，不存在该边界。

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

### exec_background

在服务器上**后台**跑一条长活命令（编译 / 训练 / `tail -f` 日志跟踪 / watch），
立刻拿 `task_id` 返回——不等它结束。与 `exec_command` 同一道 profile 门和
同一套 sudo 规则（`sudo=true` 由 broker 代跑 `sudo -S`，**别自己拼 `sudo`
前缀**，前提 `has_sudo=true`）。

- `timeout_seconds` 缺省 = **24 小时**，这也是硬上限（超限值被钳到 24h）；
  生效值回显 `effective_timeout_seconds`。
- **无 stdin / env / workdir 参数**——自己组命令行：`cd /var/log &&
  tail -f app.log`、`VAR=x make build`。
- **每 project 上限 32 个任务**（运行中 + 完成后保留的都算，含并发启动的
  预约）。满了还有已完成任务 → 最旧的完成记录被驱逐（其 task_id 随即失效）；
  全是运行中 → 拒绝并提示「wait for a running task to finish or call
  exec_stop」。
- **任务表纯进程内：broker 重启 = 任务全部死亡**（无持久化、无恢复）——
  重启后所有 task_id 一并失效，把在跑的活当作全死了重新安排。
- 选型：短命令（≤5 分钟、马上要全量输出）→ `exec_command`；长活 / 要增量
  输出 → `exec_background` + `exec_output` 轮询。

### exec_output

按 `task_id` 拉后台任务的**增量输出**与运行/终态。反复调用 = `tail -f`。

- `stdout_offset` / `stderr_offset` 是**各通道流内的绝对字节游标**（`0` /
  缺省 = 流首）。把上次返回的 `next_stdout_offset` / `next_stderr_offset`
  原样回传即续读；两通道各一条游标、推进速率不同，别互相混用。
- **轮询惯用法（tail -f / journalctl -f）**：把跟踪类命令起成后台任务后，
  循环调 `exec_output` + `wait_seconds`（0–60，缺省 0 = 立即返回当前快照）
  长轮询——任一通道有新字节 / 任务离开 running / 预算耗尽即返回。
  **wait 建议 ≤30**：给你自己客户端的超时留余量。
- **编码两态**：`encoding="text"`（默认）原始字节按 UTF-8 直入 JSON——非法
  序列被替换为 U+FFFD（**有损**），多字节字符可能被读取边界切成两半；
  `encoding="base64"` 字节精确——你侧解码，跨读取窗重组无损、二进制安全。
  **GBK 等非 UTF-8 日志一律用 base64**。两模式 offset 恒为字节口径，切换
  编码不用改游标。
- **诚实降级**：每通道只保留**尾部 1 MiB** 滚动窗。offset 落后窗口首
  （产出快于轮询）→ `truncated=true` + `lost_stdout_bytes` /
  `lost_stderr_bytes` 告诉你丢了多少——丢的追不回来，**从 `next_*_offset`
  续读**，别用旧 offset 重试。offset 超前（越过当前流尾）会被拉回流尾。
- **终态与取尾**：`status` ∈ running / done / stopped / timeout / failed；
  `exit_code` 在 done 时有意义，`error` 在 failed 时是清洗过的文本。任务
  结束后输出仍可读 **~1 小时**（取尾、拿退出码），之后表项过期（见错误
  对照表的 task_id 失效三因）。
- 纯进程内读，不碰服务器/凭据——**零审计行**（与 `list_servers` 同级）。

### exec_stop

按 `task_id` 停一个后台任务。**立即返回触发时刻的 `status`**——运行中任务
返回 `"running"`（停止已被启动，终态 `stopped` 由下一次 `exec_output` 观察，
本调用不阻塞等任务死）。对已终态任务幂等：直接回其终态。

- **kill 语义（诚实版）**：没有信号楼梯——停止 = 关闭该任务的 SSH 会话 →
  远端进程收到 **SIGHUP**，与杀掉一个真 `ssh` 会话同语义；命令里用
  `nohup` / `setsid` 起的远端进程**会活下来**。
- 未知 `task_id` → 报错（三因见错误对照表）。

## 错误对照表（每条给"你该做什么"）

| 报错 | 含义 | 你该做 |
|---|---|---|
| `invalid or unknown token`（stdio：broker 起不来）；serve：任意调用 HTTP 401 | token 错了 / 被 owner `rotate` 换发 / project 被 disable/revoke | **报告 owner**；别反复重试。owner 会核 status、必要时换发并更新 `.mcp.json`（见 [agent-access.md](./agent-access.md)「Project 生命周期」） |
| `server is not in your profile — call list_servers ...` | id 不在授权清单（用错 id、拿 name 当 id、或它不在你 profile） | **重新 `list_servers` 核对 id**；还不在就是 owner 没授权，报告 owner，别试别的 id |
| `server has no credential configured (set one with: ...)`（`no_credential`） | owner 建了服务器但没配登录凭据——连接前就被拒 | **报告 owner** 按错误里的提示配凭据；重试无意义 |
| 结果里 `timed_out: true`（不是报错） | 前台命令超过 timeout（默认 120s、硬顶 5 分钟）被杀 | **拆小命令**：分页/分文件；长活改走 `exec_background` + `exec_output` 轮询（前台永远只有 5 分钟） |
| 结果里 `truncated: true`（不是报错） | 输出超每通道 1 MiB（download 是文件超 1 MiB），你只有前缀 | **refine**：`tail -n` / `head -n` / `grep` 取目标段重跑；看 `*_bytes` 判断真实体量；别硬拉全量 |
| `host key mismatch: possible MITM, connection rejected` | 服务器 host key 和首次记录的不一致——可能是中间人（TOFU fail-closed） | **报告 owner 核实**，**绝对别**尝试绕过（没有任何"跳过检查"参数） |
| `ssh dial: connect failed: connection refused`（等分类短语） | 连不上目标机（地址细节已按可见性边界清洗） | 核对 server_id 是否正确；网络问题报告 owner |
| `sudo not configured for server <name> (call list_servers: has_sudo tells you)` | `sudo=true` 但该机 `has_sudo=false` | 回 `list_servers` 核对；确实需要提权就报告 owner 配 sudo 凭据 |
| `no open tunnel with id <id>` | 隧道已关/已被 ~10 分钟自动回收/从未开过 | 正常现象；需要转发就重开 `forward_port` |
| `background task limit (32) reached — wait for a running task to finish or call exec_stop` | 后台任务满员：该 project 已有 32 个任务且全在运行（完成态会被驱逐腾位，全运行态才拒绝） | 等一个运行中任务自然结束，或对不要的任务调 `exec_stop`，再重试 `exec_background` |
| `unknown task_id — it may never have existed, expired after the retention window (1h), been evicted for capacity (32-task limit), or the broker restarted; task records are in-process only` | task_id 失效（三因 + 从未存在）：完成超 ~1h 保留期过期 / 32 满员时被驱逐 / **broker 重启**（任务表纯进程内，重启即全失） | broker 重启 = 在跑任务全死，重新安排活；过期/驱逐的按需重跑命令；失效 id 别反复重试 |
| `file <path> (<N> bytes) exceeds upload cap <cap> — refused before transfer` | 单文件严格大于 1 MiB，传输前被拒（零字节移动） | 按错误里的 size/cap **拆分或压缩**后重传 |
| `store is read-only (offline cache); connect to the server to mutate`（ErrReadOnly） | broker 跑在离线缓存模式（见三态环境） | **报告 owner** 切回在线/本机模式；**别**重试写操作（见下） |

## 三态环境（你通常无需分辨）

broker 有三种部署形态，**工具面完全一致**（同 10 个工具、同 profile 隔离、同审计），
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
| 超时硬上限 5 分钟、超限值钳到上限并**回显生效值**（`clampExecTimeout` → `ExecOutput.EffectiveTimeoutSeconds` 恒存在，spec §6 钳制改响） | `internal/mcpserver/core.go:18-27,157,194`；`internal/mcpserver/types.go:31-45` |
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
| 后台缺省/上限 24h + 回显（`clampBgTimeout` → `BgStartOutput.EffectiveTimeoutSeconds`）；无 env/workdir/stdin 参数（自组 `cd /dir && VAR=x cmd`） | `internal/mcpserver/tasks.go:618-627`；`internal/mcpserver/bgtools.go:146-150`；`internal/mcpserver/types.go:106-122` |
| 32/project 上限（表项 + in-flight 预约都计数）；满员驱逐最旧终态（零审计行）；全 running 拒绝 + 引导文案 | `internal/mcpserver/tasks.go:193-217` |
| 终态保留 ~1h 过期；sweeper 不删 running；任务表纯进程内——broker 重启任务全失 | `internal/mcpserver/tasks.go:25,305-323`；`internal/mcpserver/run.go:48-49`（stdio 收口） |
| exec_output：offset = 通道流内绝对字节游标、超前回拉流尾、落后保留窗诚实降级（truncated + lost_*_bytes）；每通道 1 MiB 尾部滚动窗 | `internal/mcpserver/tasks.go:438-499,506-526`；`internal/sshbroker/rolling.go:54`（Snapshot 三分支） |
| wait_seconds 钳制（0/缺省→0 不等待、>60→60）；wait≤30 建议；编码两态（text 有损 U+FFFD/切半、base64 字节精确、GBK 建议） | `internal/mcpserver/bgtools.go:156-170,203-227`；`internal/mcpserver/server.go:201` |
| unknown task_id 文案（从未存在 + 过期/驱逐/重启三因，`ErrBgUnknownTask` 逐字） | `internal/mcpserver/bgtools.go:175` |
| exec_stop 立即返回触发时刻 status（running）；已终态幂等；kill = 关会话 → 远端 SIGHUP、无信号楼梯、nohup/setsid 进程存活 | `internal/mcpserver/tasks.go:533-547`；`internal/mcpserver/server.go:220` |
| exec_output / exec_stop 零审计行（纯进程内读，与 list_servers 同级不审计；stop 触发的终态仍由任务侧落 exec-bg-end 生命周期行） | `internal/mcpserver/bgtools.go:177-250`；`internal/mcpserver/core.go:31-75`（list_servers 同无审计） |
| serve 模式 revoke **不杀**运行中后台任务（活到自然结束或 24h 钳定上限；revoke 后 exec_output/exec_stop 逐请求 401） | `internal/mcpserver/revoke_semantics_test.go:139`；`internal/mcpserver/serve.go:74-81`（Close 只在进程关闭时清） |
| serve 模式 401 = token 失效（rotate/disable/revoke），HTTP 中间件在工具层之前拒；**已开隧道吊销后继续转发**（不级联拆） | `internal/mcpserver/serve.go:83-96`；`internal/mcpserver/revoke_semantics_test.go:88-130,26-87` |
| stdio 模式 token 无效 → broker 进程起不来（stderr `invalid or unknown token` 后退出） | `internal/mcpserver/run.go:29-35`；`internal/cli/mcp.go:70-73` |
| 工具报错形态 = IsError=true + 错误文本（非传输层错误） | `internal/mcpserver/server.go:83-89` |
| broker 启动检测散落 SSH 凭据 → stderr `WARNING: ssh credential files detected`（仅本机 stdio 模式） | `internal/cli/mcp.go:63-67`；另见 docs/agent-access.md「隔离与排错」 |
| 三态工具面一致（cache 与在线同 9 工具、同 profile 隔离、同审计；仅写操作被拒 + 审计走 sidecar） | `internal/mcpserver/run.go:230-249`；`internal/mcpserver/server.go:27-59` |
| ErrReadOnly 文案 + SetReadOnly 语义（cache hydrate 后置只读） | `internal/store/store.go:46-54`；`internal/mcpserver/run.go:76` |
| 离线 cache 模式：未知 host key 被拒（TOFU 无法记录，包 ErrReadOnly；已知 key 正常匹配） | `internal/sshbroker/hostkey.go:33-34`；`internal/sshbroker/hostkey_readonly_test.go:33-58`；`internal/mcpserver/run.go:211-213` |
| 凭据字节永不出现在任何工具结果（owner 侧模型） | `internal/mcpserver/types.go:5`；docs/agent-access.md「安全模型回顾（铁律）」；../README.md "The security model" |
| 每次工具调用全程审计（project/server/action/status/命令/耗时） | `internal/mcpserver/core.go:81-89` 等；../README.md "What the agent gets" |
