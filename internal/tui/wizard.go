// wizard.go is the first-run role wizard (Plan 19, spec §2): a top-level
// tea.Model whose first screen asks the consequence-driven two-level question
// (spec §2.1). The chosen role is written to role.json IMMEDIATELY with
// setup_complete:false — the anti-dead-state invariant — so any interruption
// (q / Esc / Ctrl+C / crash) is a safe pause and the next `tui` resumes here
// via roles.ResolveMode's ResumeSetup flag.
package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"ssh-manager-mcp/internal/roles"
)

// wizStep is the wizard's coarse state machine. Task 2 ships stepPick (the
// first screen) and stepRoleDone (a placeholder); Tasks 3-5 replace the
// placeholder with per-role step models entered from stepRoleDone.
type wizStep int

const (
	stepPick wizStep = iota
	stepRoleDone
)

// wizardData holds the per-role flow answers. Task 2 has none yet; Tasks 3-5
// grow it (server entries, serve config, client connection form).
type wizardData struct{}

// wizAnswers holds the first-screen huh bindings. Heap-allocated ONCE in
// newWizard and referenced by pointer: wizardModel travels by value through
// Update, so a value field's address would go stale after the first copy.
type wizAnswers struct {
	keep  string // q1: "yes" = this machine keeps the credentials
	share string // q2 (asked only when keep=yes): "self" | "share"
}

type wizardModel struct {
	launch roles.Launch
	step   wizStep
	role   roles.Role
	data   wizardData

	askShare       bool // stepPick sub-phase: q1 answered "yes", q2 on screen
	ans            *wizAnswers
	form           *huh.Form
	residualClient bool // stale client role.json detected → hint `clear`
	saveErr        error
}

// newWizard builds the first screen (spec §2.1): one huh Select per level —
// q1 凭据保管 [是/否]; only a "是" answer asks q2 (单机 vs server), a "否"
// answer goes straight to client.
func newWizard(l roles.Launch) wizardModel {
	ans := &wizAnswers{}
	w := wizardModel{
		launch: l,
		step:   stepPick,
		ans:    ans,
	}
	// Residual client data hint (spec §1.3: client → vault roles run the
	// wizard on the client machine; clear can wipe the leftovers first).
	if st, err := roles.Load(); err == nil && st != nil && st.Role == roles.RoleClient {
		w.residualClient = true
	}
	w.form = huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("这台电脑要保管所有 SSH 凭据吗？").
			Options(
				huh.NewOption("是——凭据只存这台机（其他机器不能用了它就都用不了）", "yes"),
				huh.NewOption("否——凭据在另一台机器上，这台只连它 → client（需先在 server 机完成设置）", "no"),
			).Value(&ans.keep),
	))
	return w
}

// newWizardForRole resumes with the role already chosen (role.json on disk,
// setup_complete=false): skip the picker, enter the role flow directly.
func newWizardForRole(l roles.Launch) wizardModel {
	w := newWizard(l)
	w.role = l.Role
	w.step = stepRoleDone
	return w
}

// chooseRole records the choice: role.json is written NOW (setup_complete:
// false) — the moment of choosing is the moment of persisting. After this any
// exit is a safe pause; re-running tui comes back to the role flow.
func (w *wizardModel) chooseRole(r roles.Role) {
	w.role = r
	w.step = stepRoleDone
	w.saveErr = roles.Save(roles.State{Role: r, SetupComplete: false})
}

func (w wizardModel) Init() tea.Cmd { return w.form.Init() }

func (w wizardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case tea.KeyPressMsg:
		k := m.Key()
		// q/Ctrl+C quit; Esc = 暂停退出 — all safe: role.json (if any) is
		// already on disk. Intercepted BEFORE the form so "q" never lands in
		// huh's key handling.
		if k.Text == "q" || (k.Code == 'c' && k.Mod == tea.ModCtrl) || k.Code == tea.KeyEsc {
			return w, tea.Quit
		}
		if w.step == stepRoleDone {
			return w, nil // placeholder page has no other keys
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
		if !w.askShare {
			if w.ans.keep == "no" {
				w.chooseRole(roles.RoleClient)
				return w, nil
			}
			w.askShare = true
			w.form = w.shareForm()
			return w, w.form.Init()
		}
		if w.ans.share == "share" {
			w.chooseRole(roles.RoleServer)
		} else {
			w.chooseRole(roles.RoleStandalone)
		}
		return w, nil
	}
	return w, nil
}

// shareForm is q2 (only reached after a "yes" on q1): does this machine's
// agent need to reach other machines → 单机 vs server.
func (w wizardModel) shareForm() *huh.Form {
	return huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("这台机器上的 agent 需要连别的电脑吗？").
			Options(
				huh.NewOption("只有本机用 → 单机", "self"),
				huh.NewOption("要给其他机器共享 → server", "share"),
			).Value(&w.ans.share),
	))
}

func (w wizardModel) View() tea.View {
	var b strings.Builder
	switch w.step {
	case stepPick:
		b.WriteString(titleStyle.Render(" 第一次使用 ssh-manager ") + "\n\n")
		b.WriteString(w.form.View() + "\n\n")
		b.WriteString(footerStyle.Render("概念图解：docs/concepts.md（或 --help）") + "\n")
		if w.residualClient {
			b.WriteString(footerStyle.Render("检测到本机曾有 client 配置，可运行 ssh-manager clear 清理") + "\n")
		}
	case stepRoleDone:
		role := string(w.role)
		if label, ok := roleLabels[w.role]; ok {
			role = label
		}
		b.WriteString(titleStyle.Render(" 角色已确定："+role+" ") + "\n\n")
		b.WriteString(fmt.Sprintf("%s 角色流程将在下一步实现\n", role))
		if w.saveErr != nil {
			b.WriteString("\n" + errStyle.Render(fmt.Sprintf("⚠ role.json 写入失败：%v", w.saveErr)) + "\n")
		}
		b.WriteString("\n" + footerStyle.Render("q 退出（进度已保存，重开 tui 会继续）") + "\n")
	}
	return tea.NewView(b.String())
}

var roleLabels = map[roles.Role]string{
	roles.RoleStandalone: "单机",
	roles.RoleServer:     "服务器（server）",
	roles.RoleClient:     "客户端（client）",
}
