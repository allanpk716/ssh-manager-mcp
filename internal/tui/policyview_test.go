package tui

// 试算 overlay(主控台)与客户端只读策略查看器(票 #16)的行为测试:
// 试算输入命令与 shell 类型后显示处置、命中规则编号、风险说明与替代
// 做法,规则集分别来自权威库与缓存快照;客户端查看器只读——修改键只给
// 提示,不存在任何修改入口。判定语义在 internal/policy,这里只测接线。

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"ssh-manager-mcp/internal/policy"
	"ssh-manager-mcp/internal/store"
)

// policySnapFixture 造一份带策略段的缓存快照:一条自定义规则拦下 reboot,
// 一条停用记录停掉 POSIX-FORK-BOMB。
func policySnapFixture() *store.Snapshot {
	return &store.Snapshot{
		PolicyRules: []store.SnapshotPolicyRule{{
			ID: "NO-REBOOT", Shells: []string{"posix"}, Commands: []string{"reboot"},
			Disposition: "block",
			Explanation: "重启由 owner 排期,不由 agent 执行",
			Alternative: "请 owner 安排重启窗口",
		}},
		PolicyDisabledDefaults: []string{"POSIX-FORK-BOMB"},
	}
}

// typeInto 把一段文字逐字符打进当前聚焦的输入框(与真实按键同形;逐字
// 按键同步生效,其 cmd 是光标闪烁等外观件,不需排空)。
func typeInto(t *testing.T, m tea.Model, text string) tea.Model {
	t.Helper()
	for _, r := range text {
		nm, _ := m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
		m = nm
	}
	return m
}

// drivePolicyTestResult 把已聚焦在命令输入框的试算表单驱动到结论屏:
// 每字段一次 Enter(每次排空 cmd 链,表单完成→后台判定→结论消息都在
// 链上),直到 done 谓词成立。驱动过头会直接把结论屏按掉,由谓词固定
// 停在结论态。
func drivePolicyTestResult(t *testing.T, m tea.Model, done func(tea.Model) bool) tea.Model {
	t.Helper()
	for i := 0; i < 6 && !done(m); i++ {
		nm, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		m = drain(t, nm, cmd)
	}
	if !done(m) {
		t.Fatalf("test form must reach the result screen:\n%s", m.View().Content)
	}
	return m
}

// seedPolicyRule 往权威库种一条自定义规则(试算的判定数据面)。
func seedPolicyRule(st *store.Store) error {
	return st.AddPolicyRule(policy.CustomRule{
		ID: "NO-REBOOT", Shells: []policy.ShellType{policy.ShellPOSIX},
		Commands:    []string{"reboot"},
		Disposition: policy.DispositionBlock,
		Explanation: "重启由 owner 排期,不由 agent 执行",
		Alternative: "请 owner 安排重启窗口",
	})
}

// overlayDone 报告主控台试算 overlay 已到结论态(或出错态)。
func overlayDone(m tea.Model) bool {
	o, ok := m.(*policyTestOverlay)
	return ok && (o.result != nil || o.err != nil)
}

// viewerDone 报告客户端查看器已到试算结论态。
func viewerDone(m tea.Model) bool {
	v, ok := m.(*policyViewer)
	return ok && v.mode == policyViewerResult
}

// TestPolicyTestOverlay_DecisionFromVault:主控台 [t] 试算:结论屏给出
// 处置、命中规则编号、风险说明、替代做法;规则集读权威库(自定义规则
// 在场即证)。放行档不显示规则编号等三行。
func TestPolicyTestOverlay_DecisionFromVault(t *testing.T) {
	st := newStore(t)
	if err := seedPolicyRule(st); err != nil {
		t.Fatal(err)
	}
	o := newBrokerPolicyTest(st)
	m := tea.Model(o)
	m = drain(t, m, o.Init())
	m = typeInto(t, m, "sudo reboot")
	m = drivePolicyTestResult(t, m, overlayDone)
	v := m.View().Content
	for _, want := range []string{"拦截", "NO-REBOOT", "重启由 owner 排期", "请 owner 安排重启窗口"} {
		if !strings.Contains(v, want) {
			t.Fatalf("test result screen missing %q:\n%s", want, v)
		}
	}
	// 结论屏按任意键 → formDoneMsg,主控台关掉 overlay。
	if _, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); cmd == nil {
		t.Fatal("any key on the result screen must close the overlay")
	} else if _, ok := cmd().(formDoneMsg); !ok {
		t.Fatalf("result screen must close via formDoneMsg, got %T", cmd())
	}

	// 放行档:命令不命中任何规则,规则编号行不出现。
	o2 := newBrokerPolicyTest(st)
	m = drain(t, tea.Model(o2), o2.Init())
	m = typeInto(t, m, "uptime")
	m = drivePolicyTestResult(t, m, overlayDone)
	v = m.View().Content
	if !strings.Contains(v, "放行") {
		t.Fatalf("uptime must be allowed:\n%s", v)
	}
	if strings.Contains(v, "规则编号") {
		t.Fatalf("an allowed command must not name a rule:\n%s", v)
	}
}

// TestPolicyTestOverlay_HitsDefaultRule:默认规则照常参与试算(拦截档
// 的格式化命令),结论屏带其编号与说明。
func TestPolicyTestOverlay_HitsDefaultRule(t *testing.T) {
	st := newStore(t)
	o := newBrokerPolicyTest(st)
	m := drain(t, tea.Model(o), o.Init())
	m = typeInto(t, m, "mkfs.ext4 /dev/sdb")
	m = drivePolicyTestResult(t, m, overlayDone)
	v := m.View().Content
	if !strings.Contains(v, "拦截") || !strings.Contains(v, "POSIX-FORMAT") {
		t.Fatalf("mkfs must hit the POSIX-FORMAT default rule:\n%s", v)
	}
}

// TestPolicyViewer_ReadOnlyFromSnapshot:客户端查看器从缓存快照列出
// 规则(自定义规则在场、停用的默认规则带标注),修改键 a/d/s 只给只读
// 提示——不开任何表单、状态不变;视面带只读说明。
func TestPolicyViewer_ReadOnlyFromSnapshot(t *testing.T) {
	v := newPolicyViewer(policySnapFixture())
	view := v.View().Content
	for _, want := range []string{"POSIX-FORMAT", "NO-REBOOT", "已停用", "只读"} {
		if !strings.Contains(view, want) {
			t.Fatalf("client policy view missing %q:\n%s", want, view)
		}
	}
	for _, key := range []string{"a", "d", "s"} {
		m, cmd := v.Update(tea.KeyPressMsg{Code: rune(key[0]), Text: key})
		got := m.(*policyViewer)
		if got.mode != policyViewerList || cmd != nil {
			t.Fatalf("key %q must stay a read-only no-op: mode=%v cmd=%v", key, got.mode, cmd)
		}
		if !strings.Contains(got.View().Content, "客户端只读") {
			t.Fatalf("key %q must explain the read-only posture:\n%s", key, got.View().Content)
		}
	}
}

// TestPolicyViewer_TestFromSnapshotRules:客户端试算按缓存快照的规则
// 判定——快照里的自定义规则拦下 reboot;被停用的默认规则放行 fork bomb
// (证明读的是快照形态,不是全量默认规则)。结论屏按键回到规则列表。
func TestPolicyViewer_TestFromSnapshotRules(t *testing.T) {
	v := newPolicyViewer(policySnapFixture())
	m := tea.Model(v)
	m = drain(t, m, v.Init())

	// 自定义规则(快照)拦下 reboot。
	m, cmd := m.Update(tea.KeyPressMsg{Code: 't', Text: "t"})
	m = drain(t, m, cmd)
	m = typeInto(t, m, "reboot")
	m = drivePolicyTestResult(t, m, viewerDone)
	pv := m.(*policyViewer)
	if pv.result == nil || pv.result.decision.Disposition != policy.DispositionBlock ||
		pv.result.decision.RuleID != "NO-REBOOT" {
		t.Fatalf("snapshot custom rule must fire, got %+v", pv.result)
	}
	if content := m.View().Content; !strings.Contains(content, "按任意键返回列表") {
		t.Fatalf("result screen must be showing:\n%s", content)
	}
	// 结论屏按任意键回到规则列表(查看器不整体退出)。
	m, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = drain(t, m, cmd)
	if got := m.(*policyViewer); got.mode != policyViewerList {
		t.Fatalf("result screen must return to the rule list, mode=%v", got.mode)
	}

	// 停用的默认规则:fork bomb 放行(证明读的是快照形态)。
	m, cmd = m.Update(tea.KeyPressMsg{Code: 't', Text: "t"})
	m = drain(t, m, cmd)
	m = typeInto(t, m, ":(){ :|:& };:")
	m = drivePolicyTestResult(t, m, viewerDone)
	pv = m.(*policyViewer)
	if pv.result == nil || pv.result.decision.Disposition != policy.DispositionAllow ||
		pv.result.decision.RuleID != "" {
		t.Fatalf("fork bomb disabled in the snapshot must be allowed on the client, got %+v", pv.result)
	}
}

// TestClientModel_PolicyKeyOpensViewer:客户端页 [p] 打开只读策略查看器,
// 页脚宣传该键;查看器关闭(formDoneMsg)回到服务器列表。
func TestClientModel_PolicyKeyOpensViewer(t *testing.T) {
	m := newClientModel()
	m.snap = policySnapFixture()
	// 页脚先干口宣传该键(打开查看器后视面属于 overlay)。
	if v := m.View().Content; !strings.Contains(v, "[p]策略") {
		t.Fatalf("client footer must advertise the policy key:\n%s", v)
	}
	mm, _ := m.Update(tea.KeyPressMsg{Code: 'p', Text: "p"})
	cm := mm.(clientModel)
	// 打开之后门住自动实例选择器:formDoneMsg 的收尾会触发一次数据刷新,
	// 本机若有已命名实例会弹选择器,与本测试的对象(查看器关闭语义)无关。
	cm.pickerChecked = true
	if _, ok := cm.overlay.(*policyViewer); !ok {
		t.Fatalf("p must open the read-only policy viewer, got %T", cm.overlay)
	}
	if v := cm.View().Content; !strings.Contains(v, "命令策略") {
		t.Fatalf("viewer must take over the screen:\n%s", v)
	}
	// 查看器 Esc 关闭 → formDoneMsg → clientModel 落回服务器列表。
	m2, c := cm.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if c == nil {
		t.Fatal("Esc must produce the close handoff cmd")
	}
	m3 := drain(t, m2, c)
	if got := m3.(clientModel); got.overlay != nil {
		t.Fatalf("viewer must close, still %T", got.overlay)
	}
}
