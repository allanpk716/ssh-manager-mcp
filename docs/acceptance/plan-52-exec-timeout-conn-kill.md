# Plan 52 验收册:exec 超时升级为断连拆除(v0.19.1 起)

> 来源:spec(`.scratch/exec-timeout-conn-kill/spec.md`,过程账)+ 计划文档(`docs/superpowers/plans/2026-10-08-plan-52-exec-timeout-conn-kill.md`);评审账 `.xcheck/20261009-075840/`(gitignore 过程账)。
> 环境:4090x2(OpenSSH 9.6p1/Ubuntu 24.04/root,server_id `LcXe1qH2IeU`——实测不配合通道级拆除的机器)+ 3090x2(OpenSSH 8.9p1,server_id `7_758HkSaA0`——对照机)。

## 状态(2026-10-09 建册)

**全部五项为 owner 门,夜链未执行**——待 v0.19.1 发版+双端部署后按册执行(agent 代跑留证,证据回写本册与 compat-matrix;判据不满足登记 backlog、不放宽)。

## 前置

- [ ] 双端 ≥ v0.19.1(owner 发版 tag 后 NUC10 `sshmgr update --yes`+本机 client 同步)
- [ ] CI(master push)+ release(tag v0.19.1,含 CI 门)双绿

## 判据总表

| 项 | 内容 | 硬判据 |
|---|---|---|
| A1 | 4090x2:`exec_command(server_id=LcXe1qH2IeU, "sleep 600", timeout_seconds=15)` | ≤18 秒返回 `timed_out=true`、无错误(修前形态:挂 600 秒到 sleep 自然结束) |
| A2 | 同机查残留进程 | **如实记录**后手工 kill——该机预期残留(服务器侧行为,客户端不能根治);记录不判失败 |
| A3 | `exec_background` sleep 600 超时 15 → 观察终态;再一例 `exec_stop` | ~20 秒内经 `exec_output` 观察进终态 timeout;stop 例及时进终态 stopped(修前两者永远 running) |
| A4 | 3090x2 对照同款调用 | 快速路径无回归(15 秒级返回 `timed_out=true`+远端进程死) |
| A5 | 超时用例后正常命令往返 | 新连接 `echo` 往返成功——断连无连带伤害 |

## A1 有界返回(4090x2)【owner 门】

- 步骤:对 `LcXe1qH2IeU` 发 `exec_command("sleep 600", timeout_seconds=15)`,记录墙钟耗时。
- 取证:命令原文+完整返回对象(elapsed/timed_out/stdout 字段,失败也是证据原样留档)。
- 判据:elapsed ≤ 18 秒(超时 15s+宽限默认 2s+余量 1s);`timed_out=true`;工具不报错。

## A2 残留进程如实记录(4090x2)【owner 门】

- 步骤:A1 返回后同机 `exec_command("pgrep -af 'sleep 600' || true")` 查残留;有则记录进程号后手工 `kill`,复查。
- 取证:pgrep 输出原文(空结果也是证据)+kill 后复查输出。
- 判据:结果**如实记录**——预期残留(OpenSSH 9.6 断连不清理子进程,服务器侧行为,backlog 活跃面 13 登记);本项以「记录+清理完成」为准,不以「进程必死」为通过条件。

## A3 后台任务终态(4090x2)【owner 门】

- 步骤:① `exec_background(server_id=LcXe1qH2IeU, "sleep 600", timeout_seconds=15)` 后轮询 `exec_output` 观察终态;② 再起一例同款任务后立即 `exec_stop`,观察终态。
- 取证:task_id、每轮 exec_output 的 status 与墙钟时刻。
- 判据:① ~20 秒内(超时 15s+宽限+轮询余量)进终态 `timeout`;② stop 后及时进终态 `stopped`;两者均非永远 running。

## A4 对照无回归(3090x2)【owner 门】

- 步骤:对 `7_758HkSaA0` 发 A1 同款调用(sleep 600/timeout 15);可加同款后台用例。
- 取证:同 A1。
- 判据:15 秒级返回 `timed_out=true`;配合型服务器行为与 v0.19.0 无差异(远端进程死)。

## A5 超时后正常往返【owner 门】

- 步骤:A1/A4 完成后,对两机各发一条正常命令(`echo ok`)。
- 取证:返回对象。
- 判据:新连接往返成功——超时强拆只作用于当次调用的专用连接,无连带伤害。

## 遗留

- OpenSSH 9.6 断连不清理子进程的服务器端根因等范围外五项,已登记 backlog(docs/backlog.md 活跃面 13–17)。
