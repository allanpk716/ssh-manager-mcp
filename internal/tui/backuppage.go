// backuppage.go is the broker console's sixth page 「备份」 (Plan 49 票 03):
// a status area (config summary, newest-first *.sme list with the 25-hour
// freshness warning, read-only scheduled-task probe) plus three actions —
// [b] run a backup now, [v] verify the newest one, [e] edit backup.json. All
// three share the CLI's exact chain (same validation / create / verify) via
// the BackupEntry hook: tui cannot import internal/cli (cli imports tui for
// the `tui` command — import cycle), so the cli package injects the entry
// points through SetBackupEntry — the same bridge pattern as
// SetServeInstaller (Plan 19 T4). The page runs the actions as background
// tea.Cmds and never touches vault data or the scheduler itself.
package tui

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"ssh-manager-mcp/internal/paths"
)

// BackupConfig is the console's read-only view of a validated backup.json
// (filled by the cli side; the fields mirror the CLI's backupConfig).
type BackupConfig struct {
	Dir            string
	Keep           int // 0 = 不轮转
	PassphraseFile string
}

// BackupEntry is the backup-chain entry points the backup page shares with
// the CLI — the page shows and writes exactly what `backup
// create/verify/--config` would. The cli package wires implementations via
// SetBackupEntry (import cycle — see SetServeInstaller). LoadConfig returns
// (nil, err) for a defective config file; the page renders the guide state.
type BackupEntry struct {
	LoadConfig     func(path string) (*BackupConfig, error)
	ValidateValues func(dir, passphraseFile string) error
	SaveConfig     func(configPath, dir string, keep int, passphraseFile string) error
	Create         func(configPath string, stdout, stderr io.Writer) error
	Verify         func(file, passphraseSrc string, stdout io.Writer) error
}

// backupEntry is the wiring point (zero value = not wired, only reachable
// outside the real CLI — the page then fails loud with 备份功能未初始化
// instead of silently skipping).
var backupEntry BackupEntry

// SetBackupEntry wires the CLI's backup chain into the console's backup page.
func SetBackupEntry(e BackupEntry) { backupEntry = e }

// backupConfigPath resolves the console's backup.json: the vault directory's
// backup.json (spec: 配置文件放 vault 目录, 计划任务/TUI/手工共用). Package
// var = test seam (newTestApp points it at a temp path so no test touches the
// real vault dir).
var backupConfigPath = func() string {
	dir, err := paths.VaultDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "backup.json")
}

const (
	// backupFreshnessBudget is the freshness threshold (spec D6): the newest
	// *.sme older than this renders the warning state.
	backupFreshnessBudget = 25 * time.Hour
	// backupTaskName is the Windows scheduled task the deployment installs;
	// the page only PROBES its presence (never installs or edits it).
	backupTaskName = "sshmgr-backup"
	// backupListLimit caps the visible rows; the total stays in the detail.
	backupListLimit = 10
)

type backupPage struct {
	configPath     string        // the backup.json this console reads and edits
	cfg            *BackupConfig // nil = guide state (config missing / defective)
	cfgErr         error
	files          []smeFile // newest first
	stale          bool      // the newest file exceeds the freshness budget
	schedInstalled bool
	schedProbed    bool
	entry          BackupEntry // copied from backupEntry at construction; tests stub it
	probeFn        func() (installed, probed bool)
	panelList
}

// smeFile is one *.sme backup in the configured dir.
type smeFile struct {
	name    string
	path    string
	modTime time.Time
}

func newBackupPage(configPath string) *backupPage {
	p := &backupPage{
		configPath: configPath,
		entry:      backupEntry,
		probeFn:    defaultScheduleProbe,
	}
	p.reload()
	p.panelList = newPanelList("备份")
	p.syncList()
	return p
}

// reload re-reads the config, the dir listing, and the schedule probe.
// FetchAll rebuilds the page on every refresh, so a just-finished [b] shows
// up on the post-action refetch without extra plumbing.
func (p *backupPage) reload() {
	p.cfg, p.cfgErr = nil, nil
	p.files, p.stale = nil, false
	switch {
	case !p.entryReady():
		p.cfgErr = errors.New("备份功能未初始化(仅 CLI 入口可用)")
	case p.configPath == "":
		p.cfgErr = errors.New("无法定位 vault 目录")
	default:
		p.cfg, p.cfgErr = p.entry.LoadConfig(p.configPath)
	}
	if p.cfg != nil {
		p.files = listSmeFiles(p.cfg.Dir)
		if len(p.files) > 0 {
			p.stale = time.Since(p.files[0].modTime) > backupFreshnessBudget
		}
	}
	p.schedInstalled, p.schedProbed = p.probeFn()
}

// entryReady reports whether the CLI chain was wired. A nil field anywhere
// means the page runs outside the real CLI.
func (p *backupPage) entryReady() bool {
	e := p.entry
	return e.LoadConfig != nil && e.ValidateValues != nil && e.SaveConfig != nil &&
		e.Create != nil && e.Verify != nil
}

// listSmeFiles lists dir's *.sme backups newest-first (mod time desc, name
// desc as the tiebreak). A missing/unreadable dir yields nil — the page
// renders 尚无备份 rather than erroring (the dir may not exist yet).
func listSmeFiles(dir string) []smeFile {
	matches, err := filepath.Glob(filepath.Join(dir, "*.sme"))
	if err != nil {
		return nil
	}
	out := make([]smeFile, 0, len(matches))
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil && !fi.IsDir() {
			out = append(out, smeFile{name: filepath.Base(m), path: m, modTime: fi.ModTime()})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].modTime.Equal(out[j].modTime) {
			return out[i].modTime.After(out[j].modTime)
		}
		return out[i].name > out[j].name
	})
	return out
}

// defaultScheduleProbe is the READ-ONLY presence probe of the sshmgr-backup
// scheduled task (schtasks /Query only — the page never performs any write
// operation on the scheduler). Non-Windows reports 未探测 (probed=false).
func defaultScheduleProbe() (installed, probed bool) {
	if runtime.GOOS != "windows" {
		return false, false
	}
	return exec.Command("schtasks", "/Query", "/TN", backupTaskName).Run() == nil, true
}

func (p *backupPage) Title() string { return "备份" }

// plainItem is a single static row (the guide state's guidance line).
type plainItem string

func (i plainItem) FilterValue() string { return string(i) }
func (i plainItem) Title() string       { return string(i) }
func (i plainItem) Description() string { return "" }

// smeItem adapts one backup file to the list panel. stale marks the NEWEST
// file's freshness breach (⚠ prefix — the same warn affordance as the
// servers page's ⚠ view).
type smeItem struct {
	f     smeFile
	stale bool
}

func (i smeItem) FilterValue() string { return i.f.name }
func (i smeItem) Title() string {
	if i.stale {
		return "⚠ " + i.f.name
	}
	return i.f.name
}
func (i smeItem) Description() string { return i.f.modTime.Format("2006-01-02 15:04") }

func (p *backupPage) syncList() {
	items := make([]list.Item, 0, 1)
	if p.cfg == nil {
		items = append(items, plainItem("尚未配置备份"))
		p.setListItems(items, len(items))
		return
	}
	visible := p.files
	if len(visible) > backupListLimit {
		visible = visible[:backupListLimit]
	}
	items = make([]list.Item, len(visible))
	for i, f := range visible {
		items[i] = smeItem{f: f, stale: i == 0 && p.stale}
	}
	p.setListItems(items, len(items))
}

func (p *backupPage) Rows() []string {
	if p.cfg == nil {
		return []string{"尚未配置备份"}
	}
	visible := p.files
	if len(visible) > backupListLimit {
		visible = visible[:backupListLimit]
	}
	out := make([]string, len(visible))
	for i, f := range visible {
		if i == 0 && p.stale {
			out[i] = "⚠ " + f.name
		} else {
			out[i] = f.name
		}
	}
	return out
}

func (p *backupPage) Detail() string {
	if p.cfg == nil {
		reason := "文件不存在"
		if p.cfgErr != nil {
			reason = p.cfgErr.Error()
		}
		return fmt.Sprintf("未找到有效的备份配置。\n\n配置路径  %s\n原因      %s\n\n按 [e] 填写:备份目录 / 保留份数 / 口令文件路径。",
			p.configPath, reason)
	}
	sched := "sshmgr-backup:未探测(仅 Windows 支持探测)"
	if p.schedProbed {
		if p.schedInstalled {
			sched = "sshmgr-backup:已安装"
		} else {
			sched = "sshmgr-backup:未安装"
		}
	}
	b := &strings.Builder{}
	fmt.Fprintf(b, "配置路径  %s\n备份目录  %s\n保留份数  %d(0=不轮转)\n口令文件  %s\n计划任务  %s\n备份文件  共 %d 份",
		p.configPath, p.cfg.Dir, p.cfg.Keep, p.cfg.PassphraseFile, sched, len(p.files))
	if len(p.files) > backupListLimit {
		fmt.Fprintf(b, "(显示最近 %d 份)", backupListLimit)
	}
	if len(p.files) == 0 {
		b.WriteString("\n最新备份  —(尚无 .sme 备份)\n新鲜度    尚无备份可判定")
	} else {
		newest := p.files[0]
		fmt.Fprintf(b, "\n最新备份  %s(%s)", newest.name, newest.modTime.Format("2006-01-02 15:04"))
		if p.stale {
			b.WriteString("\n新鲜度    ⚠ 超过 25 小时 — 检查计划任务是否在跑")
		} else {
			b.WriteString("\n新鲜度    25 小时内")
		}
	}
	return b.String()
}

// Render draws the desktop-style body fitted to the terminal (shared panel
// machinery — see panels.go).
func (p *backupPage) Render(width, height int) string {
	return renderPanel(&p.list, p.Detail(), width, height)
}

// ---------------------------------------------------------------------------
// [e] 编辑配置 — huh form + the three pre-save checks
// ---------------------------------------------------------------------------

// backupConfigDraft is the [e] form's state; KeepStr is the string mirror of
// the int keep (huh Input is string-bound — same pattern as portField).
type backupConfigDraft struct {
	Dir            string
	KeepStr        string
	PassphraseFile string
}

// newConfigDraft prefills the form from the current config; the guide state
// falls back to the CLI's built-in default keep (7).
func (p *backupPage) newConfigDraft() *backupConfigDraft {
	d := &backupConfigDraft{KeepStr: "7"}
	if p.cfg != nil {
		d.Dir = p.cfg.Dir
		d.KeepStr = strconv.Itoa(p.cfg.Keep)
		d.PassphraseFile = p.cfg.PassphraseFile
	}
	return d
}

func newBackupConfigForm(d *backupConfigDraft) *huh.Form {
	return huh.NewForm(huh.NewGroup(
		huh.NewInput().Title("备份目录（绝对路径）").Value(&d.Dir).Validate(nonEmpty),
		huh.NewInput().Title("保留份数（0=不轮转）").Value(&d.KeepStr).Validate(nonEmpty),
		huh.NewInput().Title("口令文件路径（绝对路径）").Value(&d.PassphraseFile).Validate(nonEmpty),
	))
}

// parseKeep converts the form's keep mirror; 0 = 不轮转 (the CLI's semantics).
func parseKeep(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, errors.New("保留份数必须是整数")
	}
	if n < 0 {
		return 0, errors.New("保留份数不能为负（0=不轮转）")
	}
	return n, nil
}

// saveConfig runs the three pre-save checks IN ORDER — ① both paths
// absolute, ② the passphrase file not inside the backup dir subtree (①+② are
// the CLI's own validators), ③ with existing *.sme in the FORM's dir, the
// passphrase must still decrypt the newest one (the same generation rule
// `backup create` enforces before writing) — and only then writes
// backup.json. Any failure blocks the save with a plain-language reason; the
// returned string is the ✓ status line.
func (p *backupPage) saveConfig(d *backupConfigDraft) (string, error) {
	if !p.entryReady() {
		return "", errors.New("备份功能未初始化(仅 CLI 入口可用)")
	}
	dir := strings.TrimSpace(d.Dir)
	passFile := strings.TrimSpace(d.PassphraseFile)
	keep, err := parseKeep(d.KeepStr)
	if err != nil {
		return "", err
	}
	// checks ①+② — the CLI's own validators (same wording as --config loading)
	if err := p.entry.ValidateValues(dir, passFile); err != nil {
		return "", fmt.Errorf("配置未通过校验:%w", err)
	}
	// check ③ — generation check on the FORM's dir: an existing encrypted
	// lineage must still open with this passphrase, or saving would set up a
	// silent fork from an unreadable lineage.
	if latest, ok := latestSmeIn(dir); ok {
		var buf bytes.Buffer
		if err := p.entry.Verify(latest, passFile, &buf); err != nil {
			return "", fmt.Errorf("口令试解密最新备份 %s 失败,已阻止保存(口令可能已更换,或最新文件损坏)", filepath.Base(latest))
		}
	}
	if err := p.entry.SaveConfig(p.configPath, dir, keep, passFile); err != nil {
		return "", err
	}
	return "已保存备份配置 " + p.configPath, nil
}

// latestSmeIn returns the newest *.sme path in dir — the generation check's
// target, evaluated on the FORM's dir at save time (not the page's list,
// which still shows the OLD dir).
func latestSmeIn(dir string) (string, bool) {
	files := listSmeFiles(dir)
	if len(files) == 0 {
		return "", false
	}
	return files[0].path, true
}

// saveConfigCmd wraps saveConfig as the form overlay's after-action; the
// actionDoneMsg lands the shared refetch so the page reflects the new config.
func (p *backupPage) saveConfigCmd(d *backupConfigDraft) tea.Cmd {
	return func() tea.Msg {
		desc, err := p.saveConfig(d)
		if err != nil {
			return errMsg{err}
		}
		return actionDoneMsg{desc: desc}
	}
}

// ---------------------------------------------------------------------------
// [b] 立即备份 / [v] 校验最新 — App-level dispatch (page actions)
// ---------------------------------------------------------------------------

// backupNow is the [b] action: the same create chain as
// `backup create --config <path>` (marker/.git guards, lock, generation
// check, atomic write, rotation) run OFF the event loop. The CLI's own
// output lands on the ✓ status line; a failure surfaces via errMsg (marker
// missing, generation fork, …) — the reason stays visible on the error line.
func (a *App) backupNow(bp *backupPage) tea.Cmd {
	if bp.cfg == nil {
		a.status = "尚未配置备份:先按 [e] 编辑配置"
		return nil
	}
	a.status = "备份进行中…"
	return func() tea.Msg {
		var buf bytes.Buffer
		if err := bp.entry.Create(bp.configPath, &buf, &buf); err != nil {
			return errMsg{err}
		}
		return actionDoneMsg{desc: backupDoneDesc(buf.String())}
	}
}

// backupDoneDesc folds the CLI create output into a status line: the last
// non-empty line carries the outcome ("wrote vault-….sme" / the skip notes).
func backupDoneDesc(out string) string {
	line := ""
	for _, l := range strings.Split(out, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			line = t
		}
	}
	if line == "" {
		return "备份完成"
	}
	return "备份完成:" + line
}

// backupVerifyLatest is the [v] action: decrypt + re-parse the newest .sme
// through the CLI's verify gates. A decrypt failure CANNOT distinguish a
// wrong passphrase from a corrupted file (GCM auth), so both report the one
// unified wording (spec F9 裁量).
func (a *App) backupVerifyLatest(bp *backupPage) tea.Cmd {
	if bp.cfg == nil {
		a.status = "尚未配置备份:先按 [e] 编辑配置"
		return nil
	}
	if len(bp.files) == 0 {
		a.status = "备份目录还没有 .sme 文件,无可校验"
		return nil
	}
	latest := bp.files[0]
	a.status = "正在校验最新备份…"
	return func() tea.Msg {
		var buf bytes.Buffer
		if err := bp.entry.Verify(latest.path, bp.cfg.PassphraseFile, &buf); err != nil {
			return errMsg{fmt.Errorf("解密失败:口令不符或文件损坏(%v)", err)}
		}
		return actionDoneMsg{desc: fmt.Sprintf("最新备份完好:%s(已试解密+重解析)", latest.name)}
	}
}

// backupKey dispatches the backup page's action keys (b/v/e) — the same
// per-page dispatch discipline as serversKey: letters that mean nothing here
// are silent no-ops, and e opens its overlay returning the Init cmd.
func (a *App) backupKey(k tea.Key) tea.Cmd {
	bp, _ := a.pages[pageBackup].(*backupPage)
	if bp == nil {
		return nil
	}
	switch k.Text {
	case "b":
		return a.backupNow(bp)
	case "v":
		return a.backupVerifyLatest(bp)
	case "e":
		d := bp.newConfigDraft()
		a.overlay = newFormOverlay("编辑备份配置", newBackupConfigForm(d), func() tea.Cmd {
			return bp.saveConfigCmd(d)
		})
	}
	if a.overlay == nil {
		return nil
	}
	return a.overlay.Init()
}
