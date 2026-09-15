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
// result banners) and the client access card.
//
// Plan 42 批1 T8 (spec §3.1-6): the client-ROLE wizard flow is RETIRED — pair
// (`sshmgr pair`) is the only guided onboarding path for a new machine.
// Choosing client lands on a static guidance page pointing at pair; the
// server flow no longer pre-provisions a client machine (the 客户端机器名 step
// and the device-code issuance are gone — pair mints both at approval, the
// owner picks the profile then). The access card became the pair card.
package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

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
	// server flow (T4) — ①-④ reuse the standalone steps above. Plan 42 批1
	// T8: the client-provisioning steps (客户端机器名 / 设备码签发 / 密钥 2/2)
	// are RETIRED — pair (`sshmgr pair`) mints the device code + project
	// at approval; the wizard only sets up the broker itself.
	stepBindProfile  // 多 profile resume 的绑定选择（Plan 39——绝不静默绑字母序第一个）
	stepAddr         // LAN address select (spec §2.4 ⑥ 地址捕获)
	stepServeAdmin   // admin 前置提示 (overlay)
	stepServeInstall // service registration in flight (waiting)
	stepServeProbe   // post-install probe in flight (waiting)
	stepServeResult  // install + probe banners (overlay, non-blocking)
	stepPairCard     // client 入网卡(指引 sshmgr pair)(overlay)
	// client flow — Plan 42 批1 T8: reduced to a static guidance page (the
	// connection form + wizard-embedded clientModel are deleted).
	stepClientGuide
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
	serveAddr  string // 选定的 LAN 地址实值（https://<ip>:7878），进 pair 卡
	deviceFp   string // serve cert SPKI fingerprint（pair 卡展示用）
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
		w.enterClientGuide()
	default:
		w.step = stepRoleDone
	}
}

// enterClientGuide is the CLIENT role's whole flow since Plan 42 批1 T8
// (spec §3.1-6): a static guidance page pointing at `sshmgr pair` — the
// wizard-embedded connection form/panel flow is retired. Any key completes the
// setup (role.json → setup_complete:true) and exits; the next `tui` opens the
// client panel (sync/status/instance), and `sshmgr pair` itself writes
// the cache material.
func (w *wizardModel) enterClientGuide() {
	w.step = stepClientGuide
	w.ov = clientPairGuide()
	w.err, w.status = nil, ""
}

// clientPairGuide is the guidance overlay's copy: the pair command (with and
// without discovery), what approval does, and the documented manual fallback.
func clientPairGuide() overlay {
	body := strings.Join([]string{
		"client 机的入网方式已更新——本向导不再内置连接表单。",
		"",
		"新机入网（pair 为新机唯一入网路径）：",
		"  sshmgr pair --instance <本机实例名>",
		"      自动发现 LAN 内的 serve（udp/7878），按提示选择目标",
		"  sshmgr pair --instance <本机实例名> --url https://<server>:7878 --pin sha256:...",
		"      已知地址与指纹时直连（pin 硬校验；无 pin 需 --allow-tofu，不建议）",
		"",
		"在 server 机批准（其 TUI Pairing 页 / serve pair approve）并对照双方屏幕的",
		"SAS 码后，设备码、project token 与缓存自动落到本机。",
		"",
		"手工路径（CI/自动化，文档化保留）：",
		"  sshmgr cache pull --url <serve 地址> --token '<设备码>:<指纹>'",
		"",
		"按任意键完成设置并退出（q 退出）",
	}, "\n")
	return &wizStaticView{title: "client 入网：运行 sshmgr pair", body: body}
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
// resume heuristic that MIRRORS standalone's (T3 I1):
//
//	0 profiles → fresh flow: ask about the first server (the profile defaults
//	  to this machine's hostname) → shared steps ③④⑤ → token screen → serve
//	  segment…;
//	≥1 profile, 0 projects → same as standalone: reuse the EXISTING profile
//	  and resume at the project step (its token will be minted fresh);
//	≥1 profile, ≥1 project → everything the wizard itself mints is done →
//	  jump straight to the serve segment (addr picker). Plan 42 批1 T8: the
//	  device code is NOT part of this flow anymore — the client pairs with
//	  `sshmgr pair` and the owner approves (that mints the code + a
//	  dedicated pair project); the cert fingerprint is recovered via the
//	  idempotent LoadOrCreateServeCert for the pair card.
func (w *wizardModel) enterServer() {
	if !w.openVaultOrErr() {
		return
	}
	w.resumeServerFlow()
}

// resumeServerFlow runs the server-flow resume heuristic (enterServer's doc
// above). Split out of enterServer so the stepBindProfile picker (Plan 39)
// can re-enter it after the owner picks a binding — with several existing
// profiles the resume paths must NEVER silently bind the alphabetically-first
// one (the same 0/1/N discipline as the standalone→server upgrade segment).
//
// Plan 42 批1 T8 shape: the wizard itself mints profile + project only — the
// old device-code tier of the heuristic is gone (pair mints codes at
// approval), so profile+project done ⇒ straight to the serve segment
// regardless of cache-token count.
func (w *wizardModel) resumeServerFlow() {
	profiles, perr := w.st.ListProfiles()
	projects, jerr := w.st.ListProjects()
	if perr != nil || jerr != nil {
		// Cannot scan → treat as fresh; the underlying store error resurfaces
		// at the first mutating submit (same policy as dedupeProfileName).
		w.askFirstServer()
		return
	}
	switch {
	case len(profiles) == 0:
		w.askFirstServer()
	case len(projects) == 0:
		if w.data.profileID == "" && len(profiles) > 1 {
			w.openBindProfilePicker()
			return
		}
		p := profiles[0]
		if w.data.profileID != "" {
			for _, cand := range profiles {
				if cand.ID == w.data.profileID {
					p = cand
				}
			}
		} else {
			w.data.profileID = p.ID
		}
		w.data.profileName = p.Name
		w.data.projName = defaultHostName()
		w.step = stepProject
		w.form = w.projectForm()
		w.status = fmt.Sprintf("检测到既有 profile %s，跳过服务器录入与 profile 创建", p.Name)
	default:
		// Everything the wizard itself mints exists → serve segment. Recover
		// the cert fingerprint (display-only input to the pair card) via the
		// idempotent LoadOrCreateServeCert — normally the cert already exists;
		// on a pre-cert machine this creates it, which is exactly what the
		// fresh flow would have done anyway. An unreadable cert must not trap
		// the resume: fall back to a hint.
		if _, _, fp, err := mcpserver.LoadOrCreateServeCert(); err == nil {
			w.data.deviceFp = fp
		} else {
			w.data.deviceFp = "（指纹不可读：" + err.Error() + "）"
		}
		w.enterAddrForm()
		w.status = "检测到已完成的 profile/project，直接进入 serve 安装段（设备码由 client 端 sshmgr pair 配对时自动铸发）"
	}
}

// openBindProfilePicker opens the multi-profile resume binding picker (Plan 39):
// when a resume path needs a binding and several profiles exist, the owner
// picks — re-running resumeServerFlow afterwards routes to the original target
// with the chosen id already set. projectProfileOptions is shared with the
// issue-device-code / project forms (label = name, value = id).
func (w *wizardModel) openBindProfilePicker() {
	profiles, err := w.st.ListProfiles()
	if err != nil || len(profiles) < 2 {
		// Unreachable in practice (the caller just listed ≥2); fall through to
		// the sole/zero-profile routing rather than trapping the resume.
		w.resumeServerFlow()
		return
	}
	w.step = stepBindProfile
	w.form = huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("绑定哪个 profile？（决定本次向导补建的 project 与设备码的授权范围）").
			Options(projectProfileOptions(profiles)...).Value(&w.data.profileID),
	))
	w.status = "检测到多个 profile——本次向导补发的 project/设备码绑定到哪个 profile，请选择"
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
				if w.role == roles.RoleServer {
					w.enterServer() // retry (idempotent — existing vault is skipped)
				} else {
					w.enterStandalone()
				}
			}
			return w, nil
		}
		if w.form == nil {
			return w, nil
		}
		return w.feedFormMsg(m)
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
			// machine's .mcp.json on the manual path — and pair-minted devices
			// get their own dedicated token at approval (Plan 42 批1 T8: the
			// wizard mints no device code anymore, so there is no 2/2).
			w.ov = wizTokenScreen("project token", m.token,
				"手工路径:贴到 client 机 .mcp.json 的 SSHMGR_TOKEN 字段;新机亦可改走 sshmgr pair(配对完成自动铸发专属 token)",
				"主控台 Projects 页 [a] 重发")
			return w, nil
		}
		w.ov = wizTokenScreen(m.title, m.token,
			"贴到本机 .mcp.json 的 SSHMGR_TOKEN 字段",
			"主控台 Projects 页 [a] 重发")
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
				// Server flow: the device-code step is retired (Plan 42 批1 T8)
				// — the serve segment begins right after the token screen.
				w.enterAddrForm()
				return w, w.form.Init()
			}
			w.step = stepMcpConfig
			w.ov = mcpConfigScreen("上方已展示的 project token")
			return w, m.after
		case stepMcpConfig:
			return w, wizFinish(w.role) // any key on the finish screen completes setup
		case stepServeAdmin:
			// Admin notice acknowledged → run the registration.
			w.step = stepServeInstall
			return w, installServeStep(w.data.serveAddr)
		case stepServeResult:
			w.step = stepPairCard
			w.ov = clientPairCard(w.data.serveAddr, w.data.deviceFp)
			return w, nil
		case stepPairCard:
			return w, wizFinish(w.role) // any key completes the server setup
		case stepClientGuide:
			// Guidance acknowledged: complete the client setup (role.json →
			// setup_complete:true) and exit. The wizard does NOT chain into a
			// console here — the next step for the user is running
			// `sshmgr pair` in the shell (Run hands off to the broker
			// console only).
			return w, wizFinishTo(roles.RoleClient, "client")
		}
		return w, m.after
	case wizardDoneMsg:
		// Setup persisted complete: exit; Run chains into the broker console.
		w.done, w.next = true, m.next
		return w, tea.Quit
	default:
		// Plan 30 gate (注记 1 的解法): owned cases are the main switch itself
		// (they run before any overlay target); THIS branch is the target
		// selection for everything else — huh's unexported protocol msgs
		// (nextFieldMsg / nextGroupMsg — without this route every wizard form
		// is stuck on its first field in a real terminal), blink, paste,
		// resize. Static screen first (swallows q/Esc by Deliberate design —
		// the w.ov branch of the KeyPressMsg case above), else the form via
		// the shared tail. q/Esc/Ctrl+C interception stays KeyPressMsg-only
		// and AFTER the w.ov check — current semantics (向导输入框打不进 q 是
		// 既有取舍, the same trade-off importflow.go documents on its
		// supplement inputs).
		if w.ov != nil {
			ov, cmd := w.ov.Update(msg)
			w.ov, _ = ov.(overlay)
			return w, cmd
		}
		return w.feedFormMsg(msg)
	}
}

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

// stepFormDone routes a completed form to the next step. First-screen logic
// (two-phase q1/q2 → chooseRole) then the standalone flow.
func (w wizardModel) stepFormDone() (tea.Model, tea.Cmd) {
	if w.step == stepPick {
		if !w.askShare {
			if w.ans.keep == "no" {
				w.chooseRole(roles.RoleClient)
				return w, nil // 引导页(w.ov)已就位,任意键完成并退出
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
		return w, submitServer(w.st, nil, w.data.srvDraft) // add-mode: credential optional (Plan 20 C0)
	case stepProfileGrant:
		return w, w.submitProfileGrant()
	case stepProject:
		return w, w.submitProject()
	case stepBindProfile:
		// The picker set data.profileID (a huh Select always commits one
		// option); re-run the resume heuristic — it now routes past the
		// picker with the chosen binding. An (unreachable) empty selection
		// simply reopens the picker.
		w.resumeServerFlow()
		if w.form != nil {
			return w, w.form.Init()
		}
		return w, nil
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
	stepAddr:          " serve 地址 ",
}

func (w wizardModel) View() tea.View {
	if w.ov != nil {
		v := w.ov.View().Content
		// M1: a Save failure in wizFinish surfaces as errMsg while the overlay
		// is still up — render it BELOW the overlay or the error is invisible.
		if w.err != nil {
			v += "\n" + errStyle.Render("✗ "+w.err.Error()) + "\n（任意键重试）"
		}
		return altScreen(tea.NewView(v))
	}
	var b strings.Builder
	switch w.step {
	case stepPick:
		b.WriteString(titleStyle.Render(" 第一次使用 sshmgr ") + "\n\n")
		b.WriteString(w.form.View() + "\n\n")
		b.WriteString(footerStyle.Render("概念图解：docs/concepts.md（或 --help）") + "\n")
		if w.residualClient {
			b.WriteString(footerStyle.Render("检测到本机曾有 client 配置，可运行 sshmgr clear 清理") + "\n")
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
		quitHint := "q 退出（进度已保存，重开 tui 会继续）"
		if w.saveErr != nil {
			quitHint = "q 退出（role.json 写入失败，进度未保存）"
		}
		b.WriteString("\n" + footerStyle.Render(quitHint) + "\n")
	case stepVaultErr:
		b.WriteString(titleStyle.Render(" 初始化 vault 失败 ") + "\n\n")
		b.WriteString(errStyle.Render("✗ "+w.err.Error()) + "\n\n")
		if w.saveErr != nil {
			// Parity with stepRoleDone and the form steps: this screen's footer
			// promises 「角色已保存」 — with a failed role.json write that is
			// false and must not pass silently.
			b.WriteString(errStyle.Render(fmt.Sprintf("⚠ role.json 写入失败：%v", w.saveErr)) + "\n")
		}
		quitHint := "r 重试 / q 退出（角色已保存，重开 tui 会继续）"
		if w.saveErr != nil {
			quitHint = "r 重试 / q 退出（角色未保存，重开 tui 从头开始）"
		}
		b.WriteString(footerStyle.Render(quitHint) + "\n")
	case stepServeInstall, stepServeProbe:
		// In-flight steps: no form, no overlay — just what is running (and the
		// error + retry affordance if the action failed).
		titles := map[wizStep]string{
			stepServeInstall: " 安装 serve 服务 ",
			stepServeProbe:   " serve 探活 ",
		}
		b.WriteString(titleStyle.Render(titles[w.step]) + "\n\n")
		switch w.step {
		case stepServeInstall:
			b.WriteString("正在注册系统服务（绑定 0.0.0.0:7878，可能需要数秒）…\n")
			b.WriteString(footerStyle.Render("q 暂停退出（安装失败不会阻断向导）") + "\n")
		case stepServeProbe:
			b.WriteString("正在探活 " + w.data.serveAddr + " …\n")
			quitHint := "q 暂停退出（进度已保存）"
			if w.saveErr != nil {
				quitHint = "q 暂停退出（role.json 写入失败，进度未保存）"
			}
			b.WriteString(footerStyle.Render(quitHint) + "\n")
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
		quitHint := "q 暂停退出（进度已保存，重开 tui 会继续）"
		if w.saveErr != nil {
			quitHint = "q 暂停退出（role.json 写入失败，进度未保存）"
		}
		b.WriteString(footerStyle.Render(quitHint) + "\n")
	}
	return altScreen(tea.NewView(b.String()))
}

var roleLabels = map[roles.Role]string{
	roles.RoleStandalone: "单机",
	roles.RoleServer:     "服务器（server）",
	roles.RoleClient:     "客户端（client）",
}
