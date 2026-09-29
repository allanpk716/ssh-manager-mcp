# 定时备份走口令加密的本地目录，复制交 owner 自管

Plan 13 已提供明文 NAS 定时备份（`backup create` 写明文 JSON），但 2026-09-29 的部署实测定案放弃了它：owner 的备份目标是 Tailscale 远端群晖上的媒体共享，复制由 owner 自管（群晖映射/同步），恰好踩中明文方案部署硬约束禁止的全部三条（受信 VLAN、永不开 Drive/Cloud Sync 类同步、专用锁权目录）。因此定案：定时备份统一走**口令加密的 `.sme`**（与 `sshmgr export` 产物同构，无新格式）写入 **NUC10 本地目录**，向 NAS 的复制完全交给 owner 自管——备份目录与同步链路全程不需要被信任；口令文件落 vault 目录（与 `master.key.plain` 同 ACL），新增边际暴露约等于零，但口令必须有离机副本（1Password），否则整机损毁时全部备份作废。

## Considered Options

- **明文 `backup create` 直写 NAS（UNC/SMB）**：除硬约束三连破外，盘号映射在无人值守会话不可用（实测复现）、SMB 凭据在任务计划非交互会话的读取在本项目历史上有过整段弯路（Plan 14→16），否决。
- **加密 `export` + 外部脚本轮转**：功能上可行，但轮转/验证逻辑落在无测试覆盖的脚本层；定案改为 `backup create` 内建加密模式（Go 实现可测试），三入口（计划任务/TUI/CLI）共用。

## Consequences

- 加密模式没有"无变化跳过"（密文每次必不同），每天必写新文件——以轮转为兜底，可接受。
- 调度时刻继续活在任务计划程序里，程序内配置文件（`backup.json`）只管目录/份数/口令文件路径，两边职责不越界。
- 设计详情见 `docs/superpowers/specs/2026-09-29-plan-49-encrypted-backup-tui-design.md`。
