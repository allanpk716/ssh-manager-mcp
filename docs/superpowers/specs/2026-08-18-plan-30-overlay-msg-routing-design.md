# Plan 30 设计:Overlay 消息路由修复 — backlog #9

**日期:** 2026-08-18
**状态:** 设计已获 owner 批准(范围/泵迁移/路由语义/拦截门形态四项拍板)
**来源:** docs/backlog.md #9;Plan 29 T2 实证(`internal/tui/app.go:150-163`,现 `app.go:158-164`)

## Goal

修复 TUI 三个 program 级 Update 的消息路由:overlay 打开期间,除程序自有消息外的一切消息必须送达 overlay(含 huh 的 unexported 协议消息),overlay 返回的 cmd 交还 runtime 异步执行、产出的消息回到 overlay。当前只有 KeyPressMsg 被转发,嵌入 huh 表单的 overlay 在真终端**无法前进字段/组、无法完成**(连单字段表单都完不成)。

## 背景(已取证,全部读码实证)

### 死因机制

huh v2.0.3 的前进协议全靠消息回环:

- `Input` 按 Enter(`field_input.go`)→ 返回 `NextField` cmd → **runtime 执行产出 `nextFieldMsg`** → `Group.Update` 的 `case nextFieldMsg` 才执行 `selector.Next()`(字段视觉推进);
- 组末字段 → `nextGroup` cmd → `nextGroupMsg` → `Form.Update` 处理它才切组、才置 `StateCompleted`;
- **关键推论:`StateCompleted` 的唯一置位点在 `nextGroupMsg` 处理分支里** → 回环被剪断时连单字段表单(新增 Profile、删除/轮换/吊销 Confirm、importflow 路径表单)都无法完成;
- `nextFieldMsg`/`nextGroupMsg`/`prevGroupMsg` 等**全部 unexported** → 路由层永远无法按名匹配,只能"程序自有消息白名单 + default 全转发"(结构性约束,非风格选择)。

### 三个路由点同病

| 路由点 | 位置 | 现状 | 受影响 |
|---|---|---|---|
| `App.Update` | `internal/tui/app.go:158-164` | switch 只认 KeyPressMsg + 7 个 App 自有消息,其余 `return a, nil` | 全部 formOverlay(新增服务器 3 组表单/新增项目/grant multiselect/各 confirm/签发设备码)、importFlow **整条链**(含 `importDoneMsg` 也被丢——批量导入完成永远回不来) |
| `wizardModel.Update` | `internal/tui/wizard.go:389-560` | `form.Update` 只在 `case tea.KeyPressMsg` 尾部 | 首次运行向导的服务器循环表单/授权 multiselect/项目表单 |
| `clientModel.Update` | `internal/tui/clientpage.go:197-201` | 同样只转 KeyPressMsg | 客户端连接编辑表单(editConnForm,多字段;backlog 未点名,机制相同) |

静态屏(secretView / wizTokenScreen / serveResultScreen / clientFinishScreen / mcpConfigScreen / accessCard)任意键关,不受影响。

### 规避先例

editpage(Plan 29)内部同步泵 `feedForm`/`pumpForm`:Enter/Tab/confirm 白名单键同步执行 cmd 并回灌(blink 同步追会阻塞 ~530ms 所以丢弃)。它是路由断裂的**规避**,非修复。

### 为什么测试全绿而真终端死

所有现有 TUI 表单测试直接驱动 `overlay.Update`/`page.Update`,**没有一个测试经过 program 级 Update 路由层跑消息回环**——这正是本设计要补的测试缺口。

### 考据

KeyPressMsg-only 路由自 `f8f2559`(2026-08-14 TUI 骨架,bubbletea v2 元年)出生自带,非近期回归。

## 设计决策(owner 已拍板)

1. **范围:三处路由点全修**(App + wizardModel + clientModel)——同一刀同一模式;留 clientModel 带病等于没修完。
2. **editpage 同步泵迁移到异步路由**——删白名单+泵(~40 行),editpage 改纯异步回环;顺带修好白名单漏的键(Shift+Tab 回退上一字段,今天在 editpage 也是死的)与光标闪烁。
3. **完整路由语义**——WindowSizeMsg 记录宽高后转发;blink/tick 默认转发(表单内光标开始闪烁,bubbles 标准生命周期)。
4. **拦截门形态:集中式**(方案 1)——"overlay 拥有除程序自有消息外的一切"这条不变量集中一处、owned 清单一望而知;否决最小 diff 方案(语义散三处,靠人记一致性)。

## 核心不变量(评审按此判)

**overlay 打开期间,除程序自有消息外的一切消息(按键、huh 协议消息、blink/tick、paste、resize)必须送达 overlay;overlay 返回的 cmd 交还 runtime 异步执行,产出的消息再次回到 overlay。**

## 拦截门(三处同构)

每个 program 级 Update 入口加同一形状的门:

```go
if <overlay/form 活跃> {
    switch msg.(type) {
    case <owned 集合>:
        // 程序自有 → fall through 到原有 switch
    case tea.WindowSizeMsg:
        // 记录宽高(若有状态)后转发,返回 overlay 的 cmd
    default:
        ov, cmd := overlay.Update(msg)
        return m, cmd // 异步回环,runtime 驱动
    }
}
```

各自 owned 集合(读码逐一枚举自各自现有 case 列表):

| 路由点 | owned 集合 | 门生效条件 |
|---|---|---|
| `App.Update` | `errMsg, actionDoneMsg, formDoneMsg, serveInstalledMsg, serveProbeMsg, deviceCodeIssuedMsg, tokenIssuedMsg` | `a.overlay != nil` |
| `wizardModel.Update` | `errMsg, actionDoneMsg, tokenIssuedMsg, deviceCodeIssuedMsg, serveInstalledMsg, serveProbeMsg, formDoneMsg, wizardDoneMsg` | `w.ov != nil` 或 `w.form != nil` |
| `clientModel.Update` | `dataReadyMsg, syncDoneMsg, pullSucceededMsg, connSavedMsg, clientStatusMsg, errMsg, formDoneMsg` | `m.overlay != nil` |

细则:

- `App.Update` 大 switch 里 `case tea.KeyPressMsg` 的 overlay 分支删除(被门吸收);顶部 `listMsg` 块维持 `overlay == nil` 才走。
- wizardModel 的门是**双目标**:`w.ov != nil` 时转发给 w.ov,否则(`w.form != nil` 且非 stepClient/静态步骤)喂 w.form——与 App/clientModel 的单一 overlay 目标不同,评审时注意。保留 KeyPressMsg 里 q/Esc/Ctrl+C 在喂表单**前**的拦截(现有语义:q 全局退出向导);`stepClient` 委托段维持现状(内嵌 clientModel 有自己的门)。
- **防漏登记兜底**:门旁注释写死约定"新增程序自有消息类型必须登记进此集合",外加端到端回环测试兜关键路径。

## 第二层:overlay 内部默认转发

路由门是必要条件;**embed 了 huh 表单且自己按消息类型 switch 的 overlay,还必须把未知消息转给自己的表单**:

| overlay | 现状 | 改动 |
|---|---|---|
| `formOverlay.Update` | 已透明(无条件 `o.form.Update(msg)`) | 零改动 |
| `importFlow.Update` | 只 case KeyPressMsg + importDoneMsg,其余丢 | 加 default 分支:form 非空时喂表单;KeyPressMsg 尾部的 StateAborted/StateCompleted 处理提取成共享方法两条路复用 |
| `editpage` | 同步泵 | 见下节,迁移 |
| 静态屏族 | 任意键关 | 零改动(未知消息喂进去 no-op) |

## editpage 泵迁移

- 删 `feedForm` 的 Enter/Tab/confirm 白名单判断 + 删 `pumpForm`(约 40 行);`feedForm` 变成:喂表单 → abort/complete 检查(语义不变)→ **返回 cmd 给 App**。
- `serverEditPage.Update` 加 default 分支:field 态且 form 非空 → `feedForm(msg)`。
- `openCurrent` 改为返回 `p.form.Init()` 的 cmd(原"cmds are all droppable"注释失效——回环通了,Init 的消息该走真路由)。
- 免费修复:Shift+Tab 回退上一字段、字段内光标闪烁、resize 生效(editpage 的 WindowSizeMsg 处理器今天是死代码)。

## 免费修复清单(同一刀顺带,验收显式覆盖)

1. `importDoneMsg` 不再被 App 丢——importflow 批量导入完成能回来了(今天整条链死在更早的路径表单)。
2. 表单内 paste(`tea.PasteMsg`)恢复。
3. 表单内光标闪烁恢复。
4. editpage 的 WindowSizeMsg 处理器激活(列表跟随终端宽度)。

## 测试策略(单测层)

1. **路由门单测 ×3**:spy overlay(实现 overlay 接口、记录收到的消息)——断言:未知消息送达;owned 集合逐个**不**送达且走程序逻辑(如 formDoneMsg 关 overlay);WindowSizeMsg 送达且宽高已记录。
2. **端到端回环测试(App 层,防整类回归的网)**:真 huh 表单 + 测试 store,drain helper 模拟 runtime(执行 cmd → 喂回消息 → 循环,**丢弃 blink/tick**——它们自续会让 drain 不终止——上限 N 次防死循环)。至少两条:新增 Profile(单字段完成 → store 落库)、新增服务器(3 组推进 → 完成)。
3. **editpage 现有测试迁移**:`tap` helper 内置 drain,~10 处调用点机械不动;既有断言(字段提交、脏标记、Esc 恢复、confirm y/n)必须全数保持绿。
4. 全量 `go test ./...` 绿;gofmt/vet 干净。

## 真终端冒烟矩阵(owner gate,合并前最后一步)

新增服务器(3 组推进+提交)、新增 Profile(单字段完成)、grant multiselect、删除/轮换/吊销 confirm、**importflow 全链**(路径→pick→批量→补全→结果)、首次运行向导 standalone(服务器循环+授权+项目)、client 编辑连接表单、editpage 复验(字段推进、Shift+Tab、resize、光标闪烁可见)。

## 文档回写

- `docs/backlog.md` #9 移除/标记完成;
- 注释改写:`app.go:142-149` 路由注释块、`editpage.go` 头部与 `feedForm`/`pumpForm` 相关注释("App's routing would drop"、"App does not forward WindowSizeMsg"、"cmds are all droppable")随代码实况更新;
- compat-matrix 不涉及(纯 TUI 内部行为,无协议/存储变化)。

## 范围外(明示不做)

- overlay 关闭时页面 list 的 blink/spinner 暂停问题(现状即如此,非本刀回归);
- backlog 其余条目(#1-#8)一律不动;
- 无新依赖、无存储/协议变更、无 CLI/MCP 面变化。
