package tui

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
)

// clientModel is the top-level model for client (cache) mode: a single screen
// with the connection header, a read-only server list from the cached
// snapshot, manual sync, and a connection-edit form. It deliberately shares
// NOTHING mutable with the broker App: client mode writes no vault, only
// cache.auth.json via clientops.WriteCacheCred.
//
// WIZARD FORM (Plan 19 T5): the same model with wizard=true IS the client
// role's first-run wizard — the connection form opens immediately with a
// source hint, a failed first pull reopens it with the previous input under a
// classified banner (classifyPullError), and a successful pull leads to the
// .mcp.json finish screen (clientFinishScreen) → wizFinishTo(client).
type clientModel struct {
	cred      *clientops.CacheCred
	snap      *store.Snapshot
	cacheAge  time.Duration
	panelList     // servers list panel: cursor/pagination//`/` filter (2026-08-17 桌面化)
	width     int // terminal width from WindowSizeMsg (0 = not yet reported)
	height    int // terminal height from WindowSizeMsg (0 = not yet reported)
	status    string
	err       error
	busy      bool
	overlay   overlay // connection-edit form / wizard finish screen

	wizard bool       // first-run flow active (source hint + pull-driven transitions)
	draft  *connDraft // last submitted connection draft (input preservation on failed pull)
	finish bool       // the overlay is the wizard's finish screen (any key completes)
}

func newClientModel() clientModel {
	m := clientModel{}
	m.panelList = newPanelList("服务器")
	return m
}

func (m clientModel) Init() tea.Cmd {
	if m.wizard && m.overlay != nil {
		// Fresh wizard: the form owns the screen. refreshDataCmd would only
		// produce "cache.auth.json 不存在" noise under the form on a fresh
		// machine — the cred arrives via connSavedMsg instead.
		return m.overlay.Init()
	}
	return refreshDataCmd
}

type dataReadyMsg struct {
	cred *clientops.CacheCred
	snap *store.Snapshot
	age  time.Duration
}

type syncDoneMsg struct{ err error }

// pullSucceededMsg is the WIZARD-only success signal of the first pull: panel
// mode reports success as syncDoneMsg{nil} ("同步完成"); the wizard instead
// routes to the .mcp.json finish screen. See syncCmdMode.
type pullSucceededMsg struct{}

// connSavedMsg carries the just-written cred (+ the draft it came from) back
// from the connection form. Wizard mode uses it to start the first pull and to
// retain the user's input for the failed-pull retry path; panel mode treats it
// as the success line.
type connSavedMsg struct {
	cred  *clientops.CacheCred
	draft *connDraft
}

// clientStatusMsg reports a user-visible success line (e.g. cred saved).
type clientStatusMsg string

// refreshDataCmd re-reads cred + snapshot + cache.bin mtime. Any failure rides
// errMsg so the banner explains why the panel is empty.
func refreshDataCmd() tea.Msg {
	cred, err := clientops.ReadCacheCred()
	if err != nil || cred == nil {
		if err == nil {
			err = fmt.Errorf("读取连接配置失败: cache.auth.json 不存在")
		} else {
			err = fmt.Errorf("读取连接配置失败: %w", err)
		}
		return errMsg{err}
	}
	snap, err := clientops.LoadCacheSnapshot()
	if err != nil {
		return errMsg{err}
	}
	_, bin, _, _, err := clientops.CachePaths()
	if err != nil {
		return errMsg{err}
	}
	var age time.Duration
	if fi, err := os.Stat(bin); err == nil {
		age = time.Since(fi.ModTime())
	}
	return dataReadyMsg{cred: cred, snap: snap, age: age}
}

// syncCmdMode is the pull command (panel and wizard share it). In WIZARD mode
// a successful pull returns pullSucceededMsg (→ the .mcp.json finish screen)
// instead of the panel's syncDoneMsg{nil}; every failure rides syncDoneMsg so
// the wizard can reopen the form under a classified banner
// (classifyPullError). The pin from the stored cred is mandatory — the TUI
// NEVER offers plaintext pulls (AllowPlain stays false).
func syncCmdMode(cred *clientops.CacheCred, wizard bool) tea.Cmd {
	return func() tea.Msg {
		if cred == nil {
			return syncDoneMsg{fmt.Errorf("连接配置未加载，无法同步")}
		}
		if cred.Pin == "" {
			return syncDoneMsg{fmt.Errorf("连接配置缺 pin（本界面永不走明文拉取）——请 [c] 编辑连接补上")}
		}
		err := clientops.DoPull(cred.URL, cred.Token, cred.Pin, clientops.PullOpts{Timeout: clientops.LazyPullTimeout})
		if err != nil {
			return syncDoneMsg{err}
		}
		if wizard {
			return pullSucceededMsg{}
		}
		return syncDoneMsg{nil}
	}
}

func (m clientModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Plan 30 gate (same shape as the App's). owned ⇔ the switch below has a
	// case (注记 4: the old KeyPressMsg overlay branch sat BEFORE the quit
	// case, so overlay-open Ctrl+C/q already went to the overlay today —
	// absorbing KeyPressMsg here changes nothing). huh advances fields/groups
	// via unexported msgs (nextFieldMsg/nextGroupMsg) — they can only be
	// routed by "owned allowlist + forward everything else". NEW
	// client-owned message types MUST be registered here (checklist item).
	if m.overlay != nil {
		switch msg := msg.(type) {
		case dataReadyMsg, syncDoneMsg, pullSucceededMsg, connSavedMsg,
			clientStatusMsg, errMsg, formDoneMsg:
			// owned: fall through to the switch below
		case tea.WindowSizeMsg:
			m.width, m.height = msg.Width, msg.Height
			ov, cmd := m.overlay.Update(msg)
			m.overlay, _ = ov.(overlay) // comma-ok failure = unreachable defense (spy tests lock the type)
			return m, cmd
		default:
			ov, cmd := m.overlay.Update(msg)
			m.overlay, _ = ov.(overlay)
			return m, cmd
		}
	}
	switch kp := msg.(type) {
	case dataReadyMsg:
		m.cred, m.snap, m.cacheAge = kp.cred, kp.snap, kp.age
		m.syncList()
		return m, nil
	case syncDoneMsg:
		m.busy = false
		if kp.err != nil {
			if m.wizard {
				// Failed FIRST pull: reopen the form WITH the previous input
				// (editConnForm prefills from the retained draft; the masked
				// code stays empty) under the classified banner.
				m.err, m.status = errors.New(classifyPullError(kp.err)), ""
				m.overlay = m.editConnForm()
				return m, m.overlay.Init()
			}
			m.err, m.status = kp.err, ""
		} else {
			m.err, m.status = nil, "同步完成"
		}
		return m, refreshDataCmd
	case pullSucceededMsg:
		// Wizard only (syncCmdMode): the first pull worked — cache is live,
		// show the .mcp.json finish screen. refreshDataCmd loads the fresh
		// snapshot so the panel behind the overlay is current.
		m.busy = false
		m.err, m.status = nil, "首次同步完成"
		m.finish = true
		m.overlay = clientFinishScreen()
		return m, tea.Batch(m.overlay.Init(), refreshDataCmd)
	case connSavedMsg:
		m.err, m.status = nil, ""
		m.cred, m.draft = kp.cred, kp.draft
		if m.wizard {
			m.busy = true // first pull in flight
			return m, syncCmdMode(kp.cred, true)
		}
		m.status = "连接配置已保存"
		return m, refreshDataCmd
	case clientStatusMsg:
		m.err, m.status = nil, string(kp)
		return m, refreshDataCmd
	case errMsg:
		m.err, m.status = kp.err, ""
		return m, nil
	case formDoneMsg:
		if m.finish {
			// Finish screen dismissed: keep the overlay up until the
			// completion Save succeeds — a failure arrives as errMsg
			// (rendered below the overlay) and the next key retries.
			return m, wizFinishTo(roles.RoleClient, "client")
		}
		m.overlay = nil
		return m, tea.Batch(kp.after, refreshDataCmd)
	case tea.KeyPressMsg:
		// (the pre-gate overlay branch lived here; keys now route through the
		// gate above — absorbing KeyPressMsg changed no behavior, see 注记 4)
		// List panel event stream (see listMsg): while the `/` filter input is
		// active it owns EVERY keypress; browsing keypresses fall through to
		// the s/c/t/q actions below (the list only consumes bound keys).
		if listMsg(msg) {
			cmd := m.listUpdate(msg)
			if _, press := msg.(tea.KeyPressMsg); !press || m.filtering() {
				return m, cmd
			}
		}
		k := kp.Key()
		switch {
		case k.Text == "q" || (k.Code == 'c' && k.Mod == tea.ModCtrl):
			return m, tea.Quit
		case k.Text == "s" && !m.busy:
			m.busy, m.err, m.status = true, nil, ""
			return m, syncCmdMode(m.cred, m.wizard)
		case k.Text == "c":
			// Wizard mode may edit on a fresh machine (no stored cred yet) —
			// the form IS the flow's entry; panel mode still requires a cred
			// to keep (the code field's 留空=保持不变 needs something to keep).
			if !m.wizard && m.cred == nil {
				m.err, m.status = fmt.Errorf("连接配置未加载，无法编辑"), ""
				return m, nil
			}
			m.overlay = m.editConnForm()
			return m, m.overlay.Init()
		case k.Text == "t":
			m.status = "TTL 由 .mcp.json 的 --cache-max-age 控制（默认 30m；0=关闭自动拉取）"
			return m, nil
		case k.Code == tea.KeyUp && k.Mod == 0, k.Text == "k",
			k.Code == tea.KeyDown && k.Mod == 0, k.Text == "j":
			// consumed by the list panel in the routing block above
			return m, nil
		}
	case tea.WindowSizeMsg:
		// the Plan 30 gate above records the same fields while an overlay is
		// open (resize reaches the model even when the overlay eats the msg) —
		// two writes, one semantic; keep them in sync (anti-drift).
		m.width, m.height = kp.Width, kp.Height
		return m, nil
	}
	return m, nil
}

// syncList mirrors the snapshot's servers into the list panel.
func (m *clientModel) syncList() {
	items := make([]list.Item, 0)
	if m.snap != nil {
		for i := range m.snap.Servers {
			items = append(items, clientItem{srv: &m.snap.Servers[i]})
		}
	}
	m.setListItems(items, len(items))
}

// clientItem adapts a snapshot server to the list panel: name, then user@host.
type clientItem struct{ srv *store.SnapshotServer }

func (i clientItem) FilterValue() string { return i.srv.Name }
func (i clientItem) Title() string       { return i.srv.Name }
func (i clientItem) Description() string {
	host := i.srv.Host
	if i.srv.Port != 0 && i.srv.Port != 22 {
		host = fmt.Sprintf("%s:%d", host, i.srv.Port)
	}
	return i.srv.User + "@" + host
}

// current resolves the cursor to the snapshot server it points at in the
// (possibly `/`-filtered) visible subset.
func (m clientModel) current() *store.SnapshotServer {
	vis := m.list.VisibleItems()
	i := m.list.Index()
	if i < 0 || i >= len(vis) {
		return nil
	}
	it, ok := vis[i].(clientItem)
	if !ok {
		return nil
	}
	return it.srv
}

// validServeURL gates the form's URL field: parseable and https-only, so a
// plaintext http:// serve addr can never be persisted to cache.auth.json.
func validServeURL(v string) error {
	u, err := url.Parse(strings.TrimSpace(v))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("必须是 https:// 开头的合法地址")
	}
	return nil
}

// validPin gates the form's pin field with the same check clientops uses at
// pull time (mcpserver.ParsePin), so a malformed fingerprint is rejected
// BEFORE WriteCacheCred persists it — DoPull then never sees a bad pin.
func validPin(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return errors.New("pin 不能为空（本界面永不走明文拉取）")
	}
	if _, ok := mcpserver.ParsePin(v); !ok {
		return errors.New("pin 须为 sha256:<64 位十六进制> 的 SPKI 指纹")
	}
	return nil
}

// connDraft backs the connection-edit form. Code (设备码) is the ONLY secret:
// masked and NOT prefilled — empty keeps the existing token. Pin is a public
// SPKI fingerprint, so it is shown plainly and prefilled.
type connDraft struct {
	URL, Code, Pin string
}

// editConnForm builds the connection form. Prefill order: the LAST SUBMITTED
// draft first (a failed wizard pull reopens with the user's url/pin intact —
// input preservation, T5), else the stored cred, else blank (fresh machine).
// The code field is NEVER prefilled — a masked secret is not re-echoed; empty
// keeps the existing token, and when NO token exists at all (fresh wizard
// machine) an empty code is rejected at submit.
func (m clientModel) editConnForm() overlay {
	urlVal, pinVal, token0 := "", "", ""
	if m.cred != nil {
		urlVal, pinVal, token0 = m.cred.URL, m.cred.Pin, m.cred.Token
	}
	if m.draft != nil { // input preservation: the last submitted draft wins
		urlVal, pinVal = m.draft.URL, m.draft.Pin
	}
	wizard := m.wizard
	d := &connDraft{URL: urlVal, Pin: pinVal}
	form := huh.NewForm(huh.NewGroup(
		huh.NewInput().Title("serve 地址").Value(&d.URL).Validate(validServeURL),
		huh.NewInput().Title("设备码（留空=保持不变）").Value(&d.Code).EchoMode(huh.EchoModePassword),
		huh.NewInput().Title("pin（SPKI 指纹，公开信息）").Value(&d.Pin).Validate(validPin),
	))
	return newFormOverlay("编辑连接", form, func() tea.Cmd {
		return func() tea.Msg {
			token := token0
			if code := strings.TrimSpace(d.Code); code != "" {
				token = code
			}
			if token == "" {
				return errMsg{errors.New("设备码不能为空（本机没有已保存的设备码可保持）")}
			}
			cred := &clientops.CacheCred{
				URL:   strings.TrimSpace(d.URL),
				Token: token,
				Pin:   strings.TrimSpace(d.Pin),
			}
			if err := clientops.WriteCacheCred(cred); err != nil {
				return errMsg{err}
			}
			if wizard {
				// Carry the draft + fresh cred back so the model can start the
				// first pull AND retain the input for the failed-pull retry.
				return connSavedMsg{cred: cred, draft: d}
			}
			return clientStatusMsg("连接配置已保存")
		}
	})
}

// classifyPullError turns a raw pull error into the client wizard's four-state
// diagnosis (T5 brief): dial / no such host → 地址不通; 401 / authorization →
// 设备码无效; mismatch / fingerprint → 指纹失配; Timeout → 超时. The matched
// category's guidance is prefixed to the RAW error text — the classification
// tells the user what to fix, the original tells them what happened.
func classifyPullError(err error) string {
	if err == nil {
		return "同步失败：<nil>"
	}
	s := strings.ToLower(err.Error())
	var kind string
	switch {
	case strings.Contains(s, "dial"), strings.Contains(s, "no such host"):
		kind = "地址不通：检查 serve 地址拼写与网络/防火墙"
	case strings.Contains(s, "server returned 401"), strings.Contains(s, "authorization"):
		kind = "设备码无效：核对 server 机签发的设备码（丢失可在其主控台重发）"
	case strings.Contains(s, "mismatch"), strings.Contains(s, "fingerprint"):
		kind = "指纹失配：核对 server 机接入卡上的 pin 指纹"
	case strings.Contains(s, "timeout"), strings.Contains(s, "client.timeout"):
		kind = "超时：server 可能未启动或网络不通，稍后重试"
	default:
		return "同步失败：" + err.Error()
	}
	return kind + "（" + err.Error() + "）"
}

// clientFinishScreen is the CLIENT role's .mcp.json finish screen (T5): the
// --cache variant of mcpConfigScreen. The SSHMGR_TOKEN here is the SERVER machine's
// project token — NOT the device code just used for the pull (a cache token
// authorizes pulls only; the agent's MCP auth is the project token). The
// client machine never sees that token during enrollment, so the snippet shows
// a placeholder pointing at where it comes from. Token rides env, not argv
// (ps/proc visibility — Plan 20 B2).
func clientFinishScreen() overlay {
	body := strings.Join(append(mcpConfigLines(
		[]string{
			`"args": ["mcp", "--cache"]`,
			`"env": { "SSHMGR_TOKEN": "<project token>" }`,
		},
		[]string{
			"client 角色用 --cache 离线缓存模式启动；SSHMGR_TOKEN 填 server 机 Projects 页签发的 project token（不是设备码——设备码只用于拉取缓存，刚才已保存）。",
			`Windows 建议写绝对路径，如 "command": "C:\\Tools\\ssh-manager.exe"。`,
			".mcp.json 含 token，不要提交进 git。",
		},
	), "", "按任意键进入 client 面板", ""), "\n")
	return &wizStaticView{title: "配置 agent 的 .mcp.json（client 模式）", body: body}
}

// clientHeader renders the one-line connection summary: broker host, pin
// fingerprint prefix, cache age, snapshot server count.
func clientHeader(cred *clientops.CacheCred, nServers int, age time.Duration) string {
	host, pin := "-", "-"
	if cred != nil {
		if u, err := url.Parse(cred.URL); err == nil && u.Host != "" {
			host = u.Host
		}
		if cred.Pin != "" {
			pin = cred.Pin
		}
	}
	return fmt.Sprintf("连接 %s · pin %s · %d 服务器 · 缓存于 %s 前", host, pin, nServers, age.Round(time.Minute))
}

// clientServerRows renders the read-only server list (one row per snapshot server).
func clientServerRows(snap *store.Snapshot) []string {
	if snap == nil {
		return nil
	}
	rows := make([]string, len(snap.Servers))
	for i, s := range snap.Servers {
		rows[i] = fmt.Sprintf("%s  %s@%s", s.Name, s.User, s.Host)
	}
	return rows
}

// clientServerDetail renders the detail pane for the cursor row (orDash-style).
func clientServerDetail(s *store.SnapshotServer) string {
	if s == nil {
		return "(空)"
	}
	port := 0
	if s.Port != 0 {
		port = s.Port
	}
	return fmt.Sprintf("名称   %s\nHost   %s\n端口   %d\n用户   %s\n认证   %s\n硬件   %s\n位置   %s\n角色   %s\n服务   %s\nCaveats %s\n备注   %s",
		orDash(s.Name), orDash(s.Host), port, orDash(s.User), orDash(s.AuthMethod),
		orDash(s.Hardware), orDash(s.Location), orDash(s.Role), orDash(s.Services), orDash(s.Caveats), orDash(s.Description))
}

// clientWizardHint is the wizard form's source-hint line (T5 brief): where
// the two hard-to-guess inputs come from.
const clientWizardHint = "设备码与服务器指纹在 server 机 TUI『设备码』页签发"

func (m clientModel) View() tea.View {
	hint := ""
	if m.wizard {
		hint = warnStyle.Render("ℹ "+clientWizardHint) + "\n"
	}
	if m.overlay != nil {
		v := hint + m.overlay.View().Content
		// M1 parity with the wizard: an error set while the overlay is up
		// (classified pull failure / finish-Save failure) renders BELOW it or
		// it is invisible.
		if m.err != nil {
			v += "\n" + errStyle.Render("✗ "+m.err.Error())
		}
		return altScreen(tea.NewView(v))
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render(" ssh-manager (client)") + "\n")
	b.WriteString(hint)
	n := 0
	if m.snap != nil {
		n = len(m.snap.Servers)
	}
	b.WriteString(clientHeader(m.cred, n, m.cacheAge) + "\n")
	if m.width > 0 {
		// desktop panels (2026-08-17): list + detail fitted to the terminal;
		// body height = frame minus header/hint/status/footer rows.
		chrome := 4
		if m.wizard {
			chrome++ // the hint line
		}
		if m.busy {
			chrome++ // the 同步中… line
		}
		b.WriteString(renderPanel(&m.list, clientServerDetail(m.current()), m.width, max(m.height-chrome, 6)))
	} else if rows := clientServerRows(m.snap); len(rows) > 0 {
		b.WriteString(columns(m.width, strings.Join(rows, "\n"), clientServerDetail(m.current())))
	} else {
		b.WriteString("（缓存快照中无服务器）")
	}
	b.WriteString("\n")
	if m.busy {
		b.WriteString(footerStyle.Render("同步中…") + "\n")
	}
	if m.err != nil {
		b.WriteString(errStyle.Render("✗ "+m.err.Error()) + "\n")
	} else if m.status != "" {
		b.WriteString(footerStyle.Render("✓ "+m.status) + "\n")
	}
	b.WriteString(clip(m.width, footerStyle.Render("[s]同步 [c]编辑连接 [t]TTL  q 退出")))
	return altScreen(tea.NewView(b.String()))
}
