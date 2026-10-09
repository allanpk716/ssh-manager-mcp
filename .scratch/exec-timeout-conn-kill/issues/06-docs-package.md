# 票 06 · Plan 52 文档包（计划文档/验收册/backlog/agent-tools/compat-matrix）

## What to build

补齐本次修复的仓库文档：Plan 52 计划文档、验收册与册目、backlog 五条范围外登记、agent-tools 超时语义更新、compat-matrix v0.20.1 行。内容以 spec（.scratch/exec-timeout-conn-kill/spec.md）与评审链（.xcheck/20261009-075840/）为准；发版/部署/真机验收在文档中如实标注为 owner 门（夜里不做）。

## 验收标准

- [ ] docs/superpowers/plans/2026-10-08-plan-52-exec-timeout-conn-kill.md：照 docs/superpowers/plans/2026-10-08-plan-51-client-metadata-edit.md 的结构（标题/日期与状态行/事实基线（真机证据 + 不重审注记）/任务分解表/取舍（旋钮理由对照 relayRunCap 反惯例、共享函数提取、构造时解析）/测试策略（testsshd 限制 + 黑洞夹具绕法 + conformance）/发版与验收门）。事实基线引用 .xcheck 机器账路径与关键代码行号（exec.go runSession、sudo.go runSessionRaw、x/crypto session.go:399/channel.go:559）
- [ ] docs/acceptance/plan-52-exec-timeout-conn-kill.md 新建 + docs/acceptance/README.md 册目表加行：A1 4090x2（server_id LcXe1qH2IeU）exec_command sleep 600 timeout 15 → ≤18 秒返回 timed_out=true 无错误；A2 残留进程如实记录后手工 kill；A3 exec_background 超时→~20 秒内终态 timeout + exec_stop→及时 stopped；A4 3090x2（7_758HkSaA0）对照无回归；A5 超时后正常往返。全部标注 owner 门（夜链未执行）
- [ ] docs/backlog.md 活跃面登记 5 条：①OpenSSH 9.6 断连不清理子进程的服务器端根因（systemd 会话 scope/PAM 假说未深挖）②远端进程墓碑/孤儿扫描工具（需远端进程注册表）③testsshd 无法仿真 OpenSSH 通道关闭语义（x/crypto 库层自动回关；黑洞代理为快速通道替代）④SFTP 上传/中继停止路径同类挂死风险（upload.go:48-56、relay.go:732-738，独立调查）⑤sudo Start→写密码窗口超时分类为 error 的毫秒级微边缘
- [ ] docs/agent-tools.md：exec_command 节"超时不是错误"条目扩为"到点后发出 SIGKILL 请求并断开连接，返回以超时点为界（加一小段宽限）；少数服务器不清理远端进程，超时后用 ps/pgrep 自查"；排障表与机制表中内核引用更新为共享看门狗函数（不再指 exec.go 旧行号）；高延迟可经 SSHMGR_EXEC_KILL_GRACE 上调一句
- [ ] docs/compat-matrix.md：登记 v0.20.1 行——纯行为修复，工具 schema 参数零变化、仅描述文本；混布矩阵按 v0.19.0 行同款口径
- [ ] 全部文档遵守仓库 CLAUDE.md 写作规范（禁非常见缩写、名称自含可懂、中文）；引用 .xcheck 账本用相对路径
- [ ] 不改动 docs/ssh-conformance/（票 05 所有）

## Blocked by

无，可立即开始（内容以 spec 为准，不依赖代码票落地时序）

## 涉及路径

- docs/superpowers/plans/2026-10-08-plan-52-exec-timeout-conn-kill.md（新）
- docs/acceptance/plan-52-exec-timeout-conn-kill.md（新）
- docs/acceptance/README.md
- docs/backlog.md
- docs/agent-tools.md
- docs/compat-matrix.md

## 副作用声明

纯文档票：无验证命令；自查引用路径存在（如 .xcheck 目录、代码行号）与既有文档格式一致

## decision_refs

D4、D5（backlog 五条）、D6（版本与流程文档）

## review_blocks

无
