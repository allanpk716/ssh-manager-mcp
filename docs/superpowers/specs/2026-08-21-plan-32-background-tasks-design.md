# Plan 32 设计：后台任务三件套 exec_background / exec_output / exec_stop

> backlog #13 · P0。2026-08-21 grilling 已拍板的决策（三工具形态、broker 进程内存任务表、24h 运行上限、完成后保留 1h、每通道滚动 1 MiB、字节 offset 游标、前台保 5min 硬顶但钳制改响、不加 env/workdir 参数、PTY 不做）不在本文重议；本文是实现设计。brainstorm 补拍板（2026-08-21）：**任务数上限 32/project**、**审计只记状态转换**（exec_output 不审计）、**后台缺省 timeout 即 24h 上限**、**offset 落后缓冲起点时诚实降级**（truncated + lost 计数，不静默跳字节、不报错拒读）。

## 0. 目标

补齐「日常使用没有级」缺口：长活命令（编译/训练/日志跟踪）当前只能靠前台 `exec_command` 反复整跑，5min 硬顶内拿不到增量。三件套对齐 Claude Code Bash 工具体验（agent 零学习成本）：

- `exec_background(server_id, command, sudo?, timeout_seconds?)` → task_id
- `exec_output(task_id, wait_seconds?, stdout_offset?, stderr_offset?)` → 增量输出 + 运行/终态
- `exec_stop(task_id)` → 停

前台 `exec_command` 行为不变（默认 120s / 硬顶 5min），唯一变化：静默钳制改响——返回体加 `effective_timeout_seconds`。

## 1. 架构总览

```
exec_background ─┐  mcpserver.TaskManager（TunnelManager 的结构性镜像）        [新]
exec_output     ─┼─   map[taskID]*bgTask + mu（单锁房子风格）
exec_stop       ─┘   sweeper goroutine（1min tick，StartSweeper/CloseAll 同款）
sshbroker:  runSession(ctx, cmd, timeout, stdout, stderr io.Writer) 内核     [新 seam]
            Exec / ExecSudo = 现签名不变，内部改经内核（cappedBuffer 包装，前台零行为变化）
            rollingBuffer：保留尾 N 字节 + 字节游标 + 内部锁                  [新]
```

- **TaskManager per-Server 实例**（跨 project 隔离结构性成立，照 tunnels 先例）：`NewServer*` 同时构造 `tunnels` + `tasks` 并返回；`RunStdio` / `RunStdioCache` defer 两个 CloseAll；serve 的 `scopedServer` 增 `tasks` 字段、`ServeRunner.Close` 同步关。构造点唯一，三模式（stdio / serve / --cache 离线）一处接线。
- **每任务独占一条 ssh.Client**（forward_port 同姿势）：`exec_background` 同步走完 exec_command 的全部门链（profile 门 → GetServer → AuthForServer → HostKeyTOFU → Connect，含 Plan 31 错误清洗——连接错误当场返回给 agent），成功后才入表；Client 生死全归 TaskManager。32 任务 = 至多 32 条连接，资源上界可预期。
- **task_id = UUID v4**（tunnel 同款，零信息量，无 host 泄露面）。
- **任务 ctx 与工具调用 ctx 解耦**：任务 goroutine 挂 `context.Background()+WithTimeout(钳定 timeout)`——exec_background 返回后任务必须继续活；tool-call ctx 只管启动那一次连接。
- task 记录**不持有任何秘密**：sudo 密码在任务 goroutine 启动时传给 ExecSudo 内核用完即弃，struct 里不存。

## 2. sshbroker 改动（writer-seam + rollingBuffer）

### 2.1 runSession 内核抽取

`Exec`（exec.go:33）现体拆出 `runSession(ctx, cmd, timeout, stdout, stderr io.Writer) (exitCode, timedOut, err)`：NewSession、watchdog（`Signal(SIGKILL)+sess.Close()` on ctx.Done、`done` channel 防漏）、ExitError/timeout/cancel 分类逻辑原样搬移。`Exec(cmd, timeout, maxBytes)` = runSession + 两个 cappedBuffer + 组装 ExecResult；`ExecSudo`（sudo.go 的密码喂入舞步）同型拆 `runSudoSession` 收 writers。**验收锚：前台路径既有测试全绿零改动 = 回归网**。

### 2.2 rollingBuffer

```go
type rollingBuffer struct { mu sync.Mutex; buf []byte; cap, total int64 }
// Write: 追加，超 cap 丢头；total 累计全流字节数
// Snapshot(since int64) (chunk []byte, next int64, start int64)
//   start = total - len(buf)（缓冲区首字节的流内偏移）
//   since <  start → gap：chunk=整个 buf、truncated、lost = start - since（诚实降级）
//   since >= total → 空 chunk（无新字节）
//   否则           → chunk = buf[since-start:]，next = since + len(chunk)
```

- 内部锁必须：session goroutine 写 / exec_output 快照读并发（-race 测试钉住）。
- 滚动容量 = `MaxOutputBytes`（1 MiB，复用现有常量——前台留前缀 1 MiB、后台留尾部 1 MiB，同一数值两种保留策略，语义各自文档）。
- 通道分离：stdout/stderr 各一个实例、各一条游标（两通道推进速率不同，单游标无法对齐——**backlog 措辞 `since_offset` 的刻意修正**，brainstorm 已确认方向）。

## 3. TaskManager 状态机与生命周期

```
running ──自然退出──▶ done(exit_code)          # ExitError/0 都算 done，码值看 exit_code
   │─────▶ failed(ssh 层错误)                  # 会话级 err≠nil，error 文本入记录
   │─24h/钳定 timeout 到点─▶ timeout            # ctx deadline → watchdog 杀会话
   │─exec_stop────▶ stopped                    # stopRequested 标志 + cancel 区分于其他 cancel
   └─CloseAll────▶ 进程内消失                   # 不保证终态落审计（见 §5 留痕）
任一终态保留 1h（可继续 exec_output 取尾）→ sweeper 删表项、缓冲内存释放
```

- **启动钳制（纯函数 `clampBackgroundTimeout`）**：`<=0/缺省 → 24h`；`>24h → 24h`；响式回显 `effective_timeout_seconds`。
- **上限 32/project**（含保留期内的已终态任务，map 大小即计数）：超限 `exec_background` 返回明确错误 + 引导（先 exec_stop / 等已终态任务过期）。
- **sweeper（1min tick，`SweepExpired` 可直调供白盒测试，照 SweepIdle 先例）**：
  - 终态 && `now > finishedAt+1h` → 删表项；
  - running && `now > deadline+宽限`（正常路径 ctx 会先到，此为防御）→ 再 cancel 一次；**绝不删除 running 态表项**（sess.Close 保证 Run 必解阻塞，既有 Exec 契约，无界 goroutine 泄漏不存在）。
- **常量 seam（Plan 19 T7 教训：新生产路径必须可被测试触碰）**：`24h 运行上限` / `1h 保留` / `32 上限` 三常量做成包级 var + 构造期 env 覆盖（`SSHMGR_BG_RUN_CAP` / `SSHMGR_BG_RETAIN` / `SSHMGR_BG_MAX_TASKS`，time.ParseDuration/strconv 解析，**非法值 → 构造报错拒绝启动**，fail-closed）；生产默认值钉死在代码里，env 属测试/运维旋钮，不进 agent 可见面。
- **exec_output 的 wait 钳制（纯函数 `clampWaitSeconds`，单测覆盖，不需要 env seam）**：`<=0 → 0`（不等待立即返回）；`>60 → 60`。长轮询实现按小值测，不真等 60s。
- **broker 重启即失**：任务表纯进程内，无持久化、无恢复——agent-tools.md 明示（agent 拿旧 task_id 会得到 unknown task 错误，错误文本附「broker may have restarted」提示）。

## 4. 工具契约（jsonschema 恒定字段，空值显式表达——房子惯例）

### exec_background

入参：`server_id, command, sudo?, timeout_seconds?`。出参：

```json
{ "task_id": "uuid", "effective_timeout_seconds": 86400, "status": "running" }
```

- 错误分支与 exec_command 逐一对齐（denied / not found / no_credential / auth_error / hostkey_mismatch / connect_error / cancelled / no_sudo / 超限 / error），文本经同一 Connect 清洗链，**Plan 31 no-leak 回归网扩到本工具全错误分支**。
- 无 stdin、无 env/workdir 参数（grilling 已拍板：agent 自组 `cd /dir && VAR=x cmd`——agent-tools.md 惯用法节）。

### exec_output

入参：`task_id, wait_seconds?, stdout_offset?, stderr_offset?`（offset 为对应通道的**流内绝对字节偏移**，`0`/缺省 = 流首——首轮即落后会触发诚实降级，不静默跳）。出参：

```json
{ "status": "running|done|stopped|timeout|failed",
  "exit_code": 0, "error": "",
  "stdout": "增量字节", "stderr": "",
  "next_stdout_offset": 1234, "next_stderr_offset": 0,
  "stdout_bytes_total": 5678, "stderr_bytes_total": 0,
  "truncated": false, "lost_stdout_bytes": 0, "lost_stderr_bytes": 0 }
```

- **长轮询语义**：wait>0 时等到「任一通道有新字节（相对所传 offset）或任务离开 running 或 wait 钳定上限」才返回；tool-call ctx 取消 → 立即返回当前快照（不报错）。
- **诚实降级**：offset < 缓冲首偏移 → `truncated:true` + `lost_*_bytes`（丢弃量）+ 从缓冲首给可用字节。
- **纯进程内读**：不碰服务器/凭据/任务状态 → **不审计**（与 list_servers 同级；tail -f 分钟级轮询若逐次审计会把 audit_log 淹成轮询日志——owner 拍板）。
- 未知/过期 task_id → 报错（附 broker 重启提示）。幂等：重复传同 offset 拿同数据。

### exec_stop

入参：`task_id`。出参：`{ "status": "终态" }`。运行中 → `stopRequested` 标志 + cancel → watchdog `Signal(SIGKILL)+sess.Close()`（**x/crypto/ssh 无信号楼梯可用：OpenSSH 服务端忽略 signal 请求，实际靠会话关闭 → 远端 SIGHUP；nohup/setsid 的远端进程会活——如实写进 agent-tools.md，与真 ssh 杀会话同语义**）→ 终态 stopped。对已终态任务 = 幂等 ok（回其终态，不产生新审计行）。未知 → 报错。

## 5. 审计口径（只记状态转换，owner 拍板）

| 行 | Action | Command 字段 | Status | 其他 |
|---|---|---|---|---|
| 启动尝试 | `exec-bg-start` | 命令原文 | denied/hostkey_mismatch/connect_error/no_sudo/no_credential/auth_error/cancelled/error/ok（全分支，照 exec_command 词汇表） | Sudo、ServerID、ProjectID；ok = 已入表运行 |
| 终态 | `exec-bg-end` | **task_id**（关联键，照 close-forward 记 tunnel_id 先例） | ok（自然退出，码值看 ExitCode）/ stopped / timeout / failed | ExitCode、DurationMS=运行时长 |

- `exec_output` 零审计行；`exec_stop` 对已终态任务零行（无转换）。
- **留痕（诚实缺口）**：broker 重启杀运行中任务 → 有 start 行无 end 行——缺失的 end 行本身就是取证信号；CloseAll 时不做 best-effort 补写（stdio 拆链时序不可靠，伪数据比缺行更糟）。
- 终态行由任务 goroutine 落笔（终态判定与写行同点，无竞态窗口）。

## 6. 前台 exec_command 钳制改响

- `ExecOutput` 增 `EffectiveTimeoutSeconds int`（`json:"effective_timeout_seconds"`）——**恒存在**（schema 稳定，agent 总可读，brainstorm 拍板）；值 = clamp 后实际生效秒数。
- 工具描述（server.go:78）补一句 5min 硬顶与 effective 回显。
- owner CLI `ssh` 路径不受影响（ownerSSHDeadline 独立）。
- **纯增量、非破坏**：新增字段对既有消费方向后兼容。

## 7. 测试与验收

验收方法学：生命周期用**时间可控的白盒测试**（同包直改常量/直调 SweepExpired）+ **agent 可见面用 in-memory MCP 客户端 e2e**（照 TestE2EIronRule 形态）；SSH 行为层全部走 in-process testsshd，docker-gated conformance 补真 OpenSSH 一层。

| 层 | 内容 |
|---|---|
| 单测：rollingBuffer | 游标推进 / gap（offset<start 的 lost 计数）/ since≥total 空 chunk / 超 cap 丢头 / **并发读写 -race** / cap=0 边界 |
| 单测：runSession 抽取 | **前台回归网：exec/sudo 既有测试全绿零改动**（拆移不改行为的直接证据）；writers 直写断言 |
| 单测：TaskManager | 32 上限（超限报错+引导）/ clampBackgroundTimeout、clampWaitSeconds 纯函数全分支 / sweeper 删终态过期项、不删 running / CloseAll 后表空 + client 全关（捕获 ref 断言后续操作报错，照 tunnels 无泄漏验证姿势）/ stop 已终态幂等 |
| 单测：testsshd 生命周期 e2e | 起后台（脚本逐行 sleep 输出）→ exec_output 拿增量（offset 推进）→ 自然退出拿 exit_code → stop 路径（cancel→stopped）→ timeout 路径（timeout_seconds=1 跑 sleep）→ sudo 路径；「exec_background 返回后连接仍活」断言 |
| 单测：ForProfile 包装 | profile 拒绝（denied）/ 全错误分支 **no-leak 断言扩三新工具**（Plan 31 assertBranch 分支签名锁同型）/ 审计行两型（start 全分支 + end 四终态，task_id 关联键）/ exec_output 无审计行断言 / 未知 task_id 报错文本含重启提示 |
| e2e：in-memory MCP | 三件套全流（initialize → exec_background → 轮询 → stop），零容忍面自动扩展（**BrokerTools 单源切片追加三名——scoreT6/scoreT8 结构性覆盖新工具，无需改 scorer**；grep `BrokerTools` 硬编码下标/长度的测试连带核对） |
| conformance（§13 gated） | 真 OpenSSH：后台 start→增量→stop 生命周期 + 前台 interop/differential 零回归（runSession 拆移的 SSH 层证据）；**differences-ledger 登记偏差：三件套无 ssh 二进制对应物（`ssh host 'cmd &'`+远端落盘是不同动物），不做 differential** |
| eval（§12 gated） | 新任务 T9：agent 起后台逐行输出任务 → wait 轮询增量 → 收齐行 → stop 一个 sleep 任务；确定性 scorer（行齐 + offset 推进）；`seedBroker` 零改动（按 id 寻址） |
| 前台钳制响应 | `effective_timeout_seconds` 恒存在 + 钳定值正确（0→120、>300→300、中值直通）单测 |
| env seam | 三 env 生效/非法值拒绝启动/缺省回落生产默认值 |

## 8. 文档落点

| 文件 | 改动 |
|---|---|
| `agent-tools.md` | 三件套节：语义 + **tail -f / journalctl -f 轮询惯用法**（wait≤30s 配合客户端超时）+ `cd /dir && VAR=x cmd` 惯用法 + **重启即失** + kill 语义诚实段（SIGHUP/nohup 存活）+ offset/诚实降级说明 + 前台 effective_timeout_seconds；错误对照表补新错误形态 |
| `README.md` | 工具清单加三行；v0.10 callout（纯增量） |
| `agent-access.md` | 「断连语义（四层）」各层补后台任务行为（同隧道类材料）：stdio=会话重启任务即失 / serve=token revoke 后下次 exec_output 逐请求 401、**运行中任务不被 revoke 杀**（活到 24h 钳定上限或 exec_stop）/ 离线 cache 无涉 |
| `threat-model.md` | §3.5 补一句：任务表=进程内状态（同隧道类），非新增披露面；sudo 密码传递不落记录 |
| `concepts.md` | 工具清单提三件套一句（长活走后台） |
| `compat-matrix.md` | v0.10.0 行：纯增量（3 新工具 + ExecOutput 新字段），无破坏；已验证组合表发版后双端实测回写（惯例） |
| `docs/ssh-conformance/differences-ledger.md` | 三件套偏差登记（无 ssh 对应物、kill=会话关闭 SIGHUP 语义） |
| `server.go` 工具描述 | 三新工具描述写足惯用法（Agent 说明书即描述）；exec_command 描述补 effective 句 |

## 9. 明确不做（scope 纪律留痕）

- **PTY / 交互式会话**（grilling 已拍板，长活一律走后台）。
- **stdin / env / workdir 参数**（agent 自组命令行）。
- **任务持久化 / broker 重启恢复 / 跨进程任务表**（backlog 拍板进程内即失）。
- **exec_list 工具**（agent 自己记 task_id；YAGNI）。
- **流式推送**（pull 模型：反复 exec_output 拉增量，backlog 已拍板）。
- **跨 project 任务可见性**（结构性不可见：per-Server 实例隔离，无需额外 ACL）。
- **优雅信号楼梯**（SIGTERM→SIGKILL：OpenSSH 忽略 signal 请求，协议层不存在该选项——如实文档而非假装支持）。
- **远端落盘轮询方案**（brainstorm 方案三，已弃：污染远端 FS、stderr 通道合流、stop 要追杀远端 PID）。
