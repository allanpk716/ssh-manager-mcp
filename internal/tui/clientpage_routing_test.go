package tui

// Plan 30 T4: the clientModel gate — same shape as the App gate. Plan 42 批1
// T8 retired the connect-form overlay; the overlays are now the instance
// picker and (Plan 45 T3) the pairing wizard. The wizard's five INTERNAL async
// messages ride the gate's default branch (forwarded); only its two terminal
// messages + the picker's pair request are client-owned.

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"ssh-manager-mcp/internal/store"
)

func newClientModelForGate(t *testing.T) clientModel {
	// newClientModel initializes the panelList — the 'c' keypress path runs
	// listUpdate before the action switch, so the list must be constructed.
	m := newClientModel()
	m.width, m.height = 80, 24
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
	if cmd == nil {
		t.Fatal("gate must hand the overlay's cmd back to the runtime")
	}
	if _, ok := cmd().(probeMsg); !ok {
		t.Fatal("handed-back cmd must be the spy's sentinel probeMsg")
	}
}

// TestClientGateOwnedFallsThrough: every client-owned type must run the
// model's own case even while an overlay is open — the gate may not starve
// the model of its messages.
func TestClientGateOwnedFallsThrough(t *testing.T) {
	m := newClientModelForGate(t)
	spy := &spyOverlay{}
	for _, owned := range []tea.Msg{
		dataReadyMsg{}, syncDoneMsg{},
		clientStatusMsg(""), errMsg{}, formDoneMsg{},
	} {
		m.overlay = spy
		nm, _ := m.Update(owned)
		if _, ok := nm.(clientModel); !ok {
			t.Fatalf("Update must return clientModel, got %T", nm)
		}
		if spy.spySaw(owned) {
			t.Fatalf("owned %T must fall through to clientModel's own case", owned)
		}
	}
}

// TestClientGateFormDoneClosesOverlay: formDoneMsg is owned AND closing — the
// model's own case (not the gate) must nil the overlay.
func TestClientGateFormDoneClosesOverlay(t *testing.T) {
	m := newClientModelForGate(t)
	m.overlay = &spyOverlay{}
	m2, _ := m.Update(formDoneMsg{})
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
	if !spy.spySaw(tea.WindowSizeMsg{}) {
		t.Fatal("resize must reach the overlay")
	}
	if cm.width != 60 || cm.height != 30 {
		t.Fatalf("resize must be recorded, got %dx%d", cm.width, cm.height)
	}
}

// TestClientPanel_CKeyStartsPairWizard (Plan 45 T3; REWRITES Plan 42 批1 T8's
// TestClientPanel_CKeyPointsAtPair): the [c] key REALLY opens the pairing
// wizard again — Plan 42 had retired the connect-form and reduced [c] to a
// status-line pointer at `sshmgr pair`; Plan 45 gives the affordance its
// guided path back (pairwizard.go).
func TestClientPanel_CKeyStartsPairWizard(t *testing.T) {
	isolatedConfigDir(t) // clears both single-slot override envs
	m := newClientModelForGate(t)
	m2, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Text: "c"})
	cm := m2.(clientModel)
	if _, ok := cm.overlay.(*pairWizard); !ok {
		t.Fatalf("[c] must open the pairing wizard, got overlay %T", cm.overlay)
	}
	if cmd == nil {
		t.Fatal("[c] must hand the wizard's Init cmd back to the runtime")
	}
}

// TestClientPanel_CKeyRefusedUnderSingleSlotOverride: newPairWizard's own
// single-slot mutual exclusion stays the authority — a direct [c] under an
// override env opens nothing and surfaces the refusal as the panel error.
// (The footer stops advertising [c] in this mode; the guard is defense in
// depth, not the only line.)
func TestClientPanel_CKeyRefusedUnderSingleSlotOverride(t *testing.T) {
	isolatedConfigDir(t)
	t.Setenv("SSHMGR_CACHE_DIR", t.TempDir()) // AFTER the helper's clear: full override
	m := newClientModelForGate(t)
	m2, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Text: "c"})
	cm := m2.(clientModel)
	if cm.overlay != nil {
		t.Fatalf("single-slot override must refuse the wizard, got overlay %T", cm.overlay)
	}
	if cmd != nil {
		t.Fatal("a refused start must not hand back an init cmd")
	}
	if cm.err == nil {
		t.Fatal("the refusal must surface as a panel error")
	}
}

// TestClientGate_RegistersWizardMsgs (Plan 30 checklist): the wizard's two
// terminal messages + the picker's re-pair request are CLIENT-owned types —
// while ANY overlay is open they must fall through to clientModel's own
// switch, never be swallowed by the gate's default branch. The wizard's five
// INTERNAL async messages (discover/enroll/approval/write/tick) stay
// UNREGISTERED on purpose: the default branch forwards them to the overlay.
func TestClientGate_RegistersWizardMsgs(t *testing.T) {
	isolatedConfigDir(t)
	m := newClientModelForGate(t)
	spy := &spyOverlay{}
	for _, owned := range []tea.Msg{
		pairWizardDoneMsg{}, pairWizardClosedMsg{}, instancePickerPairMsg{},
	} {
		m.overlay = spy
		nm, _ := m.Update(owned)
		if _, ok := nm.(clientModel); !ok {
			t.Fatalf("Update must return clientModel, got %T", nm)
		}
		if spy.spySaw(owned) {
			t.Fatalf("owned %T must fall through to clientModel's own case", owned)
		}
	}
}

// TestClientPanel_CKeyEscFullChain: [c] → wizard → Esc → back on the page
// (overlay dropped, slot untouched) — the full escape hatch the brief pins
// ("Esc 全链退回原页"), exercised through the gate's default branch.
func TestClientPanel_CKeyEscFullChain(t *testing.T) {
	isolatedConfigDir(t)
	m := newClientModelForGate(t)
	m.instance = "agentA"
	m2, _ := m.Update(tea.KeyPressMsg{Code: 'c', Text: "c"})
	m = m2.(clientModel)
	if _, ok := m.overlay.(*pairWizard); !ok {
		t.Fatalf("precondition: [c] opens the wizard, got %T", m.overlay)
	}
	_, wcmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc}) // gate default → wizard
	closed, ok := wcmd().(pairWizardClosedMsg)
	if !ok {
		t.Fatalf("Esc must close the wizard, got %T", wcmd())
	}
	m3, _ := m.Update(closed)
	cm := m3.(clientModel)
	if cm.overlay != nil {
		t.Fatalf("the chain must land back on the page, got overlay %T", cm.overlay)
	}
	if cm.instance != "agentA" {
		t.Fatalf("a bare Esc must not switch the slot, got %q", cm.instance)
	}
}

// TestClientModel_EscChainByContext:非向导 Esc 键链——同一个 Esc 键在三种
// 上下文的分化行为钉成组。列表过滤态 Esc=清空过滤(不触发任何动作、不退页);
// 覆盖层在场 Esc=收覆盖层(键先归覆盖层,已应用的过滤与会话槽都不动);无
// 覆盖无过滤 Esc=无操作(退出键是 q,不是 Esc)。已不在此重复的部分:删除
// 确认框的 Esc 取消路(TestClientModel_DeleteConfirm_CancelReopensPicker,
// Esc 与 Enter 两条取消路都回实例列表)、broker 侧列表的过滤 Esc
// (TestServersPage_ListFilterFlow)、选择器自身的 Esc 消息
// (TestInstancePicker_EscCloses);配对向导的 Esc 链归配对向导两级表单的
// 契约测试,不在本组。
func TestClientModel_EscChainByContext(t *testing.T) {
	// seedPanel 建一个带两台服务器的客户端页(两行,过滤后可见行数可断言)。
	seedPanel := func(t *testing.T) clientModel {
		t.Helper()
		isolatedConfigDir(t)
		m := newClientModelForGate(t)
		m.snap = &store.Snapshot{Servers: []store.SnapshotServer{
			{ID: "s1", Name: "gpu", Host: "192.0.2.10", User: "u"},
			{ID: "s2", Name: "nuc10", Host: "192.0.2.5", User: "allan"},
		}}
		m.syncList()
		return m
	}

	t.Run("filter esc clears", func(t *testing.T) {
		m := seedPanel(t)
		nm, _ := m.Update(tea.KeyPressMsg{Code: '/', Text: "/"}) // 打开过滤输入
		m = nm.(clientModel)
		nm, _ = m.Update(tea.KeyPressMsg{Code: 'g', Text: "g"}) // 打进过滤框
		m = nm.(clientModel)
		if !m.filtering() || m.filterText() != "g" {
			t.Fatalf("前置:过滤输入必须持有按键, filtering=%v text=%q", m.filtering(), m.filterText())
		}
		nm, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
		m = drain(t, nm, cmd).(clientModel)
		if m.filtering() || m.filterText() != "" {
			t.Fatalf("过滤态 Esc 必须清空过滤, filtering=%v text=%q", m.filtering(), m.filterText())
		}
		if n := len(m.list.VisibleItems()); n != 2 {
			t.Fatalf("清空过滤后两行必须复见, got %d", n)
		}
		if m.busy || m.overlay != nil {
			t.Fatalf("清过滤不得触发任何动作, busy=%v overlay=%T", m.busy, m.overlay)
		}
	})

	t.Run("overlay eats esc, filter untouched", func(t *testing.T) {
		m := seedPanel(t)
		mkInstanceDir(t, "agentA") // 在 seedPanel 的重定向之后建:选择器有行可列
		m.applyFilter("gpu")       // 已应用过滤(生产重取回填用的同一函数)
		if n := len(m.list.VisibleItems()); n != 1 {
			t.Fatalf("前置:过滤 gpu 必须只留一行, got %d", n)
		}
		nm, cmd := m.Update(tea.KeyPressMsg{Code: 'i', Text: "i"}) // 开实例选择器
		m = drain(t, nm, cmd).(clientModel)
		if _, ok := m.overlay.(*instancePicker); !ok {
			t.Fatalf("前置:[i] 必须开实例选择器, got %T", m.overlay)
		}
		// 第一击 Esc:归覆盖层——收选择器,已应用的过滤与会话槽都不动。
		nm, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
		m = drain(t, nm, cmd).(clientModel)
		if m.overlay != nil {
			t.Fatalf("覆盖层在场时 Esc 必须先收覆盖层, got %T", m.overlay)
		}
		if m.filterText() != "gpu" || len(m.list.VisibleItems()) != 1 {
			t.Fatalf("收覆盖层不得动过滤, text=%q visible=%d", m.filterText(), len(m.list.VisibleItems()))
		}
		if m.instance != "" {
			t.Fatalf("收选择器不得动会话槽, got %q", m.instance)
		}
		// 第二击 Esc:覆盖已收,这才轮到过滤——清空复见。同一键先后两击、
		// 两种结果,即键链的分化本身。
		nm, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
		m = drain(t, nm, cmd).(clientModel)
		if m.filterText() != "" || len(m.list.VisibleItems()) != 2 {
			t.Fatalf("第二击 Esc 必须清空过滤, text=%q visible=%d", m.filterText(), len(m.list.VisibleItems()))
		}
	})

	t.Run("no overlay no filter: no-op", func(t *testing.T) {
		m := seedPanel(t)
		nm, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
		m = drain(t, nm, cmd).(clientModel)
		if m.overlay != nil || m.busy || m.err != nil {
			t.Fatalf("无覆盖无过滤时 Esc 必须无操作, overlay=%T busy=%v err=%v", m.overlay, m.busy, m.err)
		}
		if cmd != nil {
			if msg := cmd(); msg != nil {
				if _, quit := msg.(tea.QuitMsg); quit {
					t.Fatal("Esc 不得退出(退出键是 q)")
				}
			}
		}
		if v := m.View().Content; !strings.Contains(v, "[s]同步") {
			t.Fatalf("页面必须原地不动(页脚照常), got:\n%s", v)
		}
	})
}
