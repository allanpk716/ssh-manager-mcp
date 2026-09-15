package tui

// Plan 46 T3 —— [d] 删除流(clientModel 侧):确认 overlay → busy 后台
// RemoveInstance → 完成/失败/取消三路收口。删当前槽成功 = 回落默认槽 + 内存
// 态清空 + 列表刷新([s]/[i] 全路由默认槽,无悬空引用);失败 = 错误(含 T2
// 残留物清单)挂在重开的列表下方(M1 parity),不回落;取消 = 回列表零副作用。
//
// 环境隔离沿用 instancepicker_test.go 的 seed 形态:APPDATA/XDG_CONFIG_HOME
// 重定向 + 单槽双 env 清空;DEK 根经 SSHMGR_CACHE_DEK_DIR 指向临时目录——
// 生产 DekProvider(FileKeyProvider)走这里,测试绝不触真 vault。

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// withInstance sets the session slot on a fresh model (the [d] flow's
// "deleting the CURRENT slot" / "deleting ANOTHER slot" arrangements).
func (m clientModel) withInstance(inst string) clientModel {
	m.instance = inst
	return m
}

// deleteFlowDrives opens the confirm overlay for instance and drains the Init
// chain (huh forms need it before keypresses — TestAppProfilesDeleteKey shape).
func deleteFlowDrives(t *testing.T, m clientModel, instance string) clientModel {
	t.Helper()
	nm, cmd := m.Update(instancePickerDeleteMsg{instance: instance})
	m2 := drain(t, nm, cmd)
	cm, ok := m2.(clientModel)
	if !ok {
		t.Fatalf("Update must return clientModel, got %T", m2)
	}
	if _, ok := cm.overlay.(*instanceDeleteConfirm); !ok {
		t.Fatalf("[d] must open the delete confirm overlay, got %T", cm.overlay)
	}
	return cm
}

// TestClientModel_DeleteFlow_CurrentSlotFallsBack:删当前槽成功 → 回落默认槽 +
// 内存态清空(cred/snap 不悬空)+ 双根真删(槽目录与 DEK 文件都消失)+ 列表
// 刷新;随后的 [s]/[i] 全路由默认槽。
func TestClientModel_DeleteFlow_CurrentSlotFallsBack(t *testing.T) {
	base := mkInstanceDir(t, "agentA")
	dekDir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DEK_DIR", dekDir)
	dekFile := filepath.Join(dekDir, "cache-dek-agentA.key")
	if err := os.WriteFile(dekFile, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}

	m := deleteFlowDrives(t, newClientModelForGate(t).withInstance("agentA"), "agentA")

	// y = huh Confirm 肯定单键(Enter 落否定项——profiles_test 既证形态);
	// drain 一路执行:confirmed → busy+RemoveInstance → done → 回落+刷新。
	nm, cmd := m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	cm := drain(t, nm, cmd).(clientModel)

	if cm.instance != "" {
		t.Fatalf("deleting the CURRENT slot must fall back to the default slot, got %q", cm.instance)
	}
	if cm.busy {
		t.Fatal("busy must clear once the deletion settles")
	}
	if cm.cred != nil || cm.snap != nil {
		t.Fatal("the deleted slot's in-memory cred/snap must not survive the fallback (no dangling [s] route)")
	}
	if _, err := os.Stat(filepath.Join(base, "instances", "agentA")); !os.IsNotExist(err) {
		t.Fatalf("slot dir must be gone, stat err=%v", err)
	}
	if _, err := os.Stat(dekFile); !os.IsNotExist(err) {
		t.Fatalf("DEK file must be gone, stat err=%v", err)
	}
	pk, ok := cm.overlay.(*instancePicker)
	if !ok {
		t.Fatalf("the list must refresh (picker reopened), got %T", cm.overlay)
	}
	for _, r := range pk.rows {
		if r.instance == "agentA" {
			t.Fatal("the deleted instance must be gone from the refreshed list")
		}
	}

	// 删当前槽后 [s]/[i] 全路由默认槽(无悬空引用):
	//  - Esc 收列表(overlay 在场时按键归 picker,先收掉)再按 [s]:cred 已清
	//    → 拒绝同步,而不是拿已删槽的凭据打默认槽。
	nm, cmd = cm.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	cm = drain(t, nm, cmd).(clientModel)
	if cm.overlay != nil {
		t.Fatalf("Esc must close the picker, got %T", cm.overlay)
	}
	nm, cmd = cm.Update(tea.KeyPressMsg{Code: 's', Text: "s"})
	cm = nm.(clientModel)
	if cmd == nil {
		t.Fatal("[s] must stay wired after the fallback")
	}
	syncDone, ok := cmd().(syncDoneMsg)
	if !ok || syncDone.err == nil {
		t.Fatalf("with the fallback cred cleared [s] must refuse (no dangling cred), got %T (%+v)", cmd(), syncDone)
	}
	// runtime 会把 syncDoneMsg 送回模型:busy 收口(refresh 命令不追执行)。
	n2, _ := cm.Update(syncDone)
	cm = n2.(clientModel)
	if cm.busy {
		t.Fatal("syncDoneMsg must clear the busy window")
	}
	//  - [i] 重开的列表无 agentA(路由值 "" = 默认槽,★ 已挪回默认行)。
	nm, cmd = cm.Update(tea.KeyPressMsg{Code: 'i', Text: "i"})
	cm = nm.(clientModel)
	pk, ok = cm.overlay.(*instancePicker)
	if !ok {
		t.Fatalf("[i] must reopen the picker, got %T", cm.overlay)
	}
	currentRows := 0
	for _, r := range pk.rows {
		if r.instance == "agentA" {
			t.Fatal("the reopened picker must not list the deleted instance")
		}
		if r.current {
			currentRows++
			if r.instance != "" {
				t.Fatalf("★ must sit on the DEFAULT row after the fallback, got %+v", r)
			}
		}
	}
	if currentRows != 1 {
		t.Fatalf("exactly one row must carry ★ after the fallback, got %d", currentRows)
	}
}

// TestClientModel_DeleteFlow_OtherSlotStaysPut:删非当前槽成功 → 会话原地不动
// (不回落、凭据不动),列表刷新后该行消失、其余行健在。
func TestClientModel_DeleteFlow_OtherSlotStaysPut(t *testing.T) {
	base := mkInstanceDir(t, "agentA", "agentB")
	t.Setenv("SSHMGR_CACHE_DEK_DIR", t.TempDir())

	m := deleteFlowDrives(t, newClientModelForGate(t).withInstance("agentA"), "agentB")
	nm, cmd := m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	cm := drain(t, nm, cmd).(clientModel)

	if cm.instance != "agentA" {
		t.Fatalf("deleting another slot must not move the session, got %q", cm.instance)
	}
	if cm.busy {
		t.Fatal("busy must clear once the deletion settles")
	}
	if _, err := os.Stat(filepath.Join(base, "instances", "agentB")); !os.IsNotExist(err) {
		t.Fatalf("agentB must be gone, stat err=%v", err)
	}
	pk, ok := cm.overlay.(*instancePicker)
	if !ok {
		t.Fatalf("the list must refresh, got %T", cm.overlay)
	}
	var keptA, sawB bool
	for _, r := range pk.rows {
		if r.instance == "agentB" {
			sawB = true
		}
		if r.instance == "agentA" && r.current {
			keptA = true
		}
	}
	if sawB || !keptA {
		t.Fatalf("agentB must be gone and agentA must stay current, sawB=%v keptA=%v", sawB, keptA)
	}
}

// TestClientModel_DeleteFlow_FailureNoFallback:RemoveInstance 失败(seam 注入
// T2 残留清单形态的错误)→ 错误挂在重开的列表下方且肉眼可见(M1 parity),
// 槽与会话路由不动(不回落),busy 收口。
func TestClientModel_DeleteFlow_FailureNoFallback(t *testing.T) {
	mkInstanceDir(t, "agentB")
	t.Setenv("SSHMGR_CACHE_DEK_DIR", t.TempDir())
	prev := removeInstanceFn
	removeInstanceFn = func(string) error {
		return errors.New(`instance "agentB" removal incomplete — leftovers remain: X (slot dir: busy); the command is idempotent — re-run to finish`)
	}
	t.Cleanup(func() { removeInstanceFn = prev })

	m := deleteFlowDrives(t, newClientModelForGate(t).withInstance("agentB"), "agentB")
	nm, cmd := m.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	cm := drain(t, nm, cmd).(clientModel)

	if cm.instance != "agentB" {
		t.Fatalf("a failed removal must NOT fall back, got %q", cm.instance)
	}
	if cm.busy {
		t.Fatal("busy must clear on the failure path too")
	}
	if cm.err == nil || !strings.Contains(cm.err.Error(), "leftovers") {
		t.Fatalf("the failure (with T2's residue list) must surface, err=%v", cm.err)
	}
	if _, ok := cm.overlay.(*instancePicker); !ok {
		t.Fatalf("the list must reopen so the error renders below it (M1 parity), got %T", cm.overlay)
	}
	if v := cm.View().Content; !strings.Contains(v, "leftovers") {
		t.Fatalf("the error must be VISIBLE under the reopened picker, got:\n%s", v)
	}
}

// TestClientModel_DeleteConfirm_BusyWindow:确认即开 busy 线(点名实例)盖住
// RemoveInstance 的阻塞窗口(含等在途 [s] 拉取的共享写锁排空);overlay 落下
// (busy 渲染在页面上);done 收口 busy,幂等 rm 对不存在的槽 = 成功。
func TestClientModel_DeleteConfirm_BusyWindow(t *testing.T) {
	t.Setenv("SSHMGR_CACHE_DEK_DIR", t.TempDir())
	m := newClientModelForGate(t)
	nm, cmd := m.Update(instanceDeleteConfirmedMsg{instance: "agentX", confirmed: true})
	cm := nm.(clientModel)
	if !cm.busy || !strings.Contains(cm.busyLabel, "agentX") {
		t.Fatalf("confirming must open the busy window with the instance named, busy=%v label=%q", cm.busy, cm.busyLabel)
	}
	if cm.overlay != nil {
		t.Fatalf("the confirm overlay must be down during the busy window, got %T", cm.overlay)
	}
	done, ok := cmd().(instanceDeleteDoneMsg)
	if !ok {
		t.Fatalf("the delete cmd must report instanceDeleteDoneMsg, got %T", cmd())
	}
	if done.err != nil {
		t.Fatalf("RemoveInstance on an absent slot is idempotent success, got %v", done.err)
	}
	n2, _ := cm.Update(done)
	if cm2 := n2.(clientModel); cm2.busy {
		t.Fatal("done must clear the busy window")
	}
}

// TestClientModel_DeleteConfirm_CancelReopensPicker:两条取消路(Esc;Enter 落
// 「取消」项)都收口回实例列表、零副作用(槽目录健在、会话槽不动)。
func TestClientModel_DeleteConfirm_CancelReopensPicker(t *testing.T) {
	base := mkInstanceDir(t, "agentA")
	t.Setenv("SSHMGR_CACHE_DEK_DIR", t.TempDir())

	m := deleteFlowDrives(t, newClientModelForGate(t).withInstance("agentA"), "agentA")
	nm, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	cm := drain(t, nm, cmd).(clientModel)
	if _, ok := cm.overlay.(*instancePicker); !ok {
		t.Fatalf("Esc must land back on the picker, got %T", cm.overlay)
	}
	if cm.instance != "agentA" || cm.busy || cm.err != nil {
		t.Fatalf("cancel must touch nothing, instance=%q busy=%v err=%v", cm.instance, cm.busy, cm.err)
	}
	if _, err := os.Stat(filepath.Join(base, "instances", "agentA")); err != nil {
		t.Fatalf("cancel must leave the slot dir alone, stat err=%v", err)
	}

	// 第二条路:Enter 落否定项(huh Confirm 默认)→ confirmed=false → 同收口。
	nm, cmd = cm.Update(tea.KeyPressMsg{Code: tea.KeyEsc}) // 先收掉刚重开的列表
	m = drain(t, nm, cmd).(clientModel)
	m = deleteFlowDrives(t, m, "agentA")
	nm, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	cm = drain(t, nm, cmd).(clientModel)
	if _, ok := cm.overlay.(*instancePicker); !ok {
		t.Fatalf("Enter(取消) must land back on the picker, got %T", cm.overlay)
	}
	if cm.instance != "agentA" || cm.busy {
		t.Fatalf("cancel must not move the session or stay busy, instance=%q busy=%v", cm.instance, cm.busy)
	}
}

// TestClientGate_RegistersDeleteMsgs (Plan 30 checklist):the three delete-flow
// messages are CLIENT-owned types — while ANY overlay is open they must fall
// through to clientModel's own switch, never be swallowed by the gate's
// default branch.
func TestClientGate_RegistersDeleteMsgs(t *testing.T) {
	isolatedConfigDir(t)
	m := newClientModelForGate(t)
	spy := &spyOverlay{}
	for _, owned := range []tea.Msg{instancePickerDeleteMsg{}, instanceDeleteConfirmedMsg{}, instanceDeleteDoneMsg{}} {
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
