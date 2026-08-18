# Plan 30 设计:Overlay 消息路由修复 — backlog #9(rev1)

**日期:** 2026-08-18
**状态:** 设计已获 owner 批准(范围/泵迁移/路由语义/拦截门形态四项拍板);rev1 = 按闭环必改清单修订(伪码精度、语义界定、wizard 门谓词、测试矩阵、措辞)
**来源:** docs/backlog.md #9;Plan 29 T2 实证(`internal/tui/app.go:150-163`,现 `app.go:158-164`)

## Goal

修复 TUI 三个 program 级 Update 的消息路由:overlay 打开期间,除程序自有消息外的一切消息必须送达 overlay(含 huh 的 unexported 协议消息),overlay 返回的 cmd 交还 runtime 异步执行、产出的消息按 owned 归属路由。当前只有 KeyPressMsg 被转发,嵌入 huh 表单的 overlay 在真终端**无法前进字段/组、无法完成**(连单字段表单都完不成)。

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
4. **拦截门形态:集中式**——"overlay 拥有除程序自有消息外的一切"这条不变量集中一处、owned 清单一望而知;否决最小 diff 方案(语义散三处,靠人记一致性)。

## 核心不变量(评审按此判)

**overlay 打开期间,除程序 owned 集合外的消息(按键、huh 协议消息、blink/tick、paste、resize)送达 overlay;overlay 返回的 cmd 交还 runtime 异步执行,产出的消息再进 Update 时按同一规则路由。**

归属规则(定死,防实现歧义):

- **owned 按消费者定义**:一个消息类型属于某 program 的 owned 集合 ⇔ 该 program 的大 switch 有它的 case,与产出方无关——overlay 的 cmd 产出的 `errMsg` 同样归程序(全局错误展示是现状语义,非回归)。
- **App 控制键例外(非回归,写明)**:现状 `app.go:158-164` 的 overlay 分支位于 ctrl+c(:167)/q(:181) case 之前——overlay 打开时 Ctrl+C/q 本来就到不了程序层,被 overlay 吃掉;门吸收 KeyPressMsg 后行为不变。
- **listMsg 族有意排除**:overlay 打开期间页面列表不更新是现状也是新设计的有意行为(`listMsg` 是谓词不是消息类型,无法按名登记 owned;FilterMatchesMsg 在 overlay 态不可能出现——过滤输入在 overlay 态收不到按键)。
- **resize 无需补偿(实证)**:页面渲染每帧动态取宽(`sizedPage.Render(a.width,…)` / `renderPanel(width,…)`),resize 落在 overlay 期间、关闭后不留旧状态(差分对照实验:100→开 overlay→60→关,渲染与直接 60 完全一致)。唯一需要 WindowSizeMsg 的 overlay 是自渲染的 editpage,门的转发已覆盖。

## 拦截门(三处同构)

每个 program 级 Update 入口加同一形状的门。**控制流四种情况显式写出**(Go type switch 无 fall through;owned 分支 = 空体落出门):

```go
// gate:overlay 打开时的消息分派(以 App 为例;wizard/clientModel 同构)
if a.overlay != nil {
    switch msg.(type) {
    case errMsg, actionDoneMsg, formDoneMsg, serveInstalledMsg,
        serveProbeMsg, deviceCodeIssuedMsg, tokenIssuedMsg:
        // owned:空体,落到下面的原有 switch 由程序处理
    case tea.WindowSizeMsg:
        a.width, a.height = msg.(tea.WindowSizeMsg).Width, msg.(tea.WindowSizeMsg).Height
        ov, cmd := a.overlay.Update(msg) // 记录后转发(early-return)
        a.overlay, _ = ov.(overlay)      // 写回!不写则表单状态静默丢失
        return a, cmd
    default:
        // KeyPressMsg / huh 协议消息 / blink / tick / paste / importDoneMsg…
        ov, cmd := a.overlay.Update(msg)
        a.overlay, _ = ov.(overlay)      // 写回(现状 app.go:161-163 即此形状)
        return a, cmd
    }
}
// ← owned 消息从这里继续走原有 switch
```

各自 owned 集合(读码逐一枚举自各自现有 case 列表):

| 路由点 | owned 集合 | 门生效条件 |
|---|---|---|
| `App.Update` | `errMsg, actionDoneMsg, formDoneMsg, serveInstalledMsg, serveProbeMsg, deviceCodeIssuedMsg, tokenIssuedMsg` | `a.overlay != nil` |
| `wizardModel.Update` | `errMsg, actionDoneMsg, tokenIssuedMsg, deviceCodeIssuedMsg, serveInstalledMsg, serveProbeMsg, formDoneMsg, wizardDoneMsg` | 见下方谓词 |
| `clientModel.Update` | `dataReadyMsg, syncDoneMsg, pullSucceededMsg, connSavedMsg, clientStatusMsg, errMsg, formDoneMsg` | `m.overlay != nil` |

细则:

- `App.Update` 大 switch 里 `case tea.KeyPressMsg` 的 overlay 分支删除(被门吸收);顶部 `listMsg` 块维持 `overlay == nil` 才走。
- **wizardModel 门是显式谓词 + 固定判定顺序**(表与细则合一;实证:wizard.go 全文无 `w.form = nil`,步骤切换从不清理旧表单,stepClient 委托靠 `step == stepClient && w.client != nil` 前置判断——顺序错会喂 stale form 或截胡内嵌 clientModel):

  ```go
  // wizardModel gate 判定顺序(定死):
  // 1. stepClient 委托最先(step==stepClient && client!=nil → 全权委托,见现有 Update 头部)
  // 2. 次之 w.ov(静态屏:任意键关,未知消息喂入 no-op)
  // 3. 末之 w.form(表单步骤:KeyPressMsg 先过 q/Esc/Ctrl+C 前置拦截——现有语义,
  //    q 全局退出向导;其余按门规则转发/喂表单)
  // 静态步骤(stepRoleDone/stepVaultErr/stepDeviceIssue 等)w.form 通常为 nil,
  // 但因无置 nil 生命周期,谓词不得依赖"form 非 nil ⇔ 表单步骤"的假设——
  // 必须按上述顺序短路,靠 w.ov/w.form 的实际占用而非步骤枚举判定。
  ```

  stepClient 委托段维持现状(内嵌 clientModel 有自己的门)。**跨层语义(注释写进代码)**:stepClient 委托期,wizard 自有消息(tokenIssuedMsg/deviceCodeIssuedMsg 等)不在 clientModel owned 集合中,若内嵌 clientModel 的 overlay 打开会按其门转发吞掉——现状同样是丢,非本刀回归,注释防后人误判。
- **clientModel 无需第二层改动**:`editConnForm()` 返回 `newFormOverlay(...)`(clientpage.go:335,formOverlay 包裹且该类型 Update 无条件喂表单=已透明),经 `m.overlay` 装载——单条件门够用。
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
- `openCurrent` 改为返回 `p.form.Init()` 的 cmd(原"cmds are all droppable"注释失效——回环通了,Init 的消息该走真路由)。**列为显式验证点**(见测试策略 blink 断言)。
- 免费修复:Shift+Tab 回退上一字段、字段内光标闪烁、resize 生效(editpage 的 WindowSizeMsg 处理器今天是死代码)。

## 免费修复清单(同一刀顺带,验收显式覆盖)

1. `importDoneMsg` 不再被 App 丢——importflow 批量导入完成能回来了(今天整条链死在更早的路径表单)。
2. **表单步骤内** paste(`tea.PasteMsg`)恢复(限定:importFlow 的非表单步骤 stateImporting/stateResult 未知消息仍丢,现状语义)。
3. **表单步骤内**光标闪烁恢复(同上限定)。
4. editpage 的 WindowSizeMsg 处理器激活(列表跟随终端宽度)。

## 测试策略(单测层)

1. **路由门单测 ×3**:spy overlay(实现 overlay 接口、记录收到的消息)——断言:未知消息送达;owned 集合逐个**不**送达且走程序逻辑(如 formDoneMsg 关 overlay);WindowSizeMsg 送达且宽高已记录。**wizard 门加三态分派表驱动测试**(stepClient 委托态/静态屏态/表单态 × huh 协议消息 + spy 标记消息,断言分派目标与判定顺序短路)。
2. **端到端回环测试(App 层,防整类回归的网)**:真 huh 表单 + 测试 store,drain helper 模拟 runtime(执行 cmd → 喂回消息 → 循环,**丢弃 blink/tick**——它们自续会让 drain 不终止——上限 N 次防死循环)。至少两条:新增 Profile(单字段完成 → store 落库)、新增服务器(3 组推进 → 完成)。
3. **wizard 层回环测试**:真表单驱动 wizardModel.Update 走完服务器表单(多字段推进 + 完成 + 步骤推进)——首次运行向导是本事故真终端死路径,必须有自己的回环回归网,不只靠 spy。
4. **clientModel 门测试**:spy overlay 断言门转发/owned 拦截(clientModel 的表单本身是 formOverlay 已透明,门测即可)。
5. **editpage 现有测试迁移**:`tap` helper 内置 drain,~10 处调用点机械不动;既有断言(字段提交、脏标记、Esc 恢复、confirm y/n)必须全数保持绿。**blink 链路显式断言**:进字段态后执行返回的 cmd 产出 BlinkMsg、喂回后仍返回非 nil cmd(自续)——防"光标闪烁免费修复"静默落空。
6. 全量 `go test ./...` 绿;gofmt/vet 干净。

## 真终端冒烟矩阵(owner gate,合并前最后一步)

新增服务器(3 组推进+提交)、新增 Profile(单字段完成)、grant multiselect、删除/轮换/吊销 confirm、**importflow 全链**(路径→pick→批量→补全→结果)、首次运行向导 standalone(服务器循环+授权+项目)、client 编辑连接表单、editpage 复验(字段推进、Shift+Tab、resize、光标闪烁可见)、**overlay 打开时 Ctrl+C/q 行为**(现状语义:被 overlay 吃;冒烟确认非回归)。

## 文档回写

- `docs/backlog.md` #9 移除/标记完成;
- 注释改写:`app.go:142-149` 路由注释块、`editpage.go` 头部与 `feedForm`/`pumpForm` 相关注释("App's routing would drop"、"App does not forward WindowSizeMsg"、"cmds are all droppable")随代码实况更新;
- compat-matrix 不涉及(纯 TUI 内部行为,无协议/存储变化)。

## 范围外(明示不做)

- overlay 关闭时页面 list 的 blink/spinner 暂停问题(现状即如此,非本刀回归);
- stepClient 委托期 wizard 自有消息被内层门吞(既有行为,注释说明,不改);
- backlog 其余条目(#1-#8)一律不动;
- 无新依赖、无存储/协议变更、无 CLI/MCP 面变化。

## rev1 修订记录(闭环必改 5 项 + 2 项用户拍板转必改)

1. 拦截门伪码:补写回(`a.overlay, _ = ov.(overlay)`)与四种控制流显式结构(owned 空体落出 / WindowSizeMsg 记录后 early-return / default 转发 return)。
2. 语义界定:不变量改"按 owned 归属路由"(消费者定义);App 控制键例外、listMsg 族有意排除、resize 无需补偿(差分实验实证)写明;冒烟矩阵补 Ctrl+C/q 项。
3. wizard 门:显式谓词 + 固定判定顺序(stepClient 委托最先 → w.ov → w.form),表与细则合一;实证依据(w.form 无置 nil 生命周期)写明;跨层语义注明。
4. 测试矩阵:wizard 层真表单回环测试、wizard 门三态分派表驱动测试、clientModel 门测试、editpage blink 链路显式断言。
5. 措辞:免费修复清单加"表单步骤"限定;clientModel 包含关系(editConnForm → newFormOverlay 经 m.overlay)写明;stepClient 跨层语义入范围外+注释。
