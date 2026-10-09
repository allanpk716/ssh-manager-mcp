# Plan 52 — exec 超时升级为断连拆除(三段式看门狗)

日期:2026-10-08 立项 ｜ 2026-10-09 定案(评审链一轮修订收敛,两家盲评复审通过) ｜ 状态:夜链实施中(代码/测试/文档;发版 v0.19.1、双端部署、真机验收 A1–A5 为 owner 门,夜里不做)

spec:`.scratch/exec-timeout-conn-kill/spec.md`(事实源,本文件只做任务分解;`.scratch/` 过程账路径)。
评审账:`.xcheck/20261009-075840/proposal.md`(收敛稿=方案定稿)、`.xcheck/20261009-075840/FINDINGS.md`(六条审核问题 F1–F6 与裁定)、`.xcheck/20261009-074751/NIGHT.md`(夜链账)。`.xcheck/` 在 .gitignore 内不入库,以上引用仅作过程账。

## 事实基线(2026-10-09 真机调查定案;按 D7 不重审,评审对象是修复方案对这些事实的应对)

- **根因**:两个执行内核——`internal/sshbroker/exec.go` 的 `runSession`(前台 exec_command/exec_context/后台任务普通命令)与 `internal/sshbroker/sudo.go` 的 `runSessionRaw`(sudo 提权路径)——超时/取消时只发一条 SIGKILL 信号请求+关闭单个会话通道。而 golang.org/x/crypto v0.41.0 的 `Session.Wait` 挂在等服务端回关通道(`ssh/session.go:399`,挂起在 :403 `<-s.exitStatus`),客户端 `sess.Close()` 只发一条关闭消息、发完即返回不等回执(`ssh/channel.go:559`)。真实 OpenSSH sshd 要等子进程退出并冲刷完输出才回发关闭——「不配合的服务器+活着的子进程」组合下等待永久挂起:工具挂死、SSH 连接保持、远端进程失控,直到远端进程自然结束才返回。
- **真机证据**:4090x2(OpenSSH 9.6p1/Ubuntu 24.04/root,server_id `LcXe1qH2IeU`,`sleep 240`+timeout 15)→ 挂到 sleep 自然结束(240 秒)同一时刻才返回 `timed_out:true`;杀 sshd 进程、`ss -K` 内核销毁 TCP socket 两种断连方式都立即解锁挂着的调用;断连后该机进程仍残留(服务器侧行为)。3090x2(OpenSSH 8.9p1,server_id `7_758HkSaA0`)对照正常:15 秒返回+远端进程死。
- **反馈三点裁定**:P1 属实(挂到远端自然结束才返回,客户端早已按静默中止);P2 部分属实(running 任务不被保留期回收——清扫器只删已结束任务;反馈里的 `kd0ofyezy` 是客户端任务号非 sshmgr 的 UUID;真问题=exec_stop 与后台任务超时在不配合服务器上停不掉、任务永远 running);P3 属实(unknown 错误多因混报且不提示远端进程可能还在跑)。
- **测试盲区成因**:测试用 testsshd(进程内测试用 sshd)底层是 x/crypto 服务端库,收到通道关闭自动立即回关——与真实 OpenSSH「等子进程退出才回关」根本不同,挂死形态在测试内不可复现(「测试全绿、真机挂死」的完整解释)。

## 任务分解

| # | 任务 | 文件 | 票 |
|---|---|---|---|
| T1 | 共享三段式看门狗 `killWatchdog`(挂在 `*Client` 上,exec.go):①`sess.Signal(SIGKILL)`+`sess.Close()`(现行为) ②等 `c.killGrace` 宽限(只看 done 通道) ③仍未返回→`c.Close()`(包装版,连带停保活循环;closeOnce 幂等)。两内核内联看门狗协程(exec.go:58-65/sudo.go:387-394)替换为 `go c.killWatchdog(...)`;runSessionRaw 中保持在 `sess.Start` 之前启动(现顺序);宽限 seam 新文件 execenv.go:`killGraceFromEnv()` 解析 `SSHMGR_EXEC_KILL_GRACE`(默认 2 秒,范围 [100ms, 30s],非法/非正/越界拒绝启动,错误文本含变量名与范围),connectWith 拨号前解析存 `Client.killGrace` 不可变字段;注释修正(删 sudo.go:391「closing forces Wait to return」假注释——已被真机证伪;exec.go:26-35 陈旧注释改三段式描述);分类语义不变(连接死错误由既有 `switch ctx.Err()` 折叠为 timed_out/取消错误,不泄漏传输错误) | internal/sshbroker/{exec.go, sudo.go, execenv.go(新), client.go, connect.go} | 票 01 |
| T2 | 前台黑洞用例:黑洞代理夹具+普通超时/sudo 超时/上下文取消三用例(先输出可识别文本再阻塞;断言有界返回+正确分类+无错误泄漏+返回值包含先前输出);白盒三件:seam 解析单测/宽限上界回归(宽限 300ms+超时 200ms→有界返回)/不误伤测试(配合型服务器超时返回后同连接二次 Exec 成功) | internal/sshbroker 测试 | 票 02 |
| T3 | 后台黑洞用例 2 条(仿 killableProxy 先例建本包夹具):后台任务超时→经 exec_output 观察进终态 timeout;exec_stop→进终态 stopped;注明保活判死阈值(30s×3=90s)远大于用例外部死线(10s)——唯一解锁源=看门狗第三段 | internal/mcpserver 测试 | 票 03 |
| T4 | 工具描述与错误文案:exec_command 补「超时=发出 SIGKILL 请求并断开连接、按时返回 timed_out=true 与已接收的部分输出;少数服务器不清理远端进程——ps/pgrep 自查;高延迟链路可用 SSHMGR_EXEC_KILL_GRACE 上调宽限」;exec_background/exec_output/exec_stop 补「任务终态是 broker 侧判定,不是远端进程已死的证明」;ErrBgUnknownTask 追加「unknown 任务号不代表远端进程已消失,用 ps/pgrep 检查并手工清理」 | internal/mcpserver/{server.go, bgtools.go} | 票 04 |
| T5 | conformance 真线用例(SSHMGR_CONFORMANCE=1 门控+docker 真 OpenSSH):短超时+阻塞命令→有界返回+TimedOut+无传输错误+容器仍健康;differences-ledger 登记「逐命令超时杀除」「exec_stop 杀除语义」两行 | internal/conformance, docs/ssh-conformance/differences-ledger.md | 票 05 |
| T6 | 文档包:计划文档(本文件)+验收册 A1–A5+验收册目加行+backlog 五条+agent-tools.md 超时语义+compat-matrix v0.19.1 行 | docs/ | 票 06 |

## 取舍

- **旋钮理由(对照 relayRunCap 反惯例)**:`SSHMGR_EXEC_KILL_GRACE` 有环境变量 seam,而 relay 时长上限(72 小时常量)刻意不做 env——`relayenv.go:61-69` 注释自述「an own seam would go to the backlog first」。分界:宽限直接决定强拆(关整条连接)的触发时机,是**门控生产风险的升级路径**;CI 与回归需要调短宽限;只影响 exec 这单一面——三者同时成立才配 seam。fail-closed 照 `SSHMGR_BG_RUN_CAP` 先例(tasks.go:134-158,非法值拒绝启动、错误文本含变量名);不进 doctorEnvSeams。
- **共享函数提取**:两内核看门狗协程逐字重复(exec.go:58-65 与 sudo.go:387-394),提取为 `(c *Client) killWatchdog` 单份实现,防第三处漂移。
- **构造时解析**:宽限在 connectWith 拨号前解析一次存 `Client.killGrace` 不可变字段——看门狗协程读它免同步;不做进程级缓存,与既有主机密钥算法旋钮(`connect.go:20-45`)同为每次构造解析,测试可按连接覆盖。
- **关连接用包装版 `Client.Close`**:而非裸底层连接 Close——连带停保活循环(库内先例 client.go:125 keepAliveLoop 判死关连接);幂等(closeOnce),与保活循环/TaskManager.CloseAll 并发关连接交互均已评估为幂等。

## 测试策略(testsshd 限制下的三层覆盖;只测外部行为:有界返回/分类/输出保留/任务终态,不测看门狗内部时序)

- **限制**:testsshd 底层 x/crypto 服务端库自动回关通道,复现不了「服务器不回关」形态——黑洞代理为快速通道替代。
- **黑洞代理夹具**:客户端与 testsshd 之间的转发代理,连上后切换为「停止转发但双侧 TCP 保持开放」——客户端看到的正是 OpenSSH 9.6 形状(关闭消息发出永远无回音)。夹具纪律:前台用例用无保活的 `Connect` 出品连接;后台用例经 `ConnectKeepAlive` 并注明保活判死(30s×3=90s)远大于用例外部死线(10s)——防保活循环「代打」解锁的假通过(F4);cleanup 关闭代理双侧连接防协程泄漏;全部用例 goroutine+10 秒外部死线包裹——修前代码必红而非挂死。
- **三层**:①快速路径回归——现有 9 个超时/取消/停止用例不动必须绿(配合型服务器零行为变化);②白盒新增(seam 解析单测/宽限上界回归/不误伤测试/黑洞五用例);③conformance 真线(docker 真 OpenSSH+SSHMGR_CONFORMANCE=1 门控)——断言钉契约(有界返回+分类正确+容器健康)不钉走了哪一段,两种服务器行为都绿。
- **部分输出保留契约**:黑洞超时用例先输出可识别文本再阻塞,断言返回值包含该文本——已收字节存调用方本地缓冲(cappedBuffer),断连只丢网络在途尾部(F1 裁定补钉)。

## 发版与验收门

- 版本 **v0.19.1**(patch;buildinfo.Version 由 tag 注入,代码零改动)。发版打 tag、双端部署(NUC10 broker+本机 client,`sshmgr update --yes`)、4090x2 真机验收 A1–A5——**全部 owner 门,夜链不做**(晨报列出)。
- 验收判据见 [acceptance/plan-52-exec-timeout-conn-kill.md](../../acceptance/plan-52-exec-timeout-conn-kill.md):A1 有界返回(≤18 秒 `timed_out=true`)/A2 残留进程如实记录后手工 kill/A3 后台任务 ~20 秒内进终态/A4 3090x2 对照无回归/A5 超时后正常往返。
- compat-matrix 登记 v0.19.1 行(纯行为修复,工具 schema 参数零变化、仅描述文本);backlog 范围外五条登记(docs/backlog.md 活跃面 13–17)。
