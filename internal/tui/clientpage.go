package tui

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/store"
)

// clientModel is the top-level model for client (cache) mode: a single screen
// with the connection header, a read-only server list from the cached
// snapshot, manual sync, and a connection-edit form. It deliberately shares
// NOTHING mutable with the broker App: client mode writes no vault, only
// cache.auth.json via clientops.WriteCacheCred.
type clientModel struct {
	cred     *clientops.CacheCred
	snap     *store.Snapshot
	cacheAge time.Duration
	cursor   int
	status   string
	err      error
	busy     bool
	overlay  overlay // connection-edit form
}

func newClientModel() clientModel { return clientModel{} }

func (m clientModel) Init() tea.Cmd { return refreshDataCmd }

type dataReadyMsg struct {
	cred *clientops.CacheCred
	snap *store.Snapshot
	age  time.Duration
}

type syncDoneMsg struct{ err error }

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

// syncCmd pulls a fresh snapshot off the UI loop. The pin from the stored cred
// is mandatory — the TUI NEVER offers plaintext pulls (AllowPlain stays false).
func syncCmd(cred *clientops.CacheCred) tea.Cmd {
	return func() tea.Msg {
		if cred == nil {
			return syncDoneMsg{fmt.Errorf("连接配置未加载，无法同步")}
		}
		if cred.Pin == "" {
			return syncDoneMsg{fmt.Errorf("连接配置缺 pin（本界面永不走明文拉取）——请 [c] 编辑连接补上")}
		}
		err := clientops.DoPull(cred.URL, cred.Token, cred.Pin, clientops.PullOpts{Timeout: clientops.LazyPullTimeout})
		return syncDoneMsg{err}
	}
}

func (m clientModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch kp := msg.(type) {
	case dataReadyMsg:
		m.cred, m.snap, m.cacheAge = kp.cred, kp.snap, kp.age
		if m.cursor >= len(clientServerRows(m.snap)) {
			m.cursor = 0
		}
		return m, nil
	case syncDoneMsg:
		m.busy = false
		if kp.err != nil {
			m.err, m.status = kp.err, ""
		} else {
			m.err, m.status = nil, "同步完成"
		}
		return m, refreshDataCmd
	case clientStatusMsg:
		m.err, m.status = nil, string(kp)
		return m, refreshDataCmd
	case errMsg:
		m.err, m.status = kp.err, ""
		return m, nil
	case formDoneMsg:
		m.overlay = nil
		return m, tea.Batch(kp.after, refreshDataCmd)
	case tea.KeyPressMsg:
		if m.overlay != nil { // overlay owns keys until done
			ov, cmd := m.overlay.Update(msg)
			m.overlay, _ = ov.(overlay)
			return m, cmd
		}
		k := kp.Key()
		switch {
		case k.Text == "q" || (k.Code == 'c' && k.Mod == tea.ModCtrl):
			return m, tea.Quit
		case k.Text == "s" && !m.busy:
			m.busy, m.err, m.status = true, nil, ""
			return m, syncCmd(m.cred)
		case k.Text == "c":
			if m.cred == nil {
				m.err, m.status = fmt.Errorf("连接配置未加载，无法编辑"), ""
				return m, nil
			}
			m.overlay = m.editConnForm()
			return m, m.overlay.Init()
		case k.Text == "t":
			m.status = "TTL 由 .mcp.json 的 --cache-max-age 控制（默认 30m；0=关闭自动拉取）"
			return m, nil
		case k.Code == tea.KeyUp && k.Mod == 0, k.Text == "k":
			m.move(-1)
		case k.Code == tea.KeyDown && k.Mod == 0, k.Text == "j":
			m.move(1)
		}
	case tea.WindowSizeMsg:
		return m, nil
	}
	return m, nil
}

func (m *clientModel) move(d int) {
	n := len(clientServerRows(m.snap))
	if n == 0 {
		return
	}
	c := m.cursor + d
	if c < 0 {
		c = 0
	}
	if c >= n {
		c = n - 1
	}
	m.cursor = c
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

func (m clientModel) editConnForm() overlay {
	old := m.cred
	d := &connDraft{URL: old.URL, Pin: old.Pin}
	form := huh.NewForm(huh.NewGroup(
		huh.NewInput().Title("serve 地址").Value(&d.URL).Validate(validServeURL),
		huh.NewInput().Title("设备码（留空=保持不变）").Value(&d.Code).EchoMode(huh.EchoModePassword),
		huh.NewInput().Title("pin（SPKI 指纹，公开信息）").Value(&d.Pin).Validate(validPin),
	))
	return newFormOverlay("编辑连接", form, func() tea.Cmd {
		return func() tea.Msg {
			token := old.Token
			if code := strings.TrimSpace(d.Code); code != "" {
				token = code
			}
			if err := clientops.WriteCacheCred(&clientops.CacheCred{
				URL:   strings.TrimSpace(d.URL),
				Token: token,
				Pin:   strings.TrimSpace(d.Pin),
			}); err != nil {
				return errMsg{err}
			}
			return clientStatusMsg("连接配置已保存")
		}
	})
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
func clientServerDetail(snap *store.Snapshot, cursor int) string {
	if snap == nil || cursor < 0 || cursor >= len(snap.Servers) {
		return "(空)"
	}
	s := snap.Servers[cursor]
	port := 0
	if s.Port != 0 {
		port = s.Port
	}
	return fmt.Sprintf("名称   %s\nHost   %s\n端口   %d\n用户   %s\n认证   %s\n硬件   %s\n位置   %s\n角色   %s\n服务   %s\nCaveats %s\n备注   %s",
		orDash(s.Name), orDash(s.Host), port, orDash(s.User), orDash(s.AuthMethod),
		orDash(s.Hardware), orDash(s.Location), orDash(s.Role), orDash(s.Services), orDash(s.Caveats), orDash(s.Description))
}

func (m clientModel) View() tea.View {
	if m.overlay != nil {
		return m.overlay.View()
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render(" ssh-manager (client)") + "\n")
	n := 0
	if m.snap != nil {
		n = len(m.snap.Servers)
	}
	b.WriteString(clientHeader(m.cred, n, m.cacheAge) + "\n")
	rows := clientServerRows(m.snap)
	if len(rows) > 0 {
		for i := range rows {
			if i == m.cursor {
				rows[i] = selStyle.Render(rows[i])
			}
		}
		b.WriteString(lipColumns(strings.Join(rows, "\n"), detailStyle.Render(clientServerDetail(m.snap, m.cursor))))
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
	b.WriteString(footerStyle.Render("[s]同步 [c]编辑连接 [t]TTL  q 退出"))
	return tea.NewView(b.String())
}
