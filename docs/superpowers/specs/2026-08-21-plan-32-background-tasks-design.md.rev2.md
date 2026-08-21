# Plan 32 设计：后台任务三件套 exec_background / exec_output / exec_stop

> backlog #13 · P0。2026-08-21 grilling 已拍板的决策（三工具形态、broker 进程内存任务表、24h 运行上限、完成后保留 1h、每通道滚动 1 MiB、字节 offset 游标、前台保 5min 硬顶但钳制改响、不加 env/workdir 参数、PTY 不做）不在本文重议；本文是实现设计。brainstorm 补拍板（2026-08-21）：**任务数上限 32/project**、**审计只记状态转换**（exec_output 不审计）、**后台缺省 timeout 即 24h 上限**、**offset 落后缓冲起点时诚实降级**（truncated + lost 计数，不静默跳字节、不报错拒读）。
> 本版为第三版（2026-08-21 二轮收敛修订）。第二版吸收 11 项必改（配额驱逐/failed 态防御清洗/唤醒原语/游标三分支与深拷贝/连接即关/revoke 钉住/stop 语义/竞态互斥/边界校验/内存上界），两项经实验证伪排除（排空窗口——库 Wait 语义实测已保证；运行期错误含地址——三种连接死亡形态实测均为无地址文本）。本版再吸收二轮 11 项必改：**输出编码参数**（text 默认有损/base64 字节精确，owner 拍板）、**唤醒原语升级为代际广播+钉死等待回路与绝对 deadline**、**入表路径持锁复检**（驱逐/拒绝/入表同段互斥）、**CloseAll 机制钉死+终态补写抑制**、拒绝路径 client 兜底、驱逐时机钉死、负值入参统一拒负、exec_stop 返回态写死、幂等措辞改 offset 稳定性、unknown task_id 文案三因泛化、内存上界补瞬时口径。

## 0. 目标

补齐「日常使用没有级」缺口：长活命令（编译/训练/日志跟踪）当前只能靠前台 `exec_command` 反复整跑，5min 硬顶内拿不到增量。三件套对齐 Claude Code Bash 工具体验（agent 零学习成本）：

- `exec_background(server_id, command, sudo?, timeout_seconds?)` → task_id
- `exec_output(task_id, wait_seconds?, stdout_offset?, stderr_offset?, encoding?)` → 增量输出 + 运行/终态
- `exec_stop(task_id)` → 停

前台 `exec_command` 行为不变（默认 120s / 硬顶 5min），唯一变化：静默钳制改响——返回体加 `effective_timeout_seconds`。

## 1. 架构总览

```
exec_background ─┐  mcpserver.TaskManager（TunnelManager 的结构性镜像）        [新]
exec_output     ─┼─   map[taskID]*bgTask + mu（单锁房子风格）
exec_stop       ─┘   sweeper goroutine（1min tick，StartSweeper/CloseAll 同款）
sshbroker:  runSession(ctx, cmd, timeout, stdout, stderr io.Writer) 内核     [新 seam]
            Exec / ExecSudo = 现签名不变，内部改经内核（cappedBuffer 包装，前台零行为变化）
            rollingBuffer：保留尾 N 字节 + 字节游标 + 内部锁 + 变更通知      [新]
```

- **TaskManager per-Server 实例**（跨 project 隔离结构性成立，照 tunnels 先例）：`NewServer*` 同时构造 `tunnels` + `tasks` 并返回；`RunStdio` / `RunStdioCache` defer 两个 CloseAll；serve 的 `scopedServer` 增 `tasks` 字段、`ServeRunner.Close` 同步关。构造点唯一，三模式（stdio / serve / --cache 离线）一处接线。
- **每任务独占一条 ssh.Client**（forward_port 同姿势）：`exec_background` 同步走完 exec_command 的全部门链（profile 门 → GetServer → AuthForServer → HostKeyTOFU → Connect，含 Plan 31 错误清洗——连接错误当场返回给 agent）；**入表路径钉死**：Connect（慢操作，锁外）成功后**持锁复检**（计数+驱逐决策+入表同段互斥，见 §3），任何拒绝/失败路径**兜底 close 刚建的 client**（零连接泄漏）。**Client 生死全归 TaskManager，session 返回即关 Client**——终态保留期只保 task 记录 + rollingBuffer，绝不保留空闲连接。32 任务 = 至多 32 条**运行中**连接。
- **资源上界（内存）**：**常驻**缓冲上界 = 每通道 1 MiB × 2 通道 × 32 任务 = **64 MiB/project**（serve 模式随 project 数线性放大）；**瞬时**分配另有界：每次 exec_output 的 Snapshot 深拷贝（≤1 MiB/通道）+ 响应序列化，按并发请求数线性（无新无界项）。文档如实点明。
- **task_id = UUID v4**（tunnel 同款，零信息量，无 host 泄露面）。
- **任务 ctx 与工具调用 ctx 解耦**：任务 goroutine 挂 `context.Background()+WithTimeout(钳定 timeout)`——exec_background 返回后任务必须继续活；tool-call ctx 只管启动那一次连接。
- task 记录**不持有任何秘密**：sudo 密码在任务 goroutine 启动时传给 ExecSudo 内核用完即弃，struct 里不存。

## 2. sshbroker 改动（writer-seam + rollingBuffer）

### 2.1 runSession 内核抽取

`Exec`（exec.go:33）现体拆出 `runSession(ctx, cmd, timeout, stdout, stderr io.Writer) (exitCode, timedOut, err)`：NewSession、watchdog（`Signal(SIGKILL)+sess.Close()` on ctx.Done、`done` channel 防漏）、ExitError/timeout/cancel 分类逻辑原样搬移。`Exec(cmd, timeout, maxBytes)` = runSession + 两个 cappedBuffer + 组装 ExecResult；`ExecSudo`（sudo.go 的密码喂入舞步）同型拆 `runSudoSession` 收 writers。**验收锚：前台路径既有测试全绿零改动 = 回归网**。

### 2.2 rollingBuffer

```go
type rollingBuffer struct { mu sync.Mutex; buf []byte; cap, total int64 }
// Write: 追加，超 cap 丢头；total 累计全流字节数；写后触发 §2.3 的代际广播
// Snapshot(since int64) (chunk []byte, next int64, start int64)   —— 三个分支的 next 全部钉死：
//   start = total - len(buf)（缓冲区首字节的流内偏移）
//   since <  start（gap）→ chunk=整个 buf、truncated、lost = start - since、**next = total**
//   since >= total（超前）→ 空 chunk、**next = total**（自恢复：游标回拉到流尾，绝不把 agent 的越界值原样回传——否则游标永久停在流前方静默漏字节）
//   否则（正常）→ chunk = buf[since-start:]、next = since + len(chunk)
```

- **Snapshot 在锁内深拷贝返回**（append/copy 出新切片），绝不返回内部 buf 的子切片——否则逃逸切片会被后续 Write 的丢头挪动腐蚀，且腐蚀全程持锁、**-race 抓不到**（该腐蚀路径属必须靠规范禁止而非测试捕捉的类别）。
- 内部锁必须：session goroutine 写 / exec_output 快照读并发（-race 测试钉住读写竞态；切片腐蚀由上述拷贝规范杜绝）。
- 滚动容量 = `MaxOutputBytes`（1 MiB，复用现有常量——前台留前缀 1 MiB、后台留尾部 1 MiB，同一数值两种保留策略，语义各自文档）。
- 通道分离：stdout/stderr 各一个实例、各一条游标（两通道推进速率不同，单游标无法对齐——**backlog 措辞 `since_offset` 的刻意修正**，brainstorm 已确认方向）。

### 2.3 变更通知（代际广播）与等待回路（钉死）

**通知原语 = 代际广播**（解决单消费者饥饿与丢唤醒）：每任务持有 `gen uint64` + `waitCh chan struct{}`。`notify()` 在 TaskManager.mu 内执行 `close(waitCh); waitCh = make(chan struct{}); gen++`——**close 唤醒所有持有旧代通道引用的等待者**（广播），新等待者取新代。触发点两处：① rollingBuffer 落笔后（新字节）；② 任务状态离开 running（终态转换）。

**等待回路（结构钉死，禁自由发挥）**：

```
deadline = now + clampWaitSeconds(wait)          // 绝对截止，进入时一次定死，不随唤醒重置
loop:
    mu.Lock()
      ch := t.waitCh                              // 代捕获
      cond := (任一通道相对所传 offset 有新字节) || (status != running)   // 条件检查
    mu.Unlock()
    if cond || 到达 deadline || ctx.Done: 返回当前快照
    select {
      case <-ch:          continue loop          // 唤醒后必须重查条件——虚假/陈旧唤醒不满足则续等（不提前返回）
      case <-time.After(deadline - now):          // 每轮以剩余时间建 timer，总预算恒 ≤ 钳定 wait
      case <-ctx.Done:    返回当前快照
    }
```

- **代捕获与条件检查同持一把锁** → 无丢唤醒窗口（notify 也持同锁）。
- **绝对 deadline 不重置**：高频写另一通道时，饥饿通道的实际阻塞仍 ≤ wait 钳定值（timer 每轮只等剩余时间）。
- **禁止轮询 sleep 退化实现**。
- 测试钉三件：虚假/陈旧唤醒不提前返回；N 个并发等待者同任务全部被唤醒（广播）；wait 预算不被高频写重置（另一通道高频写，断言总阻塞 ≤ wait+ε）。

## 3. TaskManager 状态机与生命周期

```
running ──自然退出──▶ done(exit_code)          # ExitError/0 都算 done，码值看 exit_code
   │─────▶ failed(ssh 层错误)                  # 会话级 err≠nil，error 文本入记录（过 §4 清洗）
   │─24h/钳定 timeout 到点─▶ timeout            # ctx deadline → watchdog 杀会话
   │─exec_stop────▶ stopped                    # stopRequested 标志 + cancel 区分于其他 cancel
   └─CloseAll────▶ 进程内消失                   # 机制顺序钉死见下；终态不落审计（closed 抑制）
终态项保留 ≤1h（可继续 exec_output 取尾输出）→ sweeper 删表项、缓冲内存释放（连接已在终态时关，见 §1）
```

- **启动钳制（纯函数 `clampBackgroundTimeout`）**：`0/缺省 → 24h`；`>24h → 24h`；响式回显 `effective_timeout_seconds`。**负值不进钳制函数**——schema 层拒（见 §4 入参口径）。
- **上限 32/project（map 大小即计数）+ 满员驱逐（入表路径钉死）**：Connect 成功后**持 TaskManager.mu 复检**：未满员 → 直接入表；满员且存在终态项 → **驱逐最旧终态项**（`finishedAt` 最旧优先；此刻驱逐 = Connect 已成功，连接失败零驱逐损耗）后入表；满员且全 running → 拒绝（状态=超限），**锁外 close 刚建的 client**（零泄漏）。计数+驱逐+入表同段互斥——**无锁外 check-then-act**（与 stop 竞态同款原则）。驱逐是保留期记账、不产生审计行（end 行已落；与 sweeper 过期删除同级）。全 running 拒绝的引导文案：「wait for a running task to finish or call exec_stop」。
- **exec_stop 立即返回**（异步取消，不阻塞等终态）：置 `stopRequested` + cancel 后即返回；**返回值写死 = 触发时刻的当前 status**（对 running 任务即 `"running"`——status 枚举无 "stopping"，终态经 exec_output 观察）。对已终态任务 = 幂等 ok（回其终态，不产生新审计行）。
- **stop/自然退出竞态（钉死互斥边界）**：stopRequested 置位、终态写入、审计落笔**全部经 TaskManager.mu**；stop 路径持锁后「见终态即返、不覆盖状态、不补审计行」——自然退出先落 done 则 stop 返回 done（幂等语义），stop 先置位则任务 goroutine 落 stopped。无锁外 check-then-act。
- **CloseAll（机制顺序钉死）**：① 持锁置 `closed=true`、摘全部表项、对每个 running 任务 cancel（触发 watchdog 杀会话）+ close 其 Client；② `wg.Wait` 有界等任务 goroutine 退出（sess.Close 保证 Run 必解阻塞，既有 Exec 契约）。**终态补写抑制**：任务 goroutine 收尾时持锁见 `closed==true` → 跳过终态状态写入与 exec-bg-end 审计行（兑现「不做 best-effort 补写」口径——否则 goroutine 会按普通路径照写）。goroutine 仍负责关自己持有的资源（幂等）。
- **revoke 不杀任务（机制声明 + 测试钉住）**：serve 模式 token revoke 走逐请求 401（verifyToken），**绝不触发 TaskManager.CloseAll / scopedServer 驱逐**（现状如此——scopedServer 只在 ServeRunner.Close 进程关闭时清；spec 钉死该不变量 + 测试 `TestRevokedProjectKeepsBackgroundTaskRunning`，照 Plan 25 `TestRevokedProjectKeepsOpenTunnelForwarding` 先例）。**留痕代价**：revoke 后运行中任务最长活到 24h 钳定上限且占用 32 槽之一（revoke 后 exec_output/exec_stop 均 401，无停手段）——已文档化的取舍，威胁模型口径与隧道一致。
- **sweeper（1min tick，`SweepExpired` 可直调供白盒测试，照 SweepIdle 先例）**：
  - 终态 && `now > finishedAt+1h` → 删表项（记账，无审计行）；
  - 满员驱逐在 exec_background 调用点同步执行（§上），不归 sweeper；
  - running && `now > deadline+宽限`（正常路径 ctx 会先到，此为防御）→ 再 cancel 一次；**绝不删除 running 态表项**。
- **常量 seam（Plan 19 T7 教训：新生产路径必须可被测试触碰）**：`24h 运行上限` / `1h 保留` / `32 上限` 三常量做成包级 var + 构造期 env 覆盖（`SSHMGR_BG_RUN_CAP` / `SSHMGR_BG_RETAIN` / `SSHMGR_BG_MAX_TASKS`，time.ParseDuration/strconv 解析，**非法值或非正数（"0"/负值——ParseDuration 对它们是合法解析但语义破坏）→ 构造报错拒绝启动**，fail-closed）；生产默认值钉死在代码里，env 属测试/运维旋钮，不进 agent 可见面。
- **exec_output 的 wait 钳制（纯函数 `clampWaitSeconds`，单测覆盖，不需要 env seam）**：`0/缺省 → 0`（不等待立即返回）；`>60 → 60`；负值 schema 层拒。长轮询实现按小值测，不真等 60s。
- **broker 重启即失**：任务表纯进程内，无持久化、无恢复——agent-tools.md 明示；unknown task_id 错误文案见 §4。

## 4. 工具契约（jsonschema 恒定字段，空值显式表达——房子惯例）

**入参口径（统一）**：四个数值入参 `timeout_seconds` / `wait_seconds` / `stdout_offset` / `stderr_offset` **schema 层一律拒绝负值**；`0`/缺省保留各自默认语义（timeout→24h、wait→不等待、offset→流首）。

### exec_background

入参：`server_id, command, sudo?, timeout_seconds?`。出参：

```json
{ "task_id": "uuid", "effective_timeout_seconds": 86400, "status": "running" }
```

- 错误分支与 exec_command 逐一对齐（denied / not found / no_credential / auth_error / hostkey_mismatch / connect_error / cancelled / no_sudo / 超限 / error），文本经同一 Connect 清洗链，**Plan 31 no-leak 回归网扩到本工具全错误分支**。
- 无 stdin、无 env/workdir 参数（grilling 已拍板：agent 自组 `cd /dir && VAR=x cmd`——agent-tools.md 惯用法节）。

### exec_output

入参：`task_id, wait_seconds?, stdout_offset?, stderr_offset?, encoding?`。offset 为对应通道的**流内绝对字节偏移**（`0`/缺省 = 流首——首轮即落后触发诚实降级，不静默跳）；`encoding`：**`"text"`（默认）** / **`"base64"`**（owner 拍板）。出参：

```json
{ "status": "running|done|stopped|timeout|failed",
  "exit_code": 0, "error": "",
  "stdout": "增量字节（按 encoding 编码）", "stderr": "",
  "next_stdout_offset": 1234, "next_stderr_offset": 0,
  "stdout_bytes_total": 5678, "stderr_bytes_total": 0,
  "truncated": false, "lost_stdout_bytes": 0, "lost_stderr_bytes": 0 }
```

- **编码语义（钉死）**：`text` = 与前台 exec_command 同语义——原始字节按 UTF-8 直入 JSON 字符串，**非法 UTF-8 序列被替换为 U+FFFD（有损）**，多字节字符**可能被窗口边界切断**（前窗尾+后窗头各损半，属 text 模式固有语义，文档写明；GBK 等非 UTF-8 日志建议 base64）；`base64` = chunk 字节精确（agent 侧解码得原始字节，跨窗口重组无损，二进制安全）。**两模式 offset 恒为字节口径**（同一游标，切换编码不改语义）。
- **长轮询语义**：wait>0 时按 §2.3 钉死的回路等到「任一通道有新字节（相对所传 offset）或任务离开 running 或绝对 deadline」才返回；tool-call ctx 取消 → 立即返回当前快照（不报错）。
- **诚实降级**：offset < 缓冲首偏移 → `truncated:true` + `lost_*_bytes`（丢弃量）+ 从缓冲首给可用字节。
- **offset 稳定性**（措辞修正，不称"幂等"）：缓冲未滚动时，同 offset 重复调用返回同数据；若两次调用之间发生丢头滚动，第二次按诚实降级返回（truncated+lost）——绝不静默跳。
- **纯进程内读**：不碰服务器/凭据/任务状态 → **不审计**（与 list_servers 同级；tail -f 分钟级轮询若逐次审计会把 audit_log 淹成轮询日志——owner 拍板）。
- **unknown task_id**（泛化三因，防误导排障）：`unknown task_id — it may never have existed, expired after the retention window (1h), been evicted for capacity (32-task limit), or the broker restarted; task records are in-process only`。agent-tools.md 同步写明驱逐/过期/重启三因。

### exec_stop

入参：`task_id`。出参：`{ "status": "<触发时刻的当前 status>" }`（运行中任务触发停止后返回 `"running"`；已终态幂等回其终态）。**立即返回不阻塞**（§3）。运行中 → `stopRequested` 标志 + cancel → watchdog `Signal(SIGKILL)+sess.Close()`（**x/crypto/ssh 无信号楼梯可用：OpenSSH 服务端忽略 signal 请求，实际靠会话关闭 → 远端 SIGHUP；nohup/setsid 的远端进程会活——如实写进 agent-tools.md，与真 ssh 杀会话同语义**）→ 终态 stopped。未知 → 报错（§4 三因文案）。

### failed 态错误文本（防御性清洗）

任务落 failed 态时，存入 task 记录并经 exec_output `error` 字段回给 agent 的文本，**统一过 `redactAddr` 同款清洗**（纯函数，零成本）。实测形态记录（实验 2026-08-21）：纯输出型会话的运行期死亡（客户端拆连 / 网络级 RST / 优雅 FIN）session 层错误均为 `wait: remote command exited without exit status or exit signal`（ExitMissingError 类，零地址形态）——清洗属防御性（库升级/未测路径可能引入带地址文本），**no-leak 断言网扩 failed 分支，用 RST 代理 fixture 真触发**。

## 5. 审计口径（只记状态转换，owner 拍板）

| 行 | Action | Command 字段 | Status | 其他 |
|---|---|---|---|---|
| 启动尝试 | `exec-bg-start` | 命令原文 | denied/hostkey_mismatch/connect_error/no_sudo/no_credential/auth_error/cancelled/error/超限/ok（全分支，照 exec_command 词汇表） | Sudo、ServerID、ProjectID；ok = 已入表运行 |
| 终态 | `exec-bg-end` | **task_id**（关联键，照 close-forward 记 tunnel_id 先例） | ok（自然退出，码值看 ExitCode）/ stopped / timeout / failed | ExitCode、DurationMS=运行时长 |

- `exec_output` 零审计行；`exec_stop` 对已终态任务零行（无转换）；**满员驱逐与 sweeper 过期删除零行**（终态行已落，删表项是保留期记账）。
- **留痕（诚实缺口）**：broker 重启杀运行中任务 → 有 start 行无 end 行（CloseAll 的 closed 抑制是机制兑现，见 §3）——缺失的 end 行本身就是取证信号；CloseAll 时不做 best-effort 补写（伪数据比缺行更糟）。
- 终态行由任务 goroutine 落笔（终态判定与写行同点、同持 TaskManager.mu，无竞态窗口）。

## 6. 前台 exec_command 钳制改响

- `ExecOutput` 增 `EffectiveTimeoutSeconds int`（`json:"effective_timeout_seconds"`）——**恒存在**（schema 稳定，agent 总可读，brainstorm 拍板）；值 = clamp 后实际生效秒数。
- 工具描述（server.go:78）补一句 5min 硬顶与 effective 回显。
- owner CLI `ssh` 路径不受影响（ownerSSHDeadline 独立）。
- **纯增量、非破坏**：新增字段对既有消费方向后兼容。

## 7. 测试与验收

验收方法学：生命周期用**时间可控的白盒测试**（同包直改常量/直调 SweepExpired）+ **agent 可见面用 in-memory MCP 客户端 e2e**（照 TestE2EIronRule 形态）；SSH 行为层全部走 in-process testsshd，docker-gated conformance 补真 OpenSSH 一层。

| 层 | 内容 |
|---|---|
| 单测：rollingBuffer | 游标推进 / gap（offset<start 的 lost 计数 + **next=total**）/ since≥total（空 chunk + **next=total 回拉**）/ 超 cap 丢头 / **并发读写 -race** / cap=0 边界 / **Snapshot 返回深拷贝**（返回后继续 Write，断言已取快照不被腐蚀） |
| 单测：唤醒原语 | **虚假/陈旧唤醒不提前返回**（制造信号后无条件变化，断言等待者续等到条件或 deadline）/ **N 并发等待者全被唤醒**（广播语义）/ **wait 预算不被高频写重置**（另一通道高频写，断言总阻塞 ≤ wait+ε） |
| 单测：runSession 抽取 | **前台回归网：exec/sudo 既有测试全绿零改动**；writers 直写断言 |
| 单测：TaskManager | 32 上限 + **满员驱逐**（最旧终态被驱逐、running 永不驱逐、全 running 才报错且引导文案真实）/ **入表路径并发压满**（N 并发 exec_background 于 map=31，断言最终 map ≤ 32、无过度驱逐）/ **拒绝路径零连接泄漏**（捕获 client ref 断言已关）/ clampBackgroundTimeout、clampWaitSeconds 纯函数全分支（负值不进函数——schema 已拒）/ sweeper 删终态过期项、不删 running / **终态即关 Client**（记录仍在而连接已关）/ CloseAll：**机制顺序**（closed→cancel→有界等退出）+ **零补写审计行断言** + 表空 + client 全关 / stop 已终态幂等 / **stop 与自然退出竞态**（并发触发两路径，终态唯一、审计恰一行、-race） |
| 单测：testsshd 生命周期 e2e | 起后台（脚本逐行 sleep 输出）→ exec_output 拿增量（offset 推进）→ 自然退出拿 exit_code → **终态后取尾输出（offset 推到 next==total 无新增）——该断言同时钉住「Run 返回即 writer 排空」库不变量** → stop 路径（cancel→stopped）→ timeout 路径（timeout_seconds=1 跑 sleep）→ sudo 路径；「exec_background 返回后连接仍活」断言；**failed 路径用 RST 代理 fixture 真触发** |
| 单测：编码两态 | text：非法 UTF-8 字节 → U+FFFD 替换断言；**base64：多字节字符跨窗口边界切断后重组无损**（首窗尾+次窗头 base64 拼接解码 == 原始字节）——codex 高危的回归锚；offset 在两模式下同一字节口径 |
| 单测：ForProfile 包装 | profile 拒绝（denied）/ 全错误分支 **no-leak 断言扩三新工具**（Plan 31 assertBranch 分支签名锁同型）/ **failed 分支 no-leak（RST fixture，断言 error 文本过清洗）** / 审计行两型（start 全分支含超限 + end 四终态，task_id 关联键）/ exec_output 无审计行断言 / **unknown task_id 三因文案断言** / **负值入参 schema 拒绝断言**（timeout/wait/offset 四参数） |
| e2e：in-memory MCP | 三件套全流（initialize → exec_background → 轮询 → stop），零容忍面自动扩展（**BrokerTools 单源切片追加三名——scoreT6/scoreT8 结构性覆盖新工具**；grep `BrokerTools` 硬编码下标/长度的测试连带核对） |
| 单测：revoke 钉住 | `TestRevokedProjectKeepsBackgroundTaskRunning`（serve 路径 revoke 后任务不被杀，照隧道先例） |
| conformance（§13 gated） | 真 OpenSSH：后台 start→增量→stop 生命周期 + 前台 interop/differential 零回归；**differences-ledger 登记偏差：三件套无 ssh 二进制对应物，不做 differential** |
| eval（§12 gated） | 新任务 T9：agent 起后台逐行输出任务 → wait 轮询增量 → 收齐行 → stop 一个 sleep 任务；确定性 scorer（行齐 + offset 推进）；`seedBroker` 零改动（按 id 寻址） |
| 前台钳制响应 | `effective_timeout_seconds` 恒存在 + 钳定值正确（0→120、>300→300、中值直通）单测 |
| env seam | 三 env 生效 / **非法值与非正数均拒绝启动** / 缺省回落生产默认值 |

## 8. 文档落点

| 文件 | 改动 |
|---|---|
| `agent-tools.md` | 三件套节：语义 + **tail -f / journalctl -f 轮询惯用法**（wait≤30s 配合客户端超时）+ `cd /dir && VAR=x cmd` 惯用法 + **重启即失** + **task_id 失效三因**（过期/驱逐/重启）+ kill 语义诚实段（SIGHUP/nohup 存活）+ offset/诚实降级说明 + **编码两态语义**（text 有损边界/base64 精确，GBK 日志建议 base64）+ 前台 effective_timeout_seconds；错误对照表补新错误形态 |
| `README.md` | 工具清单加三行；v0.10 callout（纯增量） |
| `agent-access.md` | 「断连语义（四层）」各层补后台任务行为（同隧道类材料）：stdio=会话重启任务即失 / serve=token revoke 后下次 exec_output 逐请求 401、**运行中任务不被 revoke 杀**（活到 24h 钳定上限或 exec_stop；测试钉住）/ 离线 cache 无涉 |
| `threat-model.md` | §3.5 补一句：任务表=进程内状态（同隧道类），非新增披露面；sudo 密码传递不落记录；failed 态文本过清洗 |
| `concepts.md` | 工具清单提三件套一句（长活走后台） |
| `compat-matrix.md` | v0.10.0 行：纯增量（3 新工具 + ExecOutput 新字段），无破坏；已验证组合表发版后双端实测回写（惯例） |
| `docs/ssh-conformance/differences-ledger.md` | 三件套偏差登记（无 ssh 对应物、kill=会话关闭 SIGHUP 语义） |
| `server.go` 工具描述 | 三新工具描述写足惯用法（含 encoding 两态与 offset 续读法——Agent 说明书即描述）；exec_command 描述补 effective 句 |

## 9. 明确不做（scope 纪律留痕）

- **PTY / 交互式会话**（grilling 已拍板，长活一律走后台）。
- **stdin / env / workdir 参数**（agent 自组命令行）。
- **任务持久化 / broker 重启恢复 / 跨进程任务表**（backlog 拍板进程内即失）。
- **exec_list 工具**（agent 自己记 task_id；YAGNI）。
- **流式推送**（pull 模型：反复 exec_output 拉增量，backlog 已拍板）。
- **跨 project 任务可见性**（结构性不可见：per-Server 实例隔离，无需额外 ACL）。
- **优雅信号楼梯**（SIGTERM→SIGKILL：OpenSSH 忽略 signal 请求，协议层不存在该选项——如实文档而非假装支持）。
- **远端落盘轮询方案**（brainstorm 方案三，已弃：污染远端 FS、stderr 通道合流、stop 要追杀远端 PID）。
- **revoke 级联杀任务**（与隧道语义一致，测试反向钉住「不杀」；代价=最长 24h 占槽，§3 留痕）。
