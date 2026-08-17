# Plan 29：TUI 服务器编辑流 — 字段选择器页（分页 + 脏标记）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 TUI 的服务器**编辑**流从 huh 三组长表单重构为**字段选择器页**：bubbles list 分页列出全部字段，↑↓ 选中、Enter 进单字段编辑、改过的字段亮色 + `●` 标记 + 值即时可见，未编辑字段原样显示；显式页码指示；底部保存项。

**Architecture:** 新 `serverEditPage` tea.Model（overlay，与 formOverlay 同生命周期——发 formDoneMsg 结束）内部两态：list 态（bubbles list，分页导航）↔ field 态（嵌入单字段 huh 表单，照 importflow 的 embedded-form 模式）。保存复用既有 `submitServer(st, orig, draft)`——**提交语义零变化**（还是整 draft 提交，只是收集方式变了）。新增/向导流程不动（保持现有 newServerForm）。

**Tech Stack:** 既有依赖（charm.land bubbles/v2 list —— panels.go:32 已是仓内先例；huh v2 单字段表单；lipgloss）零新增。

## Global Constraints

- **只改编辑流**：`newServerForm` 及其调用方（新增 `a`、向导、importflow 补全）一律不动；`submitServer`/`prefill`/`serverDraft` 结构不动（编辑页是它的第三个消费者）。
- **提交语义零变化**：保存仍走 `submitServer(a.st, cur, draft)` 整体提交；Esc 取消 = 现有 formDoneMsg{aborted} 语义（不落库）。
- **秘密值永不明文渲染**：密码/密钥口令/sudo 密码在列表值预览中只显示掩码（`••••` 或"(已设)"），编辑态用 EchoModePassword（沿用现有字段构造器）。
- **铁律不动**；无新依赖；测试密闭（t.TempDir + 表驱动）；gofmt/vet 干净；双 lane CI 绿。
- **文案与已实现行为一致**：页码、帮助行、脏标记说明如实。
- **TTY 交互最终验证是 owner gate**（本 plan 验收的最后一步由用户在真终端做——控制器无 TTY）。

## 背景（已取证）

1. 现状 `e` 键流（app.go:456-462）：`prefill(cur)` 填 serverDraft → `newFormOverlay("编辑服务器", newServerForm(draft, true), submit)`——三组 huh 长表单，组内导航在真终端失灵（用户实测），无页指示无脏标记。
2. panels.go:32 已用 `list.New(nil, list.NewDefaultDelegate(), 30, 12)`——bubbles list v2 是仓内既有栈，自带分页器。
3. serverDraft 15 个可编辑字段：Name/Host/Port/User + Password/KeyPath/KeyPass/SudoPassword/ClearCredential(编辑态) + Description/Location/Hardware/Services/Role/Caveats。**无 Tags**（TUI 编辑本就不改 tags，维持现状）。
4. importflow.go 的 embedded-form 模式（`f.form = …; f.form.Init(); Update 转发`）是嵌入 huh 表单的仓内先例。

## 设计决策（定死，评审按此判）

- **字段清单固定 15 项，顺序**：名称/Host/端口/SSH 用户/密码/私钥路径/密钥口令/sudo 密码/清除凭据(编辑态)/硬件/位置/角色/服务/Caveats/备注 + **末尾固定一项「✓ 保存并退出」**。
- **脏标记**：进入编辑页时对 draft 做快照（深拷贝字符串/bool/int）；列表项 Title = `● <字段名>`（脏，lipgloss 亮色）或 `<字段名>`（净）；值预览 = 当前 draft 值（净=原值原样显示；脏=新值 + `（已改）` 后缀）。
- **页码**：list 自带分页点 + 页眉/页脚显式 `第 X/Y 页`（从 list 分页器状态计算）；终端宽度感知沿用面板化既有机制（list.SetSize 跟随 app 宽度）。
- **field 态**：Enter 在列表项上 → 构造**单字段 huh 表单**（复用 forms.go 既有构造器：`passwordField(d, true)`/`portField(&d.Port)`/`huh.NewInput().Title(…).Value(&d.X)`；清除凭据用 `huh.NewConfirm`）→ 编辑完成（Enter 提交）回 list 态并刷新脏标记；Esc 在 field 态 = 放弃本次字段修改回 list（**需要保存进入 field 态前的值**，huh 的 Value 是指针直绑——field 态取消要么接受"改了就改了"要么快照该单字段——**定死：field 态 Esc 恢复进入该字段前的值**，单字段快照足够）。
- **保存项**：列表最后一项，Enter → `submitServer` + formDoneMsg；Esc 在 list 态 = 整体取消（不落库）。
- **帮助行**：list 态 `↑↓ 选择 · Enter 编辑 · Esc 取消`；field 态 `Enter 确认 · Esc 放弃本字段`。

## 任务间接口

- T1 产出纯逻辑层（字段表/脏计算/值预览格式化）——T2/T3 消费。
- T2 产出 `serverEditPage`——T3 只做 app.go 接线（约 5 行）。

---

### Task 1: 编辑页纯逻辑层（字段表 + 脏计算 + 预览格式化）

**Files:**
- Create: `internal/tui/editfields.go`
- Create: `internal/tui/editfields_test.go`

**Interfaces:**
```go
// editField 描述一个可编辑字段：如何读写 draft、如何显示。
type editField struct {
	Key     string                             // 稳定标识（测试/脏快照键）
	Label   string                             // 列表显示名
	Secret  bool                               // true=值预览掩码
	Get     func(d *serverDraft) string        // 当前值（Secret 字段返回 "已设"/"" 类状态而非内容）
	Set     func(d *serverDraft, v string)     // 写回（端口等在此做 Atoi 并夹验证）
	Build   func(d *serverDraft) *huh.Form     // 单字段编辑表单（复用 forms.go 构造器）
}

// editFields(editing bool) []editField —— 15+1 项固定序（设计决策节）
// snapshotDraft(d) map[string]string / dirtyAgainst(d, snap) map[string]bool
// fieldPreview(f editField, d *serverDraft, dirty bool) (title, desc string)
```

- [ ] **Step 1: 失败测试**：字段表完整性（15 项 + 保存项、顺序、editing=false 无清除凭据项）；Get/Set 往返（含端口 Atoi 失败路径）；脏计算（改 Hardware→仅 hardware 脏；改回原值→净）；秘密预览不含明文（哨兵断言：设 Password=SENTINEL 后 preview 不含 SENTINEL）。
- [ ] **Step 2: 红 → Step 3: 实现 → Step 4: 绿**（`go test ./internal/tui/ -count=1`）。
- [ ] **Step 5: Commit** `feat(tui): edit-field table + dirty snapshot + masked previews (Plan 29 T1)`。

### Task 2: serverEditPage 模型（list↔field 两态状态机）

**Files:**
- Create: `internal/tui/editpage.go`
- Create: `internal/tui/editpage_test.go`

**Interfaces:**
```go
// newServerEditPage(st *store.Store, orig *models.Server, d *serverDraft, width int) *serverEditPage
// 实现 tea.Model：Init/Update/View；结束时发 formDoneMsg（保存成功或 aborted）。
// list 态：bubbles list（panels.go:32 先例）+ 页眉「编辑服务器: <name>」+ 第 X/Y 页
// field 态：嵌入单字段 huh 表单（importflow 先例）；Esc 恢复进入前值
```

- [ ] **Step 1: 失败测试**（tea.KeyMsg 驱动 Update，不跑真 Program）：① 初始 View 含全部字段名 + 页码「第 1/Y 页」；② Enter 进 field 态（View 变为单字段表单）→ 表单提交回 list 且对应字段变 `●`+`（已改）`；③ field 态 Esc → 值恢复 + 脏标记不变；④ 光标移到「保存并退出」Enter → 产生 submit 动作 + formDoneMsg（用假 store/捕获 cmd）；⑤ list 态 Esc → formDoneMsg{aborted} 且 store 无写入；⑥ 秘密字段预览掩码（哨兵）；⑦ 宽度 60/120 两档 View 均不破碎（行宽 ≤ width 断言）。
- [ ] **Step 2: 红 → Step 3: 实现**（分页器显式页码从 list 状态推导；宽度跟随）→ **Step 4: 绿**。
- [ ] **Step 5: Commit** `feat(tui): serverEditPage — field picker with pagination and dirty marks (Plan 29 T2)`。

### Task 3: app.go 接线 + 回归

**Files:**
- Modify: `internal/tui/app.go`（serversKey 的 `e` 分支：换 overlay 构造，~5 行；宽度从面板化状态传入）
- Modify: `internal/tui/app_test.go`（既有编辑流测试适配——读现有 e 键测试后按新交互改写）

- [ ] **Step 1: 失败测试**：`e` 键打开的是 serverEditPage（View 含「编辑服务器」+ 字段列表）而非旧长表单；既有 servers 页其余按键回归不破。
- [ ] **Step 2: 红 → 实现 → 绿**（`go test ./internal/tui/ -count=1`）。
- [ ] **Step 3: Commit** `feat(tui): wire servers 'e' to the field-picker edit page (Plan 29 T3)`。

### Task 4: 文档 + 全量验证 + owner gate 移交

**Files:**
- Modify: `docs/managing-servers.md` 或 `docs/scenarios.md` 中 TUI 编辑操作说明处（grep `tui` 定位；无则 README TUI 段）——3 行内说明新编辑页交互（↑↓/Enter/Esc/保存项/脏标记/页码）。

- [ ] **Step 1: 全量验证**：`go build ./...`、`go vet ./...`、`gofmt -l .`、`go test ./... -count=1`。
- [ ] **Step 2: 文档**。
- [ ] **Step 3: Commit + 移交 owner gate**：真终端冒烟清单（编辑页进出无残留 / ↑↓ 跨页 / Enter 编辑改值后 ● 亮显 / field Esc 撤销 / 保存落库 / 整体 Esc 不落库 / 窄终端宽度）写进交付报告，交用户执行。
- [ ] **Step 4: Commit** `docs: field-picker edit page usage + owner smoke handoff (Plan 29 T4)`。

---

## 验收（整 plan）

1. `e` 打开字段选择器页：↑↓ 可跨页选到**每一个**字段（用户原始痛点），页码 `第 X/Y 页` 可见，可翻页。
2. 编辑过的字段 `●` + 亮色 + 新值；未编辑字段原样显示原值；秘密值全程掩码（测试哨兵 + 冒烟目视）。
3. 保存 = submitServer 原语义（含清除凭据/再凭据路径回归绿）；Esc 取消不落库。
4. 新增/向导/importflow 补全流程零变化（既有测试全绿）。
5. 全量测试双 lane 绿；gofmt/vet 零输出。
6. **Owner gate（用户，真终端）**：T4 Step 3 清单逐项过。
