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
