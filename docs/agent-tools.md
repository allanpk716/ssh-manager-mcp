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

## 机密项输出遮蔽

执行工具的 sshmgr 进程会用它当前持有的全部机密项值，统一遮蔽所有 MCP 工具
的成功返回和报错文本。单机进程、多机客户端桥进程、权威端远程 `serve` 都在
持有值的一侧执行遮蔽；owner 与 agent 都没有关闭遮蔽的配置键、环境变量或
工具参数。

覆盖范围：

- 机密项的当前值和上一个值都会匹配。换值后，服务器上残留的旧配置、旧日志
  仍会被还原成同一个名字。
- 匹配不限于执行输出；工具结果里的嵌套结构、切片、映射的键与值，以及报错
  文本中出现的机密项值都会遮蔽。
- 遮蔽使用本进程持有的全部机密项值，不只是允许写入当前服务器的那部分。
  两个机密项值完全相同时，若工具输入带有服务器条目标识，优先选允许写入这
  台服务器的名字。
- 数据库读取失败时，工具直接返回报错，不会放行未遮蔽的原文。带环境变量
  注入计划的执行工具为了维持「命令照常执行、变量返回未注入警告」的行为，
  输出遮蔽会跳过无法解密的值；其他工具遇到单个机密值解密失败同样直接
  返回报错。
- 会话提示通道与审计日志不改变；审计本来不记录工具输出。

遮蔽结果写成 `{{sshmgr-secret:NAME}}`。结构化输出里的 `masked` 字段列出本次
实际被遮蔽的机密项名字，并按字母顺序去重；没有遮蔽时这个字段省略。报错文本
没有结构化输出，若其中出现机密项值，报错末尾会附一行「Masked secrets:」并
列出名字。只有出现在 `masked` 里的名字（或报错末尾名单里的名字）对应的
占位符才是 sshmgr 的遮蔽结果；文件或输出里原本就有的占位符字面量会原样
返回，名字也不会被加入 `masked`。

## 机密项环境变量注入

owner 可以给机密项设置一个环境变量名，并把这台服务器加入该机密项的可写入
服务器列表。此后在 POSIX、PowerShell 与 cmd 服务器的普通层执行
`exec_command` 或启动 `exec_background` 任务时，你可以直接在命令里引用这个
环境变量（POSIX 写 `$环境变量名`，PowerShell 写 `$env:环境变量名`，cmd 写
`%环境变量名%`）；POSIX 服务器的 `exec_command` 与 `exec_background` 都支持在
`sudo=true` 的提权层引用 `$环境变量名`。不需要把值拼进命令文本，也不需要先
在服务器上写配置文件。

- 值只写入这次 SSH 会话的标准输入，由远端 shell 在执行你的命令前导出；
  封装命令里只有变量名，PowerShell 封装命令还带每个值的字节长度。PowerShell
  按字节长度读帧；cmd 用 `set /p` 逐行读值，每个值后面跟一次回车换行作为
  分隔。你的命令开始时，这批帧已经读完，标准输入随即结束。sshmgr 不在
  服务器上留下注入文件。
- POSIX 提权层的读帧与导出发生在 `sudo -S ... bash -c` 的内部脚本里；同一
  SSH 会话标准输入的顺序是 sudo 密码行（若有）、随机分隔行、值帧。内部脚本
  会先丢弃分隔行之前的内容，因此免密 sudo 或已缓存的 sudo 凭据没有消费密码
  行时，密码不会变成环境变量值。
- PowerShell 按字节读值，不改变用户命令的输出编码；cmd 只在读值期间切到
  UTF-8 代码页，读完先恢复原代码页再执行用户命令。
- 命令策略检查与审计看到的是你提交的原始命令；审计只额外记录实际注入的
  环境变量名，不记录值。工具报错也不会携带值。
- 只有设置了环境变量名、且当前服务器在可写入服务器列表内的机密项会注入。
  文件传输（`upload_file`、`upload_content`、`download_file`、`relay_file`）
  与端口转发不经过这个步骤。
- 当前版本覆盖 POSIX、PowerShell 与 cmd 服务器的普通层，以及 POSIX 服务器
  `exec_command` 与 `exec_background` 的 `sudo=true` 提权层。Windows 服务器
  的 `sudo=true` 仍维持现有提权行为，不做环境变量注入。
- 无法注入时命令照常执行。`exec_command` 会在标准输出末尾为每个变量追加
  一行 `环境变量 <变量名> 未注入：<原因>`；`exec_background` 的启动结果会用
  `injection_warnings` 字段列出同样的警告行，任务结束后 `exec_output` 返回的
  标准输出末尾也会追加这些行。本版本可能出现的原因为：服务器 shell 类型
  未知、值含空字节、Windows 上值不是合法 UTF-8、cmd 下值含换行、读取机密值
  失败。前台警告行在遮蔽和截断之后追加，不参与输出截断。
- 注入的值仍受输出遮蔽保护。命令打印这个值时，返回里会变成
  `{{sshmgr-secret:<机密项名>}}`，`masked` 字段会列出该机密项名。
- `exec_context` 的返回包含 `injected_environment_names` 与
  `uninjected_environment_variables`，分别列出这一层实际会注入的变量名和
  无法注入的变量名及原因；结果不含值，也不会为了列名单额外连接服务器取值。
- 后台任务在启动时固定这次注入使用的值；任务运行期间 owner 撤销授权不影响
  已启动的任务。撤销后的下一次 `exec_background` 会用当时的机密项快照重新
  生成注入计划，不再注入已撤销的变量。

### Windows 下值的可见范围

Windows 上的值同样不出现在 SSH 命令文本、目标机进程命令行参数、命令策略
检查、审计日志、工具报错或服务器文件里；它只在读取帧的短暂窗口内经过远端
shell 的标准输入，然后进入这次 shell 进程和你的命令启动的子进程的环境块。
同一 Windows 用户、管理员以及你命令自己启动的进程都能读取环境变量；这是
环境变量注入的既有边界。你的命令把值打印出来时，输出遮蔽仍会把它还原为
占位符。

### 授权撤销与环境变量注入

每条新命令都会用 sshmgr 当时的机密项快照重新生成注入计划。在线客户端同步到
撤销授权后的快照后，新命令立即不再注入；离线客户端会继续使用本地缓存快照，
直到下次同步成功才停止注入。因此撤销授权不是立即让已经拿到值的进程遗忘该
值，真正的撤销手段是换掉机密项的值。若同一台服务器上还有旧进程或旧日志，
输出遮蔽仍会把当前值与上一个值还原成占位符。

环境变量注入和命令策略一样是护栏，不是安全边界；它防止的是把值无意写进
命令文本或文件，不承诺阻止服务器上的进程主动读取、编码或外传环境变量。
边界与威胁模型见
[ADR 0007](./adr/0007-command-policy-client-enforced-guardrail.md)。

### 后台任务与 relay_file

`exec_background` 的标准输出、标准错误，以及 `relay_file` 写入任务标准输出的
进度文本，都在进入后台输出缓冲前完成遮蔽。因此 `exec_output` 返回的
`stdout`、`stderr`、`next_stdout_offset`、`next_stderr_offset`、
`stdout_bytes_total`、`stderr_bytes_total`、`lost_stdout_bytes`、
`lost_stderr_bytes` 与 1 MiB 保留窗口，全部按遮蔽后的数据流计算；把
`next_*_offset` 原样传回即可续读，不会错位或重复。base64 模式也是对缓冲里
已经遮蔽的字节编码后再返回。

任务仍在运行时，每个输出通道会暂留足以跨越机密项值边界的尾部字节；这些
字节还没有计入可读偏移。任务正常结束、超时或被停止时，sshmgr 会先放出这段
尾部并把任务标成结束状态，再让 `exec_output` 看到结束状态。后台任务结果里的
`masked` 按整个任务累计，而不是只统计本次读取区间；即使早先输出已滚出 1 MiB
窗口，对应名字仍会在后续读取的 `masked` 里出现。

后台任务每次写入输出时都会取当前 store 的机密项值集合。多机客户端换到新快照
store 后立即生效；owner 在同一个 sshmgr 进程内换值或删除机密项后，下一次写入
立即生效。owner 通过另一个进程（例如命令行 `sshmgr secrets set` 或
`sshmgr secrets rm`）修改同一个数据库文件时，后台任务最多 1 秒后改用新值
集合；前台工具每次调用都重新读取。任何改值都只影响之后写入的输出，已经进入
缓冲的字节不会重扫。若读取值集合失败，该通道会丢弃后续远端输出，并写入
一行「sshmgr: output dropped because secret values could not be read」作为
报错占位，绝不把未遮蔽原文放进缓冲。

### 前台输出与下载的截断顺序

`exec_command` 与 `download_file` 都先读取、后遮蔽、最后截断：sshmgr 会在
1 MiB 返回上限之外再多读「全部机密项匹配串中最长一串」的字节数，对保留
下来的整段内容做遮蔽，然后把遮蔽后的内容截回 1 MiB。因此截断点最多只会
切开占位符，不会留下机密项值的半个前缀。`stdout_bytes`、`stderr_bytes`
与 `bytes`
仍表示原始输出或远端文件的真实字节数；`truncated` 仍按真实字节数是否超过
1 MiB 判定。

输出遮蔽与命令策略一样，是**护栏，不是安全边界**
（[ADR 0007](./adr/0007-command-policy-client-enforced-guardrail.md)）。它防
的是机密项值无意中进入 agent 上下文；不承诺识别 URL 编码、
JSON 转义、压缩、加密等变换后的值，也不防 agent 有意绕开遮蔽。真正的访问
控制仍由 SSH 凭据、profile/project 授权、命令策略与 owner 管理的机密项
可写入范围承担。

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
| `device` | owner 登记的设备名；`device` 值相同的多行服务器条目属于同一台登记机器，重启这台机器会让这些条目一起断开。空字符串表示未登记设备 |
| `site` | 设备的物理局域网所属站点名；多个站点按名字排序后用「、」连接。空字符串表示未登记 |
| `networks` | 设备所属网络名；物理局域网在前、虚拟局域网在后，同类按名字排序。空数组表示未登记 |
| `role` | 这台机器的用途（如 "prod pg primary"） |
| `services` | 上面部署/运行着什么 |
| `caveats` | **操作须知 & 特殊处理规则——动手前读**；空 = 无 |
| `location` / `hardware` | 部署位置 / 硬件配置 |
| `tags` / `description` | 自由标签 / owner 补充说明（优先看上面的结构化字段） |

**拓扑字段只是登记信息**——它们说明 owner 把机器、站点和网络登记在什么
位置，**不代表当前可达**；可达性与地址不由 `list_servers` 判断。拓扑字段
只含名字，不含地址，也不受 host 披露开关影响。

**永不含凭据**——任何字段都不出现密码/私钥，也不出现拓扑地址。空字符串/
空数组的字段表示"没有"，不是错误。哪些 server 会出现在你的清单里、owner
怎么改，见
[agent-access.md](./agent-access.md)；背后的概念模型见 [concepts.md](./concepts.md)。

### list_secrets

只读发现你能引用的机密项。每个机密项一行，格式是
`名字 | 备注 | 能写入的服务器名`；服务器名用逗号分隔，只包含你的授权范围内的
服务器。

- 可选 `query` 在名字与备注里做不区分大小写的子串匹配。
- 可选 `server` 填服务器名，只返回能写入这台服务器的机密项；两个参数同时给
  时取交集。
- 服务器不存在与不在你的授权范围内的输出完全相同，都是
  `没有可用的机密项`；筛选后没有结果时也返回这句话。
- 结果永不包含机密项的值、值的长度、值的哈希、时间或授权范围外的服务器，也
  不写审计。
- 后续功能的引用写法是占位符 `{{sshmgr-secret:NAME}}`。当前版本还没有提供
  机密项填值能力；不要把它理解成可以写入机密项的值。

### list_shares

只读列出这个会话里能看到的 SMB 网络共享登记，以及每台受管服务器上约定
的挂载点。结构与字段：

| 字段 | 含义 |
|---|---|
| `name` | 共享名，同一 vault 内唯一（不区分大小写） |
| `provider_device` | 提供这处共享的设备名（owner 在网络拓扑里登记的设备） |
| `note` | owner 备注的自由文本 |
| `directories` | 目录约定数组，每项是 `{purpose, path}`：用途对应共享内部的子路径 |
| `drive` | Windows 固定盘符（D 到 Z 的单个字母） |
| `version` | 共享存储版本号，只增不减；owner 修改登记、换挂载密码、提供方设备网络成员变化或可写入服务器列表变化都会加一 |
| `change_note` | 当前版本对应的 owner 变更说明，可以为空 |
| `servers` | 当前会话可见且被授权可挂载这处共享的服务器，每项是 `{server_id, name, mount_point}` |

- **挂载点是约定路径**：PowerShell 与 cmd 服务器为 `<盘符>:`（如 `Z:`），
  其余为 `/mnt/sshmgr/<共享名>`。结果里的 `notice` 固定说明「挂载点
  是约定路径，本工具不检测是否已挂载」——本工具不做任何网络、SSH 或
  文件系统检测。
- 可选 `server` 填服务器编号或名字；给出时只返回这台服务器能挂载的共享，
  每个共享的 `servers` 只带这一台。服务器不在你的授权范围内时报
  `server is not in your profile`（见错误对照表）。
- 可挂载范围来自挂载密码机密项的可写入服务器列表，再与你的会话可见
  服务器取交集；交集为空的共享不会出现在结果里。
- 结果永不包含挂载密码、挂载账号用户名或候选访问地址。没有任何 agent
  工具能登记、修改或删除共享存储——这些只能由 owner 在命令行或
  管理界面完成。

### exec_command

在服务器上跑一条 shell 命令。

- POSIX、PowerShell 与 cmd 普通层，以及 POSIX 服务器的 `sudo=true` 提权层，
  都会按「机密项环境变量注入」一节自动导出已授权的环境变量；命令里直接
  引用对应写法的环境变量即可，不要把值写进命令文本。
- **`sudo=true` 让 broker 替你跑 `sudo -S`**——broker 把存储的 sudo 密码写进
  远端 sudo 的 stdin，你**别自己在 command 里拼 `sudo` 前缀**（会变成
  `sudo -S -p '' -- sudo ...`，密码喂不进去）。前提 `has_sudo=true`。
- **命令策略：执行前有一道严重后果兜底检查**（拦截 / 需确认 / 放行三档，
  判定在建立连接之前——被拦下的命令一个字节都不会到服务器）。
  **拦截**：你收到
  `blocked by command policy [<规则编号>]: <为什么危险> — instead: <更安全的做法>`
  ——**不要换写法重试**（改写、拆段、换写法绕过都是错误反应）；把规则
  编号、风险说明与建议的替代做法解释给用户；用户确实需要执行时报告
  owner 裁量（规则的停用是 owner 的决定，不是你的）。
  **需确认**：你收到
  `confirmation required by command policy ... | confirmation code XXXXXX: ...`
  ——**先把风险说给用户、征得同意**，然后用**同一条命令**重调，附
  `confirmation_code`（那串码）与 `confirmation_reason`（用户同意了什么的
  简短说明）。码与「服务器 + 命令完整文本」绑定、5 分钟有效、单次
  使用——命令改一个字符或换一台服务器，都要重新走一轮确认。你看不到
  完整规则清单（设计如此），每次返回里只有当次命中的那一条。
- **超时只作用于命令执行阶段**：`timeout_seconds` 缺省 = **120 秒**；硬上限
  **5 分钟**（超限值被钳到 5 分钟，**生效值以 `effective_timeout_seconds`
  字段回显**——恒存在，读它别猜）。连接建立不受这个参数约束（由 MCP 工具
  调用本身的上下文管理）。**长活别塞前台**：会超 5 分钟的活（编译 / 训练 /
  日志跟踪）用 `exec_background` 后台跑 + `exec_output` 轮询增量；前台留给
  短命令，要缩量时分页/分文件拆步。
- **超时不是错误**：到点后 broker 发出 SIGKILL 信号请求并断开连接，你收到正常
  结果对象，`timed_out=true` + 已接收的部分输出——返回以超时点为界（加一小段
  宽限，默认 2 秒），不会挂到远端进程自然结束。据此判断是拆步还是后台化。
  非零退出码同理（`exit_code` 承载它，不是工具报错）。
- **少数服务器不清理远端进程**：断开连接后，部分服务器（实测 OpenSSH 9.6）上的
  远端进程可能残留——超时后不确定进程状态就用 `ps`/`pgrep` 自查，必要时报告
  owner 手工清理。高延迟链路可经环境变量 `SSHMGR_EXEC_KILL_GRACE` 上调宽限
  （默认 2 秒，允许 100 毫秒–30 秒；broker 进程的 env，owner 操作）。
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
机器**上的绝对路径——就是**你脚下的机器**（单机与多机皆然：多机形态的 broker
子进程跑在本地）。
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

- **你生成/持有的内容**（配置、脚本、小产物）走本工具直推目标机——没有
  "broker 够不到你机器上的文件"的问题：多机形态下 broker 子进程就跑在**你脚下的
  机器**上，`upload_file` 一样可用；本工具的价值是免落盘直写。
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

> 传输上限（8 MiB 解码后）对两形态一致；stdio 的对端是本机 agent 进程、
> 无 HTTP 请求体封顶。**二进制 / 高转义内容（大量 `"`/`\`/控制字符）一律走
> base64**——无需 JSON 转义、字节精确（GBK 等非 UTF-8 同理）。

### relay_file

把**大文件**从一台服务器**经 broker 中继**到另一台——源也可以是 **broker 本机
盘**（`from_server_id` 省略/留空）。**零上下文大文件通道**：文件字节只在 broker
持有的两条 SSH 连接间分块流式转发，**工具结果、`exec_output`、审计里都只有
元数据**（task_id / 块数 / 进度 / 摘要），文件内容永远不进你的上下文。适用：
文件超过 `upload_file` 的 1 MiB 单文件帽（模型权重、GB 级产物），或文件在一台
机上、必须落到另一台机（在线机下载好 → 送真空离线机）。**无 ssh 二进制直接
对应物**：`scp serverA:file serverB:` 形似，但那是两端直连、两端都要操作者的
凭据；relay 是 broker 代理、凭据不出 vault——属 Broker-specific 能力。

- **参数**：`to_server_id` + `to_path`（绝对路径，目标恒为远程服务器）+ 源二选一：
  `from_server_id` + `from_path`（该机上的绝对路径），或只给 `from_path`
  （**broker 所在机器**上的绝对路径）。路径 `/` 开头或 Windows 盘符形 `C:/`。
  目标父目录不存在会自动创建；完成后**原子替换**目标路径上已存在的文件
  （真名出现 = 传完的可见保证）。同源同径（from==to 同机同路径）会被拒
  （自我覆盖 no-op）。
- **三件套用法**：调用立即返回 `task_id`（`exec_background` 同款形态）——
  `exec_output(task_id)` 轮询逐块进度与最终摘要，`exec_stop(task_id)` 中途停。
  relay 任务占**同一张后台任务表**（每 project 32 上限、任务表纯进程内
  **broker 重启即失**——但进度不丢：重跑 relay_file 同参数即续传，断点锚在
  目标端盘上）。单任务时长上限 **72 小时**（常量；到点 = 可续传的 timeout 终态）。
- **续传语义（核心）**：中断 / 停止 / 失败都会在目标端留下
  `<to_path>.sshmgr-partial` + `<to_path>.sshmgr-manifest.json`（已完成块清单
  ——**续传唯一事实源**，落在目标盘、不随 broker 死）。**重跑 relay_file 同参数
  = 只补缺失块**；返回里的 `resumed_chunks` 就是接上的已完成块数。块大小来自
  broker 构造期 env `SSHMGR_TRANSFER_CHUNK`（缺省 256 MiB，越界 broker 拒启动）
  ——**续传两端块网格必须一致**，改过 env 的重跑会被拒（错误指引还原 env 或
  `fresh=true`）。
- **`fresh=true`**：丢弃目标端 partial + manifest，从 byte 0 重传。只在该放弃
  进度时用——清单不符 / 非常态残留的错误文本都会明示 "pass fresh=true"。
- **双摘要验证配方（重要）**：最后一行进度带摘要，按任务形态二选一：
  - 本任务**从 byte 0 连续流到 EOF**（fresh 起步或空清单自愈）→ 报
    `file_sha256`——直接 `exec sha256sum <to_path>` 比对即可；
  - **续传了已完成块**的任务报**块 merkle 根**（`root=sha256:…`）——每块写盘时
    已对 manifest 校验过；要独立复验得按块大小切块、逐块 sha256、再把摘要串接
    哈希（很少需要；日常用下面的离线机闭环故事即可）。
- **目录不支持**：源端先 `exec tar czf /tmp/x.tgz <dir>`，relay 那个 tar，
  目标端 untar。
- **离线机完整故事（核心场景）**：`exec_background` 在在线机上后台下载大文件 →
  `relay_file` 送到离线机 → 在离线机 `exec sha256sum` 与 `exec_output` 里的
  `file_sha256` 比对——全程零字节过你的上下文。
- **无 sudo**：SFTP 通道，root 属主目标路径不可写。
- **`space_check` 字段**：`"unavailable"` = 目标端报告不了剩余空间（部分
  Windows 目标）——**知情继续是安全的**：盘真满只是停在某块边界，腾空间后续传。
  空间**已知不足**则是 refusal（错误带 avail/need 证据，零字节移动）。
- **posix-rename 硬依赖**：目标端 SFTP 服务端必须支持
  `posix-rename@openssh.com`（OpenSSH 全系含 Win32 端口都支持；非 OpenSSH 系
  sftp 目标出局）——不支持的任务启动即 failed，错误明示 "lacks posix-rename"，
  无参数可绕。
- **弃疗残留清理责任在你**：不打算续传时，目标端的 partial（+ manifest）残留
  **无害但占盘**——`exec rm <to_path>.sshmgr-partial <to_path>.sshmgr-manifest.json`
  清掉。例外：真名已在 + manifest 残留 = 提交成功后的无害碎片，随手删 manifest
  即可。

### update_server_metadata

**只在缓存模式（多机部署的工作机）存在**——把服务器条目的六个描述性字段
（`role` / `services` / `location` / `hardware` / `caveats` / `description`）
写回共享 vault：agent 探明一台服务器后（查了硬件、发现了部署了什么服务），当场
记回去，后续所有会话的 `list_servers` 都受益。**仅缓存脸**：权威端本机的
agent 面没有这个工具（owner 用 Web UI / 命令行编辑）。

- 参数：`server_id` + `expected_revision`（你在 `list_servers` 里读到的
  `revision` 值——乐观锁令牌）+ 六个字段各可选。
- **省略 = 不改，空串 = 清空**：不传 `location` 就是保持原值；传
  `"location": ""` 才是清掉它。每次至少传一个字段。
- **只写你验证过的**：这是共享事实源——把猜的、道听途说的写进去会误导所有
  后续会话。
- **冲突自愈**：别人（owner 或另一台设备）在你列出之后先改了，你会得到 409
  错误——错误里带**当前的 revision 和六个字段的现值**：把你的意图和现值合并，
  用新 revision 重试一次即可，不需要任何人工介入。
- 连接信息（host/port/用户/凭据）与标签（tags）**不可**经此工具修改——那是
  owner 的结构性操作。
- broker 不可达时直接失败（本地零改动、不留草稿）；写成功会自动镜像进本机
  缓存（`mirrored: false` 时按提示跑一次 `cache pull`）。
- 全程审计：owner 侧 `sshmgr audit` 可见 `meta-edit` 行（设备名 + 改了哪些
  字段 + 旧值截断）。owner 可用 `serve --metadata-edit=false` 一键全局关闭。

### forward_port

开一条本地端口转发（**只支持 `ssh -L` 语义**——本地监听、经服务器转发；没有
`-R`/`-D`）。用于够到**跑在服务器上（或服务器能访问到）的服务**：数据库、
web UI、metrics 端口。

- 参数：`server_id`（SSH 入口机）+ `remote_host` / `remote_port`——
  **从服务器视角**写转发目标（够服务器自己的回环服务就填 `127.0.0.1` + 该
  服务端口）；`local_port` 可省（broker 挑空闲口）；`listen_host` 可省
  （缺省 `127.0.0.1`）。
- **`listen_host`（可选）**：转发监听绑定的地址。**缺省/环回 = 实际上唯一的
  可用值**——多机 client（cache 模式）恒环回，非环回地址被 fail-closed 拒绝
  （②a 移除后白名单不再有管理入口，恒空）；传非环回地址**直接拒**。返回的
  `listen_host` 字段回显实际绑定地址。
- 返回 `tunnel_id` + `local_port`：转发**监听在 broker 所在机器的
  `<listen_host>:<local_port>`**（缺省 `127.0.0.1`）——就是你脚下的机器
  （单机与多机皆然：多机形态的 broker 子进程跑在本地），直接
  `curl http://127.0.0.1:<local_port>`。
- 这是唯一**有状态**的工具：broker 会为隧道**全程持有一条 SSH 连接**。
  **真空闲 ~10 分钟自动回收**（按**活动**算——隧道上有真实流量就续命，
  扫描周期 1 分钟，实际 10–11 分钟内收）；owner 侧也可随时 `tunnels kill`
  拆它（token 吊销/禁用后 ~15s 内级联拆除，属预期——端口随即不可达）。
  **用完主动 `close_port`**，别等回收。

### close_port

按 `tunnel_id` 关闭 forward_port 开的隧道——拆掉本地监听**和**背后那条 SSH
连接（broker 一直替你持有，这是释放资源的唯一途径）。不需要 server_id：
tunnel_id 是绑定在 broker 进程上的不透明句柄。

- 成功返回 `closed`；id 未知（已关过/已被 ~10 分钟空闲回收器收走/被 owner
  `tunnels kill` 拆过/从未开过）报
  `no open tunnel with id <id>`——**正常现象**，需要就重开 forward_port。
- **token 失效**（被 owner 轮换/禁用/吊销）：stdio 下 broker 重启后起不来；
  多机缓存模式下报告 owner。注意：**已经开着的隧道会在 token 吊销/禁用后
  ~15s 内被级联拆除**（owner 侧也有 `tunnels kill` 急停）——端口随后不可达
  属预期，不是你这边出了问题。

### exec_background

在服务器上**后台**跑一条长活命令（编译 / 训练 / `tail -f` 日志跟踪 / watch），
立刻拿 `task_id` 返回——不等它结束。与 `exec_command` 同一道 profile 门、
同一套 sudo 规则（`sudo=true` 由 broker 代跑 `sudo -S`，**别自己拼 `sudo`
前缀**，前提 `has_sudo=true`）和**同一道命令策略门**（拦截 / 需确认的处理
与 exec_command 完全相同——需确认时带 `confirmation_code` /
`confirmation_reason` 重调的是 exec_background 本身）。

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

### reload_self

查你正用着的这把 sshmgr 桥的版本状态,并可在盘上有新版本时触发换手(桥自升级,
v0.20.0+)。无参数。**平时你不需要调它**——owner 更新磁盘二进制后,空闲的桥约
30 秒内自动换手,当前会话不断线、新工具当轮或下一轮可用;调它多半是为了查询版本
或强制触发。

返回字段:

| 字段 | 含义 |
|---|---|
| `version` | 当前进程(正在应答你的这把桥)的版本 |
| `disk_generation` / `disk_version` | 磁盘二进制的代际与版本(owner `sshmgr update` 成功后更新;0/空 = 盘上无信号) |
| `latest` / `latest_error` | GitHub 最新版(仅本工具被调用时联网查一次;失败如实带错误,不影响其余字段) |
| `handover` | `no_new_generation`(盘上无更新)/ `started`(换手已启动,`successor_version` 给继任版本——注意本次响应仍由旧进程写回,`version` 是旧值)/ `declined_busy`(忙,见 `busy`)/ `failed`(`error` 给原因,原桥继续服务)/ `already_armed`(已有换手在进行)/ `not_first_generation`(罕见:本桥取不到自身可执行路径,无力拉继任) |
| `busy` | 仅 `declined_busy` 时出现:活跃隧道数、运行中后台任务数、在飞请求数 |

行为要点:换手在存在活跃隧道/运行中后台任务/进行中传输/未答请求时会被拒绝
(发起换手的本次调用自身不计入忙判据);忙时自动换手静默跳过、空闲后自动重试。
失败安全:继任进程起不来时,旧桥继续服务并返回失败原因,会话不受损。详细行为
与兜底路径见 [agent-access.md](./agent-access.md) 桥热升级(reload_self)一节。

### probe_connectivity

探测「从这里/某台受管服务器出发，到登记设备的指定传输控制协议端口现在
通不通、建连多快」。参数：

| 字段 | 含义 |
|---|---|
| `from` | 起点：`self`（当前 sshmgr 实例对应的本机设备）或授权范围内某台服务器的编号 |
| `targets` | 目标列表，最多 10 个（去重后）；每项是服务器编号或设备名，可附「:端口」 |

- 只测 **TCP 建连与耗时**，不测带宽；端口缺省时目标写服务器编号用条目端
  口，否则用 22。
- **只探测登记过的组合**：按起点设备与目标设备的共同网络逐个探测登记
  地址（物理局域网在前、虚拟局域网其次）；没有共同网络的目标返回「无共
  同网络，未探测」，不自动发现、不测兜底主机。
- 结果为紧凑文本（不暴露 IP），**只存在发起实例本地**的探测结果文件，
  不回传权威端。
- `from` 写服务器编号时经 SSH 在那台服务器上执行 sshmgr 生成的**固定命令**
  （按条目 shell 类型），**不经命令策略，但写审计**（与 `exec_context`
  同一先例：策略是护栏不是安全边界，见 ADR 0007）；审计摘要会注明这一
  点。
- 每个地址超时 1.5 秒（`SSHMGR_SSH_ADDRESS_TIMEOUT`）、整次调用总时限
  30 秒、最多 5 个目标并发；超时或被打断的地址标「探测未完成」。

## 错误对照表（每条给"你该做什么"）

| 报错 | 含义 | 你该做 |
|---|---|---|
| `invalid or unknown token`（stdio：broker 起不来；多机缓存：spawn 即拒） | token 错了 / 被 owner `rotate` 换发 / project 被 disable/revoke | **报告 owner**；别反复重试。owner 会核 status、必要时换发并更新 `.mcp.json`（见 [agent-access.md](./agent-access.md)「Project 生命周期」） |
| `server is not in your profile — call list_servers ...` | id 不在授权清单（用错 id、拿 name 当 id、或它不在你 profile） | **重新 `list_servers` 核对 id**；还不在就是 owner 没授权，报告 owner，别试别的 id |
| `权威端版本过旧，不支持网络拓扑与共享存储，请先升级权威端` | 多机客户端拿到的快照来自不写推荐版本的旧权威端，网络拓扑与共享存储工具整体降级 | **报告 owner** 先升级权威端，客户端随后同步缓存；重试无效 |
| `server has no credential configured (set one with: ...)`（`no_credential`） | owner 建了服务器但没配登录凭据——连接前就被拒 | **报告 owner** 按错误里的提示配凭据；重试无意义 |
| 结果里 `timed_out: true`（不是报错） | 前台命令超过 timeout（默认 120s、硬顶 5 分钟）到点——broker 发出 SIGKILL 请求并断开连接，按时返回 | **拆小命令**：分页/分文件；长活改走 `exec_background` + `exec_output` 轮询（前台永远只有 5 分钟）；少数服务器断连后远端进程残留，用 `ps`/`pgrep` 自查、必要时报告 owner 清理 |
| 结果里 `truncated: true`（不是报错） | 输出超每通道 1 MiB（download 是文件超 1 MiB），你只有前缀 | **refine**：`tail -n` / `head -n` / `grep` 取目标段重跑；看 `*_bytes` 判断真实体量；别硬拉全量 |
| `host key mismatch: possible MITM, connection rejected` | 服务器 host key 和首次记录的不一致——可能是中间人（TOFU fail-closed） | **报告 owner 核实**，**绝对别**尝试绕过（没有任何"跳过检查"参数） |
| `ssh dial: connect failed: connection refused`（等分类短语） | 连不上目标机（地址细节已按可见性边界清洗） | 核对 server_id 是否正确；网络问题报告 owner |
| `sudo not configured for server <name> (call list_servers: has_sudo tells you)` | `sudo=true` 但该机 `has_sudo=false` | 回 `list_servers` 核对；确实需要提权就报告 owner 配 sudo 凭据 |
| `blocked by command policy [<规则编号>]: <为什么危险> — instead: <更安全的做法>` | 命令策略把这条命令判为**拦截**（严重后果兜底里「不可恢复损失」一档）；判定发生在建立连接之前，命令零字节到服务器 | **不要换写法重试**；把规则编号、风险说明与建议的替代做法解释给用户；用户确实要执行就报告 owner 裁量（规则停用是 owner 的决定） |
| `confirmation required by command policy [<规则编号>]: … — instead: … \| confirmation code XXXXXX: ask the user for consent …` | 命令策略判为**需确认**（锁在门外 / 整卷整池销毁 / 删除目标执行前无法确定一类）；随行附一枚一次性确认码 | **先向用户转述风险并征求同意**，同意后**同一条命令**原样重调 + `confirmation_code` + `confirmation_reason`（用户同意了什么的简短说明）；码 5 分钟有效、单次使用、绑定此命令与此服务器 |
| `confirmation required … the confirmation code was not accepted (wrong code, expired, already used, or issued for a different command or server); a new one follows — confirmation code YYYYYY` | 确认码没对上（码错 / 过期 / 已用 / 命令或服务器对不上）；系统已签发新码 | 用**新码**重走流程（先确认用户仍同意，再带新码与理由重调）；别拿旧码反复试 |
| `confirmation_reason is required together with confirmation_code — describe what the user consented to, then call again …` | 带了 `confirmation_code` 但缺 `confirmation_reason` | 补上理由（用户同意了什么的简短说明）重调同一条命令；已签发的码不作废 |
| `no open tunnel with id <id>` | 隧道已关/已被 ~10 分钟**空闲**回收/被 owner `tunnels kill` 拆除/revoke-disable 级联拆除/从未开过 | 正常现象；需要转发就重开 `forward_port` |
| `background task limit (32) reached — wait for a running task to finish or call exec_stop` | 后台任务满员：该 project 已有 32 个任务且全在运行（完成态会被驱逐腾位，全运行态才拒绝） | 等一个运行中任务自然结束，或对不要的任务调 `exec_stop`，再重试 `exec_background` |
| `unknown task_id — it may never have existed, expired after the retention window (1h), been evicted for capacity (32-task limit), or the broker restarted; task records are in-process only` | task_id 失效（三因 + 从未存在）：完成超 ~1h 保留期过期 / 32 满员时被驱逐 / **broker 重启**（任务表纯进程内，重启即全失） | broker 重启 = 在跑任务全死，重新安排活；过期/驱逐的按需重跑命令；失效 id 别反复重试 |
| `file <path> (<N> bytes) exceeds upload cap <cap> — refused before transfer` | 单文件严格大于 1 MiB，传输前被拒（零字节移动） | 按错误里的 size/cap **拆分或压缩**后重传 |
| `destination has N bytes available but the transfer needs M (...) — refusing before any byte moves` | relay 目标盘空间**已知不足**（预检 refusal，零字节移动） | 目标机腾空间后重跑（续传只补缺块）；或换目标路径。别对着满盘反复重试 |
| `... pass fresh=true to discard and restart`（manifest/partial 非常态族：partial 缺失 / 清单谎报进度 / 残留碎片等，错误文本各自明示） | relay 目标端续传状态不合法，断点不可信 | 判断该进度可弃 → `fresh=true` 重传；不可弃 → **报告 owner** 上目标机手工核查 `<to_path>.sshmgr-*` 残留 |
| `destination SFTP server lacks posix-rename@openssh.com — relay requires it ...` | relay 目标端不是支持该扩展的 OpenSSH 系 sftp（硬依赖，fail-closed） | **报告 owner** 换/升级目标端 sftp 服务；没有任何参数可绕 |
| `store is read-only (offline cache); connect to the server to mutate`（ErrReadOnly） | broker 跑在离线缓存模式（见三态环境） | **报告 owner** 切回在线/本机模式；**别**重试写操作（见下） |

## 部署形态（你通常无需分辨）

broker 有两种部署形态，核心工具面一致（含 `list_secrets`，同 profile 隔离；
缓存模式另有 `update_server_metadata`），
差别只在可写性：

| 形态 | 什么样 | 可写性 |
|---|---|---|
| 单机 / stdio | broker 跑在你脚下的机器，直开本机 vault（`.mcp.json` 里 `command: sshmgr`） | **可写** |
| 多机 / cache | broker 从本地只读快照服务（`mcp --cache`；经 `sshmgr pair` 或手工 `cache pull` 入网，见 [multi-machine.md](./multi-machine.md)） | **只读** |

（旧的第三形态「在线 serve」——agent 经远程 HTTP 直连 broker——已在 Plan 42 批1
移除：serve 不再提供任何远程 MCP 面。多机 agent 一律走本地缓存。）

**多机只读铁律**：多机形态下 agent **只读 + 执行**——加改删 server / profile /
project / 凭据、发码、批准配对等一切写操作都被拒（`ErrReadOnly`），写操作只属于
owner 的**管理面**（权威端 Web UI / `serve pair` 命令行）。这不是临时限制，
是设计：工作机上的快照可被吊销、可自毁、范围仅限授权 profile，而权威 vault 只在
一台机器上被一个写者（serve/管理面）修改。

**吊销三路径（owner 侧吊销后，你的工具面何时失效）**：

1. **project token 吊销、设备码仍活**：下次保鲜（在线 ≤30min）拉到的新快照已无
   该 project → 本地 spawn 闸拒绝——之后的调用报 token 无效。
2. **设备码吊销**：下次 pull 收到 pinned 401 → **quarantine**（本地缓存就地销毁）
   ——工具面即刻断供，spawn 报明确的 quarantined 归因错误。
3. **永离线设备**（一直不联网）：旧快照 + 本地 token 的可用窗口 =
   **`max_offline` 硬上限**（pair 下发默认 24h；到期缓存自毁拒载）——**不是 30
   分钟**；最终兜底是 owner 轮换服务器凭据。

cache 模式下你最可能撞上的具体形态是**首次连接一台缓存里没有 host key 记录的
服务器**——TOFU 想记录新 key 但 store 只读，拒绝并包着 ErrReadOnly。遇到任何
`read-only` 字样的报错：**报告 owner 去管理面操作，别重试写操作**——重试一万次
也是同样的错。

## 附录：贴进你项目的规则模板（CLAUDE.md / AGENTS.md）

把下面这段贴进你所在项目的 `CLAUDE.md` / `AGENTS.md`，让每个会话的你自动带着
这套纪律：

```text
# SSH 访问铁律
所有远程服务器操作（执行命令/传文件/端口转发）一律用 mcp__ssh__* 工具，
禁止裸 ssh/scp/寻找私钥（本机没有可用凭据，直连必失败）。
- 先 list_servers 拿真实 id（name ≠ id），动手前读目标机的 caveats/role。
- 提权用 sudo=true 参数，不要自己拼 sudo 前缀。
- 命令策略拦截（blocked by command policy…）不换写法重试，向用户解释规则
  编号与原因；需确认（confirmation required…）先征求用户同意，再用同一条
  命令带 confirmation_code + confirmation_reason 重调（码 5 分钟有效、单次、
  绑定此命令与服务器）。
- 工具报错先查 docs/agent-tools.md 错误对照表；read-only 报错=多机缓存
  （只读形态），报告 owner 去管理面操作，不要重试写操作。
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
| 超时是 result 不是 error（`timed_out=true` + 部分输出）；非零退出码同理；超时升级 = 共享三段式看门狗 `killWatchdog`（SIGKILL 请求+关会话通道 → 宽限 `SSHMGR_EXEC_KILL_GRACE` → 关整条连接），两个执行内核（exec.go 的 runSession、sudo.go 的 runSessionRaw）共用 | `internal/sshbroker/exec.go`（runSession / killWatchdog，符号名为准）；`internal/sshbroker/sudo.go`（runSessionRaw）；`internal/sshbroker/execenv.go`（宽限解析） |
| 输出每通道 1 MiB 封顶（`MaxOutputBytes = 1 << 20`）、前缀 + truncated + 真实总字节 | `internal/mcpserver/types.go`（MaxOutputBytes，符号名为准）；`internal/sshbroker/output.go`（cappedBuffer，符号名为准） |
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
| 隧道 **真空闲** ~10 分钟自动回收（按活动算：真实流量经 onActivity 钩子推进 lastActivity，持续在用不收）；扫描周期 1 分钟（实际 10–11 分钟内） | `internal/mcpserver/tunnels.go`（forwardIdleTimeout + Touch）；`internal/sshbroker/tunnel.go`（onActivity 30s 节流钩子） |
| forward_port `listen_host`：缺省/环回恒允许；非环回 **fail-closed 拒**（②a 移除后白名单无管理入口、恒空 = 环回 only；拒绝文本不披露原因细节；audit `bind_denied`） | `internal/mcpserver/core.go`（ForwardForProfile gate 链） |
| revoke/disable → 已开隧道 **≤15s（一个控制 tick）级联拆除**（端口不可达 + 镜像行删）；owner `tunnels kill` / `kill --project` 同域；store 持续故障 ≤~2min 有界关闭 | `internal/mcpserver/tunnels_control.go`（runControlTick / cascadeCheck）；`internal/cli/tunnels.go` |
| close_port 拆监听 + 背后 SSH 连接（broker 全程持有）；id 未知报 `no open tunnel with id ...` | `internal/mcpserver/tunnels.go:125-142`；`internal/mcpserver/core.go:537-540`；`internal/mcpserver/server.go:151` |
| 后台缺省/上限 24h + 回显（`clampBgTimeout` → `BgStartOutput.EffectiveTimeoutSeconds`）；无 env/workdir/stdin 参数（自组 `cd /dir && VAR=x cmd`） | `internal/mcpserver/tasks.go:618-627`；`internal/mcpserver/bgtools.go:146-150`；`internal/mcpserver/types.go:106-122` |
| 32/project 上限（表项 + in-flight 预约都计数）；满员驱逐最旧终态（零审计行）；全 running 拒绝 + 引导文案 | `internal/mcpserver/tasks.go:193-217` |
| 终态保留 ~1h 过期；sweeper 不删 running；任务表纯进程内——broker 重启任务全失 | `internal/mcpserver/tasks.go:25,305-323`；`internal/mcpserver/run.go:48-49`（stdio 收口） |
| exec_output：offset = 通道流内绝对字节游标、超前回拉流尾、落后保留窗诚实降级（truncated + lost_*_bytes）；每通道 1 MiB 尾部滚动窗 | `internal/mcpserver/tasks.go:438-499,506-526`；`internal/sshbroker/rolling.go:54`（Snapshot 三分支） |
| wait_seconds 钳制（0/缺省→0 不等待、>60→60）；wait≤30 建议；编码两态（text 有损 U+FFFD/切半、base64 字节精确、GBK 建议） | `internal/mcpserver/bgtools.go:156-170,203-227`；`internal/mcpserver/server.go:201` |
| unknown task_id 文案（从未存在 + 过期/驱逐/重启三因，`ErrBgUnknownTask` 逐字） | `internal/mcpserver/bgtools.go:175` |
| exec_stop 立即返回触发时刻 status（running）；已终态幂等；kill = 关会话 → 远端 SIGHUP、无信号楼梯、nohup/setsid 进程存活 | `internal/mcpserver/tasks.go:533-547`；`internal/mcpserver/server.go:220` |
| exec_output / exec_stop 零审计行（纯进程内读，与 list_servers 同级不审计；stop 触发的终态仍由任务侧落 exec-bg-end 生命周期行） | `internal/mcpserver/bgtools.go:177-250`；`internal/mcpserver/core.go:31-75`（list_servers 同无审计） |
| token 吊销/禁用**不杀**运行中后台任务（活到自然结束或 24h 钳定上限；运行中会话服务至进程退出；后台任务不在级联域——Plan 32 契约，Plan 42 后仍钉住） | `internal/mcpserver/revoke_semantics_test.go`（TestRevokedProjectKeepsBackgroundTaskRunning）；`internal/mcpserver/serve.go`（Close 只在进程关闭时清） |
| token 吊销/禁用 → **已开隧道 ≤15s（一个控制 tick）级联拆除**（端口不可达 + 镜像行删）；owner `tunnels kill` / `kill --project` 同域；store 持续故障 ≤~2min 有界关闭（②a 的 serve 侧 401 闸随远程 MCP 面移除——远程 client 经快照刷新/隔离感知吊销，见部署形态节的吊销三路径） | `internal/mcpserver/revoke_semantics_test.go`（TestRevokedProjectTunnelsTornByControlTick）；`internal/mcpserver/tunnels_control.go` |
| stdio 模式 token 无效 → broker 进程起不来（stderr `invalid or unknown token` 后退出） | `internal/mcpserver/run.go:29-35`；`internal/cli/mcp.go:70-73` |
| 工具报错形态 = IsError=true + 错误文本（非传输层错误） | `internal/mcpserver/server.go:83-89` |
| relay_file 三件套/零上下文：工具描述即口径（task_id 立返；结果/exec_output 只有元数据零文件内容）；第 12 个工具单源注册 | `internal/mcpserver/server.go:298`（描述模板 `%d`=构造期块大小）；`internal/mcpserver/relay.go:55`（RelayForProfile preflight ①–⑨） |
| relay 块大小 env seam：缺省 256 MiB、[16 MiB, 1 GiB] 之外构造失败（fail-closed）；`SSHMGR_TRANSFER_PARALLEL` v1 只接受 1 | `internal/mcpserver/relayenv.go:30,53`（resolveRelayChunk / resolveRelayParallel） |
| relay 单任务 72h 常量上限（非 env）；块数闸 16384（超出 refusal + 指引 raise SSHMGR_TRANSFER_CHUNK） | `internal/mcpserver/relayenv.go:69,76`（relayRunCap / relayMaxChunks）；`internal/mcpserver/relay.go:524` |
| relay 续传进度行（`relay plan: … (resumed N)` / `chunk i/n ok` / `relay done: root=… [file_sha256=…]`）经任务 stdout → exec_output 可见；file_sha256 仅 byte0→EOF 全程任务有，续传任务只有块 merkle 根 | `internal/mcpserver/relay.go:726,966-968` |
| relay posix-rename@openssh.com 硬依赖：stage 0 零 IO 探测，不支持即 failed（与 IO 错误分流） | `internal/mcpserver/relay.go:781` |
| relay 空间预检：已知不足 refusal（avail/need 文本）；报不了 = `space_check:"unavailable"` fail-open 知情继续 | `internal/mcpserver/relay.go:428-438` |
| broker 启动检测散落 SSH 凭据 → stderr `WARNING: ssh credential files detected`（仅本机 stdio 模式） | `internal/cli/mcp.go:63-67`；另见 docs/agent-access.md「隔离与排错」 |
| 工具面一致性（cache 与在线共用同一注册包装点与 profile 授权；只读工具含 `list_secrets`，仅写操作被拒 + 审计走 sidecar） | `internal/mcpserver/run.go:230-249`；`internal/mcpserver/server.go` |
| ErrReadOnly 文案 + SetReadOnly 语义（cache hydrate 后置只读） | `internal/store/store.go:46-54`；`internal/mcpserver/run.go:76` |
| 离线 cache 模式：未知 host key 被拒（TOFU 无法记录，包 ErrReadOnly；已知 key 正常匹配） | `internal/sshbroker/hostkey.go:33-34`；`internal/sshbroker/hostkey_readonly_test.go:33-58`；`internal/mcpserver/run.go:211-213` |
| 凭据字节永不出现在任何工具结果（owner 侧模型） | `internal/mcpserver/types.go:5`；docs/agent-access.md「安全模型回顾（铁律）」；../README.md "The security model" |
| 每次工具调用全程审计（project/server/action/status/命令/耗时） | `internal/mcpserver/core.go:81-89` 等；../README.md "What the agent gets" |
| 命令策略门在取凭据、建 SSH 连接之前（拦截 / 需确认都不建连接，拦截零字节到服务器）；判定内核是纯函数，权威端与客户端同一条代码路径，离线用最后一次拉到的规则 | `internal/mcpserver/policygate.go`（policyJudge）；`internal/mcpserver/core.go`（ExecCommandForProfile 的 gate 段）；`internal/mcpserver/bgtools.go`（后台同款门）；`internal/policy/policy.go`（Evaluate） |
| probe_connectivity：只测 TCP 建连与耗时、只探测登记过的共同网络组合（不测兜底主机）、结果只存发起实例本地；经服务器探测执行固定命令、不经命令策略但写审计 | 规格 #142；ADR 0007（命令策略是护栏不是安全边界） |
| list_shares：只读登记与挂载点约定、不做任何现场检测；可挂载范围 = 挂载密码机密项可写入列表 ∩ 会话可见服务器，交集为空不返回；旧权威端快照按 ADR 0014 报错 | `internal/mcpserver/list_shares.go`（ListSharesForProfile / sessionVisibleServerIDs）；`internal/store/shared_storage.go`（SharedStorageMountPoint）；`internal/mcpserver/run.go`（hydrateCacheStore 设置快照推荐版本） |
| 确认码与「服务器编号 + 命令完整文本」的哈希绑定、5 分钟有效、单次作废、进程内存不持久化；约 6 位字母数字（字母表去 0/O、1/I/L） | `internal/mcpserver/confirmcode.go`（confirmationTTL / confirmBindingKey / issue / consume / confirmationCodeAlphabet） |
| 命令策略审计动作：前台 exec-blocked / exec-confirm-required / exec-confirmed，后台 exec-bg-blocked / exec-bg-confirm-required / exec-bg-confirmed（Status 列存命中规则编号；带码执行行的 Command 列存「命令原文 + 确认理由」JSON 摘要） | `internal/mcpserver/core.go`；`internal/mcpserver/bgtools.go`；`internal/mcpserver/confirmcode.go`（gateExecConfirmation） |
