// wizard.go is the first-run role wizard (Plan 19, spec §2): a top-level
// tea.Model whose first screen asks the consequence-driven two-level question
// (spec §2.1). The chosen role is written to role.json IMMEDIATELY with
// setup_complete:false — the anti-dead-state invariant — so any interruption
// (q / Esc / Ctrl+C / crash) is a safe pause and the next `tui` resumes here
// via roles.ResolveMode's ResumeSetup flag.
//
// Task 3 wires the STANDALONE flow end to end (spec §2.2): vault init →
// server-entry loop (skippable) → profile+grant → project → one-time token →
// .mcp.json finish → wizFinish hands off to the broker console. The server
// (Task 4) and client (Task 5) flows stay on the stepRoleDone placeholder.
package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vault"
)

// wizStep is the wizard's coarse state machine.
type wizStep int

const (
	stepPick     wizStep = iota
	stepRoleDone         // placeholder: server (T4) / client (T5) flows land here
	// standalone flow (T3)
	stepVaultErr      // wizEnsureVault / store open failed → r 重试
	stepServerAsk     // 「现在录入第一台服务器？」(skip = zero servers allowed)
	stepServerForm    // serverDraft form (add mode)
	stepServerConfirm // 「继续添加下一台服务器？」loop
	stepProfileGrant  // profile name (default hostname) + grant multi-select
	stepProject       // project name (default hostname)
	stepToken         // one-time token screen (overlay)
	stepMcpConfig     // .mcp.json finish screen (overlay)
)

// wizardData holds the standalone flow's answers. Heap-allocated ONCE in
// newWizard and referenced by pointer — same rationale as wizAnswers: the
// model travels by value through Update, so huh's Value-pointer bindings
// (&d.more, &d.profileName, …) must point at ONE stable allocation or they
// would go stale after the first copy.
type wizardData struct {
	srvDraft    *serverDraft
	more        bool // server-loop confirm (first-add ask + 继续添加)
	profileName string
	profileID   string
	chosen      []string // granted server ids (value=id discipline)
	projName    string
	servers     []*models.Server
}

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
	data   *wizardData

	askShare bool // stepPick sub-phase: q1 answered "yes", q2 on screen
	ans      *wizAnswers
	form     *huh.Form
	ov       overlay // token / mcp-config screens; owns keys until formDoneMsg
	st       *store.Store

	residualClient bool  // stale client role.json detected → hint `clear`
	saveErr        error // role.json write failure (first screen)
	err            error
	status         string

	done bool   // flow complete → Run hands off to the target console
	next string // handoff target ("broker")
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
		data:   &wizardData{},
	}
	// Residual client data hint (spec §1.3: client → vault roles runs the
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
	w.startRoleFlow()
	return w
}

// chooseRole records the choice: role.json is written NOW (setup_complete:
// false) — the moment of choosing is the moment of persisting. After this any
// exit is a safe pause; re-running tui comes back to the role flow.
func (w *wizardModel) chooseRole(r roles.Role) {
	w.role = r
	w.step = stepRoleDone
	w.saveErr = roles.Save(roles.State{Role: r, SetupComplete: false})
	w.startRoleFlow()
}

// startRoleFlow dispatches into the per-role flow after the role is fixed.
// Server (T4) and client (T5) stay on the placeholder page for now.
func (w *wizardModel) startRoleFlow() {
	switch w.role {
	case roles.RoleStandalone:
		w.enterStandalone()
	default:
		w.step = stepRoleDone
	}
}

// enterStandalone boots the standalone flow: ensure a vault, open the store
// (kept open for the flow's mutations and the later broker handoff), then ask
// about the first server. Failure lands on stepVaultErr (banner + r 重试) —
// role.json is already saved, so quitting here is a safe pause.
//
// RESUME IDEMPOTENCY (review I1): quitting mid-flow and re-running tui must
// not re-run entity creation — a naive re-entry would askFirstServer again and
// mint a SECOND profile (hostname-2) + project. Heuristic (documented, simple):
// after the store opens, count existing profiles/projects —
//
//	≥1 profile AND ≥1 project → server/profile/project steps all done → jump
//	  straight to the .mcp.json finish screen (the one-time token was shown on
//	  the earlier run and is unrecoverable; the finish screen's tokenRef points
//	  at the Projects-page reissue instead of pretending it is on screen);
//	≥1 profile, 0 projects → server loop + profile creation done → reuse the
//	  EXISTING profile (id loaded into w.data) and resume at the project step;
//	0 profiles → fresh flow, askFirstServer as before.
//
// Server-entry is treated as done whenever a profile exists — a profile with
// zero granted servers is a valid earlier outcome (the skip gate), and extra
// servers can always be added from the broker console later.
func (w *wizardModel) enterStandalone() {
	if err := wizEnsureVault(); err != nil {
		w.step, w.err, w.form = stepVaultErr, err, nil
		return
	}
	st, err := vault.OpenStore(store.FileKeyProvider{})
	if err != nil {
		w.step, w.err, w.form = stepVaultErr, fmt.Errorf("打开 vault：%w", err), nil
		return
	}
	w.st = st
	w.err, w.status = nil, "vault 已就绪"
	profiles, perr := st.ListProfiles()
	projects, jerr := st.ListProjects()
	if perr == nil && jerr == nil && len(profiles) > 0 {
		// Reuse the first (alphabetically) existing profile — its grants are
		// already in the vault; GrantServers again would be a redundant no-op
		// at best and a confusing re-ask at worst.
		w.data.profileName, w.data.profileID = profiles[0].Name, profiles[0].ID
		if len(projects) > 0 {
			w.step = stepMcpConfig
			w.ov = mcpConfigScreen("既有 project 的 token（丢失可在主控台 Projects 页 [a] 重发）")
			w.status = "检测到已完成的 profile 与 project，直接进入收尾"
			return
		}
		w.data.projName = defaultHostName()
		w.step = stepProject
		w.form = w.projectForm()
		w.status = fmt.Sprintf("检测到既有 profile %s，跳过服务器录入与 profile 创建", profiles[0].Name)
		return
	}
	w.askFirstServer()
}

// askFirstServer: the skip gate. Skipping is ALLOWED (zero servers) but the
// consequence is spelled out — an empty profile means the agent sees nothing.
func (w *wizardModel) askFirstServer() {
	w.data.more = true
	w.step = stepServerAsk
	w.form = huh.NewForm(huh.NewGroup(huh.NewConfirm().
		Title("现在录入第一台服务器？").
		Description("（跳过 = profile 暂无成员，agent 将看不到任何服务器；之后可在主控台随时补录）").
		Value(&w.data.more)))
}

func (w wizardModel) Init() tea.Cmd {
	if w.form == nil {
		return nil
	}
	return w.form.Init()
}

func (w wizardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case tea.KeyPressMsg:
		if w.ov != nil { // overlay owns keys until formDoneMsg.
			// Deliberate: the one-time secret screens (token / .mcp.json) swallow
			// q/Esc — their data is ALREADY persisted (profile/project/token
			// hash are in the vault), so there is nothing to "pause" back to;
			// any key advances the flow.
			ov, cmd := w.ov.Update(msg)
			w.ov, _ = ov.(overlay)
			return w, cmd
		}
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
		if w.step == stepVaultErr {
			if k.Text == "r" {
				w.enterStandalone() // retry (idempotent — existing vault is skipped)
			}
			return w, nil
		}
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
	case errMsg:
		w.err, w.status = m.err, ""
		// A failed mutation reopens the SAME form bound to the SAME state, so
		// the user fixes whatever failed (duplicate name, bad key path…) and
		// resubmits — never loses typed context.
		switch w.step {
		case stepServerForm:
			w.form = wizServerLoopForm(w.data.srvDraft)
			return w, w.form.Init()
		case stepProfileGrant:
			w.form = wizProfileGrantForm(&w.data.profileName, w.data.servers, &w.data.chosen)
			return w, w.form.Init()
		case stepProject:
			w.form = w.projectForm()
			return w, w.form.Init()
		}
		return w, nil
	case actionDoneMsg:
		w.err, w.status = nil, m.desc
		switch w.step {
		case stepServerForm:
			w.step = stepServerConfirm
			w.data.more = false // Enter = 完成，常见于只录一台的场景
			w.form = huh.NewForm(huh.NewGroup(huh.NewConfirm().
				Title("继续添加下一台服务器？").Value(&w.data.more)))
			return w, w.form.Init()
		case stepProfileGrant:
			return w.enterProject()
		}
		return w, nil
	case tokenIssuedMsg:
		// The plaintext transits this one message and then lives only inside
		// the overlay (same discipline as App's token flow).
		w.step, w.err, w.status = stepToken, nil, ""
		w.ov = wizTokenScreen(m.title, m.token,
			"贴到本机 .mcp.json 的 --token 参数",
			"主控台 Projects 页 [a] 重发")
		return w, nil
	case formDoneMsg:
		switch w.step {
		case stepToken:
			w.step = stepMcpConfig
			w.ov = mcpConfigScreen("上方已展示的 project token")
			return w, m.after
		case stepMcpConfig:
			return w, wizFinish(w.role) // any key on the finish screen completes setup
		}
		return w, m.after
	case wizardDoneMsg:
		// Setup persisted complete: exit; Run chains into the broker console.
		w.done, w.next = true, m.next
		return w, tea.Quit
	}
	return w, nil
}

// stepFormDone routes a completed form to the next step. First-screen logic
// (two-phase q1/q2 → chooseRole) then the standalone flow.
func (w wizardModel) stepFormDone() (tea.Model, tea.Cmd) {
	if w.step == stepPick {
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
		if w.step == stepRoleDone || w.form == nil { // placeholder (T4/T5), or standalone landed on stepVaultErr (form cleared)
			return w, nil
		}
		return w, w.form.Init() // standalone entered a flow step with a form
	}
	switch w.step {
	case stepServerAsk:
		if !w.data.more {
			return w.enterProfileGrant() // skip: zero servers allowed
		}
		return w.openServerForm()
	case stepServerConfirm:
		if w.data.more {
			return w.openServerForm()
		}
		return w.enterProfileGrant()
	case stepServerForm:
		return w, submitServer(w.st, nil, w.data.srvDraft) // add-mode: password-or-key enforced
	case stepProfileGrant:
		return w, w.submitProfileGrant()
	case stepProject:
		return w, w.submitProject()
	}
	return w, nil
}

// openServerForm starts (or restarts) one loop iteration with a fresh draft.
func (w wizardModel) openServerForm() (tea.Model, tea.Cmd) {
	w.data.srvDraft = &serverDraft{}
	w.step = stepServerForm
	w.form = wizServerLoopForm(w.data.srvDraft)
	return w, w.form.Init()
}

// enterProfileGrant loads the (possibly empty) server list and asks for the
// profile name (default = hostname, conflicts auto-suffixed at submit) plus
// the grant multi-select.
func (w wizardModel) enterProfileGrant() (tea.Model, tea.Cmd) {
	servers, err := w.st.ListServers()
	if err != nil {
		w.err = fmt.Errorf("列出服务器：%w", err)
		return w, nil
	}
	w.data.servers = servers
	w.data.profileName = defaultHostName()
	w.data.chosen = nil
	w.step = stepProfileGrant
	w.form = wizProfileGrantForm(&w.data.profileName, servers, &w.data.chosen)
	return w, w.form.Init()
}

// submitProfileGrant creates the profile (dedupe suffixes -2 on conflict) and
// grants the chosen servers in one action. w.data is a pointer, so the cmd's
// writes (profileName/profileID) survive model copies.
func (w wizardModel) submitProfileGrant() tea.Cmd {
	return doAction(w.st, func() (string, error) {
		name := dedupeProfileName(w.st, strings.TrimSpace(w.data.profileName))
		id, err := w.st.AddProfile(name)
		if err != nil {
			return "", err
		}
		w.data.profileName, w.data.profileID = name, id
		if len(w.data.chosen) > 0 {
			if err := w.st.GrantServers(id, w.data.chosen); err != nil {
				return "", err
			}
		}
		return fmt.Sprintf("已创建 profile %s（授权 %d 台服务器）", name, len(w.data.chosen)), nil
	})
}

// projectForm: single field — the project binds to the profile just created
// (w.data.profileID); there is exactly one profile at this point, so no
// profile select is needed (unlike the broker console's newProjectForm).
func (w wizardModel) projectForm() *huh.Form {
	return huh.NewForm(huh.NewGroup(
		huh.NewInput().Title("项目名称（agent 的访问身份，绑定上面的 profile）").
			Value(&w.data.projName).Validate(nonEmpty),
	))
}

func (w wizardModel) enterProject() (tea.Model, tea.Cmd) {
	w.data.projName = defaultHostName()
	w.step = stepProject
	w.form = w.projectForm()
	return w, w.form.Init()
}

// submitProject mints the project + its one-time token; the token rides
// tokenIssuedMsg straight into the wizTokenScreen overlay.
func (w wizardModel) submitProject() tea.Cmd {
	return func() tea.Msg {
		_, token, err := w.st.AddProject(strings.TrimSpace(w.data.projName), w.data.profileID)
		if err != nil {
			return errMsg{err}
		}
		return tokenIssuedMsg{title: "project token", token: token}
	}
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

var wizStepTitles = map[wizStep]string{
	stepServerAsk:     " 服务器录入 ",
	stepServerForm:    " 新增服务器 ",
	stepServerConfirm: " 服务器录入 ",
	stepProfileGrant:  " Profile + 授权 ",
	stepProject:       " 创建项目 ",
}

func (w wizardModel) View() tea.View {
	if w.ov != nil {
		v := w.ov.View().Content
		// M1: a Save failure in wizFinish surfaces as errMsg while the overlay
		// is still up — render it BELOW the overlay or the error is invisible.
		if w.err != nil {
			v += "\n" + errStyle.Render("✗ "+w.err.Error()) + "\n（任意键重试）"
		}
		return tea.NewView(v)
	}
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
	case stepVaultErr:
		b.WriteString(titleStyle.Render(" 初始化 vault 失败 ") + "\n\n")
		b.WriteString(errStyle.Render("✗ "+w.err.Error()) + "\n\n")
		b.WriteString(footerStyle.Render("r 重试 / q 退出（角色已保存，重开 tui 会继续）") + "\n")
	default: // standalone form steps
		b.WriteString(titleStyle.Render(wizStepTitles[w.step]) + "\n\n")
		b.WriteString(w.form.View() + "\n\n")
		if w.step == stepProfileGrant && len(w.data.servers) == 0 {
			b.WriteString(footerStyle.Render("（未录入任何服务器 = profile 暂无成员，agent 将看不到任何服务器）") + "\n")
		}
		if w.err != nil {
			b.WriteString(errStyle.Render("✗ "+w.err.Error()) + "\n")
		} else if w.status != "" {
			b.WriteString(footerStyle.Render("✓ "+w.status) + "\n")
		}
		b.WriteString(footerStyle.Render("q 暂停退出（进度已保存，重开 tui 会继续）") + "\n")
	}
	return tea.NewView(b.String())
}

var roleLabels = map[roles.Role]string{
	roles.RoleStandalone: "单机",
	roles.RoleServer:     "服务器（server）",
	roles.RoleClient:     "客户端（client）",
}
