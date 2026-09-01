package tui

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/store"
)

// clientModel is the top-level model for client (cache) mode: a single screen
// with the connection header, a read-only server list from the cached
// snapshot, and manual sync. It deliberately shares NOTHING mutable with the
// broker App: client mode writes no vault, only cache.auth.json via
// clientops.WriteCacheCred.
//
// Plan 42 批1 T8: the connection-edit form is RETIRED — it never comes back.
// Plan 45 T3: the [c] affordance is real again as the SAS pairing wizard
// (pairwizard.go), so a new machine pairs without leaving the TUI; `sshmgr
// pair` stays the CLI path and the manual path (`cache pull` + hand-written
// .mcp.json) stays documented for CI/automation.
type clientModel struct {
	cred          *clientops.CacheCred
	snap          *store.Snapshot
	scoped        bool // cache pulled from a Plan-39 serve (X-Sshmgr-Snapshot-Scope) — the profile header is only honest when true
	cacheAge      time.Duration
	instance      string // Plan 40 批2 §3.1: selected slot ("" = default), session-only
	pickerChecked bool   // Plan 40 批2 §3.2: auto-picker one-shot latch
	panelList            // servers list panel: cursor/pagination//`/` filter (2026-08-17 桌面化)
	width         int    // terminal width from WindowSizeMsg (0 = not yet reported)
	height        int    // terminal height from WindowSizeMsg (0 = not yet reported)
	status        string
	err           error
	busy          bool
	overlay       overlay // instance picker
}

func newClientModel() clientModel {
	m := clientModel{}
	m.panelList = newPanelList("服务器")
	return m
}

func (m clientModel) Init() tea.Cmd {
	return refreshDataCmd
}

type dataReadyMsg struct {
	instance string // which slot this reply belongs to (stale replies are dropped)
	cred     *clientops.CacheCred
	snap     *store.Snapshot
	scoped   bool
	age      time.Duration
}

type syncDoneMsg struct{ err error }

// clientStatusMsg reports a user-visible success line.
type clientStatusMsg string

// refreshDataCmdFor re-reads ONE slot's cred + snapshot + cache.bin mtime.
// Any failure rides errMsg so the banner explains why the panel is empty.
func refreshDataCmdFor(instance string) tea.Cmd {
	return func() tea.Msg {
		cred, err := clientops.ReadCacheCredFor(instance)
		if err != nil || cred == nil {
			if err == nil {
				err = fmt.Errorf("读取连接配置失败: cache.auth.json 不存在")
			} else {
				err = fmt.Errorf("读取连接配置失败: %w", err)
			}
			return errMsg{err}
		}
		snap, err := clientops.LoadCacheSnapshotFor(instance)
		// Plan 34: 报文归因面钉在 mcp --cache 与 cache status 两处（spec §4）；TUI 保持原始错误文本——pull 路径已有哨兵信息。
		if err != nil {
			return errMsg{err}
		}
		_, bin, _, _, err := clientops.CachePathsFor(instance)
		if err != nil {
			return errMsg{err}
		}
		var age time.Duration
		if fi, serr := os.Stat(bin); serr == nil {
			age = time.Since(fi.ModTime())
		}
		return dataReadyMsg{instance: instance, cred: cred, snap: snap, scoped: clientops.CacheScopeVerifiedFor(instance), age: age}
	}
}

var refreshDataCmd = refreshDataCmdFor("") // zero-change wrapper for existing callers

// syncCmdMode is the pull command (panel [s]). The pin from the stored cred is
// mandatory — the TUI NEVER offers plaintext pulls (AllowPlain stays false).
func syncCmdMode(cred *clientops.CacheCred, instance string) tea.Cmd {
	return func() tea.Msg {
		if cred == nil {
			return syncDoneMsg{fmt.Errorf("连接配置未加载，无法同步")}
		}
		if cred.Pin == "" {
			return syncDoneMsg{fmt.Errorf("连接配置缺 pin（本界面永不走明文拉取）——请运行 sshmgr pair 重新入网")}
		}
		_, err := clientops.DoPull(cred.URL, cred.Token, cred.Pin, clientops.PullOpts{Timeout: clientops.LazyPullTimeout, Instance: instance})
		if err != nil {
			return syncDoneMsg{err}
		}
		return syncDoneMsg{nil}
	}
}

func (m clientModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Plan 30 gate (same shape as the App's). owned ⇔ the switch below has a
	// case. huh advances fields/groups via unexported msgs (nextFieldMsg/
	// nextGroupMsg) — they can only be routed by "owned allowlist + forward
	// everything else". NEW client-owned message types MUST be registered here
	// (checklist item).
	if m.overlay != nil {
		switch msg := msg.(type) {
		case dataReadyMsg, syncDoneMsg,
			clientStatusMsg, errMsg, formDoneMsg,
			instancePickedMsg, instancePickerClosedMsg, // Plan 40 批2 T6
			pairWizardDoneMsg, pairWizardClosedMsg, // Plan 45 T3: the wizard's terminal msgs
			instancePickerPairMsg: // Plan 45 T3: the picker's [p] re-pair request
			// owned: fall through to the switch below. The wizard's five INTERNAL
			// async msgs (discover/enroll/approval/write/tick) stay unregistered
			// on purpose — the default branch forwards them to the overlay.
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
		if kp.instance != m.instance {
			return m, nil // stale slot reply (user switched mid-flight)
		}
		if !m.pickerChecked && m.autoPickerIfVacuum() {
			return m, m.overlay.Init()
		}
		m.cred, m.snap, m.scoped, m.cacheAge = kp.cred, kp.snap, kp.scoped, kp.age
		m.syncList()
		return m, nil
	case syncDoneMsg:
		m.busy = false
		if kp.err != nil {
			m.err, m.status = kp.err, ""
		} else {
			m.err, m.status = nil, "同步完成"
		}
		return m, refreshDataCmdFor(m.instance)

	case clientStatusMsg:
		m.err, m.status = nil, string(kp)
		return m, refreshDataCmdFor(m.instance)
	case errMsg:
		if !m.pickerChecked && m.autoPickerIfVacuum() {
			return m, m.overlay.Init()
		}
		m.err, m.status = kp.err, ""
		return m, nil
	case formDoneMsg:
		m.overlay = nil
		return m, tea.Batch(kp.after, refreshDataCmdFor(m.instance))
	case instancePickedMsg:
		// §3.1 in-session slot switch: drop the picker, retarget the session,
		// and re-read THAT slot — its reply carries the matching instance.
		m.instance, m.overlay, m.err = kp.instance, nil, nil
		return m, refreshDataCmdFor(kp.instance)
	case instancePickerClosedMsg:
		m.overlay = nil
		return m, nil
	case instancePickerPairMsg:
		// Plan 45 T3: [p] on a paired picker row = force re-pair through the
		// wizard (prefill Instance+Force; its own confirm screen gates the
		// cleanup before any file is touched).
		w, werr := newPairWizard(PairWizardPrefill{Instance: kp.instance, Force: true})
		if werr != nil {
			// Single-slot override refusal: keep the picker up, render the
			// error below it (M1 parity — an error under an overlay must be
			// visible).
			m.err = werr
			return m, nil
		}
		m.overlay = w
		return m, w.Init()
	case pairWizardDoneMsg:
		// Plan 45 T3: pairing success = in-session slot switch (instancePickedMsg
		// semantics): retarget the session to the freshly paired instance, drop
		// the wizard, and re-read THAT slot — its reply carries the matching
		// instance (formDoneMsg's close-then-refresh shape, retargeted).
		m.instance, m.overlay, m.err = kp.instance, nil, nil
		return m, refreshDataCmdFor(kp.instance)
	case pairWizardClosedMsg:
		// Plan 45 T3: Esc at any wizard step = pure return to the page (the
		// wizard's aborts are zero-residue; if a mid-flight force abort left the
		// slot half-cleaned, the next [s]/refresh surfaces the honest error).
		m.overlay = nil
		return m, nil
	case tea.KeyPressMsg:
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
			return m, syncCmdMode(m.cred, m.instance)
		case k.Text == "i" && !m.busy:
			// §3.5: single-slot override envs keep this UI off (T7 refines the
			// banner); busy swallows the key above via the guard.
			if clientops.SingleSlotOverrideEnvSet() {
				m.status = "单槽模式（SSHMGR_CACHE_DIR/SSHMGR_CACHE_DEK 覆盖中）——多实例 UI 已禁用"
				return m, nil
			}
			m.overlay = newInstancePicker()
			return m, m.overlay.Init()
		case k.Text == "c" && !m.busy:
			// Plan 45 T3: the affordance is real again — [c] starts the SAS
			// pairing wizard (Plan 42 批1 T8 had reduced it to a pointer at
			// `sshmgr pair`). busy swallows the key above via the guard, same
			// as [s]/[i]. A single-slot override refuses inside newPairWizard;
			// the refusal shows as the panel error.
			w, werr := newPairWizard(PairWizardPrefill{})
			if werr != nil {
				m.err = werr
				return m, nil
			}
			m.overlay = w
			return m, w.Init()
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

// autoPickerIfVacuum is the §3.2 one-shot probe, latched by pickerChecked: the
// FIRST errMsg/dataReadyMsg after startup (stale replies don't count — they
// return before reaching it) consults it. The default slot must be a TRUE
// four-file vacuum (the same judgment as first-enroll relocation), single-slot
// overrides must be off, no overlay may be up, and at least one named instance
// must exist — only then does the picker open INSTEAD of this message being
// processed.
func (m *clientModel) autoPickerIfVacuum() bool {
	m.pickerChecked = true // latch regardless of outcome
	vac, verr := clientops.DefaultSlotVacuum()
	if verr != nil || !vac || clientops.SingleSlotOverrideEnvSet() || m.overlay != nil {
		return false
	}
	names, lerr := clientops.ListInstances()
	if lerr != nil || len(names) == 0 {
		return false
	}
	m.overlay = newInstancePicker()
	return true
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

// clientHeader renders the one-line connection summary: broker host, pin
// fingerprint prefix, bound profile (ONLY when the pull recorded the Plan-39
// scope header — a legacy single-profile whole-vault snapshot is
// shape-identical, code-review #3), snapshot server count, cache age.
func clientHeader(cred *clientops.CacheCred, snap *store.Snapshot, scoped bool, nServers int, age time.Duration) string {
	host, pin := "-", "-"
	if cred != nil {
		if u, err := url.Parse(cred.URL); err == nil && u.Host != "" {
			host = u.Host
		}
		if cred.Pin != "" {
			pin = cred.Pin
		}
	}
	profile := ""
	if scoped && snap != nil && len(snap.Profiles) == 1 {
		profile = " · profile " + snap.Profiles[0].Name
	}
	return fmt.Sprintf("连接 %s · pin %s%s · %d 服务器 · 缓存于 %s 前", host, pin, profile, nServers, age.Round(time.Minute))
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
	return fmt.Sprintf("名称   %s\nHost   %s\n端口   %d\n用户   %s\n认证   %s\n硬件   %s\n位置   %s\n角色   %s\n服务   %s\nCaveats %s\n暴露Host %s\n备注   %s",
		orDash(s.Name), orDash(s.Host), port, orDash(s.User), orDash(s.AuthMethod),
		orDash(s.Hardware), orDash(s.Location), orDash(s.Role), orDash(s.Services), orDash(s.Caveats), exposeLabel(s.ExposeHost), orDash(s.Description))
}

func (m clientModel) View() tea.View {
	if m.overlay != nil {
		v := m.overlay.View().Content
		// M1 parity: an error set while the overlay is up renders BELOW it or
		// it is invisible.
		if m.err != nil {
			v += "\n" + errStyle.Render("✗ "+m.err.Error())
		}
		return altScreen(tea.NewView(v))
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render(" sshmgr (client)") + "\n")
	singleSlot := clientops.SingleSlotOverrideEnvSet()
	if singleSlot {
		// §3.5: the two override envs pin this process to ONE cache slot —
		// say so where a user would otherwise wonder why [i] is gone.
		b.WriteString(warnStyle.Render("⚠ 单槽模式（SSHMGR_CACHE_DIR/SSHMGR_CACHE_DEK 覆盖中）——多实例 UI 已禁用") + "\n")
	}
	if m.instance != "" {
		// §3.4: a named slot is always visible in the chrome — there is no way
		// to forget which instance this panel is showing.
		b.WriteString(warnStyle.Render("· 实例 "+m.instance) + "\n")
	}
	if m.cred == nil {
		// Plan 45 T3 (supersedes Plan 42 批1 前置 #4's pair-only wording): the
		// empty panel now points at the in-TUI wizard first, with the CLI path
		// kept in the sentence.
		b.WriteString(warnStyle.Render("ℹ 新机入网:按 [c] 启动配对向导(或运行 sshmgr pair)") + "\n")
	}
	n := 0
	if m.snap != nil {
		n = len(m.snap.Servers)
	}
	b.WriteString(clientHeader(m.cred, m.snap, m.scoped, n, m.cacheAge) + "\n")
	if m.width > 0 {
		// desktop panels (2026-08-17): list + detail fitted to the terminal;
		// body height = frame minus header/banner/status/footer rows.
		chrome := 4
		if singleSlot {
			chrome++ // the §3.5 banner line (conditional like every other row)
		}
		if m.instance != "" {
			chrome++ // the named-instance line (Plan 40 批2 T6)
		}
		if m.cred == nil {
			chrome++ // the pair-guidance line (Plan 42 批1 T8)
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
	// §3.5 footer variant: the [i] key would bounce off the single-slot guard
	// in Update — don't advertise it while that mode is on. Since Plan 45 T3
	// [c] really starts the pairing wizard, and newPairWizard refuses under a
	// single-slot override — the hint goes dark there too (don't lie).
	clientFooter := "[s]同步 [i]实例 [c]入网 [t]TTL  q 退出"
	if singleSlot {
		clientFooter = "[s]同步 [t]TTL  q 退出"
	}
	b.WriteString(clip(m.width, footerStyle.Render(clientFooter)))
	return altScreen(tea.NewView(b.String()))
}
