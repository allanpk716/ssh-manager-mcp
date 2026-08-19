# Backlog（已裁决、未排期）

决策记录在案（xcheck 收敛 2026-08-16 / Plan 25），均为 owner 拍板"暂不改行为"：

1. **serve 隧道 owner 急停**（kill-tunnel CLI 或 revoke 级联拆隧道）——现状：revoke 后既有隧道存活至创建后 ~10 分钟回收或 broker 重启，无 owner 侧拆除手段（close_port 是 MCP 请求，revoke 后 401）。
2. **隧道回收活动感知**（Touch 活动刷新）——现状：回收按创建时间计（Touch 无生产调用方），持续流量不延长。
3. **离线 cache 快照失效机制**（snapshot epoch/serial）——现状：revoke/rotate 不擦已落盘快照，唯一失效手段是轮换服务器凭据；`cache-tokens revoke` 只断"拉新"。
4. **受控监听地址**（forward_port listen_host）——现状：转发只绑 broker 机环回，远程 serve 模式下 agent 拿到的地址本机不可达（文档已如实声明）。
5. **doctor serve 探活二期**（绿/黄/红语义）——现状：doctor 首版只做本机自检（P4，独立 plan）。
6. **Windows DACL readback 检查**——现状：doctor v1 在 Windows 上对 master.key 的硬化校验只有 32 字节长度（Unix 上是权限位），没有真正的 DACL 读回校验；`internal/store/acl_windows.go` 的 `getDACLForTest` 是 test 名构件，产品化需要生产命名的包装 API 才能进 doctor。
7. **doctor 退出码 2 的接线**——现状：`doctorExitCode` 的 2（内部错误）分支保留且被测试钉住，但 main.go 把所有 cobra 错误映射为 exit 1，用户可见契约是 0/1；doctor 将来长出会内部出错的检查（如二期 HTTP 探活）前，必须先把 2 接进真实退出路径，避免保留码静默烂掉。
8. **TestConnectCancelContext Windows wsarecv 间歇 flake**——现状：`internal/sshbroker` 该测试在 Windows 偶发 wsarecv 竞态（2026-08-17 本地全量首跑中 1 次，重跑即绿；CI windows lane 连续绿），修法大概是 retry 式稳定化，与本 repo 任何在飞分支无关。
10. **TUI 测试套件耗时 ~89s**——现状：Plan 30 后 `internal/tui` 从 ~3s 涨到 ~89s（editpage ~71s + wizard 回环 ~7s + e2e ~7s）：drain 测试 helper 同步执行 huh Focus 的 cursor blink `tea.Tick` 闭包（530ms 包级 const），睡完才丢弃 BlinkMsg；纯测试墙钟，零生产影响（生产 cmd 在 runtime goroutine 执行）。跟进方向（Plan 30 终审裁决）：`press()` 对普通字符键免 drain（field 态字符键只产 blink 重臂 cmd）和/或睡眠主导测试加 `t.Parallel()`；**BlinkSpeed 测试 seam 已证不可行**（cursor.Model per-instance 字段，huh 构造器不暴露内部 cursor）。
11. **TUI 表单光标常亮（闪烁宣称撤回）**——现状：Plan 30 曾宣称"表单内光标闪烁恢复"，真终端实测常亮。探针实证（2026-08-19）：blink 消息链活着（自续多轮）但表单视图从不切换——huh v2.0.3 `Group.Update` 对聚焦字段双重更新产生两条竞争 blink 血统，与 bubbles cursor 的 id/tag 防重机制相互作用，Set 相互覆盖致渲染恒定；属 huh/bubbles v2 嵌入式表单上游行为，本仓代码全程未设 cursor 模式。跟进方向：升级 huh/bubbles 后复验（复用探针：连喂 BlinkMsg 比对 form.View() 变化）。
