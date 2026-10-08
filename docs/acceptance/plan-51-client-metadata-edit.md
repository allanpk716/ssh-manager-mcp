# Plan 51 验收册:客户端元数据编辑(v0.19.0 起)

> 来源:spec §12(`docs/superpowers/specs/2026-10-08-plan-51-client-metadata-edit-design.md`);术语见 CONTEXT.md「服务器元数据/受审计转发写/元数据编辑」;取舍 ADR 0005。
> 环境:NUC10(serve v0.19.0,经 SSH 工具面可达)+ 笔记本(本机,v0.19.0,缓存实例 laptop-v050)。

## 前置

- [x] 双端 ≥ v0.19.0(2026-10-08 自更新,同 SHA256 `b02ddcff…` 命中 checksums——双端同源二进制)
- [x] NUC10 `sshmgr serve status` overall HEALTHY(service Running / http responding 401·200 / vault ok)
- [x] 笔记本 `cache pull` 成功(13 servers / 13 credentials,age 0s)
- [x] CI(master push)+ release(tag v0.19.0,含 CI 门)双绿;Release 资产 6 平台 + checksums

## 状态(2026-10-08 首轮 agent 代跑,发版当日闭环)

| 项 | 判定 | 说明 |
|---|---|---|
| A1 agent 改一笔 + NUC10 审计行 + 新值可见 | **过** | 两轮全链探针(MCP stdio→`update_server_metadata`→真路由→真审计);audit 四行 meta-edit 逐字段对版(见 [runs/2026-10-08-plan51-e2e.md](./runs/2026-10-08-plan51-e2e.md)) |
| A2 并发/陈旧互踩 → 409 现值一跳自愈 | **过(生产活证据)** | 设计内取证(探针 edit2 故意用过期 revision)+ 意外活证据(第二轮进程从旧快照水合 revision 0 对上 broker revision 2 → 409 带六字段全部现值 → 按提示重试收敛——正是陈旧缓存的收敛路径,零 cache pull) |
| A3 断 broker → 主文案 | 单测代证 | forwarder 分支表逐字断言(T6,`metadata_forward_test.go`);真机断 serve 会打断全家拉取,收益不抵扰动——owner 如需可随时拔线复跑 |
| A4 开关关断 → disabled 文案 | 单测代证 | 四层解析+关断 403(T4/T5);真机翻转需改服务环境重装(Settings 写入面属批 2 backlog) |
| A5 权威面无此工具 | 进程内测试代证 | T7+e2e:NewServer(权威脸)注册面=authorityTools(12),`update_server_metadata` 不在其中;缓存脸 tools/list 13 含之(生产实证,见 runs) |
| A6 拉取一致性(另一设备 pull 后 revision 一致) | **过(单设备两轮形态)** | 第二轮探针 list_servers 读到 revision 2 == broker 真值(第一轮两笔写入后 pull 带回);跨设备形态待第二台工作机升级后随手核 |

## A1 agent 改一笔回 vault【agent 代跑】

- 步骤:① 笔记本起缓存模式 MCP 子进程 → tools/list;② `list_servers` 读目标(NUC10 条目)revision;③ `update_server_metadata` 打一笔 description 标记;④ 故意持旧 revision 再打(应 409);⑤ 按 409 文案里 current_revision 清回空串;⑥ 再 `list_servers` 核终态。
- 取证:探针逐行 JSON(runs 文件全文);NUC10 `sshmgr audit --limit 6` 原文(四行 meta-edit + v0.19.0 serve 重启的 serve-start 行)。
- 判据:① 13 工具且含 `update_server_metadata`(缓存脸注册);③ `mirrored:true` 且 revision +1;④ 409 文案含 current revision 与六字段现值(逐字=clientops 契约);⑤⑥ `mirrored:true`、description 复原为空、revision 再 +1;audit 行 `fields=description device=laptop-v050 via=meta-edit rev=N->N+1 old.description=<旧值截断>`。

## 遗留(不阻塞)

- A3/A4 的真机形态(断 serve / 翻开关)owner 可选补跑;A6 跨设备核验待第二台工作机升级。
- 批 2(客户端 CLI/TUI 表单 + Settings 开关写入面)backlog 已登记。
