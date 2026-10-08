# Plan 51 — 客户端元数据编辑(受审计转发写:乐观锁更新)

日期:2026-10-08 ｜ 状态:已实施(测试矩阵 T1–T8 全绿 + linux/darwin 交叉编译绿;待 owner 审/合并/发版 v0.19.0)

spec:`docs/superpowers/specs/2026-10-08-plan-51-client-metadata-edit-design.md`(事实源,本文件只做任务分解)
术语:CONTEXT.md「服务器元数据/受审计转发写/元数据编辑」;取舍:ADR 0005。

## 任务分解

| # | 任务 | 文件 | spec 节 |
|---|---|---|---|
| T1 | store:revision 列迁移+新库建表+全 SELECT/INSERT/UPDATE 路径(model/scan/insert/updateServerTx/UpdateServerWithCredentials) | store/store.go, servers.go, tx.go, models/models.go | §4 |
| T2 | store:快照链(两条导出 SELECT+SnapshotServer+ImportSnapshot)与 ServerInfo/list_servers 带 revision | store/export.go, mcpserver/types.go, mcpserver/core.go | §4 |
| T3 | store:`UpdateForwardedMetadata`(单事务 CAS+同事务审计+旧值截断 Command)+`ApplyForwardedMetadata`(只读窄缝+单调守卫)+`ErrStaleRevision` | store/metadata.go(新) | §3/§5 |
| T4 | serve:路由+`handleServerMetadata`(守卫①-⑦+stderr 行+请求响应类型+32 KiB 上限) | mcpserver/serve.go | §1 |
| T5 | serve:第三开关全套(setting/env/flag/默认 true/RefreshSwitches 六参/MetadataEditEnabled)+CLI flag | mcpserver/switches.go, cli/serve 侧 | §6 |
| T6 | clientops:`MetadataForwarder`(构造+Edit+§2.1 分支表全文案+30s 超时) | clientops/metadata_forward.go(新) | §2 |
| T7 | 工具面:`BrokerTools[12]`+`NewServerFromSource` meta 参+条件注册+schema/处理器/输出+`RunStdioCache`/`NewCacheBroker` 穿参+cli 注入 | mcpserver/server.go, run.go, cli/mcp.go | §7 |
| T8 | 测试矩阵 T1–T8(spec §9)全落地 | 各包 _test | §9 |
| T9 | 文档:agent-tools.md 工具节+multi-machine.md 路由/开关节+backlog 批 2 登记 | docs/ | §11 |
| T10 | 回归:go test ./... 全绿+gofmt/vet 净+eval 零改动确认 | — | §9 |

## 批 2(backlog,非本批)

客户端 CLI/TUI 元数据编辑表单(同一路由);TUI Settings 面 serve.metadata_edit 开关写入位。

## 发版与验收门

捆发 v0.19.0(假定);发版门=测试矩阵全绿;真机验收六项(spec §12)发版后进验收册。
