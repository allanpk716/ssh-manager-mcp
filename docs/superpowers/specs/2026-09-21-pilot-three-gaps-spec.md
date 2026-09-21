# 测试自助化试点三项缺口修复 · 实施规格

> 来源:2026-09-21 Plan 48 验收册首轮 agent 代跑(`docs/acceptance/runs/2026-09-21-plan48-pilot.md`)暴露的三个产品缺口;评审链 `.xcheck/20260921-110152`(round0)→ `.xcheck/20260921-111405`(round1,收敛)。
> 术语遵守 CONTEXT.md 词表;「审计行」指 vault 审计表(audit_log)中的一行记录。

## Problem Statement(用户视角)

1. owner 在服务器上执行设备码签发/吊销/绑定、服务器条目增删改、轮廓增授权删等变更操作后,审计表里查不到任何痕迹——出了问题无法追溯「谁在什么时候改了什么」(2026-09-21 清毒演练的「吊销有痕」验收子判据因此不可满足)。
2. agent 在无人值守环境(无交互终端)里无法删除缓存实例——`cache instances rm` 的防误删护栏要求交互式输入实例名,没有等价的显式确认通道,一次性材料清理被卡住。
3. 服务器 id 大小写敏感,抄录一位之差全部被「不在授权集」拒绝,错误信息不给近似提示,用户/agent 需要花很长时间排查(试点实测约 40 分钟)。

## Solution(用户视角)

1. 九个 owner 变更命令全部写审计行:执行成功或失败都能在 `sshmgr audit` 里按动作名(如 `cache-token.revoke`)查到,摘要带名称/地址类字段,绝不带密钥类明文。
2. `sshmgr cache instances rm <实例名> --yes`:显式确认旗标,非交互环境可完成删除;不带旗标时护栏行为不变。
3. 工具面「不在授权集」错误在大写小写差异恰好对应唯一条目时,直接把正确 id 回显在错误里,一次自纠。

## User Stories

1. 作为 owner,我想要每次设备码签发/吊销/绑定都留下审计行,以便事后追溯设备授权变更。
2. 作为 owner,我想要每次服务器条目新增/删除/编辑都留下审计行(含主机地址与端口),以便追溯基础设施变更。
3. 作为 owner,我想要每次轮廓新增/授权/删除都留下审计行(含授权条目清单),以便追溯授权范围变更。
4. 作为 owner,我想要失败的变更命令也留痕(状态=error),以便区分「没做」和「做了没成」。
5. 作为 owner,我想要审计摘要永不包含口令/密钥类明文,以便审计库本身不成为泄密面。
6. 作为 agent,我想要在非交互终端用 `--yes` 完成一次性实例删除,以便无人值守清理不留残留。
7. 作为 owner,我想要交互终端下 `--yes` 同样生效(跳过输名确认),以便命令行为可预期、与 `update --yes` 一致。
8. 作为 agent,我想要抄错大小写的 id 在报错里拿到正确 id 提示,以便一次自纠不再反复被拒。
9. 作为 owner,我想要该提示只在唯一命中时出现(多个或零个命中维持原报错),以便不产生误导性提示。

## Implementation Decisions

- **审计写入**:沿用既有 `store.AuditRow`(字段 TS/ProjectID/ServerID/Action/Command/Sudo/Status/ExitCode/DurationMS)与写入管道;ProjectID 留空表示 owner 操作(与 pin-*/pair.* 现有形态一致)。Command 为脱敏 JSON 摘要。
- **覆盖矩阵与动作名**:`cache-tokens add/revoke/bind` → `cache-token.add / cache-token.revoke / cache-token.bind`;`servers add/rm/edit` → `server.add / server.rm / server.edit`;`profiles add/grant/remove` → `profile.add / profile.grant / profile.rm`。
- **Command 摘要逐命令白名单**(白名单外字段一律不落;未知/复杂参数默认不落——宁缺勿泄):
  - cache-token.add / bind:`{"name","profile"}`;cache-token.revoke:`{"name"}`
  - server.add:`{"name","host","port","user"}`;server.rm:`{"name","host","port"}`;server.edit:`{"name","changed":[变更字段名]}`
  - profile.add:`{"name"}`;profile.grant:`{"profile","servers":"N台:名列表"}`;profile.rm:`{"name"}`
  - `--password`/`--key`/`--key-passphrase`/`--sudo-password` 等参数值永不出现在摘要。
  - host/user/port 入 owner 审计摘要的依据:Plan 31 清洗纪律约束 agent 可见工具文本,不约束 owner-only 审计面;既有 pin-* 审计行已含 host。
- **失败行**:命令失败(轮廓不存在、条目未找到等)也写行,Status=error;ExitCode/DurationMS 按既有 AuditRow 惯例取值;摘要按白名单能取多少取多少。
- **读取面**:不变——只经 owner-only `sshmgr audit` CLI;不新增任何 MCP 工具(Plan 36 裁决)。`--action` 过滤为 SQL IN 透传,新动作名无需登记即可查询。
- **--yes 语义**:旗标形态(非环境变量);在确认点一视同仁 assume-yes——TTY 带 `--yes` 跳过输名确认,非 TTY 带 `--yes` 直接执行,非 TTY 无 `--yes` 维持现状拒绝(错误文案不变);无其他确认类旗标,无组合面;删除语义(槽目录+数据加密密钥双根清理)与交互路径同一实现。对齐 `update --yes` 先例(其在确认点不分交互与否;其自愈确认不豁免是该命令特有设计,本命令不引入对应物)。
- **大小写近邻提示**:把散在各处的「server 是否在授权集」判定收敛为一个共享帮助函数(internal/mcpserver;现状 core.go 各 *ForProfile 函数 + context.go + bgtools.go + relay.go 各自 contains 检查);拒绝时对请求 id 与授权集 id 做大小写折叠比较,恰一个命中 → 在错误文本追加「(did you mean "<正确id>"? server ids are case-sensitive)」;多个或零个命中维持原文案。只做 id 近邻,不扩展按名字匹配。错误文本其余清洗纪律不变(id 本经 list_servers 暴露)。

## Testing Decisions

- 只测外部行为,不测实现细节。
- 审计:internal/cli 既有命令测试先例(如 profiles/servers/cache-tokens 的 *_test.go)追加「执行后审计行存在、动作名/摘要字段正确、敏感参数零出现、失败路径留 error 行」断言;store 层新辅助函数按 store 包测试先例单测。
- --yes:三路径钉住(交互跳过如何注入由测试基建决定,至少覆盖非 TTY+--yes 成删、非 TTY 无 --yes 拒绝;TTY+--yes 语义经共享确认函数单测)。
- 近邻提示:单测钉住——大小写变体请求错误文本含正确 id;完全无关 id 维持原错误;授权集内两个仅大小写不同的 id 时维持原文案;经既有 gate 断言先例(core_test.go 的 assertBranch 形态)验证一处修复全覆盖。

## Out of Scope

- 试点遗留的 owner 物理待办(工控板上电、补跑验收册 A1 步骤 1/A3)与 A4 裁决。
- `agentA` 数据加密密钥孤儿清理、两条历史孤立锚清理。
- 其余 backlog 项(P2 四条、TUI 测试治理等);发版流程。
- exec/exec-context 等既有审计族的行为变更。

## Further Notes

- 评审链留档:.xcheck/20260921-110152/(round0)+ .xcheck/20260921-111405/(round1 复审,收敛;F1-F6 记录)。
- 随票下发的非阻断提示:失败行断言(F5)、大小写多命中用例(F6);夜链对象为时序推断,晨报首项确认(F3)。
- 完成后回写:backlog 活跃面第 6/7/8 条销项登记随合并由 owner 决定;验收册 A6「吊销有痕」子判据的补验在合并部署后进行。
