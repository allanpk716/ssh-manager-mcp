# 票 02 · 黑洞代理夹具 + 前台/取消黑洞用例（3 条）

## What to build

在 internal/sshbroker 新增黑洞代理测试夹具：一个 TCP 代理转发客户端与 testsshd 之间的字节，blackout() 调用后停止双向转发但保持两侧 TCP 连接开放——客户端看到的就是"关闭消息发出去永远没有回音"的不配合服务器形状（复刻 OpenSSH 9.6 实测行为；进程内 testsshd 底层库自动回关，无法自然产生该形态）。用该夹具证明三段式看门狗的第三段（关整条连接）在"通道级拆除被无视"时必然解锁等待。详见 spec Testing Decisions 第 2 层。

## 验收标准

- [ ] 新文件 exec_blackhole_test.go 含夹具：listen 在 127.0.0.1:0，接受一条客户端连接并连向 testsshd 地址，双向 io.Copy 转发；blackout() 后两个 copy 循环停止读转发（两侧 conn 保持打开不关闭）；cleanup 关闭代理两侧连接防协程泄漏
- [ ] TestExecTimeoutEscalatesToConnClose：客户端经代理连 testsshd → blackout → Exec(阻塞命令, 超时 300ms)；命令先输出一段可识别文本（如 "before-block\n"）再阻塞（testsshd 的 Exec 回调先 return 部分文本再 sleep）；goroutine + 10 秒外部死线包裹（select 到时 t.Fatal 而非挂死）；断言：返回、TimedOut=true、err==nil（ExitMissingError 不得泄漏）、有界（elapsed < 5s）、**Stdout 包含先前的可识别文本**（部分输出保留契约）
- [ ] TestExecSudoTimeoutEscalatesToConnClose：同夹具走 ExecSudo 路径（sudo 密码注入形态照 sudo_test.go 既有先例），断言同上
- [ ] TestExecCancelEscalatesToConnClose：同夹具 + context.WithCancel 在 100ms 取消 → 断言 errors.Is(err, context.Canceled)、TimedOut=false、有界返回
- [ ] 夹具纪律：客户端用无保活的 Connect 出品连接（connectTest 先例）；文件头注释写明"黑洞形态 = OpenSSH 9.6 客户端可见形状；testsshd 库层自动回关无法自然产生，故用代理模拟"
- [ ] go test ./internal/sshbroker/ -count=1 全绿（含本票新增 3 条与既有全部用例）

## Blocked by

票 01（三段式行为落地后这些用例才可能绿；修前代码在本夹具下 10 秒死线内必红——写用例时可先跑一次确认红再等 01 绿）

## 涉及路径

- internal/sshbroker/exec_blackhole_test.go（新）

## 副作用声明

独占验证命令：go test ./internal/sshbroker/ -run 'Blackhole|Escalates' -count=1 -v（秒级）；随后全包 go test ./internal/sshbroker/ -count=1

## decision_refs

D1、D3（黑洞夹具与五条用例之前台三条）

## review_blocks

无
