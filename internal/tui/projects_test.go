package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"ssh-manager-mcp/internal/models"
)

func TestProjectTokenFlow(t *testing.T) {
	st := newStore(t)
	pid, _ := st.AddProfile("p")
	id, tok, err := st.AddProject("proj", pid)
	if err != nil || tok == "" || id == "" {
		t.Fatalf("(%q,%q,%v)", id, tok, err)
	}
	newTok, err := st.RotateProject(id)
	if err != nil || newTok == tok {
		t.Fatalf("rotate: %q %v", newTok, err)
	}
	if err := st.SetProjectStatus(id, models.ProjectDisabled); err != nil {
		t.Fatal(err)
	}
}

func TestSecretView_RendersOnceNotice(t *testing.T) {
	sv := &secretView{title: "项目 token", body: "TOK-xyz"}
	v := sv.View().Content // bubbletea v2: View() returns tea.View; content is .Content
	if !strings.Contains(v, "TOK-xyz") || !strings.Contains(v, "仅此一次") {
		t.Fatalf("view: %s", v)
	}
}

// driveForm submits a formOverlay via Enter presses, draining huh's cmd chain
// after EACH press (same loop shape as TestNewGrantFormPreselectsExisting),
// and returns the formDoneMsg the completion produced. Interleaving the drain
// between presses is mandatory: huh advances fields asynchronously (Enter →
// NextField cmd → nextFieldMsg round trip), so a back-to-back second Enter
// would land on the field the first one is still leaving.
func driveForm(t *testing.T, o *formOverlay, presses int) formDoneMsg {
	t.Helper()
	_, cmd := o.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	for n := 0; n < presses-1; n++ {
		for steps := 0; cmd != nil && steps < 100; steps++ {
			msg := cmd()
			if done, ok := msg.(formDoneMsg); ok {
				return done
			}
			_, next := o.Update(msg)
			cmd = next
		}
		_, cmd = o.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	}
	for steps := 0; cmd != nil && steps < 100; steps++ {
		msg := cmd()
		if done, ok := msg.(formDoneMsg); ok {
			return done
		}
		_, next := o.Update(msg)
		cmd = next
	}
	t.Fatal("form never completed")
	return formDoneMsg{}
}

// TestProjectTokenMsg_DualForms: 两条真片段——stdio 与 http 块都代入真 token，
// recovery 指向轮换，http 块是中立 <serve URL> 占位 + 单机忽略引导行。
func TestProjectTokenMsg_DualForms(t *testing.T) {
	m := projectTokenMsg("项目 token", "TOK-real")
	if m.title != "项目 token" || m.token != "TOK-real" {
		t.Fatalf("title/token: %+v", m)
	}
	if !strings.Contains(m.usage, ".mcp.json") || !strings.Contains(m.recovery, "轮换") {
		t.Fatalf("usage/recovery copy: %q / %q", m.usage, m.recovery)
	}
	joined := strings.Join(m.snippet, "\n")
	for _, want := range []string{
		`"args": ["mcp"],`,
		`"SSHMGR_TOKEN": "TOK-real"`,
		`"type": "http",`,
		`"Authorization": "Bearer TOK-real"`,
		"<serve URL>",
		"未部署 serve 可忽略本块",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("snippet missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "--token") {
		t.Fatalf("token must ride env, not argv:\n%s", joined)
	}
}

// TestProjectsKeyE_EmitsGuidedTokenMsg — 流程级（spec §4.3）：Projects 页 [e]
// 经真实 keypress → confirm 表单 → action 命令链，返回的必须是 projectTokenMsg
// 形态的 tokenIssuedMsg（防"helper 正确但发射点漏改"）。
func TestProjectsKeyE_EmitsGuidedTokenMsg(t *testing.T) {
	st := newStore(t)
	pid, _ := st.AddProfile("p")
	if _, _, err := st.AddProject("proj", pid); err != nil {
		t.Fatal(err)
	}
	a, err := NewBrokerApp(st) // project exists BEFORE page fetch
	if err != nil {
		t.Fatal(err)
	}
	m, _ := a.Update(tea.KeyPressMsg{Code: tea.KeyTab})        // servers → profiles
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyTab})         // profiles → projects
	m, cmd := m.Update(tea.KeyPressMsg{Code: 'e', Text: "e"})  // rotate confirm
	if cmd == nil {
		t.Fatal("[e] must open the rotate confirm form")
	}
	fo, ok := m.(App).overlay.(*formOverlay)
	if !ok {
		t.Fatalf("want formOverlay, got %T", m.(App).overlay)
	}
	// huh v2 Confirm: Enter (Next/Submit) commits the UNCHANGED value — the
	// initial false would take the no-op path (after == nil). 'right' is the
	// Toggle binding and flips the value synchronously; the Enter inside
	// driveForm then submits true.
	_, _ = fo.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	done := driveForm(t, fo, 1) // single Confirm: one Enter
	if done.after == nil {
		t.Fatal("rotate submit must carry the mutation cmd")
	}
	msg := done.after() // after = the action's mutation cmd (formOverlay stores action()'s result)
	tm, ok := msg.(tokenIssuedMsg)
	if !ok {
		t.Fatalf("rotate must emit tokenIssuedMsg, got %T", msg)
	}
	if tm.title != "项目 token（已轮换）" || !strings.Contains(tm.body(), `"SSHMGR_TOKEN": "`+tm.token) {
		t.Fatalf("rotate msg shape: %q\n%s", tm.title, tm.body())
	}
}

// TestProjectsKeyA_EmitsGuidedTokenMsg — 流程级：Projects 页 [a] 新增表单
// （输入项目名 → 选 profile → 提交）同样必须发射 guided msg。
func TestProjectsKeyA_EmitsGuidedTokenMsg(t *testing.T) {
	st := newStore(t)
	if _, err := st.AddProfile("p"); err != nil {
		t.Fatal(err)
	}
	a, err := NewBrokerApp(st)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := a.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	m, cmd := m.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	if cmd == nil {
		t.Fatal("[a] must open the new-project form")
	}
	fo, ok := m.(App).overlay.(*formOverlay)
	if !ok {
		t.Fatalf("want formOverlay, got %T", m.(App).overlay)
	}
	// 打项目名 "pj"（两个字符键进入 name 输入框），再 Enter×2（提交名字 /
	// 提交 profile 单选）——每按一次 Enter 前先排干上一键的 cmd 链（huh 的
	// 字段推进是异步 round trip，见 driveForm 注释）。
	_, _ = fo.Update(tea.KeyPressMsg{Code: 'p', Text: "p"})
	_, _ = fo.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	done := driveForm(t, fo, 2)
	if done.after == nil {
		t.Fatal("add submit must carry the mutation cmd")
	}
	msg := done.after() // after = the action's mutation cmd (formOverlay stores action()'s result)
	tm, ok := msg.(tokenIssuedMsg)
	if !ok {
		t.Fatalf("add must emit tokenIssuedMsg, got %T", msg)
	}
	if tm.title != "项目 token" || !strings.Contains(tm.body(), `"type": "http",`) {
		t.Fatalf("add msg shape: %q\n%s", tm.title, tm.body())
	}
}
