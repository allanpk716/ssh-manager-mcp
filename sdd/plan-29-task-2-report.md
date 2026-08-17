# Plan 29 Task 2 Report — serverEditPage（list↔field 两态状态机）

**Status: DONE** — committed on `worktree-plan-29-editpicker`（T1 之后）。
**Files:** `internal/tui/editpage.go`（新增，296 行）、`internal/tui/editpage_test.go`（新增，12 个测试）。
**提交信息:** `feat(tui): serverEditPage — field picker with pagination and dirty marks (Plan 29 T2)`

---

## 1. T3 接线面（T3 将对接的确切表面）

```go
// internal/tui/editpage.go
func newServerEditPage(st *store.Store, orig *models.Server, d *serverDraft, width int) *serverEditPage
// 实现 tea.Model（Init/Update/View）+ Title() string —— 满足 App 的 overlay 接口
// （editpage_test.go 里有编译期断言 var _ overlay = (*serverEditPage)(nil)）。
```

T3 的 `e` 分支预期接线（约 3 行，参考 app.go:456 现状）：

```go
case "e":
    if cur := sp.current(); cur != nil {
        draft := prefill(cur)
        a.overlay = newServerEditPage(a.st, cur, draft, a.width)
    }
...
if a.overlay != nil {
    return a, a.overlay.Init()   // Init 返回 nil（页面无异步初始化）
}
```

- **结束协议**（与 formOverlay 完全一致）：保存项 Enter → `formDoneMsg{after: submitServer(st, orig, d)}`（mutation 在 overlay 关闭后跑）；list 态 Esc → `formDoneMsg{aborted: true}`（不落库）。
- **保存缝（测试注入点）**：`serverEditPage.submit func() tea.Cmd` 字段，默认 `func() tea.Cmd { return submitServer(st, orig, d) }`。T3 无需触碰；测试替换它捕获调用（TestEditPageSaveItemFiresSubmit）。
- `submitServer` / `prefill` / `serverDraft` / `newServerForm` **零改动**（本任务只新增两个文件）。

## 2. 关键架构发现：App 的 overlay 路由丢弃 huh 内部消息（必须内部泵送）

**实证**（scratch 测试，已删）：huh 表单的 Enter → `NextField` cmd → `nextFieldMsg` → 表单推进/完成。但 `App.Update` **只把 `tea.KeyPressMsg` 转发给 overlay**（app.go:159-164），`nextFieldMsg`/`nextGroupMsg` 落进 App 的 switch 后被丢弃 —— 直连驱动时表单 State=1（完成），模拟 App 路由后 State=0（卡死）。

这正是 Plan 29 背景里"**三组长表单组内导航在真终端失灵（用户实测）**"的根因：长表单的组内 Enter 推进依赖这些被丢弃的异步消息。**同样的问题潜伏在 formOverlay/importflow/wizard 的所有嵌入表单里**（生产上"能提交"大概因为多字段表单里用户最终总能用某种路径完成——但组间/字段间推进确实是断的；这是预存问题，本任务不修、也不在范围内，建议另立 plan 处置）。

**本页的对策**：`feedForm` 只在 Enter/Tab（唯一会触发 NextField/Submit 的键，覆盖表内全部字段类型）时把表单返回的 cmd **在页面内部泵送回表单**（`pumpForm`：展开 `tea.BatchMsg`、丢弃 `cursor.BlinkMsg`/`spinner.TickMsg` 自续型 tick、递归喂回）。完成/中止都在同一次 Update 调用内可见 → 状态机立即切回 list 态。

**性能坑（同源）**：光标 blink cmd 是 `context.WithTimeout(530ms)` 的阻塞等待——同步执行每个字符键的返回 cmd 会各睡 530ms。最初全套测试 70s；限定"只泵 Enter/Tab"后 **1.0s**。这也是不无条件泵送的第二个理由。

## 3. 测试如何 headless 驱动嵌入表单（仓内首个真按键驱动 huh 的测试）

既有测试（importflow_test）绕过按键直接调内部方法/改字段；本任务的驱动法：

1. `p.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})` 逐字符打进页面 → 路由进嵌入表单 → textinput 变异 → `accessor.Set` **实时写进 draft 的绑定指针**（这就是 field 态需要单字段快照的原因，T1 缝合规则）。
2. 预填充清除：`tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl}`（textinput 的 deleteBeforeCursor，光标在末尾时清空整行）。
3. `Enter` 后页面**内部泵送**完成跳变——测试不需要任何外部 pump（对比 huh 官方测试的 `batchUpdate`：我们把它做进了被测物本身，因为真实 App 路由不给转发）。
4. 断言 `p.state`、`p.d.*`（值直读）、`p.View().Content`（含 ANSI 也可直接 `strings.Contains`，中文/标签断言不受色彩码影响；行宽断言用 `lipgloss.Width`）。

## 4. TDD 证据

- **Step 1 红**：先写 `editpage_test.go`（12 用例），`go test` 编译失败（`undefined: serverEditPage / newServerEditPage`）= 红。
- **Step 3 实现 → Step 4 绿的过程**：首版实现后 2 个用例失败（`TestEditPageInitialView`、`TestEditPageSecretMaskingSentinel`）——原因都是**测试没考虑分页**（初始 View 只显示第 1 页 3 项；修正断言为"页 1 可见项 + ↓ 走遍全列表收集后断言全部标签"，这更贴近验收条款"↑↓ 可跨页选到每一个字段"）。修的是测试不是实现。
- **绿**：`go test ./internal/tui/ -run TestEditPage -count=1` → 12/12 PASS，各 0.02s。
- **全量**：`gofmt -l .` 零输出；`go vet ./...` 干净；`go build ./...` 通过；`go test ./... -count=1` 全绿（tui 包 5.6s——新增测试只占 ~1s）。

用例 ↔ brief 7 条对照：

| brief | 测试 |
|---|---|
| ① 初始 View 全字段 + 第 1/Y 页 | TestEditPageInitialView（+ PagingAdvances 翻到 第 2/Y 页）|
| ② Enter→field→提交→●+（已改） | TestEditPageFieldEditMarksDirty |
| ③ field Esc 恢复 + 脏不变 | TestEditPageFieldEscRestores（含"二次进入基准=已提交值"）+ TestEditPageSecretFieldEscRestores（秘密字段走 Set 回写，T1 缝合规则）|
| ④ 保存项 Enter→submit+formDoneMsg | TestEditPageSaveItemFiresSubmit（seam 捕获）+ TestEditPageSaveEndToEnd（真 store 落库）|
| ⑤ list Esc→aborted 无写入 | TestEditPageListEscAbortsNoWrite（draft 已脏仍零写入）|
| ⑥ 秘密掩码哨兵 | TestEditPageSecretMaskingSentinel（list 态两字段）+ TestEditPageSecretFieldStateMasked（field 态 EchoModePassword）|
| ⑦ 宽度 60/120 不破碎 | TestEditPageWidthFit（两态、两档）+ TestEditPageWidthFollowsResize（WindowSizeMsg 跟随）|

## 5. 实现要点（与设计决策逐条对应）

- **list 态**：`list.New(nil, list.NewDefaultDelegate(), width-2, 20)` + `rebindListKeys`（panels.go 先例）；行 = `editFields(orig != nil)` 每项 `fieldPreview`（脏 → `● Label` + warnStyle 亮色 + `（已改）` 后缀）+ 末尾哨兵 `✓ 保存并退出`。页眉 = list.Title `编辑服务器: <orig.Name>`（**用 orig 名，draft 名可能已改**；orig==nil 时"新增服务器"）。页脚 = `↑↓ 选择 · Enter 编辑 · Esc 取消 · 第 X/Y 页`（X/Y 从 `list.Paginator.Page/TotalPages` 推导）。20 行高 → 每页 5 项 → 4 页：分页可见但不冗长。
- **field 态**：`f.Build(d)` 单字段表单 + `WithWidth(clamp(width-6, 24, 80))`；页脚 `Enter 确认 · Esc 放弃本字段`。进入时 `fieldSnap = snapshotDraft(d)[f.Key]`；Esc/ctrl+c → `f.Set(d, fieldSnap)` 恢复（**绝不走 f.Get**——秘密 Get 只回状态串），回 list 刷新。
- **宽度**：list `SetSize(width-2, 20)` 跟随构造宽度与 WindowSizeMsg（App 现不转发后者，处理是 T3 防御性预留）；View 整体按行 `ansi.Truncate` 硬剪到 width。
- **列表键保护**：`DisableQuitKeybindings()`（裸 q/esc 不再被列表变 tea.Quit——Esc 由页面先接）；`SetFilteringEnabled(false)`（`/` 过滤的异步 `FilterMatchesMsg` 会被 App 路由丢弃——半残不如关掉）；`SetShowStatusBar(false)`/`SetShowHelp(false)`（页面自渲染页脚）。
- **秘密零明文**：列表值预览全走 T1 的 Get→状态串；field 态输入框 EchoModePassword（复用 forms.go 构造器）；测试哨兵断言两态 View 均无明文。

## 6. Self-review / 遗留关注

1. **（预存 bug，超出本任务）App→overlay 只转发 KeyPressMsg**：formOverlay/importflow 的嵌入表单在真终端的组内推进是断的（第 2 节实证）。本页通过内部泵送自愈；其他流程建议另立 plan。T3 不受影响。
2. **ctrl+c 语义**：field 态 ctrl+c（huh abort）= 该字段撤销回 list（importflow supplement 的同类语义）；list 态 ctrl+c 走列表 ForceQuit = 整程序退出（与 App 全局一致）。两次 ctrl+c 可退出，符合直觉。
3. **editPageHeight 固定 20**：overlay 拿不到终端高度（App 不转发 WindowSizeMsg）。22 行总高适配 ≥24 行终端；更小终端会被渲染器硬剪（分页保证内容可达）。若 T4 owner 冒烟觉得拥挤，改一个常量即可。
4. **分页每页 5 项**由 20 行高推出（titlebar 2 行 + 分页点 1 行 → 17/3=5）；测试全部动态取 `Paginator.TotalPages/PerPage`，改高度不需要改测试。
5. **哨兵 desc**「Enter 提交全部改动」如实（整 draft 提交，包括未标脏的净字段——净字段本来就是原值）。
6. **铁律/秘密纪律**：无新依赖（全部 import 均为 tui 包既有）；无明文渲染路径；`submitServer` 语义零变化（TestEditPageSaveEndToEnd 断言 actionDoneMsg + store 落库）。
7. **owner gate（T4）**：headless 测试无法覆盖真终端渲染手感（alt-screen 残留、CJK 宽度目视、翻页节奏）——按 plan 由用户在 T4 冒烟。
