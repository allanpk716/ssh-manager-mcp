# 票 04 — 用户文档:加密定时备份节 + 部署 runbook

## What to build

owner 按文档即可完成部署:docs/backup-restore.md 新增「口令加密定时备份(本地目录)」节——与既有明文 NAS 节并列,写清命令、配置文件、不变式、口令文件显式 DACL(icacls 命令)、SYSTEM 计划任务(PowerShell Register-ScheduledTask,含 StartWhenAvailable/10 分钟强停)、恢复路径与注意事项;文档索引同步。

## 验收标准

- [ ] 新节覆盖:三入口一条命令、backup.json 三字段与两道 fail-closed(绝对路径/子树禁入)、跑前世代校验、.sme 与 export 同构可互相 import、恢复走 import + 设备码重发
- [ ] runbook:建目录+marker、生成口令、**icacls 显式设 DACL(backup.pass 与 backup.json 各一条,含验收记录要求)**、PowerShell 装 SYSTEM 计划任务完整命令、A1–A8 验收步骤表
- [ ] 与既有明文 NAS 节的边界写清:本节为加密路线;明文节保留并标注其部署硬约束
- [ ] docs/README.md 索引行更新
- [ ] 全文遵守仓库写作规范:无非常见缩写;名称不依赖前文回翻(领域词以 CONTEXT.md 为准:备份/备份配置/口令文件)

## Blocked by

票 01、票 02

## 涉及路径

- docs/backup-restore.md
- docs/README.md

## 副作用声明

- 无(纯文档;验证 = 通读 + 与实现行为对照)

## decision_refs: D2、D3、D5(含 F1/F5 部署语义)
## review_blocks: 无
