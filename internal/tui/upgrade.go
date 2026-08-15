// upgrade.go hosts the standalone→server upgrade segment (Plan 19 T6): a mini
// state machine owned by the broker App that reuses the T4 serve pieces
// (addr picker → admin notice → install → probe → result banners → device
// code → access card). The upgrade is NON-DESTRUCTIVE by construction: the
// only writes it can ever make are the serve service registration, the serve
// cert (idempotent LoadOrCreateServeCert), ONE client device code, and — on a
// clean install — role.json. Existing vault entities (servers / profiles /
// projects / tokens) are never touched.
package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/roles"
)

// upgStep is the upgrade segment's coarse state. Form/overlay steps own the
// keys until formDoneMsg; the in-flight steps (install/probe/issue) show no
// overlay — the main console stays visible and their msgs advance the machine.
type upgStep int

const (
	upgAddr        upgStep = iota // LAN address picker (formOverlay, reused T4 wizAddrForm)
	upgAdmin                      // admin 前置提示 (static overlay, reused T4 serveAdminNotice)
	upgInstall                    // service registration in flight (no overlay)
	upgProbe                      // post-install probe in flight (no overlay)
	upgResult                     // install + probe banners (static overlay, reused T4)
	upgClientName                 // 客户端机器名 form (one field, default hostname)
	upgDeviceIssue                // device-code mint in flight (no overlay)
	upgDeviceCode                 // 设备码 one-time screen (wizTokenScreen pattern)
	upgAccessCard                 // 客户端接入卡 (static overlay, reused T4 accessCard)
)

// upgradeSegment is heap-allocated ONCE in startUpgrade: App travels by value
// through Update, so the huh Value-pointer bindings (&seg.serveAddr,
// &seg.clientName) must point at one stable allocation or they would go stale
// after the first copy (same rationale as wizardData).
type upgradeSegment struct {
	step       upgStep
	serveAddr  string // chosen LAN addr (display-only; the install binds 0.0.0.0:7878)
	deviceFp   string // serve cert SPKI fingerprint (device-code usage + access card)
	installErr error  // serve install outcome — non-nil blocks the role flip (see upgradeComplete)
	clientName string // names the minted device code
}

// nilCmd is the no-op action passed to formOverlay when the segment — not the
// form — advances on formDoneMsg.
func nilCmd() tea.Cmd { return nil }

// startUpgrade opens the segment at the LAN-address picker. From here until
// the segment ends, App.Update routes formDoneMsg / serveInstalledMsg /
// serveProbeMsg / deviceCodeIssuedMsg into the machine below.
func (a *App) startUpgrade() {
	seg := &upgradeSegment{step: upgAddr}
	a.upg = seg
	a.err, a.status = nil, ""
	a.overlay = newFormOverlay("升级为 server — serve 地址",
		wizAddrForm(mcpserver.LocalNonLoopbackIPs(), &seg.serveAddr), nilCmd)
}

// cancelUpgrade aborts the segment back to the plain standalone console.
func (a App) cancelUpgrade() (tea.Model, tea.Cmd) {
	a.upg, a.overlay = nil, nil
	a.status = "已取消升级（保持单机角色）"
	return a, nil
}

// upgradeFormDone advances the machine when the current overlay emitted
// formDoneMsg. A form answered with an empty string (Esc aborts huh forms
// before any preset default is committed) means the user backed out: cancel.
func (a App) upgradeFormDone(after tea.Cmd) (tea.Model, tea.Cmd) {
	switch a.upg.step {
	case upgAddr:
		a.upg.serveAddr = strings.TrimSpace(a.upg.serveAddr)
		if a.upg.serveAddr == "" {
			return a.cancelUpgrade()
		}
		a.upg.step = upgAdmin
		a.overlay = serveAdminNotice()
		return a, nil
	case upgAdmin:
		a.upg.step = upgInstall
		a.overlay = nil
		return a, installServeStep(a.upg.serveAddr)
	case upgResult:
		// Banners dismissed → name the client this enrollment is for. The
		// answer names the device code (one field; default = hostname).
		a.upg.step = upgClientName
		a.upg.clientName = defaultHostName()
		a.overlay = newFormOverlay("升级为 server — 客户端机器名", huh.NewForm(huh.NewGroup(
			huh.NewInput().Title("客户端机器名（将命名签发给它的设备码；填对方电脑的名字）").
				Value(&a.upg.clientName).Validate(nonEmpty),
		)), nilCmd)
		return a, a.overlay.Init()
	case upgClientName:
		a.upg.clientName = strings.TrimSpace(a.upg.clientName)
		if a.upg.clientName == "" {
			return a.cancelUpgrade()
		}
		a.upg.step = upgDeviceIssue
		a.overlay = nil
		return a, a.issueDeviceCode()
	case upgDeviceCode:
		a.upg.step = upgAccessCard
		a.overlay = accessCard(a.upg.serveAddr, a.upg.deviceFp)
		return a, nil
	case upgAccessCard:
		return a.upgradeComplete()
	default:
		// In-flight steps have no overlay; a stray formDoneMsg just clears it.
		a.overlay = nil
		return a, after
	}
}

// issueDeviceCode mints the CLIENT device code on the server side (T6's
// counterpart of the wizard's step ⑤b). Same ordering discipline as the
// wizard's issueDeviceCode: cert FIRST, code second — if the cert init failed
// after AddCacheToken succeeded, a retry would hit the active-name collision
// on the already-minted code; this order keeps the retry idempotent.
func (a App) issueDeviceCode() tea.Cmd {
	seg, st := a.upg, a.st
	return func() tea.Msg {
		_, _, fp, err := mcpserver.LoadOrCreateServeCert()
		if err != nil {
			return errMsg{err}
		}
		_, code, err := st.AddCacheToken(seg.clientName)
		if err != nil {
			return errMsg{err}
		}
		seg.deviceFp = fp
		return deviceCodeIssuedMsg{code: code, fingerprint: fp}
	}
}

// upgradeComplete runs when the access card is dismissed. DELIBERATE
// ASYMMETRY on install failure: the walkthrough still completes (the result
// screen already showed the manual elevated command and the device code was
// already minted), but roles.Save is SKIPPED — the machine stays standalone so
// [u] remains available for a retry after the operator runs the manual
// install. Only a clean install persists {server, setup_complete:true}.
func (a App) upgradeComplete() (tea.Model, tea.Cmd) {
	installErr := a.upg.installErr
	a.upg, a.overlay = nil, nil
	if pages, err := FetchAll(a.st); err == nil { // fold in the minted device code
		a.pages = pages
	}
	if installErr != nil {
		a.err = nil
		a.status = "serve 安装失败 —— 按结果屏的手动命令安装后，再次按 u 完成升级（角色保持单机）"
		return a, nil
	}
	if err := roles.Save(roles.State{Role: roles.RoleServer, SetupComplete: true}); err != nil {
		a.err, a.status = err, ""
		return a, nil // stays standalone: [u] retries the whole segment
	}
	a.role = roles.RoleServer // footer drops [u]
	a.err = nil
	a.status = "已升级为 server"
	return a, nil
}
