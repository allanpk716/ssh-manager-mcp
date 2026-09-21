# 真机验收册(Acceptance Playbook)

> 发版后的真机验收按册执行:**agent 能跑的 agent 跑,owner 只跑人工保留面**。
> 立项背景:2026-09-21 grilling「测试自助化」主轴定案;架构约束见 [ADR 0003](../adr/0003-tui-cli-parity-agent-testable.md)。

## 总则

1. **执行者**:凡能经命令行(`sshmgr ...`,本机或远端)或 SSH 工具面(NUC10 / 目标机远程操作)完成的步骤,由 agent 代跑;证据标注「agent 代跑」后回写 [compat-matrix.md](../compat-matrix.md) / [backlog.md](../backlog.md)。
2. **人工保留面**(永远不由 agent 代跑,机器代跑要么失去意义要么造假证据):
   - **人工信任动作**:SAS 配对双屏人工比对。agent 自动化跑用 `SSHMGR_PAIR_ASSUME_SAS=1`,证据必须注明跳过;
   - **感官判断**:真终端观感目验(中文列对齐、光标常亮这类);
   - **物理操作**:拔网线、断电、设备本体操作。
3. **安全护栏**:破坏性演练(清毒 `--clear`、吊销 revoke、删除实例)**一律打一次性靶子**——临时服务器条目、临时设备码、临时实例,不碰生产条目;跑完清理靶子并留清理证据。纯读操作(doctor、计数、指纹对照、audit)可直接上生产。
4. **取证要求**:命令原文 + 完整输出;涉及完整性断言的附 sha256 对照;远端操作注明经哪台机器执行;失败也是证据,原样留档不修饰。
5. **通过判据**:每项写明「什么算过」;判据不满足时登记 backlog,**不放宽判据**。

## 与既有测试的分工

| 层 | 跑什么 | 时机 |
|---|---|---|
| 单元/集成测试(进程内) | 行为契约 | 发版前门(CI) |
| conformance / eval(docker、门控) | 协议与工具面 | 发版前门(CI) |
| **本册(真机端到端)** | 生产拓扑上的实际行为 | **发版后门** |

三层互补,不互相替代。

## 册目

| 册 | 覆盖 | 状态 |
|---|---|---|
| [plan-48-pin-forwarding.md](./plan-48-pin-forwarding.md) | 锚定转发 / 带外锚定六项(spec §12) | 待跑(v0.15.0 发版后) |
| [plan-47-relay.md](./plan-47-relay.md) | relay_file 大文件 / 断点续传 / Windows 目标特性 | 待跑(v0.14.0 发版后) |
| [plan-46-45-tui-flows.md](./plan-46-45-tui-flows.md) | 实例管理 / 配对向导流程 | 待跑(行为部分依赖进程内测试补齐) |
| [plan-34-37-cache-invalidation.md](./plan-34-37-cache-invalidation.md) | 吊销销毁 / 到龄自废 | 待跑(v0.10.0 发版后) |
