// importflow.go is the TUI ssh-config import overlay (Plan 20 T10): file-path
// form (prefill ~/.ssh/config) → candidate multiselect (vault conflicts
// already excluded via importer.PlanImport) → silent batch import (importer.PickKey
// + AddServerWithCredentials — the same seam `servers import` uses) →
// per-server supplement loop (structured fields + one conditional secret;
// Esc skips keeping the ⚠, q jumps to the result) → result screen.
package tui

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"ssh-manager-mcp/internal/importer"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

type importState int

const (
	statePathForm   importState = iota // which config file
	statePick                          // multiselect non-conflicting candidates
	stateImporting                     // batch import in flight (one tea.Cmd)
	stateSupplement                    // per-server 补全 form loop
	stateResult                        // counts + report, any key closes
)

// importFlow is an App overlay (the formOverlay pattern, multi-step). Used by
// pointer only: the App hands every msg to the SAME instance and huh's
// Value-pointer bindings (&f.pathVal, &f.pick, &f.d…) must stay pointed at
// one stable allocation across Update copies.
type importFlow struct {
	st    *store.Store
	state importState
	form  *huh.Form

	pathVal   string               // path form input
	cands     []importer.Candidate // vault-conflict-free (PlanImport) candidates offered at pick
	pick      []string             // chosen candidate names (multiselect values = names)
	skipN     int                  // vault-conflict skips at pick time (result 跳过 count)
	matchWarn bool                 // config contains Match blocks (inherited values may diverge)

	supp     []*models.Server  // supplement queue: every imported server (Role empty → ⚠)
	suppIdx  int               // position in supp
	srv      *models.Server    // current supplement target
	d        *serverDraft      // current supplement draft
	condPass bool              // target is credential-less → offer 密码（现在设置）
	condKey  bool              // target carries needs-passphrase → offer 密钥口令
	suppKeys map[string][]byte // candidate name → in-memory key bytes (condKey re-mint input)

	importedN int
	report    []string
	err       error
}

// importOutcome is one successful insert as reported by the batch cmd: the
// fully-populated server row (id + backfilled credential ids + tags) plus the
// raw key bytes when a key was read — the msg carries both so the loop never
// depends on state written from the cmd goroutine.
type importOutcome struct {
	srv *models.Server
	key []byte // non-nil iff a key file was read (present for the passphrase re-mint)
}

type importDoneMsg struct {
	outcomes []importOutcome
	report   []string
}

// newImportFlow starts at the path form, prefilled with ~/.ssh/config.
func newImportFlow(st *store.Store) *importFlow {
	f := &importFlow{st: st, suppKeys: map[string][]byte{}}
	f.pathVal = defaultConfigPath()
	f.openPathForm()
	return f
}

// defaultConfigPath expands ~/.ssh/config for the path form's prefill.
func defaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".ssh", "config")
}

func (f *importFlow) Title() string { return "导入 ssh config" }
func (f *importFlow) Init() tea.Cmd { return f.form.Init() }

// currentCmd re-inits the CURRENT form (or returns nil on the result screen,
// where there is none) — the common tail of every state transition that may
// either open the next form or finish the loop.
func (f *importFlow) currentCmd() tea.Cmd {
	if f.form == nil {
		return nil
	}
	return f.form.Init()
}

// pathForm (re)builds the file-path form. Rebuilt on a parse error the input
// is preserved: the field stays bound to f.pathVal.
func (f *importFlow) pathForm() *huh.Form {
	return huh.NewForm(huh.NewGroup(
		huh.NewInput().Title("ssh config 路径").Value(&f.pathVal).Validate(nonEmpty),
	))
}

func (f *importFlow) openPathForm() {
	f.state = statePathForm
	f.form = f.pathForm()
}

// osUserName is the ssh_config User fallback (user.Current → USERNAME → USER,
// the same ladder as the CLI's currentUserName; a local copy because the cli
// helper is unexported and the importer package stays pure).
func osUserName() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if v := os.Getenv("USERNAME"); v != "" {
		return v
	}
	return os.Getenv("USER")
}

// afterPathForm runs on path-form submit: parse the config, filter vault
// conflicts via PlanImport, then either open the multiselect (candidates
// remain) or land on the result screen (nothing importable). A parse failure
// reopens the path form with the error visible — input preserved.
func (f *importFlow) afterPathForm() (tea.Model, tea.Cmd) {
	path := strings.TrimSpace(f.pathVal)
	res, err := importer.Parse(path, osUserName())
	if err != nil {
		f.err = fmt.Errorf("读取/解析 config：%w", err)
		f.form = f.pathForm()
		return f, f.form.Init()
	}
	existing, err := f.st.ListServers()
	if err != nil {
		f.err = err
		f.form = f.pathForm()
		return f, f.form.Init()
	}
	f.err = nil
	f.matchWarn = res.MatchWarning
	ex := make([]importer.ExistingServer, len(existing))
	for i, e := range existing {
		ex[i] = importer.ExistingServer{Name: e.Name, Host: e.Host, Port: e.Port, User: e.User}
	}
	toImport, skips := importer.PlanImport(res.Candidates, ex)
	f.cands, f.skipN = toImport, len(skips)
	if len(toImport) == 0 {
		f.report = append(f.report, "无可导入候选（同名 / 同 host:port:user 均与 vault 冲突）")
		f.finishResult()
		return f, nil
	}
	f.pick = nil
	f.state = statePick
	f.form = f.pickForm()
	return f, f.form.Init()
}

// pickForm offers the conflict-free candidates, ALL preselected. Values are
// candidate names — Parse guarantees they are unique within one batch.
func (f *importFlow) pickForm() *huh.Form {
	opts := make([]huh.Option[string], len(f.cands))
	for i, c := range f.cands {
		opts[i] = huh.NewOption(fmt.Sprintf("%s — %s@%s:%d", c.Name, c.User, c.Host, c.Port), c.Name).Selected(true)
	}
	return huh.NewForm(huh.NewGroup(
		huh.NewMultiSelect[string]().Title("选择要导入的服务器（空格勾选，回车提交）").
			Options(opts...).Value(&f.pick),
	))
}

// startBatch runs the whole import in ONE tea.Cmd (per-candidate failures are
// recorded, never abort the batch): PickKey for the credential (first
// readable key, sha256 batch dedup, needs-passphrase detection — the shared
// seam), one AddServerWithCredentials transaction per server, the
// needs-passphrase TAG on encrypted keys (the CLI import writes the same
// literal — the ⚠ sort + `!` filter key on it). The picked subset keeps
// config order, not multiselect toggle order.
func (f *importFlow) startBatch() tea.Cmd {
	f.state = stateImporting
	picked := make(map[string]bool, len(f.pick))
	for _, n := range f.pick {
		picked[n] = true
	}
	cands := make([]importer.Candidate, 0, len(f.pick))
	for _, c := range f.cands {
		if picked[c.Name] {
			cands = append(cands, c)
		}
	}
	configDir := filepath.Dir(strings.TrimSpace(f.pathVal))
	st := f.st
	return func() tea.Msg {
		keyIDs := map[[32]byte]string{} // sha256(key) -> minted credential id
		var outcomes []importOutcome
		var report []string
		for _, cand := range cands {
			pick := importer.PickKey(cand, configDir, keyIDs)
			srv := &models.Server{
				Name: cand.Name, Host: cand.Host, Port: cand.Port, User: cand.User,
			}
			if pick.NeedsPass {
				srv.Tags = []string{"needs-passphrase"} // supplement loop's passphrase step removes it
			}
			id, err := st.AddServerWithCredentials(srv, pick.Cred, nil)
			if err != nil {
				report = append(report, fmt.Sprintf("%-20s FAILED: %v", cand.Name, err))
				continue // single-candidate failure does not abort the batch
			}
			if pick.Minted && pick.Cred != nil && pick.Cred.ID != "" {
				keyIDs[pick.Sum] = pick.Cred.ID // later candidates reuse this row
			}
			srv.ID = id // insertServerTx generates but does not write back the id
			note := "已导入（含私钥）"
			if pick.Cred == nil {
				note = "已导入（⚠ 无凭据，待补）"
			} else if pick.NeedsPass {
				note = "已导入（⚠ 缺密钥口令，待补）"
			}
			report = append(report, fmt.Sprintf("%-20s %s", cand.Name, note))
			var key []byte
			if pick.KeyBytes != nil {
				key = append([]byte(nil), pick.KeyBytes...)
			}
			outcomes = append(outcomes, importOutcome{srv: srv, key: key})
		}
		return importDoneMsg{outcomes: outcomes, report: report}
	}
}

// afterImport receives the batch outcome and opens the supplement loop. Every
// imported server joins the queue: imports carry no Role, so every one of
// them is ⚠ until supplemented (or skipped and left for `!` + `e` later).
func (f *importFlow) afterImport(m importDoneMsg) (tea.Model, tea.Cmd) {
	f.report = append(f.report, m.report...)
	f.importedN = len(m.outcomes)
	f.supp = nil
	for _, o := range m.outcomes {
		f.supp = append(f.supp, o.srv)
		if o.key != nil {
			f.suppKeys[o.srv.Name] = o.key
		}
	}
	if len(f.supp) == 0 {
		f.finishResult()
		return f, nil
	}
	f.suppIdx = 0
	return f, f.openSupplement()
}

// openSupplement builds the form for supp[suppIdx]: structured fields + sudo
// password + at most one conditional secret (condPass = credential-less
// target; condKey = needs-passphrase target — mutually exclusive by
// construction, a condKey server was imported WITH its key).
func (f *importFlow) openSupplement() tea.Cmd {
	f.srv = f.supp[f.suppIdx]
	f.d = prefill(f.srv)
	f.condPass = f.srv.CredentialID == ""
	f.condKey = hasTag(f.srv, "needs-passphrase")
	f.state = stateSupplement
	f.form = supplementForm(f.d, f.condPass, f.condKey)
	return f.form.Init()
}

// supplementFields builds the supplement form's field list: the structured
// fields, sudo password, and at most one conditional masked secret (condPass
// = credential-less target → 密码（现在设置）; condKey = needs-passphrase
// target → 密钥口令（补全加密私钥）; empty conditional input = leave for
// later, the ⚠ stays). Returned as its own seam so tests can pin the exact
// field set per target kind.
func supplementFields(d *serverDraft, condPass, condKey bool) []huh.Field {
	fields := append(structuredFields(d), sudoPasswordField(d))
	if condPass {
		fields = append(fields, huh.NewInput().Title("密码（现在设置）").
			Value(&d.Password).EchoMode(huh.EchoModePassword))
	}
	if condKey {
		fields = append(fields, huh.NewInput().Title("密钥口令（补全加密私钥）").
			Value(&d.KeyPass).EchoMode(huh.EchoModePassword))
	}
	return fields
}

// supplementForm wraps the supplement fields in a single-group huh form.
func supplementForm(d *serverDraft, condPass, condKey bool) *huh.Form {
	return huh.NewForm(huh.NewGroup(supplementFields(d, condPass, condKey)...))
}

// submitSupplement persists the draft via UpdateServerWithCredentials (nil
// cred/sudo keep the existing rows) and advances. The passphrase case re-mints
// the private-key credential from the IN-MEMORY key bytes the import retained
// (no disk re-read) and drops the needs-passphrase tag; the password case
// mints a password credential for a credential-less import. A store failure
// reopens the SAME form on the SAME draft — typed values survive.
func (f *importFlow) submitSupplement() tea.Cmd {
	clearTag := f.condKey && f.d.KeyPass != ""
	tags := f.srv.Tags
	if clearTag {
		tags = dropTag(tags, "needs-passphrase")
	}
	srv := &models.Server{
		ID:   f.srv.ID,
		Name: strings.TrimSpace(f.d.Name), Host: strings.TrimSpace(f.d.Host),
		Port: f.d.Port, User: strings.TrimSpace(f.d.User),
		Description: strings.TrimSpace(f.d.Description), Location: strings.TrimSpace(f.d.Location),
		Hardware: strings.TrimSpace(f.d.Hardware), Services: strings.TrimSpace(f.d.Services),
		Role: strings.TrimSpace(f.d.Role), Caveats: strings.TrimSpace(f.d.Caveats),
		Tags: tags,
	}
	var cred, sudo *models.Credential
	switch {
	case f.condPass && f.d.Password != "":
		cred = &models.Credential{Type: models.CredPassword, Secret: []byte(f.d.Password)}
	case clearTag:
		key := f.suppKeys[f.srv.Name]
		if key == nil {
			// Unreachable by construction (every needs-passphrase import
			// carries its key bytes in importOutcome) — but minting a
			// credential with an EMPTY secret must never happen silently.
			f.err = errors.New("密钥内容缺失，无法补全口令——请改用 servers edit --key-path 重设")
			f.form = supplementForm(f.d, f.condPass, f.condKey)
			return f.form.Init()
		}
		cred = &models.Credential{
			Type:       models.CredPrivateKey,
			Secret:     key, // in-memory key bytes retained at import
			Passphrase: []byte(f.d.KeyPass),
		}
	}
	if f.d.SudoPassword != "" {
		sudo = &models.Credential{Type: models.CredPassword, Secret: []byte(f.d.SudoPassword)}
	}
	if err := f.st.UpdateServerWithCredentials(srv, cred, sudo); err != nil {
		f.err = err
		f.form = supplementForm(f.d, f.condPass, f.condKey)
		return f.form.Init()
	}
	f.err = nil
	f.supp[f.suppIdx] = srv // keep the queue snapshot accurate for result counts
	f.nextSupplement()
	return f.currentCmd()
}

// nextSupplement advances to the next queue entry (Esc/skip and submit both
// land here); past the end it lands on the result screen.
func (f *importFlow) nextSupplement() {
	f.suppIdx++
	if f.suppIdx >= len(f.supp) {
		f.finishResult()
		return
	}
	f.openSupplement()
}

// finishResult closes the loop half of the state machine.
func (f *importFlow) finishResult() {
	f.state = stateResult
	f.form, f.d, f.srv = nil, nil, nil
}

// pendingCount counts imported servers still ⚠ — supp entries are snapshotted
// after each supplement submit (tags dropped, role filled, credential ids
// backfilled), so this stays live through the loop.
func (f *importFlow) pendingCount() int {
	n := 0
	for _, s := range f.supp {
		if serverNeedsAttention(s) {
			n++
		}
	}
	return n
}

// dismissCmd is what runs after the result screen closes: a plain
// actionDoneMsg refreshes the App pages (refetchPages keeps the ⚠ sort and
// the `!` filter) and leaves the 导入完成 status line.
func (f *importFlow) dismissCmd() tea.Cmd {
	return func() tea.Msg {
		return actionDoneMsg{desc: fmt.Sprintf("导入完成：%d 台（待补 %d）", f.importedN, f.pendingCount())}
	}
}

func (f *importFlow) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case tea.KeyPressMsg:
		k := m.Key()
		switch f.state {
		case stateResult:
			return f, func() tea.Msg { return formDoneMsg{after: f.dismissCmd()} }
		case stateImporting:
			// In flight: swallow everything except Ctrl+C (a hard exit must
			// stay possible while the batch runs).
			if k.Code == 'c' && k.Mod == tea.ModCtrl {
				return f, tea.Quit
			}
			return f, nil
		case stateSupplement:
			// Esc skips THIS server (keeps its ⚠ — the later `!`+`e` path);
			// q ends the whole loop, straight to the result. Bare-q means a
			// "q" cannot be typed into the supplement inputs — the same
			// trade-off the first-run wizard makes with its global q.
			if k.Code == tea.KeyEsc {
				f.nextSupplement()
				return f, f.currentCmd()
			}
			if k.Text == "q" {
				f.finishResult()
				return f, nil
			}
		default: // statePathForm, statePick
			if k.Code == tea.KeyEsc {
				return f, func() tea.Msg { return formDoneMsg{aborted: true} }
			}
		}
		if f.form == nil {
			return f, nil
		}
		fm, cmd := f.form.Update(msg)
		if nf, ok := fm.(*huh.Form); ok {
			f.form = nf
		}
		if f.form.State == huh.StateAborted { // Ctrl+C inside huh: same as Esc per state
			switch f.state {
			case stateSupplement:
				f.nextSupplement()
				return f, f.currentCmd()
			default:
				return f, func() tea.Msg { return formDoneMsg{aborted: true} }
			}
		}
		if f.form.State != huh.StateCompleted {
			return f, cmd
		}
		switch f.state {
		case statePathForm:
			return f.afterPathForm()
		case statePick:
			return f, f.startBatch()
		case stateSupplement:
			return f, f.submitSupplement()
		}
		return f, cmd
	case importDoneMsg:
		return f.afterImport(m)
	}
	return f, nil
}

func (f *importFlow) View() tea.View {
	var b strings.Builder
	switch f.state {
	case statePathForm:
		b.WriteString(titleStyle.Render(" 导入 ssh config ") + "\n\n")
		b.WriteString(f.form.View() + "\n\n")
		b.WriteString(footerStyle.Render("Esc 取消") + "\n")
	case statePick:
		b.WriteString(titleStyle.Render(" 导入 ssh config — 选择候选 ") + "\n\n")
		if f.matchWarn {
			b.WriteString(warnStyle.Render("⚠ config 含 Match 块：继承值可能与真 ssh 不一致") + "\n\n")
		}
		if f.skipN > 0 {
			b.WriteString(fmt.Sprintf("已自动排除 vault 冲突 %d 台（同名 / 同 host:port:user）\n\n", f.skipN))
		}
		b.WriteString(f.form.View() + "\n\n")
		b.WriteString(footerStyle.Render("Esc 取消") + "\n")
	case stateImporting:
		b.WriteString(titleStyle.Render(" 导入 ssh config ") + "\n\n")
		b.WriteString("正在导入…\n")
	case stateSupplement:
		b.WriteString(titleStyle.Render(fmt.Sprintf(" 补全 %d/%d ", f.suppIdx+1, len(f.supp))) + "\n\n")
		b.WriteString(fmt.Sprintf("%s @ %s:%d (%s)\n\n", f.srv.Name, f.srv.Host, f.srv.Port, f.srv.User))
		b.WriteString(f.form.View() + "\n\n")
		b.WriteString(footerStyle.Render("Esc 跳过（保留 ⚠）/ q 结束补全 / 回车提交") + "\n")
	case stateResult:
		b.WriteString(titleStyle.Render(" 导入结果 ") + "\n\n")
		b.WriteString(fmt.Sprintf("导入 %d / 跳过 %d / 待补 %d\n\n", f.importedN, f.skipN, f.pendingCount()))
		for _, line := range f.report {
			b.WriteString(line + "\n")
		}
		if n := f.pendingCount(); n > 0 {
			b.WriteString("\n" + footerStyle.Render(fmt.Sprintf("⚠ 列表按 ! 过滤后逐台 e 编辑（%d 台待补）", n)) + "\n")
		}
		b.WriteString(footerStyle.Render("任意键关闭") + "\n")
	}
	if f.err != nil {
		b.WriteString(errStyle.Render("✗ "+f.err.Error()) + "\n")
	}
	return tea.NewView(b.String())
}
