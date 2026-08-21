// wizardsteps.go holds the step functions shared by the role wizards (Plan 19
// T3): pure constructors ("build an overlay / build a form / run an action")
// that the standalone flow in wizard.go composes today and the server (T4) /
// client (T5) wizard flows reuse later. Keeping them outside the wizard models
// makes each step independently unit-testable.
package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
)

// wizEnsureVault makes sure an UNLOCKED vault exists, creating one on first
// run. Initialization mirrors cli's `unlock` (internal/cli/unlock.go): generate
// a master key, persist via FileKeyProvider.Set (fixed path, ACL-hardened on
// Windows), then store.Open (creates store.db + schema) + Close. Idempotent:
// an existing unlocked vault is left untouched (roles.VaultExists /
// VaultUnlocked are stat-first probes, no create-on-open side effect); an
// existing LOCKED vault is an error guiding `unlock` — never a silent key
// overwrite.
func wizEnsureVault() error {
	if roles.VaultExists() {
		if roles.VaultUnlocked() {
			return nil
		}
		return errors.New("本机 vault 已存在但锁定或不可读：先运行 `ssh-manager unlock`（向导不会覆盖既有 vault）")
	}
	mk, err := store.GenerateMasterKey()
	if err != nil {
		return err
	}
	fp := store.FileKeyProvider{Path: os.Getenv("SSHMGR_FILEKEY_PATH")} // cli masterKeyProvider parity
	if err := fp.Set(mk); err != nil {
		return err
	}
	path := os.Getenv("SSHMGR_STORE")
	if path == "" {
		path, err = store.DefaultStorePath()
		if err != nil {
			return err
		}
	}
	st, err := store.Open(path, mk)
	if err != nil {
		return err
	}
	return st.Close()
}

// wizServerLoopForm is the wizard's add-server entry: the Plan 18 add-mode
// server form reused verbatim (password-or-key enforced at submit by
// submitServer). The loop itself — submit → doAction(AddServer) → 「继续添加？」
// confirm → reopen — is driven by the wizard state machine, not by the form.
func wizServerLoopForm(d *serverDraft) *huh.Form {
	return newServerForm(d, false)
}

// wizProfileGrantForm: profile name + grant multi-select in one form. The
// name field defaults to the machine hostname (preset by the caller); an
// existing-name conflict is NOT a validation error — the submit action
// auto-suffixes via dedupeProfileName (a conflict that blocks would trap a
// hostname-defaulted re-run). The multi-select is value=id (GrantServers
// wants ids; names are not unique). With zero servers the select is omitted —
// the grant step degrades to a name-only form.
func wizProfileGrantForm(profileName *string, servers []*models.Server, chosen *[]string) *huh.Form {
	fields := []huh.Field{
		huh.NewInput().Title("Profile 名称（服务器的授权分组）").
			Value(profileName).Validate(nonEmpty),
	}
	if len(servers) > 0 {
		fields = append(fields, huh.NewMultiSelect[string]().
			Title("授权给这个 profile 的服务器（空格勾选，回车提交；未选=agent 暂时看不到任何服务器）").
			Options(grantOptions(servers)...).Value(chosen))
	}
	return huh.NewForm(huh.NewGroup(fields...))
}

// dedupeProfileName returns name, or name-2 / name-3 … when name is already
// taken by an existing profile (hostname defaults collide on wizard re-runs).
// On a ListProfiles error the name passes through — AddProfile surfaces the
// real error at submit.
func dedupeProfileName(st *store.Store, name string) string {
	profiles, err := st.ListProfiles()
	if err != nil {
		return name
	}
	taken := make(map[string]bool, len(profiles))
	for _, p := range profiles {
		taken[p.Name] = true
	}
	base := name
	for i := 2; taken[name]; i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	return name
}

// defaultHostName is the wizard's default for profile / project names: the
// machine hostname, with a fixed fallback when it is unavailable.
func defaultHostName() string {
	h, err := os.Hostname()
	if err != nil || strings.TrimSpace(h) == "" {
		return "my-machine"
	}
	return strings.TrimSpace(h)
}

// ---------------------------------------------------------------------------
// One-time token screen + .mcp.json finish screen (overlays)
// ---------------------------------------------------------------------------

// wizSecretView shows a one-time secret with the wizard's usage/recovery
// footer — a sibling of App's secretView (tokenview.go), which has its own
// fixed footer and formDoneMsg dismissal semantics. Any key dismisses via
// formDoneMsg{}; the OWNING WIZARD decides what step follows (standalone →
// mcpConfigScreen, T4 server flow → its own next step).
type wizSecretView struct{ title, body string }

func (s *wizSecretView) Title() string { return s.title }
func (s *wizSecretView) Init() tea.Cmd { return nil }

func (s *wizSecretView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if _, ok := msg.(tea.KeyPressMsg); ok {
		return s, func() tea.Msg { return formDoneMsg{} }
	}
	return s, nil
}

func (s *wizSecretView) View() tea.View {
	return tea.NewView(titleStyle.Render(" "+s.title+" ") + "\n\n" +
		secretStyle.Render(s.body) + "\n\n按任意键继续\n")
}

// wizTokenScreen builds the one-time token overlay. body = the token itself +
// 用途 line + 「⚠ 仅此一次。丢失 → 」recovery line — all three copy elements are
// mandatory so the operator always knows where the token goes and what to do
// when it is lost (the store keeps only a hash; plaintext is unrecoverable).
func wizTokenScreen(title, token, usage, recovery string) overlay {
	return &wizSecretView{
		title: title,
		body:  token + "\n\n用途：" + usage + "\n⚠ 仅此一次。丢失 → " + recovery,
	}
}

// wizStaticView is a passive full-screen wizard step (text walkthrough): any
// key emits formDoneMsg{}; the owning wizard decides what follows. q/Ctrl+C
// are carved out as a program QUIT — nothing is pending on these screens (the
// wizard's data is already persisted), so quit is always safe.
type wizStaticView struct{ title, body string }

func (s *wizStaticView) Title() string { return s.title }
func (s *wizStaticView) Init() tea.Cmd { return nil }

func (s *wizStaticView) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyPressMsg); ok {
		// q/Ctrl+C = quit. Deliberate contrast with the one-time secret
		// screens (wizSecretView below), which keep any-key-advance: their
		// data is also already persisted, but the secret must be explicitly
		// acknowledged rather than accidentally dismissed by a quit reflex.
		// Esc on static screens keeps advancing — only q/Ctrl+C quit.
		kk := k.Key()
		if kk.Text == "q" || (kk.Code == 'c' && kk.Mod == tea.ModCtrl) {
			return s, tea.Quit
		}
		return s, func() tea.Msg { return formDoneMsg{} }
	}
	return s, nil
}

func (s *wizStaticView) View() tea.View {
	return tea.NewView(titleStyle.Render(" "+s.title+" ") + "\n\n" + s.body)
}

// jsonValue encodes s as a complete JSON string VALUE (quotes included) with
// HTML escaping DISABLED — the pinned value-encoding discipline (spec §4.2):
// strconv.Quote-family is forbidden (Go-only \a \v escapes are illegal JSON)
// and json.Marshal's default HTML escaping would turn < > & into 6-char
// escape sequences, wrecking every angle-bracket placeholder.
func jsonValue(s string) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) // string encode never fails
	return strings.TrimSuffix(b.String(), "\n")
}

// stdioEnvLine builds the stdio member line carrying the token — the ONLY
// sanctioned way to interpolate SSHMGR_TOKEN (symmetric encoding discipline
// with the http builder's url/Bearer values).
func stdioEnvLine(token string) string {
	return `"env": { "SSHMGR_TOKEN": ` + jsonValue(token) + ` }`
}

// mcpSnippetLines renders the shared snippet skeleton — intro line, the
// pretty-printed mcpServers object with comma-joined members, and the notes
// block. Both builders (stdio mcpConfigLines / http mcpHttpConfigLines) call
// it, so the trailing-comma discipline exists in ONE place.
func mcpSnippetLines(members []string, notes []string) []string {
	lines := []string{
		"把下面的片段写进 agent 项目的 .mcp.json：",
		"",
		"{",
		`  "mcpServers": {`,
		`    "ssh": {`,
	}
	for i, m := range members {
		if i < len(members)-1 {
			m += ","
		}
		lines = append(lines, "      "+m)
	}
	lines = append(lines, `    }`, `  }`, "}", "", "说明：")
	for _, n := range notes {
		lines = append(lines, "- "+n)
	}
	return lines
}

// mcpConfigLines renders the .mcp.json snippet shared by every role's finish
// screen — only the field lines (args / env) and the notes differ per role
// (standalone/server run plain `mcp`, the client role runs `mcp --cache`).
// The "ssh" object's members (command + fieldLines) are collected first and
// comma-joined as a whole, so the LAST member never carries a trailing comma —
// an empty fieldLines list yields valid JSON too.
func mcpConfigLines(fieldLines []string, notes []string) []string {
	members := make([]string, 0, len(fieldLines)+1)
	members = append(members, `"command": "ssh-manager"`)
	members = append(members, fieldLines...)
	return mcpSnippetLines(members, notes)
}

// mcpHttpConfigLines renders the ONLINE (serve/http) .mcp.json snippet —
// sibling of mcpConfigLines (stdio shape), sharing the mcpSnippetLines
// skeleton. VALUE ENCODING (hard requirement, pinned): urlRef and the
// Authorization header are encoded via jsonValue on the COMPLETE value
// string (e.g. "Bearer "+tokenRef) — never per-fragment concatenation.
func mcpHttpConfigLines(urlRef, tokenRef string, notes []string) []string {
	members := []string{
		`"type": "http"`,
		`"url": ` + jsonValue(urlRef),
		`"headers": { "Authorization": ` + jsonValue("Bearer "+tokenRef) + ` }`,
	}
	return mcpSnippetLines(members, notes)
}

// mcpConfigScreen is the finish screen: the full .mcp.json snippet in the
// real documented shape (docs/agent-access.md). tokenRef is what stands in
// for the token in the snippet (the plaintext was on the previous screen and
// is gone for good). Standalone runs plain `mcp` — NOT the client role's
// `--cache` offline mode. The token rides the SSHMGR_TOKEN env field, not
// argv (ps/proc visibility — Plan 20 B2).
func mcpConfigScreen(tokenRef string) overlay {
	body := strings.Join(append(mcpConfigLines(
		[]string{
			`"args": ["mcp"]`,
			`"env": { "SSHMGR_TOKEN": "` + tokenRef + `" }`,
		},
		[]string{
			"单机角色用普通 mcp 启动（不要用 --cache —— 那是 client 角色的离线缓存模式）。",
			`Windows 建议写绝对路径，如 "command": "C:\\Tools\\ssh-manager.exe"。`,
			".mcp.json 含 token，不要提交进 git。",
		},
	), "", "按任意键进入主控台", ""), "\n")
	return &wizStaticView{title: "配置 agent 的 .mcp.json", body: body}
}

// ---------------------------------------------------------------------------
// Finish + handoff
// ---------------------------------------------------------------------------

// wizardDoneMsg is the sentinel the wizard exits with when its flow is
// complete: Run (mode.go) sees it on the final model and chains into the
// target console (next="broker" for standalone/server).
type wizardDoneMsg struct{ next string }

// wizFinish marks the setup complete (roles.Save with SetupComplete:true —
// the anti-dead-state counterpart of chooseRole's early Save) and returns the
// handoff sentinel. Runs as a tea.Cmd AFTER the last screen is dismissed; a
// Save failure comes back as errMsg (the wizard stays on the finish screen
// and any key retries).
func wizFinish(r roles.Role) tea.Cmd { return wizFinishTo(r, "broker") }

// wizFinishTo is wizFinish with an explicit handoff target: the client-role
// wizard (T5) chains into the CLIENT panel, not the broker console.
func wizFinishTo(r roles.Role, next string) tea.Cmd {
	return func() tea.Msg {
		if err := roles.Save(roles.State{Role: r, SetupComplete: true}); err != nil {
			return errMsg{err}
		}
		return wizardDoneMsg{next: next}
	}
}
