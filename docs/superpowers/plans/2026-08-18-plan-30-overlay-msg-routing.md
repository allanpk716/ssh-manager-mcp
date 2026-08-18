# Plan 30:Overlay 消息路由修复(backlog #9)Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 修复三个 program 级 Update(App / wizardModel / clientModel)的消息路由:overlay 打开期间除程序 owned 消息外的一切消息(含 huh 的 unexported 协议消息)送达 overlay,cmd 交还 runtime 异步回环——嵌入 huh 表单的 overlay 在真终端恢复前进/完成能力。

**Architecture:** 每个 program 级 Update 加拦截门(owned 白名单空体落出 + WindowSizeMsg 记录后转发 + default 全转发);embed 表单且按消息类型 switch 的 overlay(importFlow / editpage / wizardModel 表单尾部)加第二层默认转发(经共享尾部含状态检查);editpage 删除 Plan 29 的同步泵改纯异步。设计基线 = `docs/superpowers/specs/2026-08-18-plan-30-overlay-msg-routing-design.rev2.md` + xcheck 闭环终态的 9 条强制实现注记(`.xcheck/20260818-173526/CLOSE.md`,已并入本文各任务)。

**Tech Stack:** 既有依赖(charm.land/bubbletea/v2 v2.0.8、huh/v2 v2.0.3、bubbles/v2)零新增。

## Global Constraints

- **铁律不动**;无新依赖;无存储/协议/CLI/MCP 面变化(compat-matrix 不涉及)。
- **owned 按消费者定义**:一个消息类型属于某 program 的 owned 集合 ⇔ 该 program 的主 switch 有它的 case;`listMsg` 谓词路径(panels.go:83)不算 switch case。新增程序自有消息类型时必须同步登记进对应门的集合(门注释写死此约定)。
- **三处门语义同构、结构允许异形**:App/clientModel 是前置拦截门;wizardModel 的主 switch 本身就是 owned 层(owned case 天然先于 overlay 目标分派),门只表现为新增 default 分支(注记 1 的解法,消解 rev2 谓词第 0/1 条矛盾)。
- **wizardModel 判定顺序(定死)**:1. stepClient 委托最外(维持现状头 wizard.go:389-401,仅 wizardDoneMsg 例外,其余全权委托内嵌 clientModel——formDoneMsg/errMsg 在委托期是活消息,归内层)→ 2. owned(主 switch 各 case)→ 3. w.ov(静态屏,现状吞 q/Esc 是 wizard.go:404-411 "Deliberate" 设计)→ 4. w.form(default 分支喂,经共享尾部)。
- **editpage 迁移后既有断言全绿**(字段提交、脏标记、Esc 恢复、confirm y/n);submitServer/prefill/serverDraft 不动。
- **秘密值渲染规则不动**;测试密闭(t.TempDir + t.Setenv);gofmt/vet 干净;全量 `go test ./...` 绿。
- **TTY 交互最终验证是 owner gate**(本 plan 验收最后一步由用户在真终端做,冒烟矩阵见 Task 6)。

## 背景(已取证,实现者必读)

1. **死因**:huh v2.0.3 前进协议全靠消息回环——`Input` 按 Enter 返回 `NextField` cmd → runtime 执行产出 `nextFieldMsg`(unexported)→ `Group.Update` 处理它才 `selector.Next()`;组末 → `nextGroupMsg` → `Form.Update` 处理它才置 `StateCompleted`。**unexported ⇒ 路由层只能"owned 白名单 + default 全转发"**。
2. `formOverlay.Update`(forms.go:254-275)**已经透明**且在每次 `o.form.Update(msg)` 后无条件查 `StateCompleted/StateAborted` 并产 `formDoneMsg` cmd——主路径完成检测位置天然正确,门只需把消息送到。
3. importFlow 的 `startBatch` 置 stateImporting **不清 f.form**(importflow.go:195-196)——stale form 存在,第二层转发必须用状态谓词(注记 9)。
4. clientModel 的 editConnForm 经 `m.overlay` 装载 `newFormOverlay`(clientpage.go:335)——单条件门够用,无需第二层改动;其 overlay 分支(clientpage.go:197-202)先于 quit case(:214),与 App 同款。
5. `clientops.CachePaths()` 有 `SSHMGR_CACHE_DIR` env seam(clientops.go:96)——clientModel 真表单回环测试可用 `t.Setenv` 隔离。
6. resize 无需补偿(差分实验证伪:页面渲染每帧动态取宽);App 的 WindowSizeMsg case 仅记录(app.go:340-342);wizardModel 无 WindowSizeMsg case;clientModel 有记录(clientpage.go:237-239)。
7. 现有测试全部直接驱动 overlay/page.Update,**没有任何测试经过 program 级路由层跑回环**——本 plan 的核心缺口。

## 设计决策(定死,评审按此判)

- 拦截门伪码(以 App 为例;clientModel 同构换成自己的 owned 集):

```go
if a.overlay != nil {
	switch msg := msg.(type) {
	case errMsg, actionDoneMsg, formDoneMsg, serveInstalledMsg,
		serveProbeMsg, deviceCodeIssuedMsg, tokenIssuedMsg:
		// owned:空体,落到下方原有 switch(主 WindowSizeMsg case 只在
		// overlay==nil 时可达——两处记录语句用同一 resize helper,防漂移)
	case tea.WindowSizeMsg:
		a.resize(m...) // 见正文:记录宽高
		ov, cmd := a.overlay.Update(msg)
		a.overlay, _ = ov.(overlay) // 写回;comma-ok 失败=不可达防御(见注释)
		return a, cmd
	default:
		ov, cmd := a.overlay.Update(msg)
		a.overlay, _ = ov.(overlay)
		return a, cmd
	}
}
```

- 免费修复(验收显式覆盖):importDoneMsg 回环恢复;**表单步骤内** paste/光标闪烁恢复(importFlow 非表单步骤未知消息仍丢=现状);editpage 的 WindowSizeMsg 处理器激活;editpage Shift+Tab 回退恢复。
- 范围外:overlay 关闭时页面 list 的 blink/spinner 暂停(现状如此);stepClient 委托期 wizard 自有消息被内层吞(不可达论证 + 注释);向导 q/Esc 前置拦截既有取舍(wizard.go:417,要改另立 backlog)。

## 任务间接口

- Task 1 产出测试基建 `spyOverlay` / `drain`(internal/tui 测试共享)与 App 门——后续任务复用 drain 跑回环。
- Task 2/3/4/5 各自独立交付一个 overlay/入口的第二层转发,互不依赖(Task 3/4/5 依赖 Task 1 的 drain)。
- Task 6 收网:App 层三组表单 e2e + 文档回写 + 全量验证 + owner 冒烟清单。

---

### Task 1: 测试基建(spyOverlay + drain)+ App 拦截门 + 门单测 + 新增 Profile 回环

**Files:**
- Create: `internal/tui/routing_helpers_test.go`(spyOverlay + drain + seedStoreApp)
- Modify: `internal/tui/app.go`(Update 入口加门 :142 起;删 KeyPressMsg overlay 分支 :160-163;WindowSizeMsg case 改用 resize helper)
- Test: `internal/tui/app_routing_test.go`(新建,门单测 + Profile 回环)

**Interfaces:**
- Consumes: `newTestApp`(app_test.go:21,返回 App;注意它不返回 store——本任务自建 `seedStoreApp` 同时返回两者)
- Produces: `func drain(t *testing.T, m tea.Model, cmds ...tea.Cmd) tea.Model`;`type spyOverlay struct`;`func seedStoreApp(t *testing.T) (App, *store.Store)`;`func (a *App) resize(w, h int)`——后续任务全部复用

- [ ] **Step 1: 写失败测试(基建 + 门)**

`internal/tui/routing_helpers_test.go`:

```go
package tui

// Plan 30 T1: routing-test infrastructure shared by all gate/loop tests.

import (
	"testing"

	"charm.land/bubbles/v2/cursor"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// spyOverlay records every message the routing layer hands it. update returns
// a NEW instance when swap != nil — that lets tests assert the gate writes the
// overlay pointer back (Plan 30 注记 8).
type spyOverlay struct {
	got  []tea.Msg
	cmd  tea.Cmd // returned on every Update (sentinel hand-back assertion)
	swap *spyOverlay
}

func (s *spyOverlay) Title() string             { return "spy" }
func (s *spyOverlay) Init() tea.Cmd             { return nil }
func (s *spyOverlay) View() tea.View            { return tea.NewView("spy") }
func (s *spyOverlay) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	s.got = append(s.got, msg)
	if s.swap != nil {
		return s.swap, s.cmd
	}
	return s, s.cmd
}

// spySaw reports whether the spy received a message of the same type as want.
func (s *spyOverlay) spySaw(want tea.Msg) bool {
	for _, m := range s.got {
		if reflect.TypeOf(m) == reflect.TypeOf(want) {
			return true
		}
	}
	return false
}

// drain simulates the bubbletea runtime: execute cmds, feed each produced msg
// back into m.Update, repeat. Blink/tick msgs are DROPPED — they self-perpetuate
// and would never end the loop (same discipline as the old editpage pump).
// tea.BatchMsg unfolds. Bounded: a runaway loop fails loudly instead of hanging.
func drain(t *testing.T, m tea.Model, cmds ...tea.Cmd) tea.Model {
	t.Helper()
	queue := append([]tea.Cmd(nil), cmds...)
	for steps := 0; len(queue) > 0; steps++ {
		if steps > 300 {
			t.Fatal("drain: runaway cmd loop (>300 steps)")
		}
		cmd := queue[0]
		queue = queue[1:]
		if cmd == nil {
			continue
		}
		msg := cmd()
		switch msg := msg.(type) {
		case nil, cursor.BlinkMsg, spinner.TickMsg:
			continue
		case tea.BatchMsg:
			queue = append(queue, msg...)
			continue
		}
		var next tea.Cmd
		m, next = m.Update(msg)
		queue = append(queue, next)
	}
	return m
}

// probeMsg is an unknown-to-everyone message type: the gate must forward it.
type probeMsg struct{}

// seedStoreApp is newTestApp (app_test.go:21) plus the store handle — loop
// tests need to assert persisted rows.
func seedStoreApp(t *testing.T) (App, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "t.db"), mk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	credID, err := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("p")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddServer(&models.Server{
		Name: "gpu", Host: "192.0.2.10", User: "u", Port: 22,
		AuthMethod: models.AuthPassword, CredentialID: credID,
	}); err != nil {
		t.Fatal(err)
	}
	a, err := NewBrokerApp(st)
	if err != nil {
		t.Fatal(err)
	}
	return a, st
}
```

(import 里补 `"path/filepath"` 和 `"reflect"`。)

`internal/tui/app_routing_test.go`:

```go
package tui

// Plan 30 T1: the App gate — while an overlay is open, everything except the
// App's owned msgs reaches the overlay, and the overlay's cmd goes back to the
// (simulated) runtime. These are the regression net for the class "tests all
// green, real terminal dead".

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestGateForwardsUnknownAndHandsCmdBack(t *testing.T) {
	a, _ := seedStoreApp(t)
	spy := &spyOverlay{cmd: func() tea.Msg { return probeMsg{} }}
	a.overlay = spy
	m, cmd := a.Update(probeMsg{})
	if !m.(App).overlay.(*spyOverlay).spySaw(probeMsg{}) {
		t.Fatal("unknown msg must reach the overlay")
	}
	if cmd == nil {
		t.Fatal("gate must hand the overlay's cmd back to the runtime")
	}
	if _, ok := cmd().(probeMsg); !ok {
		t.Fatal("handed-back cmd must be the spy's sentinel")
	}
}

func TestGateWritesOverlayBack(t *testing.T) {
	a, _ := seedStoreApp(t)
	replacement := &spyOverlay{}
	spy := &spyOverlay{swap: replacement}
	a.overlay = spy
	m, _ := a.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if got := m.(App).overlay; got != tea.Model(replacement) {
		t.Fatalf("gate must write the overlay pointer back, got %T", got)
	}
}

func TestGateOwnedFallsThrough(t *testing.T) {
	a, _ := seedStoreApp(t)
	spy := &spyOverlay{}
	a.overlay = spy
	// every owned type must NOT reach the overlay and must run App logic
	for _, owned := range []tea.Msg{
		errMsg{}, actionDoneMsg{}, formDoneMsg{},
		serveInstalledMsg{}, serveProbeMsg{}, tokenIssuedMsg{},
	} {
		a.overlay = spy
		m, _ := a.Update(owned)
		app := m.(App)
		if spy.spySaw(owned) {
			t.Fatalf("owned %T must fall through to the App switch", owned)
		}
		_ = app
	}
	// formDoneMsg specifically closes the overlay
	a.overlay = spy
	m, _ := a.Update(formDoneMsg{})
	if m.(App).overlay != nil {
		t.Fatal("formDoneMsg must close the overlay")
	}
}

func TestGateWindowSizeRecordsAndForwards(t *testing.T) {
	a, _ := seedStoreApp(t)
	spy := &spyOverlay{}
	a.overlay = spy
	m, _ := a.Update(tea.WindowSizeMsg{Width: 60, Height: 30})
	app := m.(App)
	if !spy.spySaw(tea.WindowSizeMsg{}) {
		t.Fatal("resize must reach the overlay")
	}
	if app.width != 60 || app.height != 30 {
		t.Fatalf("resize must be recorded, got %dx%d", app.width, app.height)
	}
}

func TestAppLoopProfileFormCompletes(t *testing.T) {
	a, st := seedStoreApp(t)
	m, _ := a.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyTab})        // servers → profiles
	m, cmd := m.Update(tea.KeyPressMsg{Code: 'a', Text: "a"}) // 新增 Profile
	if cmd == nil {
		t.Fatal("'a' must open the form and return its Init cmd")
	}
	m = drain(t, m, cmd)
	for _, r := range "gp" {
		m, _ = m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	m, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = drain(t, m, cmd)
	if m.(App).overlay != nil {
		t.Fatal("single-field form must complete and close the overlay")
	}
	profiles, err := st.ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 1 || profiles[0].Name != "gp" {
		t.Fatalf("profile gp must be persisted, got %+v", profiles)
	}
}
```

注:`errMsg{}`/`actionDoneMsg{}` 等零值构造——`serveInstalledMsg`/`serveProbeMsg`/`tokenIssuedMsg`/`deviceCodeIssuedMsg` 若带非零字段按其定义补最小值(实现者以各自 type 定义为准,只断言类型路由,不断言处理副作用——formDoneMsg 除外)。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/tui/ -run 'TestGate|TestAppLoopProfile' -v`
Expected: FAIL——`TestGateForwardsUnknownAndHandsCmdBack`(probeMsg 被现状 default 丢,spy 收不到)、`TestGateWritesOverlayBack`(现状 KeyPressMsg 分支已写回,可能过)、`TestGateOwnedFallsThrough` 的 formDoneMsg 关 overlay 可能过(现状即有),`TestAppLoopProfileFormCompletes` FAIL(Enter 后 cmd 链丢,表单不完成)。**至少前两个必须红**。

- [ ] **Step 3: 实现 App 门**

`internal/tui/app.go`:在 `func (a App) Update` 开头(`if a.overlay == nil` 列表面板块**之前**)插入:

```go
	// Plan 30 gate: while an overlay is open it owns EVERYTHING except the
	// App's own messages. huh advances fields/groups via unexported msgs
	// (nextFieldMsg/nextGroupMsg) — they can only be routed by "owned
	// allowlist + forward everything else". owned ⇔ the main switch below
	// has a case for the type (the listMsg predicate path is NOT a case).
	// NEW App-owned message types MUST be registered here (checklist item).
	if a.overlay != nil {
		switch msg := msg.(type) {
		case errMsg, actionDoneMsg, formDoneMsg, serveInstalledMsg,
			serveProbeMsg, deviceCodeIssuedMsg, tokenIssuedMsg:
			// owned: empty body — fall out of the gate into the switch below
		case tea.WindowSizeMsg:
			a.resize(msg.Width, msg.Height)
			ov, cmd := a.overlay.Update(msg)
			a.overlay, _ = ov.(overlay) // comma-ok failure = unreachable defense (spy tests lock the type)
			return a, cmd
		default:
			ov, cmd := a.overlay.Update(msg)
			a.overlay, _ = ov.(overlay)
			return a, cmd
		}
	}
```

并:(a) 删除大 switch `case tea.KeyPressMsg` 里的 overlay 分支(`if a.overlay != nil { … }` 四行);(b) 把 `case tea.WindowSizeMsg:` 的记录改为 `a.resize(m.Width, m.Height)` 并加一行注释指向门内同名调用(防双份漂移);(c) 在 App 结构体附近加:

```go
// resize records the terminal size. Called from BOTH the overlay gate's
// WindowSizeMsg branch and the no-overlay main-switch case — keep them in
// sync through this one method (anti-drift, Plan 30 注记 6).
func (a *App) resize(w, h int) { a.width, a.height = w, h }
```

同步改写 Update 顶部的路由注释块(app.go:142-149):加一句"overlay 打开时门接管一切非 owned 消息(Plan 30)"。

- [ ] **Step 4: 跑测试确认通过 + 既有套件回归**

Run: `go test ./internal/tui/ -run 'TestGate|TestAppLoopProfile' -v` → 全 PASS
Run: `go test ./internal/tui/` → 全绿(重点 TestServersPageDispatch / TestApp_TabCyclesPages 不回归)

- [ ] **Step 5: Commit**

```bash
git add internal/tui/routing_helpers_test.go internal/tui/app_routing_test.go internal/tui/app.go
git commit -m "feat(tui): App overlay 消息路由门 + spy/drain 测试基建 + Profile 回环(Plan 30 T1)"
```

---

### Task 2: importFlow 第二层默认转发(状态谓词 + 共享尾部)

**Files:**
- Modify: `internal/tui/importflow.go`(Update 的 KeyPressMsg 尾部提取为 feedFormMsg;加 default 分支)
- Test: `internal/tui/importflow_test.go`(追加)

**Interfaces:**
- Consumes: Task 1 的 `drain`
- Produces: `func (f *importFlow) feedFormMsg(msg tea.Msg) (tea.Model, tea.Cmd)`;`func formStepActive(s importState) bool`

- [ ] **Step 1: 写失败测试**

追加到 `internal/tui/importflow_test.go`:

```go
// formStepActive is the state predicate for layer-2 forwarding: ONLY form
// steps feed the embedded form. The form POINTER is not enough — startBatch
// switches to stateImporting without clearing f.form (stale form, Plan 30 注记 9).
func TestFormStepActive(t *testing.T) {
	for s, want := range map[importState]bool{
		statePathForm: true, statePick: true, stateSupplement: true,
		stateImporting: false, stateResult: false,
	} {
		if got := formStepActive(s); got != want {
			t.Fatalf("formStepActive(%d) = %v, want %v", s, got, want)
		}
	}
}

// The path form must complete THROUGH Update's default branch: Enter returns
// a cmd whose produced msgs (nextFieldMsg/nextGroupMsg — unexported, typeless
// to us) come back via drain and flip the flow to statePick.
func TestImportFlowPathFormLoopAdvances(t *testing.T) {
	st := newImportFlowTestStore(t) // 若既有 helper 名不同,以其为准(importflow_test.go 的 setup)
	f := newImportFlow(st)
	fm, cmd := f.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // submit prefilled path
	_ = fm
	m := drain(t, fAsModel(t, f), cmd)
	got := m.(importFlowFixture(t, f)) // 见下:drain 回灌 importFlow 的桥
	_ = got
	if f.state != statePick {
		t.Fatalf("path form must complete into statePick via the loop, got %d", f.state)
	}
}
```

注意:importFlow.Update 是**指针接收者**且 flow 必须是同一实例(Value 绑定)。drain 走 tea.Model 接口会拿到同一指针——直接断言 `f.state` 即可。测试简化为:

```go
func TestImportFlowPathFormLoopAdvances(t *testing.T) {
	st := newImportFlowTestStore(t)
	f := newImportFlow(st)
	_, cmd := f.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	drain(t, f, cmd) // f implements tea.Model (pointer receiver)
	if f.state != statePick {
		t.Fatalf("path form must complete into statePick via the loop, got %d", f.state)
	}
}
```

负向测试:

```go
// stateImporting keeps a stale f.form (startBatch doesn't clear it) — unknown
// msgs must NOT be fed to it (state predicate, Plan 30 注记 9).
func TestImportFlowImportingSwallowsUnknown(t *testing.T) {
	st := newImportFlowTestStore(t)
	f := newImportFlow(st)
	f.state = stateImporting // simulate mid-batch; f.form still set from pick
	before := f.form
	m, cmd := f.Update(probeMsg{})
	_ = m
	if cmd != nil {
		t.Fatal("unknown msg in stateImporting must be a no-op")
	}
	if f.form != before {
		t.Fatal("stale form must not be touched in non-form steps")
	}
}
```

(`newImportFlowTestStore`:用 importflow_test.go 既有 setup;若无同名 helper,以其等价物替换——它建一个带一条 server 的临时 store。)

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/tui/ -run 'TestFormStepActive|TestImportFlowPathFormLoop|TestImportFlowImportingSwallows' -v`
Expected: 编译失败(`formStepActive` 未定义)→ 先加空实现再跑,`TestImportFlowPathFormLoopAdvances` FAIL(现状 default 丢 nextFieldMsg,状态停在 statePathForm)。

- [ ] **Step 3: 实现**

`internal/tui/importflow.go`:

```go
// formStepActive is the layer-2 state predicate: only form steps feed the
// embedded form. The pointer alone is NOT the signal — startBatch switches
// to stateImporting WITHOUT clearing f.form (stale form, Plan 30 注记 9).
func formStepActive(s importState) bool {
	switch s {
	case statePathForm, statePick, stateSupplement:
		return true
	}
	return false
}

// feedFormMsg forwards one message to the embedded form and runs the SHARED
// post-update tail (abort/complete handling). Used by BOTH the KeyPressMsg
// case and Update's default branch — the two paths must stay identical
// (Plan 30 注记 2). formDoneMsg production happens HERE on completion.
func (f *importFlow) feedFormMsg(msg tea.Msg) (tea.Model, tea.Cmd) {
	fm, cmd := f.form.Update(msg)
	if nf, ok := fm.(*huh.Form); ok {
		f.form = nf
	}
	if f.form.State == huh.StateAborted { // ctrl+c inside huh: same as Esc per state
		switch f.state {
		case stateSupplement:
			f.nextSupplement()
			return f, f.currentCmd()
		default:
			return f, func() tea.Msg { return formDoneMsg{aborted: true} }
		}
	}
	if f.form.State != huh.StateCompleted {
		return f, cmd
	}
	switch f.state {
	case statePathForm:
		return f.afterPathForm()
	case statePick:
		return f, f.startBatch()
	case stateSupplement:
		return f.submitSupplement()
	}
	return f, cmd
}
```

Update 内:`case tea.KeyPressMsg` 的尾部(`fm, cmd := f.form.Update(msg)` 起到 `switch f.state …` 整段)替换为 `return f.feedFormMsg(m)`;switch 末尾加:

```go
	default:
		// Plan 30 layer-2: huh's unexported protocol msgs (nextFieldMsg /
		// nextGroupMsg — forwarded here by the App gate) reach the form ONLY
		// in form steps (state predicate — the form pointer stays stale in
		// stateImporting/stateResult). Non-form steps keep today's drop.
		if f.form != nil && formStepActive(f.state) {
			return f.feedFormMsg(msg)
		}
		return f, nil
```

(原 `return f, nil` 兜底保留在 default 内。)

- [ ] **Step 4: 跑测试确认通过 + 回归**

Run: `go test ./internal/tui/ -run 'TestImportFlow|TestFormStepActive' -v` → 全 PASS(既有 TestImportFlowPathFormEscAborts 等不回归)
Run: `go test ./internal/tui/` → 全绿

- [ ] **Step 5: Commit**

```bash
git add internal/tui/importflow.go internal/tui/importflow_test.go
git commit -m "feat(tui): importFlow 第二层默认转发(状态谓词+共享尾部)(Plan 30 T2)"
```

---

### Task 3: wizardModel 门(委托最外 + owned 主 switch + default 分支)+ 三态分派 + wizard 回环

**Files:**
- Modify: `internal/tui/wizard.go`(Update 加 default 分支;KeyPressMsg 尾部提取为 feedFormMsg;注释三处)
- Test: `internal/tui/wizard_routing_test.go`(新建)

**Interfaces:**
- Consumes: Task 1 的 `drain`、`spyOverlay`、`probeMsg`
- Produces: `func (w wizardModel) feedFormMsg(msg tea.Msg) (tea.Model, tea.Cmd)`(值接收者,与 Update 同形)

- [ ] **Step 1: 写失败测试**

`internal/tui/wizard_routing_test.go`:

```go
package tui

// Plan 30 T3: wizardModel routing — delegation outermost, owned cases in the
// main switch (they beat the overlay target), default branch does target
// selection (w.ov first, else the form via the shared tail). 注记 1/2/3.

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"ssh-manager-mcp/internal/roles"
)

// Dispatch state A — stepClient delegation: unknown msgs go to the inner
// clientModel; wizardDoneMsg is the one exception (escapes to the main switch).
func TestWizardGateDelegatesInStepClient(t *testing.T) {
	var client *clientModel
	w := wizardModel{step: stepClient, client: client}
	inner := &clientModel{}
	w.client = inner
	spy := &spyOverlay{}
	inner.overlay = spy
	m, _ := w.Update(probeMsg{})
	_ = m
	if !spy.spySaw(probeMsg{}) {
		t.Fatal("unknown msg in stepClient must reach the INNER model's overlay")
	}
}

// Dispatch state B — static screen: unknown msgs reach w.ov; owned msgs
// (errMsg) do NOT — they run wizard logic even while the screen is up.
func TestWizardGateOwnedBeatsStaticScreen(t *testing.T) {
	w := wizardModel{step: stepToken}
	spy := &spyOverlay{}
	w.ov = spy
	m, _ := w.Update(probeMsg{})
	_ = m
	if !spy.spySaw(probeMsg{}) {
		t.Fatal("unknown msg must reach the static screen")
	}
	w.ov = spy
	spy.got = nil
	m, _ = w.Update(errMsg{err: errForTest()})
	_ = m
	if spy.spySaw(errMsg{}) {
		t.Fatal("owned errMsg must fall to the wizard switch, not the screen")
	}
	if w.err == nil {
		t.Fatal("errMsg must be recorded by the wizard switch")
	}
}

func errForTest() error { return errors.New("boom") }

// Loop — a real form completes through the default branch: standalone server
// form, fill the required fields, Enter, drain, step advances.
func TestWizardLoopServerFormCompletes(t *testing.T) {
	// wizard_test.go 的既有 setup(store + 起始状态);以其 helper 名为准
	w := newWizardForRoutingTest(t) // step = stepServerForm,form = wizServerLoopForm
	m, cmd := w.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	_ = m
	m = drain(t, wAsModel(w), cmd)
	w2 := m.(wizardModel)
	_ = w2
	// 填必填字段后 Enter → drain → 表单完成 → stepFormDone 推进步骤。
	// 具体断言:完成服务器表单后进入 confirm 步(actionDoneMsg 或步骤推进)
	if w2.step == stepServerForm && w2.form != nil && w2.form.State != huh.StateCompleted {
		t.Fatalf("form must advance/complete through the routed loop, step=%d", w2.step)
	}
}
```

实现者按 wizard_test.go 既有脚手架校准 `newWizardForRoutingTest`(需要:临时 store、step 停在 stepServerForm、w.form = wizServerLoopForm(w.data.srvDraft));填字段 = 对 w.Update 喂 KeyPressMsg 字符 + Enter + drain,序列与表单字段序一致(名称/Host 必填)。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/tui/ -run 'TestWizardGate|TestWizardLoop' -v`
Expected: A/B 可能已过或部分过(现状 errMsg case 本就在主 switch);`TestWizardLoopServerFormCompletes` FAIL(现状 nextFieldMsg 无路回 form)。

- [ ] **Step 3: 实现**

`internal/tui/wizard.go`:

(a) Update 的 `case tea.KeyPressMsg:` 尾部(`f, cmd := w.form.Update(msg)` 到 `return w.stepFormDone()`)提取为:

```go
// feedFormMsg is the SHARED form tail used by BOTH the KeyPressMsg case and
// Update's default branch (Plan 30 注记 2): feed the form, then the
// abort/complete checks — identical on both paths. formDoneMsg-equivalent
// progression (stepFormDone) happens HERE on completion.
func (w wizardModel) feedFormMsg(msg tea.Msg) (tea.Model, tea.Cmd) {
	if w.form == nil {
		return w, nil
	}
	f, cmd := w.form.Update(msg)
	if nf, ok := f.(*huh.Form); ok {
		w.form = nf
	}
	if w.form.State == huh.StateAborted {
		return w, tea.Quit
	}
	if w.form.State != huh.StateCompleted {
		return w, cmd
	}
	return w.stepFormDone()
}
```

KeyPressMsg case 尾部改为 `return w.feedFormMsg(m)`。

(b) Update 的 switch 末尾加 default 分支:

```go
	default:
		// Plan 30 gate (注记 1 的解法): delegation is the outermost layer
		// (the head above — formDoneMsg/errMsg are LIVE during stepClient and
		// belong to the inner model); owned cases are the main switch itself
		// (they run before any overlay target); THIS branch is the target
		// selection for everything else — huh's unexported protocol msgs,
		// blink, paste, resize. Static screen first (swallows q/Esc by
		// Deliberate design, wizard.go:404-411), else the form via the
		// shared tail. q/Esc/Ctrl+C interception stays KeyPressMsg-only and
		// AFTER the w.ov check — current semantics (向导输入框打不进 q 是
		// 既有取舍, wizard.go:417 + importflow.go:426-436 同款注释).
		if w.ov != nil {
			ov, cmd := w.ov.Update(msg)
			w.ov, _ = ov.(overlay)
			return w, cmd
		}
		return w.feedFormMsg(msg)
```

(c) 委托头(wizard.go:389-401)加注释(注记 1 的 formDoneMsg 分析):

```go
	// 1. stepClient delegation is OUTERMOST (Plan 30 注记 1): the flow IS the
	// clientModel; only wizardDoneMsg escapes. This MUST precede everything —
	// formDoneMsg/errMsg are live during delegation (editConnForm completion
	// emits formDoneMsg) and belong to the inner model, whose own gate routes
	// them. wizard-owned mint/install/probe msgs are UNREACHABLE in the client
	// branch (those steps don't run), so nothing of the wizard's is lost.
```

- [ ] **Step 4: 跑测试确认通过 + 回归**

Run: `go test ./internal/tui/ -run 'TestWizard' -v` → 新测试 PASS,既有 wizard/wizardsteps/wizardserve 测试不回归
Run: `go test ./internal/tui/` → 全绿

- [ ] **Step 5: Commit**

```bash
git add internal/tui/wizard.go internal/tui/wizard_routing_test.go
git commit -m "feat(tui): wizardModel 路由门(委托最外+owned主switch+default目标分派)(Plan 30 T3)"
```

---

### Task 4: clientModel 门 + 引证注释 + 门测试 + 真表单回环

**Files:**
- Modify: `internal/tui/clientpage.go`(Update 入口加门;删 KeyPressMsg overlay 分支 :198-202;WindowSizeMsg case 改用共享记录注释)
- Test: `internal/tui/clientpage_routing_test.go`(新建)

**Interfaces:**
- Consumes: Task 1 的 `drain`、`spyOverlay`、`probeMsg`
- Produces: 无(门是终端改动)

- [ ] **Step 1: 写失败测试**

`internal/tui/clientpage_routing_test.go`:

```go
package tui

// Plan 30 T4: the clientModel gate — same shape as the App gate. The client's
// only form overlay is editConnForm → newFormOverlay (clientpage.go:335,
// already transparent), so NO layer-2 change is needed here (注记:包含关系).

import (
	"os"
	"path/filepath"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func newClientModelForGate(t *testing.T) clientModel {
	m := clientModel{width: 80, height: 24}
	return m
}

func TestClientGateForwardsUnknownAndHandsCmdBack(t *testing.T) {
	m := newClientModelForGate(t)
	spy := &spyOverlay{cmd: func() tea.Msg { return probeMsg{} }}
	m.overlay = spy
	m2, cmd := m.Update(probeMsg{})
	if !m2.(clientModel).overlay.(*spyOverlay).spySaw(probeMsg{}) {
		t.Fatal("unknown msg must reach the overlay")
	}
	if cmd == nil || func() bool { _, ok := cmd().(probeMsg); return !ok }() {
		t.Fatal("gate must hand the overlay's cmd back")
	}
}

func TestClientGateOwnedFallsThrough(t *testing.T) {
	m := newClientModelForGate(t)
	spy := &spyOverlay{}
	m.overlay = spy
	m2, _ := m.Update(formDoneMsg{})
	if spy.spySaw(formDoneMsg{}) {
		t.Fatal("formDoneMsg must fall through to clientModel's own case")
	}
	if m2.(clientModel).overlay != nil {
		t.Fatal("formDoneMsg closes the overlay (clientModel's own case)")
	}
}

func TestClientGateWindowSizeRecordsAndForwards(t *testing.T) {
	m := newClientModelForGate(t)
	spy := &spyOverlay{}
	m.overlay = spy
	m2, _ := m.Update(tea.WindowSizeMsg{Width: 60, Height: 30})
	cm := m2.(clientModel)
	if !spy.spySaw(tea.WindowSizeMsg{}) || cm.width != 60 || cm.height != 30 {
		t.Fatal("resize must be recorded AND forwarded")
	}
}

// True-form loop (注记 8 second half): drive editConnForm to completion
// through Update + drain. SSHMGR_CACHE_DIR isolates the credential write
// (clientops.go:96 env seam).
func TestClientLoopEditConnFormCompletes(t *testing.T) {
	t.Setenv("SSHMGR_CACHE_DIR", t.TempDir())
	m := newClientModelForGate(t)
	m.wizard = false
	m2, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Text: "c"}) // panel mode requires cred… wizard=false and cred==nil → 'c' is refused!
	// → 用向导形态:'c' 在 wizard=true 且无 cred 时直接开表单(clientpage.go:223)
	m.wizard = true
	m2, cmd = m.Update(tea.KeyPressMsg{Code: 'c', Text: "c"})
	if cmd == nil {
		t.Fatal("'c' must open editConnForm and return its Init cmd")
	}
	m3 := drain(t, m2, cmd)
	for _, r := range "127.0.0.1:1" { // serve 地址(合法 URL)
		m3, _ = m3.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	// 设备码留空=保持不变,但无存量 token → 提交会产 errMsg(设备码不能为空)
	// → 先填设备码,再 pin,再 Enter。
	for _, r := range "abcd1234" {
		m3, _ = m3.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	for _, r := range "sha256/AAAA" {
		m3, _ = m3.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	m4, cmd := m3.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	_ = m4
	m5 := drain(t, m4, cmd)
	cm := m5.(clientModel)
	if cm.overlay != nil {
		t.Fatal("editConnForm must complete and close")
	}
	// cred persisted into the temp dir
	if _, err := os.Stat(filepath.Join(t.TempDir(), "cache.auth.json")); err != nil {
		t.Fatalf("cache.auth.json must be written: %v", err)
	}
}
```

实现者注意:`validServeURL`/`validPin` 的具体校验以其定义为准调整填值(断言目标是"表单经回环完成",不是具体值);若 pin 校验要求 SPKI 形态,用其测试里的合法样例。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/tui/ -run 'TestClientGate|TestClientLoop' -v`
Expected: `TestClientGateForwardsUnknownAndHandsCmdBack` FAIL(probeMsg 现状被丢);`TestClientLoopEditConnFormCompletes` FAIL(Enter 链断)。

- [ ] **Step 3: 实现**

`internal/tui/clientpage.go` Update 开头(switch 之前)加门,owned 集 = `dataReadyMsg, syncDoneMsg, pullSucceededMsg, connSavedMsg, clientStatusMsg, errMsg, formDoneMsg`:

```go
	// Plan 30 gate (same shape as the App's). owned ⇔ the switch below has a
	// case (注记 4: the overlay branch at :197-202 sat BEFORE the quit case
	// :214 — so overlay-open Ctrl+C/q already went to the overlay today;
	// absorbing KeyPressMsg here changes nothing). NEW client-owned message
	// types MUST be registered here.
	if m.overlay != nil {
		switch msg := msg.(type) {
		case dataReadyMsg, syncDoneMsg, pullSucceededMsg, connSavedMsg,
			clientStatusMsg, errMsg, formDoneMsg:
			// owned: fall through to the switch below
		case tea.WindowSizeMsg:
			m.width, m.height = msg.Width, msg.Height
			ov, cmd := m.overlay.Update(msg)
			m.overlay, _ = ov.(overlay)
			return m, cmd
		default:
			ov, cmd := m.overlay.Update(msg)
			m.overlay, _ = ov.(overlay)
			return m, cmd
		}
	}
```

删除 KeyPressMsg case 内的 overlay 分支(:198-202)。主 `case tea.WindowSizeMsg:`(现 :237-239)加一行注释指向门内记录(两处同义,防漂移)。

- [ ] **Step 4: 跑测试确认通过 + 回归**

Run: `go test ./internal/tui/ -run 'TestClient' -v` → 全 PASS(既有 TestClientWizard_PullFailureReopensFormWithDraft / TestEditConnFormRequiresCodeWhenNoToken 不回归)
Run: `go test ./internal/tui/` → 全绿

- [ ] **Step 5: Commit**

```bash
git add internal/tui/clientpage.go internal/tui/clientpage_routing_test.go
git commit -m "feat(tui): clientModel 路由门 + 真表单回环(SSHMGR_CACHE_DIR 隔离)(Plan 30 T4)"
```

---

### Task 5: editpage 泵迁移(删同步泵改纯异步)+ blink 断言 + tap 迁移

**Files:**
- Modify: `internal/tui/editpage.go`(feedForm 简化;Update 加 default;openCurrent 返回 Init cmd;删 pumpForm;确认 confirmAnswer 无其他引用后删)
- Test: `internal/tui/editpage_test.go`(tap 改造 + blink 断言追加)

**Interfaces:**
- Consumes: Task 1 的 `drain`
- Produces: `func tap(t *testing.T, p *serverEditPage, code rune) *serverEditPage`(签名变更,~10 调用点机械迁移)

- [ ] **Step 1: 改造 tap 并加 blink 失败测试**

`internal/tui/editpage_test.go`:tap 替换为:

```go
// tap sends a non-printable key (Enter/Esc/arrows) and drains the returned
// cmd loop the way the runtime would (blink/tick dropped) — the async
// replacement for the old synchronous pump (Plan 30 T5).
func tap(t *testing.T, p *serverEditPage, code rune) *serverEditPage {
	t.Helper()
	m, cmd := p.Update(tea.KeyPressMsg{Code: code})
	pp, ok := m.(*serverEditPage)
	if !ok {
		t.Fatalf("tap: update returned %T", m)
	}
	return drain(t, pp, cmd).(*serverEditPage)
}
```

调用点迁移:`tap(p, tea.KeyEnter)` → `p = tap(t, p, tea.KeyEnter)`(部分调用点已丢弃返回值/捕获 cmd 的按新签名改;`ctrl` helper 同款改造)。

追加 blink 断言(注记 8 / rev2 测试策略 6):

```go
// The field-state blink chain must be alive: opening a field returns a cmd
// whose execution yields a cursor.BlinkMsg, and feeding that back returns a
// further cmd (self-perpetuating) — the "cursor blinks" free fix, locked.
func TestEditPageFieldBlinkChainAlive(t *testing.T) {
	p, _, _ := newEditPageAt(t, 80)
	p = openField(t, p, 0) // 现有 helper;若 openField 内部不取 cmd,改为手写 Enter+断言 cmd
	var cmd tea.Cmd
	_, cmd = p.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) — // 不可:field 态 Enter 是提交。改为:
	// 进字段态的 Enter 发生在 list 态:
	m2, cmd2 := p.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	_ = m2
	if cmd2 == nil {
		t.Fatal("opening a field must return the form's Init cmd")
	}
	// unfold batch, find a BlinkMsg, feed it, expect another cmd
	queue := []tea.Cmd{cmd2}
	var fed bool
	for steps := 0; len(queue) > 0 && steps < 50; steps++ {
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		msg := c()
		switch msg := msg.(type) {
		case tea.BatchMsg:
			queue = append(queue, msg...)
		case cursor.BlinkMsg:
			var next tea.Cmd
			var m tea.Model
			m, next = p.Update(msg)
			p = m.(*serverEditPage)
			fed = true
			if next == nil {
				t.Fatal("blink must re-arm (self-perpetuating) — cursor would freeze")
			}
		}
	}
	if !fed {
		t.Fatal("field-state Init cmd chain must produce a cursor.BlinkMsg")
	}
}
```

(import 补 `"charm.land/bubbles/v2/cursor"`。实现者按 openField 的实际签名校准——若 openField 已含 Enter,直接断言其 cmd。)

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/tui/ -run 'TestEditPage|TestField' -v`
Expected: 既有 editpage 测试 FAIL(tap 未迁移编译错→先迁 tap 后跑,`TestEditPageFieldBlinkChainAlive` FAIL——现状 openCurrent 丢弃 Init cmd,链路死)。

- [ ] **Step 3: 实现**

`internal/tui/editpage.go`:

(a) `feedForm` 简化(删白名单判断):

```go
// feedForm forwards one message to the embedded form and runs the post-update
// tail (field-level undo on abort; commit + dirty refresh on complete). The
// returned cmd goes back to the App's routing — the ASYNC round trip replaces
// the old synchronous pump (Plan 30 T5; the pump's 530ms blink-block hazard
// class is gone with it).
func (p *serverEditPage) feedForm(msg tea.Msg) (tea.Model, tea.Cmd) {
	fm, cmd := p.form.Update(msg)
	if nf, ok := fm.(*huh.Form); ok {
		p.form = nf
	}
	if p.form.State == huh.StateAborted {
		return p.restoreField()
	}
	if p.form.State == huh.StateCompleted {
		p.form, p.field, p.fieldSnap = nil, editField{}, ""
		p.state = editStateList
		p.refreshItems()
		return p, nil
	}
	return p, cmd
}
```

(b) Update 加 default 分支 + WindowSizeMsg 保持:

```go
	default:
		// Plan 30 layer-2: huh's unexported protocol msgs land here via the
		// App gate. Field state only — the list state has no form.
		if p.state == editStateField && p.form != nil {
			return p.feedForm(msg)
		}
		return p, nil
```

(c) `openCurrent` 尾部:`p.form.Init()` 的孤立调用改为返回其 cmd:

```go
	p.state = editStateField
	return p, p.form.Init() // async: the cmd's msgs (blink/focus) now route back (Plan 30)
```

(d) 删除 `pumpForm` 整个方法;删除 `confirmAnswer`(先 `grep -rn confirmAnswer internal/` 确认无引用——白名单是唯一调用方);删除 feedForm 旧注释里"the App's routing would drop"表述,头部注释同步改写(含"App does not forward WindowSizeMsg"一句——现在转发了)。

- [ ] **Step 4: 跑测试确认通过 + 回归**

Run: `go test ./internal/tui/ -run 'TestEditPage|TestField|TestServerEdit' -v` → 全 PASS(既有断言:字段提交、脏标记、Esc 恢复、clear-credential confirm y/n——y/n 现在走异步回环,drain 内完成)
Run: `go test ./internal/tui/` → 全绿

- [ ] **Step 5: Commit**

```bash
git add internal/tui/editpage.go internal/tui/editpage_test.go
git commit -m "refactor(tui): editpage 同步泵迁移纯异步路由 + blink 链路断言(Plan 30 T5)"
```

---

### Task 6: App 三组表单 e2e + 文档回写 + 全量验证 + owner 冒烟清单

**Files:**
- Test: `internal/tui/app_routing_test.go`(追加新增服务器 e2e)
- Modify: `docs/backlog.md`(#9 移除)
- 全量:`go test ./...`、`gofmt -l`、`go vet ./...`

**Interfaces:**
- Consumes: Task 1-5 全部产出
- Produces: 合并就绪状态

- [ ] **Step 1: 写新增服务器(3 组)回环 e2e**

追加到 `internal/tui/app_routing_test.go`:

```go
// The 3-group server form must complete through the routed loop. Field order
// (forms.go newServerForm): 名称/Host/SSH用户/端口 | 密码/私钥路径/密钥口令/
// sudo密码 | 硬件/位置/… (structuredFields) — all optional after the first 3.
// 端口 field pre-或空值: type "22" (valid in both cases).
func TestAppLoopServerFormCompletes(t *testing.T) {
	a, st := seedStoreApp(t)
	m, _ := a.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m, cmd := m.Update(tea.KeyPressMsg{Code: 'a', Text: "a"}) // servers 新增
	if cmd == nil {
		t.Fatal("'a' must open the server form")
	}
	m = drain(t, m, cmd)
	typeWord := func(word string) {
		for _, r := range word {
			m, _ = m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
		}
		var c tea.Cmd
		m, c = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		m = drain(t, m, c)
	}
	typeWord("web")     // 名称
	typeWord("10.0.0.9") // Host
	typeWord("ops")     // SSH 用户
	typeWord("22")      // 端口(pre-fill "22" 或空,两种输入皆合法)
	// 其余字段全可选:Enter-only 推进直至表单完成(有界)
	for i := 0; i < 30 && m.(App).overlay != nil; i++ {
		var c tea.Cmd
		m, c = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		m = drain(t, m, c)
	}
	if m.(App).overlay != nil {
		t.Fatal("3-group form must complete within the Enter bound")
	}
	servers, err := st.ListServers()
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 2 {
		t.Fatalf("seeded gpu + new web expected, got %d", len(servers))
	}
}
```

- [ ] **Step 2: 跑测试确认失败→过**

现状(Task 1 完成后)大概率直接 PASS(门已通);若 FAIL,定位是哪个字段没推进——那就是真 bug,修门/二层,不许改断言。

Run: `go test ./internal/tui/ -run TestAppLoopServerFormCompletes -v` → PASS

- [ ] **Step 3: 文档回写**

`docs/backlog.md`:删除第 9 条(整行),头部计数/说明如有引用同步。

- [ ] **Step 4: 全量验证**

Run: `go test ./...` → 全绿
Run: `gofmt -l internal/` → 空;`go vet ./...` → 干净
Run: `git status` → 仅预期文件变更

- [ ] **Step 5: Commit**

```bash
git add internal/tui/app_routing_test.go docs/backlog.md
git commit -m "test(tui): 新增服务器 3 组表单回环 e2e + backlog#9 回写(Plan 30 T6)"
```

- [ ] **Step 6: owner gate — 真终端冒烟清单(用户执行,合并前最后一步)**

新开真终端跑 `ssh-manager tui`(或既有入口),逐项确认:

1. 新增服务器:3 组推进+提交 ✓
2. 新增 Profile:单字段完成 ✓
3. grant multiselect:勾选+提交 ✓
4. 删除/轮换/吊销 confirm:完成 ✓
5. importflow 全链:路径→pick→批量→补全→结果 ✓(今天整条死)
6. 首次运行向导 standalone:服务器循环+授权+项目 ✓
7. client 编辑连接表单:前进+完成 ✓
8. editpage 复验:字段推进、**Shift+Tab 回退**、**resize 跟随**、**光标闪烁可见** ✓
9. overlay 打开时 Ctrl+C/q:现状语义(被 overlay 吃)非回归 ✓
10. 向导表单内 Esc/q:现状语义(向导级前置拦截,q 打不进输入框——wizard.go:417 既有取舍)非回归 ✓
11. 向导静态屏 + q/Ctrl+C:现状语义(静态屏吞键任意前进,wizard.go:404-411 Deliberate)非回归 ✓
12. 表单步骤内 paste:粘贴文本进输入框 ✓
13. 表单内触发 errMsg 路径(如提交重名):全局错误展示、后续按键可恢复 ✓

---

## Self-Review 记录

- **Spec 覆盖**:rev2 的拦截门(T1/T3/T4)、第二层(T2/T5)、泵迁移(T5)、测试矩阵四层(T1-T6 分布)、免费修复四条(T1/T5/T6 断言+冒烟)、文档回写(T6)、9 条注记(1→T3、2→T2/T3、3→T3、4→T4、5→T2 的 FilterMatches 措辞落在 T2 注释 + T1 门注释的 owned⇔case 澄清、6→T1、7→T6 冒烟 13、8→T1 测试断言+T4 回环、9→T2 状态谓词)——逐条有落点。
- **占位符扫描**:无 TBD;测试代码均为可运行形状,个别 helper 名(newImportFlowTestStore / newWizardForRoutingTest / openField 签名)标注"以既有脚手架校准"——这是对既有代码的引用校准,不是占位。
- **类型一致性**:drain/spyOverlay/probeMsg/seedStoreApp/feedFormMsg/formStepActive 前后引用一致;tap 签名变更在 T5 内闭环。
