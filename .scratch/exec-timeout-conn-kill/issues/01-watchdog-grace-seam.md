# 票 01 · 三段式看门狗 + 宽限旋钮 + 前台白盒测试

## What to build

sshbroker 的两个执行内核（exec.go 的 runSession、sudo.go 的 runSessionRaw）超时/取消时从"发 SIGKILL 信号请求 + 关闭会话通道"升级为三段式：信号+关通道 → 等宽限期（默认 2 秒，只看执行是否已返回）→ 仍未返回则关闭整条 SSH 连接。宽限期经 SSHMGR_EXEC_KILL_GRACE 环境旋钮配置（构造连接时解析，非法值拒绝）。前台路径的超时/取消行为获得自动化测试钉住（配合型服务器形态）。详见 spec（.scratch/exec-timeout-conn-kill/spec.md）Implementation Decisions 前两条。

## 验收标准

- [ ] internal/sshbroker/execenv.go 新增 killGraceFromEnv()：未设/空→2s；"300ms"→300ms；garbage / "-1s" / "50ms" / "1m" → 报错且错误文本含 SSHMGR_EXEC_KILL_GRACE 与取值范围 [100ms, 30s]
- [ ] internal/sshbroker/client.go：Client 增 killGrace 不可变字段；connectWith 在拨号前调 killGraceFromEnv 解析（非法值→连接构造失败）；既有行为零变化
- [ ] internal/sshbroker/exec.go：新增 (c *Client) killWatchdog(ctx, sess, done) 三段式实现（第三段用包装版 c.Close() 连带停保活循环）；runSession 内联看门狗协程替换为 go c.killWatchdog(...)；exec.go 顶部陈旧注释（26-35 行一带的 "Because some servers (notably the in-process testsshd)..."）改写为三段式描述并写明"部分服务器等子进程退出才回关通道，通道级关闭不解锁等待，需升级到关整条连接"
- [ ] internal/sshbroker/sudo.go：runSessionRaw 看门狗同样替换；删除 391 行一带假注释 "some servers ignore SIGKILL; closing forces Wait to return"（后半句已被真机证伪）；helper 在 sess.Start 之前启动的顺序保持
- [ ] 分类语义不变：超时→(0, true, nil)（TimedOut）、取消→(0, false, ctx.Err())；连接死亡错误（ExitMissingError/io.ErrClosedPipe）不泄漏
- [ ] 新增 execenv_test.go：旋钮解析单测（上述取值矩阵）
- [ ] exec_test.go 新增：宽限上界回归（t.Setenv 宽限 300ms + testsshd 阻塞命令 + 超时 200ms → TimedOut 且有界返回）；不误伤测试（超时返回后同连接二次 Exec 成功——证明宽限期内返回不砍连接）
- [ ] sudo_test.go 新增同款上界/不误伤用例（走 ExecSudo 路径）
- [ ] 现有 9 个超时/取消/停止用例全部继续绿（go test ./internal/sshbroker/ -count=1）；gofmt 无 diff；go vet 过
- [ ] 代码注释遵守仓库 CLAUDE.md 写作规范（无非常见缩写、名称自含可懂），中文注释风格与周边一致

## Blocked by

无，可立即开始

## 涉及路径

- internal/sshbroker/execenv.go（新）
- internal/sshbroker/client.go
- internal/sshbroker/exec.go
- internal/sshbroker/sudo.go
- internal/sshbroker/execenv_test.go（新）
- internal/sshbroker/exec_test.go
- internal/sshbroker/sudo_test.go

## 副作用声明

独占验证命令：go test ./internal/sshbroker/ -count=1（全包，含秒级真实超时用例，预计 1-3 分钟）；另跑 gofmt -l internal/sshbroker/ 与 go vet ./internal/sshbroker/

## decision_refs

D1（三段式与共享函数）、D2（旋钮）、D3（测试分层第 1/2 层）

## review_blocks

无
