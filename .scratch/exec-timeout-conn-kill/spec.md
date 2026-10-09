# Spec · exec 超时升级为断连拆除（三段式看门狗）· v0.20.1

> 来源：Plan 52 评审收敛稿（.xcheck/20261009-075840/proposal.md，两家盲评一轮修订后 AGREE）
> 决策快照：.xcheck/20261009-075840/decisions.md（D1-D7 已确认）
> 术语：看门狗（watchdog，内核里超时/取消时终止会话的协程）、宽限期（grace，第三段前的等待窗口）、黑洞代理（停止转发但双侧 TCP 保持开放的测试代理，复刻不回应通道关闭的服务器形态）

## Problem Statement

对不配合通道级拆除的服务器（实测 OpenSSH 9.6p1 / Ubuntu 24.04），exec_command / exec_background / exec_context 的超时与停止永久挂起：远端进程活多久工具就挂多久，连接保持、进程失控、任务永远停在 running。用户报告了 3 小时的失控案例。根因是两个执行内核（internal/sshbroker/exec.go 的 runSession、internal/sshbroker/sudo.go 的 runSessionRaw）的超时路径只发 SIGKILL 信号请求 + 关闭单个会话通道，而 x/crypto 的 Session.Wait 要等服务器回关通道才返回，这类服务器等子进程退出才回关。

## Solution

超时/取消触发后升级拆除：先走现有信号+关通道，等一个宽限期（默认 2 秒），执行仍未返回就把整条 SSH 连接关掉——真机已验证连接断开必然解锁等待；sshmgr 所有执行路径都是每次调用一条专用连接，关整条连接无连带伤害。修复后超时调用在超时点+宽限内必然返回 timed_out=true（含已接收的部分输出），后台任务超时/exec_stop 必然进终态。少数服务器断连后远端进程仍残留（服务器侧行为），工具文案如实告知并给出 ps/pgrep 自查指引。

## User Stories

1. 作为调用 exec_command 的 agent，我希望超时到点后调用必然在超时+宽限内返回 timed_out=true 与已接收的部分输出，以便长跑命令不会让调用永久挂起、客户端不会按静默中止。
2. 作为使用 exec_background 的 agent，我希望任务超时或 exec_stop 后任务必然经 exec_output 观察到终态（timeout / stopped）而非永远 running，以便任务状态可信、槽位可释放。
3. 作为走 sudo 提权路径的 agent，我希望 sudo 命令的超时与取消同样有界返回，以便提权路径与普通路径行为一致。
4. 作为排查远端残留进程的 agent / owner，我希望工具描述与 unknown-task 错误明确告知"终态是 broker 侧判定、远端进程可能残留"，以便超时后主动用 ps/pgrep 自查与清理。
5. 作为运维 owner，我希望宽限期可用 SSHMGR_EXEC_KILL_GRACE 调整（默认 2s，范围 [100ms, 30s]，非法值拒绝启动），以便高延迟链路上调、CI 与回归中调短。

## Implementation Decisions

- **三段式看门狗共享函数** `killWatchdog`（放 internal/sshbroker/exec.go，方法挂在 *Client 上）：①`sess.Signal(ssh.SIGKILL)` + `sess.Close()`（现行为）②等 `c.killGrace` 只看 done 通道③仍未返回则 `c.Close()`（包装版，连带停保活循环；Client.Close 幂等 closeOnce）。两个内核把内联看门狗协程替换为 `go c.killWatchdog(ctx, sess, done)`；runSessionRaw 中保持在 `sess.Start` 之前启动（现顺序），Start→写密码窗口仍由 ctxErrOr 覆盖。
- **分类语义不变**：连接死亡错误（ExitMissingError / io.ErrClosedPipe）由两内核既有的 `switch ctx.Err()` 先行折叠——DeadlineExceeded → (0, true, nil)（TimedOut）、Canceled → (0, false, ctx.Err())；不泄漏传输错误。
- **宽限 seam**：新文件 internal/sshbroker/execenv.go，`killGraceFromEnv()` 解析 SSHMGR_EXEC_KILL_GRACE——未设/空 → 默认 2s；time.ParseDuration 非法 / 非正 / 超出 [100ms, 30s] → 报错（fail-closed，错误文本含变量名与取值范围）。在 connectWith 拨号前解析，存 Client.killGrace 不可变字段（构造时一次，协程免同步）；不进 doctorEnvSeams；不做进程级缓存（sshbroker 既有旋钮 connect.go:20-45 同为每次构造解析，且测试需要按连接覆盖）。
- **注释修正**：删除 sudo.go:391 的假注释（"closing forces Wait to return"——已被真机证伪）；exec.go:26-35 陈旧注释改为三段式描述，写明"部分服务器等子进程退出才回关通道"。
- **工具描述与错误文案**（rev1 4.4 定稿措辞）：exec_command 补"超时 = 发出 SIGKILL 请求并断开连接、按时返回 timed_out=true 与已接收的部分输出；少数服务器不清理远端进程——超时后用 ps/pgrep 自查、必要时手工清理；高延迟链路可用 SSHMGR_EXEC_KILL_GRACE 上调宽限"；exec_background / exec_output / exec_stop 各补"任务终态是 broker 侧判定，不是远端进程已死的证明；部分服务器完全无视会话拆除"；ErrBgUnknownTask（bgtools.go）追加"unknown 任务号不代表远端进程已消失——命令可能仍在服务器上运行；用 exec_command（ps/pgrep）检查并手工清理"。
- **文档**：docs/superpowers/plans/2026-10-08-plan-52-exec-timeout-conn-kill.md（计划文档，照 plan-51 结构）、docs/acceptance/plan-52-exec-timeout-conn-kill.md（验收册，A1-A5 判据见 Further Notes）、docs/backlog.md 登记 5 条范围外项、docs/agent-tools.md 超时语义两处更新、docs/compat-matrix.md 登记 v0.20.1 行（纯修复，工具参数零变化、仅描述文本）。

## Testing Decisions

testsshd（进程内测试 sshd）底层库自动回关通道，无法复现挂死形态——分三层覆盖（只测外部行为：有界返回 / 分类 / 输出保留 / 任务终态，不测看门狗内部时序）：

1. **快速路径回归**：现有 9 个超时/取消/停止用例不动，必须继续绿（对配合型服务器零行为变化）。
2. **白盒新增**（internal/sshbroker）：
   - seam 解析单测（缺省 2s；"300ms" 生效；garbage / -1s / 50ms / 1m 报错且文本含变量名与范围）；
   - 宽限上界回归（t.Setenv 宽限 300ms + 超时 200ms → 有界返回）；
   - 不误伤测试（配合型 testsshd 上超时返回后同连接二次 Exec 成功——宽限期内返回不砍连接）；
   - **黑洞代理夹具 + 5 条用例**：夹具在客户端与 testsshd 之间转发，blackout() 后停止转发但双侧 TCP 保持开放；普通超时 / sudo 超时用例构造为"先输出可识别文本再阻塞"，断言有界返回 + TimedOut + 无错误泄漏 + **返回值包含先前输出**（钉住部分输出保留契约）；上下文取消用例断言 Canceled、非 TimedOut、有界；全部 goroutine + 10 秒外部死线包裹（修前代码必红而非挂死）。夹具纪律：用无保活的 Connect 连接；cleanup 关闭代理双侧连接防协程泄漏。
   - **黑洞后台任务 2 用例**（internal/mcpserver，仿 killableProxy 先例建本包夹具）：后台任务超时 → 经 exec_output 观察进终态 timeout；exec_stop → 进终态 stopped（均断言非永远 running）；用例注明保活判死阈值（30s×3=90s）远大于外部死线 10 秒，确保唯一解锁源是看门狗第三段（防 vacuous pass）。
3. **conformance 真线**（internal/conformance，SSHMGR_CONFORMANCE=1 门控 + docker 真 OpenSSH）：短超时 + 阻塞命令 → 有界返回 + TimedOut + 无传输错误 + 容器仍健康（新连接往返）；断言钉契约不钉走了哪一段，两种服务器行为都绿。differences-ledger 登记"逐命令超时杀除"与"exec_stop 杀除语义"两行更新。

## Out of Scope

- SFTP 上传/中继停止路径的同类挂死风险（独立调查，backlog）。
- OpenSSH 9.6 断连不清理子进程的服务器端根因（systemd 会话 scope / PAM 假说，未深挖，backlog）。
- 远端进程墓碑 / 孤儿扫描工具（需远端进程注册表，backlog）。
- testsshd 语义仿真缺口（库层自动回关改不动；黑洞代理为快速通道替代，backlog 登记覆盖缺口）。
- sudo Start→写密码窗口超时分类为 error 而非 timeout 的毫秒级微边缘（backlog）。
- 发版打 tag v0.20.1、双端部署、4090x2 真机验收 A1-A5——owner 门，夜链不做（晨报列出）。

## Further Notes

- 事实基线（不重审，D7）：4090x2（OpenSSH 9.6p1 / Ubuntu 24.04 / root，server_id LcXe1qH2IeU）实测挂死+进程残留；3090x2（8.9p1）对照正常；TCP 断开（杀 sshd 进程 / ss -K 内核销毁 socket）均立即解锁挂着的调用。
- 真机验收判据（owner 门，A1：elapsed ≤ 18s 且 timed_out=true 且无错误；A3：~20s 内进终态；A2 残留进程如实记录后手工清理）。
- 风险已评估：宽限定时器与 done 通道竞态良性；连接重复关闭幂等；与保活循环 / TaskManager.CloseAll 交互幂等；Windows 时序偶发用宽上界+外部死线+多次重跑防假红。
- 版本 v0.20.1（tag 由 owner 打）；buildinfo.Version 由 tag 注入，代码零改动。
