// wizard.go is the first-run role wizard (Plan 19, spec §2): a top-level
// tea.Model whose first screen asks the consequence-driven two-level question
// (spec §2.1). The chosen role is written to role.json IMMEDIATELY with
// setup_complete:false — the anti-dead-state invariant — so any interruption
// (q / Esc / Ctrl+C / crash) is a safe pause and the next `tui` resumes here
// via roles.ResolveMode's ResumeSetup flag.
//
// Task 3 wires the STANDALONE flow end to end (spec §2.2): vault init →
// server-entry loop (skippable) → profile+grant → project → one-time token →
// .mcp.json finish → wizFinish hands off to the broker console. Task 4 adds
// the SERVER flow (spec §2.4) on the shared steps ②③④ plus the dual-secret
// screens, the serve segment (addr picker → admin notice → install → probe →
// result banners) and the client access card. Task 5 makes the CLIENT flow
// real: the wizard WRAPS clientModel in its wizard form (source hint,
// classified failure path preserving input, .mcp.json finish → client panel).
package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vault"
)

// wizStep is the wizard's coarse state machine.
type wizStep int

const (
	stepPick     wizStep = iota
	stepRoleDone         // defensive placeholder (all three roles have real flows now)
	// standalone flow (T3)
	stepVaultErr      // wizEnsureVault / store open failed → r 重试
	stepServerAsk     // 「现在录入第一台服务器？」(skip = zero servers allowed)
	stepServerForm    // serverDraft form (add mode)
	stepServerConfirm // 「继续添加下一台服务器？」loop
	stepProfileGrant  // profile name (default hostname) + grant multi-select
	stepProject       // project name (default hostname)
	stepToken         // one-time token screen (overlay)
	stepMcpConfig     // .mcp.json finish screen (overlay)
	// server flow (T4) — ①-④ reuse the standalone steps above
	stepClientName   // 客户端机器名（server 角色：profile 默认名）
	stepDeviceIssue  // device-code issuance in flight (waiting; r retry on err)
	stepDeviceToken  // 设备码 one-time screen (overlay, 密钥 2/2)
	stepAddr         // LAN address select (spec §2.4 ⑥ 地址捕获)
	stepServeAdmin   // admin 前置提示 (overlay)
	stepServeInstall // service registration in flight (waiting)
	stepServeProbe   // post-install probe in flight (waiting)
	stepServeResult  // install + probe banners (overlay, non-blocking)
	stepAccessCard   // 客户端接入卡 (overlay)
	// client flow (T5) — the step IS a clientModel in wizard form; every
	// message delegates to it (see Update).
	stepClient
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

	// server-role flow (T4)
	clientName string // 客户端机器名 — profile 默认名 + 设备码名
	serveAddr  string // 选定的 LAN 地址实值（https://<ip>:7878），进接入卡
	deviceFp   string // serve cert SPKI fingerprint（接入卡 + 设备码 usage）
	installErr error  // serve 服务安装结果（非阻断，进结果横幅）
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

	client *clientModel // client-role wizard (T5): clientModel in wizard form

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
// stepRoleDone is now only a defensive fallback — all three roles have real
// flows.
func (w *wizardModel) startRoleFlow() {
	switch w.role {
	case roles.RoleStandalone:
		w.enterStandalone()
	case roles.RoleServer:
		w.enterServer()
	case roles.RoleClient:
		w.enterClient()
	default:
		w.step = stepRoleDone
	}
}

// enterClient boots the CLIENT flow (T5): the flow IS clientModel in wizard
// form. A fresh machine opens the connection form immediately (with the
// source hint above it); a resume whose cache.auth.json already holds a
// complete cred skips the form — the panel's [s]/[c] keys drive the retry and
// re-opening the form would demand retyping a masked code for no reason.
func (w *wizardModel) enterClient() {
	cm := newClientModel()
	cm.wizard = true
	if cred, err := clientops.ReadCacheCred(); err == nil && cred != nil &&
		cred.URL != "" && cred.Token != "" && cred.Pin != "" {
		cm.cred = cred
	} else {
		cm.overlay = cm.editConnForm()
	}
	w.client = &cm
	w.step = stepClient
}

// openVaultOrErr is the shared boot of BOTH vault-role flows (standalone T3 /
// server T4): ensure a vault, open the store. Failure lands on stepVaultErr
// (banner + r 重试) — role.json is already saved, so quitting here is a safe
// pause. Returns false when the caller must stop.
func (w *wizardModel) openVaultOrErr() bool {
	if err := wizEnsureVault(); err != nil {
		w.step, w.err, w.form = stepVaultErr, err, nil
		return false
	}
	st, err := vault.OpenStore(store.FileKeyProvider{})
	if err != nil {
		w.step, w.err, w.form = stepVaultErr, fmt.Errorf("打开 vault：%w", err), nil
		return false
	}
	w.st = st
	w.err, w.status = nil, "vault 已就绪"
	return true
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
	if !w.openVaultOrErr() {
		return
	}
	profiles, perr := w.st.ListProfiles()
	projects, jerr := w.st.ListProjects()
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

// enterServer boots the SERVER flow (spec §2.4): shared vault boot, then a
// resume heuristic that MIRRORS standalone's (T3 I1) but extended one state
// deeper, because the server flow mints one more entity (the device code):
//
//	0 profiles → fresh flow: ask the client machine name first (its answer is
//	  the profile default, spec §2.4 ④) → shared steps ③④⑤ → dual secrets…;
//	≥1 profile, 0 projects → same as standalone: reuse the EXISTING profile
//	  and resume at the project step (its token will be minted fresh);
//	≥1 profile, ≥1 project, ≥1 cache token → everything minted → jump
//	  straight to the serve segment (addr picker). Both one-time secrets were
//	  shown on earlier runs and are unrecoverable — the access card points at
//	  the reissue pages instead of pretending they are on screen. The cert
//	  fingerprint is recovered via the idempotent LoadOrCreateServeCert;
//	≥1 profile, ≥1 project, 0 cache tokens → only the device code remains:
//	  re-ask the client name (it names the code) and issue it — the project
//	  token screen is skipped (already minted; reissue via Projects page).
func (w *wizardModel) enterServer() {
	if !w.openVaultOrErr() {
		return
	}
	profiles, perr := w.st.ListProfiles()
	projects, jerr := w.st.ListProjects()
	tokens, terr := w.st.ListCacheTokens()
	if perr != nil || jerr != nil || terr != nil {
		// Cannot scan → treat as fresh; the underlying store error resurfaces
		// at the first mutating submit (same policy as dedupeProfileName).
		w.startClientName()
		return
	}
	switch {
	case len(profiles) == 0:
		w.startClientName()
	case len(projects) == 0:
		w.data.profileName, w.data.profileID = profiles[0].Name, profiles[0].ID
		w.data.clientName = profiles[0].Name // prefill for issueDeviceCode
		w.data.projName = defaultHostName()
		w.step = stepProject
		w.form = w.projectForm()
		w.status = fmt.Sprintf("检测到既有 profile %s，跳过服务器录入与 profile 创建", profiles[0].Name)
	case len(tokens) > 0:
		// Everything minted → serve segment. Recover the cert fingerprint
		// (display-only input to the access card) via the idempotent
		// LoadOrCreateServeCert — normally the cert already exists (the
		// device-code step created it); on a pre-cert machine this creates
		// it, which is exactly what the fresh flow would have done anyway.
		// An unreadable cert must not trap the resume: fall back to a hint.
		if _, _, fp, err := mcpserver.LoadOrCreateServeCert(); err == nil {
			w.data.deviceFp = fp
		} else {
			w.data.deviceFp = "（指纹不可读：" + err.Error() + "）"
		}
		w.enterAddrForm()
		w.status = "检测到已完成的 profile/project/设备码，直接进入 serve 安装段（两把密钥此前已展示，丢失可在主控台重发）"
	default:
		// profile+project done, device code missing. Load the profileID so the
		// client-name submit knows entity creation is complete and routes
		// straight to the code issuance (see stepFormDone@stepClientName).
		w.data.profileID = profiles[0].ID
		w.startClientName()
		w.status = "profile/project 已完成（project token 已在此前展示，丢失可在主控台 Projects 页 [a] 重发），继续签发设备码"
	}
}

// startClientName opens the 客户端机器名 step — the server flow's first
// question, whose answer becomes the profile default name AND the device-code
// name (one name, two uses: the card's 去向表 stays self-consistent).
func (w *wizardModel) startClientName() {
	w.data.clientName = defaultHostName()
	w.step = stepClientName
	w.form = w.clientNameForm()
}

func (w wizardModel) clientNameForm() *huh.Form {
	return huh.NewForm(huh.NewGroup(
		huh.NewInput().Title("客户端机器名（将命名 profile 与设备码；填对方电脑的名字）").
			Value(&w.data.clientName).Validate(nonEmpty),
	))
}

// issueDeviceCode mints the device code named after the client and returns the
// cmd whose message (deviceCodeIssuedMsg) carries BOTH the one-time code and
// the cert fingerprint. ORDER MATTERS: cert FIRST, code second — if the cert
// init failed after AddCacheToken succeeded, a retry would hit the active-name
// collision on the already-minted code; this order keeps the retry idempotent.
// The fingerprint is also stashed into w.data.deviceFp for the access card.
func (w wizardModel) issueDeviceCode() tea.Cmd {
	return func() tea.Msg {
		_, _, fp, err := mcpserver.LoadOrCreateServeCert()
		if err != nil {
			return errMsg{err}
		}
		_, code, err := w.st.AddCacheToken(strings.TrimSpace(w.data.clientName))
		if err != nil {
			return errMsg{err}
		}
		w.data.deviceFp = fp
		return deviceCodeIssuedMsg{code: code, fingerprint: fp}
	}
}

// enterAddrForm opens the serve-segment address picker (spec §2.4 ⑥ 地址捕获).
func (w *wizardModel) enterAddrForm() {
	w.step = stepAddr
	w.form = wizAddrForm(mcpserver.LocalNonLoopbackIPs(), &w.data.serveAddr)
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
	if w.step == stepClient && w.client != nil {
		return w.client.Init()
	}
	if w.form == nil {
		return nil
	}
	return w.form.Init()
}

func (w wizardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Client-role wizard (T5): the flow IS the clientModel in wizard form —
	// every message delegates to it. wizardDoneMsg is the one exception: it
	// is the wizard's own exit sentinel and is handled by the switch below.
	if w.step == stepClient && w.client != nil {
		if _, ok := msg.(wizardDoneMsg); !ok {
			cm, cmd := w.client.Update(msg)
			if ncm, ok := cm.(clientModel); ok {
				w.client = &ncm
			}
			return w, cmd
		}
	}
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
				if w.role == roles.RoleServer {
					w.enterServer() // retry (idempotent — existing vault is skipped)
				} else {
					w.enterStandalone()
				}
			}
			return w, nil
		}
		if w.step == stepDeviceIssue {
			// The issue failed (err set via errMsg): r retries the SAME action —
			// issueDeviceCode is ordered cert-first so a retry after a
			// half-failure stays idempotent (see its comment).
			if k.Text == "r" && w.err != nil {
				w.err, w.status = nil, ""
				return w, w.issueDeviceCode()
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
		if w.role == roles.RoleServer {
			// Server flow (spec §2.4 ⑤): this token goes to the CLIENT
			// machine's .mcp.json — the usage label must say so, and the
			// screen is numbered 1/2 (the device code screen follows).
			w.ov = wizTokenScreen("密钥 1/2：project token", m.token,
				"贴到 client 机 .mcp.json 的 --token 参数",
				"主控台 Projects 页 [a] 重发")
			return w, nil
		}
		w.ov = wizTokenScreen(m.title, m.token,
			"贴到本机 .mcp.json 的 --token 参数",
			"主控台 Projects 页 [a] 重发")
		return w, nil
	case deviceCodeIssuedMsg:
		// Server flow's second secret (spec §2.4 ⑤ 密钥 2/2). The usage line
		// embeds the ready-to-paste merged token "<码>:<指纹>" (spec §3.3 形态 A
		// — the exact string cache pull's SplitTokenPin consumes).
		w.step, w.err, w.status = stepDeviceToken, nil, ""
		w.data.deviceFp = m.fingerprint
		w.ov = wizTokenScreen("密钥 2/2：设备码", m.code,
			fmt.Sprintf("填到 client 机向导；或拼 cache pull --token '%s:%s'", m.code, m.fingerprint),
			"主控台 设备码页 [a] 重发")
		return w, nil
	case serveInstalledMsg:
		// Install outcome — either way the flow CONTINUES to the probe
		// (spec §2.4 ⑥: install failure 不阻断; the result screen renders the
		// manual elevated command next to the probe verdict).
		w.data.installErr = m.err
		w.step = stepServeProbe
		return w, probeServe(w.data.serveAddr)
	case serveProbeMsg:
		w.step = stepServeResult
		w.ov = serveResultScreen(w.data.installErr, m)
		return w, nil
	case formDoneMsg:
		switch w.step {
		case stepToken:
			if w.role == roles.RoleServer {
				// Server flow: the project token screen is 1/2 — the device
				// code comes next, not the .mcp.json finisher.
				w.step = stepDeviceIssue
				return w, w.issueDeviceCode()
			}
			w.step = stepMcpConfig
			w.ov = mcpConfigScreen("上方已展示的 project token")
			return w, m.after
		case stepMcpConfig:
			return w, wizFinish(w.role) // any key on the finish screen completes setup
		case stepDeviceToken:
			// 设备码 dismissed → the serve segment begins (address capture).
			w.enterAddrForm()
			return w, w.form.Init()
		case stepServeAdmin:
			// Admin notice acknowledged → run the registration.
			w.step = stepServeInstall
			return w, installServeStep(w.data.serveAddr)
		case stepServeResult:
			w.step = stepAccessCard
			w.ov = accessCard(w.data.serveAddr, w.data.deviceFp)
			return w, nil
		case stepAccessCard:
			return w, wizFinish(w.role) // any key completes the server setup
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
				if w.client == nil {
					return w, nil // defensive: enterClient always sets it
				}
				return w, w.client.Init()
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
		if w.step == stepRoleDone || w.form == nil { // defensive placeholder, or standalone landed on stepVaultErr (form cleared)
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
	case stepClientName:
		// Fresh flow → shared server-entry loop. On a resume where profile +
		// project already exist (profileID preloaded), the client name's only
		// remaining job is naming the missing device code → issue it directly.
		w.data.clientName = strings.TrimSpace(w.data.clientName)
		if w.data.profileID != "" {
			w.step = stepDeviceIssue
			return w, w.issueDeviceCode()
		}
		w.askFirstServer()
		return w, w.form.Init()
	case stepAddr:
		w.data.serveAddr = strings.TrimSpace(w.data.serveAddr)
		w.step = stepServeAdmin
		w.ov = serveAdminNotice()
		return w, nil
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
// profile name (default = hostname for standalone; the CLIENT name for the
// server role — spec §2.4 ④ — conflicts auto-suffixed at submit) plus the
// grant multi-select.
func (w wizardModel) enterProfileGrant() (tea.Model, tea.Cmd) {
	servers, err := w.st.ListServers()
	if err != nil {
		w.err = fmt.Errorf("列出服务器：%w", err)
		return w, nil
	}
	w.data.servers = servers
	w.data.profileName = defaultHostName()
	if w.role == roles.RoleServer && strings.TrimSpace(w.data.clientName) != "" {
		w.data.profileName = w.data.clientName
	}
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
	stepClientName:    " 客户端命名 ",
	stepAddr:          " serve 地址 ",
}

func (w wizardModel) View() tea.View {
	if w.step == stepClient && w.client != nil {
		return w.client.View()
	}
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
	case stepDeviceIssue, stepServeInstall, stepServeProbe:
		// In-flight steps: no form, no overlay — just what is running (and the
		// error + retry affordance if the action failed).
		titles := map[wizStep]string{
			stepDeviceIssue:  " 签发设备码 ",
			stepServeInstall: " 安装 serve 服务 ",
			stepServeProbe:   " serve 探活 ",
		}
		b.WriteString(titleStyle.Render(titles[w.step]) + "\n\n")
		switch w.step {
		case stepDeviceIssue:
			if w.err != nil {
				b.WriteString(errStyle.Render("✗ "+w.err.Error()) + "\n")
				b.WriteString(footerStyle.Render("r 重试 / q 暂停退出（角色已保存，重开 tui 会从设备码继续）") + "\n")
			} else {
				b.WriteString("正在签发设备码…\n")
				b.WriteString(footerStyle.Render("q 暂停退出（进度已保存）") + "\n")
			}
		case stepServeInstall:
			b.WriteString("正在注册系统服务（绑定 0.0.0.0:7878，可能需要数秒）…\n")
			b.WriteString(footerStyle.Render("q 暂停退出（安装失败不会阻断向导）") + "\n")
		case stepServeProbe:
			b.WriteString("正在探活 " + w.data.serveAddr + " …\n")
			b.WriteString(footerStyle.Render("q 暂停退出（进度已保存）") + "\n")
		}
	default: // standalone form steps
		b.WriteString(titleStyle.Render(wizStepTitles[w.step]) + "\n\n")
		b.WriteString(w.form.View() + "\n\n")
		if w.step == stepProfileGrant && len(w.data.servers) == 0 {
			b.WriteString(footerStyle.Render("（未录入任何服务器 = profile 暂无成员，agent 将看不到任何服务器）") + "\n")
		}
		if w.saveErr != nil {
			// First-screen roles.Save failure must stay visible through the
			// role flows too, not only on the defensive placeholder page —
			// a silently-unwritten role.json turns "safe pause" into "start
			// over" for the user who quits here.
			b.WriteString(errStyle.Render(fmt.Sprintf("⚠ role.json 写入失败：%v", w.saveErr)) + "\n")
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
