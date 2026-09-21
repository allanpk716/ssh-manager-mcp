# 06 · profile gate 大小写近邻提示

## What to build
把工具面散在各处的「server 是否在授权集」判定收敛为一个共享帮助函数,拒绝路径生成 `ErrNotInProfile` 时按大小写折叠做近邻提示:
- 请求的 server_id 与授权集 id 大小写折叠相等且**恰一个**命中 → 错误文本在原文案后追加 `(did you mean "<正确id>"? server ids are case-sensitive)`。
- 命中多个或零个 → 维持原文案不变。
- 只做 id 近邻,不按名字匹配;错误文本其余部分不变(不含 host 等,清洗纪律照旧)。
- 收敛点:internal/mcpserver 新建共享函数(如 profilegate.go:`gateServer(st, profileID, serverID) error` 返回 ErrNotInProfile 形态错误),core.go 各 *ForProfile 函数、context.go、bgtools.go、relay.go 的 contains 检查改调它(一处修复全覆盖;错误定义仍用既有 ErrNotInProfile,提示文本为包装形态时须保证错误对照表既有匹配前缀不破坏)。

## 验收标准
- [ ] 大小写变体请求(如 mymg→mYmg 实测形态)错误文本含正确 id 与 case-sensitive 提示
- [ ] 完全无关 id 维持原错误文本
- [ ] 授权集内两个仅大小写不同的 id 时维持原文案(评审提示 F6 用例)
- [ ] exec/download/upload/upload_content/forward/exec_context/exec_background/relay 的 gate 均经共享函数(检视+既有 gate 断言先例覆盖至少两条路径)
- [ ] internal/mcpserver 既有测试全绿

## Blocked by
无,可立即开始

## 涉及路径
- internal/mcpserver/profilegate.go(新建)
- internal/mcpserver/profilegate_test.go(新建)
- internal/mcpserver/core.go
- internal/mcpserver/context.go
- internal/mcpserver/bgtools.go
- internal/mcpserver/relay.go

## 副作用声明
无(只跑 `go test ./internal/mcpserver/ -run "Profile|NotIn"`;全包跑需独占,留终局)

decision_refs: D1、D7
review_blocks: 无
