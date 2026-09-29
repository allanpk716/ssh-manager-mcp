package tui

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"ssh-manager-mcp/internal/roles"
)

// backupTestEnv is a backupPage wired to an in-memory BackupEntry plus call
// recorders — the page logic is exercised exactly as the App drives it, with
// the CLI chain stubbed out (internal/tui tests cannot link internal/cli:
// cli imports tui). modTimes seeds that many fake .sme files into the config's
// backup dir, each stamped with the given mod time.
type backupTestEnv struct {
	bp  *backupPage
	dir string // the fixture config's backup dir
	pf  string // the fixture config's passphrase file

	// recorders / scripted outcomes
	saved          bool
	savedDir       string
	savedKeep      int
	savedPF        string
	createN        int
	createErr      error
	createOut      string
	verifyN        int
	verifyErr      error
	lastVerifyFile string
	lastVerifyPF   string
}

func newBackupTestEnv(t *testing.T, modTimes ...time.Time) *backupTestEnv {
	t.Helper()
	e := &backupTestEnv{dir: t.TempDir()}
	e.pf = filepath.Join(t.TempDir(), "backup.pass")
	for i, ts := range modTimes {
		name := fmt.Sprintf("vault-202609%02d-000000.sme", 10+i)
		p := filepath.Join(e.dir, name)
		if err := os.WriteFile(p, []byte("SSHMGRV1-fake"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, ts, ts); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &BackupConfig{Dir: e.dir, Keep: 3, PassphraseFile: e.pf}
	e.bp = newBackupPage(filepath.Join(t.TempDir(), "backup.json"))
	e.bp.entry = BackupEntry{
		LoadConfig: func(string) (*BackupConfig, error) { return cfg, nil },
		ValidateValues: func(dir, pf string) error {
			if !filepath.IsAbs(dir) {
				return errors.New("dir must be an absolute path")
			}
			if !filepath.IsAbs(pf) {
				return errors.New("passphrase_file must be an absolute path")
			}
			rel, rerr := filepath.Rel(dir, pf)
			if rerr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return errors.New("passphrase_file inside the backup dir")
			}
			return nil
		},
		SaveConfig: func(configPath, dir string, keep int, pf string) error {
			e.saved, e.savedDir, e.savedKeep, e.savedPF = true, dir, keep, pf
			return nil
		},
		Create: func(configPath string, stdout, stderr io.Writer) error {
			e.createN++
			if e.createErr != nil {
				return e.createErr
			}
			_, _ = io.WriteString(stdout, e.createOut)
			return nil
		},
		Verify: func(file, pf string, stdout io.Writer) error {
			e.verifyN++
			e.lastVerifyFile, e.lastVerifyPF = file, pf
			return e.verifyErr
		},
	}
	e.bp.probeFn = func() (installed, probed bool) { return true, true }
	e.bp.reload()
	e.bp.syncList()
	return e
}

// breakConfig flips the fixture into the guide state (config load fails).
func (e *backupTestEnv) breakConfig() {
	e.bp.entry.LoadConfig = func(string) (*BackupConfig, error) {
		return nil, errors.New("backup config: file not found: " + e.bp.configPath)
	}
	e.bp.reload()
	e.bp.syncList()
}

// TestBackupPage_SixPagesAndClientNeverBuildsBrokerArray pins the page-array
// shape (Plan 49 票 03: 5→6) and the role gate: the client launch dispatches
// to the client panel and never constructs the broker page array, so the
// sixth page exists only on the broker console.
func TestBackupPage_SixPagesAndClientNeverBuildsBrokerArray(t *testing.T) {
	a := newTestApp(t)
	if int(pageCount) != 6 {
		t.Fatalf("pageCount = %d, want 6 (备份 is the sixth page)", pageCount)
	}
	if _, ok := a.pages[pageBackup].(*backupPage); !ok {
		t.Fatalf("pages[pageBackup] must be a *backupPage, got %T", a.pages[pageBackup])
	}
	if got := launchTarget(roles.Launch{Kind: roles.LaunchClient}); got != "client" {
		t.Fatalf("client launch target = %q, want client (the broker pages are never built)", got)
	}
}

func TestBackupPage_GuideStateOnlyEditAvailable(t *testing.T) {
	env := newBackupTestEnv(t)
	env.breakConfig()
	bp := env.bp
	if bp.cfg != nil {
		t.Fatal("premise: a failed config load must yield the guide state")
	}
	if rows := bp.Rows(); len(rows) != 1 || !strings.Contains(rows[0], "尚未配置") {
		t.Fatalf("guide rows: %v", rows)
	}
	if d := bp.Detail(); !strings.Contains(d, "未找到有效的备份配置") || !strings.Contains(d, "[e]") {
		t.Fatalf("guide detail: %s", d)
	}

	a := newTestApp(t)
	a.pages[pageBackup] = bp
	a.page = pageBackup
	m, cmd := a.Update(tea.KeyPressMsg{Code: 'b', Text: "b"})
	got := m.(App)
	if cmd != nil {
		t.Fatal("b must be a no-op in the guide state")
	}
	if !strings.Contains(got.status, "尚未配置") || !strings.Contains(got.status, "[e]") {
		t.Fatalf("b must point at [e] in the guide state, status: %q", got.status)
	}
	m, cmd = got.Update(tea.KeyPressMsg{Code: 'v', Text: "v"})
	got = m.(App)
	if cmd != nil {
		t.Fatal("v must be a no-op in the guide state")
	}
	// footer advertises only [e] while unconfigured
	if f := got.footer(); !strings.Contains(f, "[e]") || strings.Contains(f, "[b]") || strings.Contains(f, "[v]") {
		t.Fatalf("guide footer must advertise only [e]: %q", f)
	}
	m, cmd = got.Update(tea.KeyPressMsg{Code: 'e', Text: "e"})
	got = m.(App)
	if got.overlay == nil || got.overlay.Title() != "编辑备份配置" {
		t.Fatalf("e must open the config form, got %v", got.overlay)
	}
	if cmd == nil {
		t.Fatal("e must return the opened overlay's Init cmd")
	}
}

func TestBackupPage_StatusAreaListFreshnessProbe(t *testing.T) {
	now := time.Now()
	// three files; mod times chosen so the NEWEST is 26h old (over the 25h
	// freshness budget → the ⚠ warn state on the first row).
	env := newBackupTestEnv(t, now.Add(-30*time.Hour), now.Add(-49*time.Hour), now.Add(-26*time.Hour))
	bp := env.bp
	rows := bp.Rows()
	if len(rows) != 3 {
		t.Fatalf("rows: %v", rows)
	}
	newest := rows[0]
	if !strings.HasPrefix(newest, "⚠ ") || !strings.Contains(newest, "vault-20260912-000000.sme") {
		t.Fatalf("newest row must carry the stale ⚠ prefix, got %q (rows %v)", newest, rows)
	}
	if !strings.Contains(rows[1], "vault-20260910-000000.sme") || !strings.Contains(rows[2], "vault-20260911-000000.sme") {
		t.Fatalf("rows must be newest-first by mod time: %v", rows)
	}
	d := bp.Detail()
	for _, want := range []string{
		"共 3 份",
		"保留份数  3",
		env.pf,
		env.dir,
		"sshmgr-backup:已安装",
		"超过 25 小时",
	} {
		if !strings.Contains(d, want) {
			t.Fatalf("detail missing %q:\n%s", want, d)
		}
	}
	// a fresh newest file clears the warn state
	env2 := newBackupTestEnv(t, now.Add(-2*time.Hour))
	if r := env2.bp.Rows()[0]; strings.HasPrefix(r, "⚠ ") {
		t.Fatalf("a 2h-old newest backup must not warn: %q", r)
	}
	if d := env2.bp.Detail(); !strings.Contains(d, "25 小时内") {
		t.Fatalf("fresh detail:\n%s", d)
	}
	// more than 10 files: rows cap at 10, the total stays in the detail
	times := make([]time.Time, 12)
	for i := range times {
		times[i] = now.Add(-time.Duration(i+1) * time.Hour)
	}
	env3 := newBackupTestEnv(t, times...)
	if rows := env3.bp.Rows(); len(rows) != 10 {
		t.Fatalf("rows must cap at 10, got %d", len(rows))
	}
	if d := env3.bp.Detail(); !strings.Contains(d, "共 12 份") || !strings.Contains(d, "显示最近 10 份") {
		t.Fatalf("detail must show the total and the cap:\n%s", d)
	}
}

func TestBackupPage_BackupNowRunsCliChainAndReports(t *testing.T) {
	env := newBackupTestEnv(t)
	a := newTestApp(t)
	a.pages[pageBackup] = env.bp
	a.page = pageBackup
	env.createOut = "wrote vault-20260929-120000.sme\n"

	m, cmd := a.Update(tea.KeyPressMsg{Code: 'b', Text: "b"})
	got := m.(App)
	if cmd == nil {
		t.Fatal("b must return the background create cmd")
	}
	if got.status != "备份进行中…" {
		t.Fatalf("b must show the in-flight status, got %q", got.status)
	}
	msg := cmd()
	ad, ok := msg.(actionDoneMsg)
	if !ok {
		t.Fatalf("create must report actionDoneMsg, got %T", msg)
	}
	if env.createN != 1 {
		t.Fatalf("create must run exactly once, ran %d", env.createN)
	}
	if !strings.Contains(ad.desc, "备份完成") || !strings.Contains(ad.desc, "vault-20260929-120000.sme") {
		t.Fatalf("status desc: %q", ad.desc)
	}

	// failure branch: the reason (marker missing / generation fork / …) must
	// reach the error line through errMsg.
	env.createErr = errors.New("marker file .ssh-manager-backup-marker not found in --dir")
	m2, cmd2 := got.Update(tea.KeyPressMsg{Code: 'b', Text: "b"})
	msg2 := cmd2()
	em, ok := msg2.(errMsg)
	if !ok {
		t.Fatalf("a failed create must report errMsg, got %T", msg2)
	}
	m3, _ := m2.(App).Update(em)
	if got3 := m3.(App); got3.err == nil || !strings.Contains(got3.err.Error(), "marker") {
		t.Fatalf("the failure reason must be visible, err=%v", got3.err)
	}
}

func TestBackupPage_VerifyLatestReportsOkAndFailure(t *testing.T) {
	now := time.Now()
	env := newBackupTestEnv(t, now.Add(-26*time.Hour), now.Add(-2*time.Hour))
	a := newTestApp(t)
	a.pages[pageBackup] = env.bp
	a.page = pageBackup

	m, cmd := a.Update(tea.KeyPressMsg{Code: 'v', Text: "v"})
	got := m.(App)
	if cmd == nil {
		t.Fatal("v must return the verify cmd")
	}
	msg := cmd()
	ad, ok := msg.(actionDoneMsg)
	if !ok || !strings.Contains(ad.desc, "完好") {
		t.Fatalf("verify ok must report 完好, got %v / %q", ok, ad.desc)
	}
	// the target is the NEWEST file, verified with the config's passphrase
	if filepath.Base(env.lastVerifyFile) != "vault-20260911-000000.sme" {
		t.Fatalf("v must verify the newest file, got %q", env.lastVerifyFile)
	}
	if env.lastVerifyPF != env.pf {
		t.Fatalf("v must use the config's passphrase file, got %q", env.lastVerifyPF)
	}

	// failure branch: the unified F9 wording (a wrong passphrase and a
	// corrupted file are indistinguishable by design).
	env.verifyErr = errors.New("decrypt failed (wrong passphrase, or the file is corrupted): x")
	m2, cmd2 := got.Update(tea.KeyPressMsg{Code: 'v', Text: "v"})
	msg2 := cmd2()
	em, ok := msg2.(errMsg)
	if !ok {
		t.Fatalf("a failed verify must report errMsg, got %T", msg2)
	}
	m3, _ := m2.(App).Update(em)
	got3 := m3.(App)
	if got3.err == nil || !strings.Contains(got3.err.Error(), "解密失败:口令不符或文件损坏") {
		t.Fatalf("verify failure must carry the unified wording, err=%v", got3.err)
	}
}

func TestBackupPage_VerifyLatestWithoutFiles(t *testing.T) {
	env := newBackupTestEnv(t) // no .sme files
	a := newTestApp(t)
	a.pages[pageBackup] = env.bp
	a.page = pageBackup
	m, cmd := a.Update(tea.KeyPressMsg{Code: 'v', Text: "v"})
	got := m.(App)
	if cmd != nil {
		t.Fatal("v with no backups must be a no-op")
	}
	if !strings.Contains(got.status, "无可校验") {
		t.Fatalf("status: %q", got.status)
	}
}

func TestBackupPage_SaveThreeChecksGateTheWrite(t *testing.T) {
	draft := func(dir, keep, pf string) *backupConfigDraft {
		return &backupConfigDraft{Dir: dir, KeepStr: keep, PassphraseFile: pf}
	}

	// ① a relative dir blocks the save, nothing written
	env := newBackupTestEnv(t)
	if _, err := env.bp.saveConfig(draft("relative\\dir", "7", env.pf)); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("check ① must reject a relative dir, got %v", err)
	}
	if env.saved {
		t.Fatal("a blocked save must not write")
	}
	// negative keep blocked
	if _, err := env.bp.saveConfig(draft(env.dir, "-1", env.pf)); err == nil {
		t.Fatal("a negative keep must be rejected")
	}

	// ② a passphrase file inside the backup dir blocks the save
	inside := filepath.Join(env.dir, "backup.pass")
	if _, err := env.bp.saveConfig(draft(env.dir, "7", inside)); err == nil || !strings.Contains(err.Error(), "inside") {
		t.Fatalf("check ② must reject a passphrase file inside the dir, got %v", err)
	}
	if env.saved {
		t.Fatal("a blocked save must not write")
	}

	// ③ an existing .sme + failing decrypt blocks the save
	now := time.Now()
	env2 := newBackupTestEnv(t, now.Add(-2*time.Hour))
	env2.verifyErr = errors.New("decrypt failed")
	if _, err := env2.bp.saveConfig(draft(env2.dir, "7", env2.pf)); err == nil || !strings.Contains(err.Error(), "试解密") {
		t.Fatalf("check ③ must block on a failing generation check, got %v", err)
	}
	if env2.saved {
		t.Fatal("a blocked save must not write")
	}

	// ③ pass → the config is written with the form's values
	env2.verifyErr = nil
	desc, err := env2.bp.saveConfig(draft(env2.dir, "12", env2.pf))
	if err != nil || !strings.Contains(desc, "已保存") {
		t.Fatalf("a passing save must succeed, got %q / %v", desc, err)
	}
	if !env2.saved || env2.savedKeep != 12 || env2.savedDir != env2.dir || env2.savedPF != env2.pf {
		t.Fatalf("the saved values must match the form: %+v", env2)
	}
	// the generation check targeted the newest .sme in the FORM's dir
	if filepath.Base(env2.lastVerifyFile) != "vault-20260910-000000.sme" {
		t.Fatalf("check ③ must verify the newest .sme, got %q", env2.lastVerifyFile)
	}

	// an empty dir skips check ③ entirely (nothing to generation-check)
	env3 := newBackupTestEnv(t)
	if _, err := env3.bp.saveConfig(draft(env3.dir, "3", env3.pf)); err != nil {
		t.Fatalf("an empty dir must save without check ③, got %v", err)
	}
	if env3.verifyN != 0 {
		t.Fatalf("no .sme in dir must skip the generation check, ran %d", env3.verifyN)
	}
	if !env3.saved {
		t.Fatal("the config must be written")
	}
}

func TestBackupPage_DraftPrefillsCurrentConfig(t *testing.T) {
	env := newBackupTestEnv(t)
	d := env.bp.newConfigDraft()
	if d.Dir != env.dir || d.KeepStr != "3" || d.PassphraseFile != env.pf {
		t.Fatalf("draft prefill: %+v", d)
	}
	env.breakConfig()
	d = env.bp.newConfigDraft()
	if d.Dir != "" || d.PassphraseFile != "" || d.KeepStr != "7" {
		t.Fatalf("guide-state draft must fall back to the CLI defaults, got %+v", d)
	}
}

func TestBackupPage_ListRefreshAfterAction(t *testing.T) {
	env := newBackupTestEnv(t) // empty dir
	a := newTestApp(t)
	a.pages[pageBackup] = env.bp
	a.page = pageBackup
	env.createOut = "wrote vault-20260929-130000.sme\n"

	m, cmd := a.Update(tea.KeyPressMsg{Code: 'b', Text: "b"})
	msg := cmd()
	// a completed action must trigger the async refetch (the shared refresh
	// chain — the pagesMsg rebuild lands a freshly constructed page)
	m2, refetch := m.(App).Update(msg)
	if refetch == nil {
		t.Fatal("a completed action must trigger the async refetch")
	}
	_ = m2
	// the rebuild path is FetchAll → newBackupPage → reload: a file that
	// appeared after the run must show up
	p := filepath.Join(env.dir, "vault-20260929-130000.sme")
	now := time.Now()
	if err := os.WriteFile(p, []byte("SSHMGRV1-fake"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, now, now); err != nil {
		t.Fatal(err)
	}
	env.bp.reload()
	env.bp.syncList()
	rows := env.bp.Rows()
	if len(rows) != 1 || !strings.Contains(rows[0], "vault-20260929-130000.sme") {
		t.Fatalf("the refreshed list must show the new backup, got %v", rows)
	}
}

// TestBackupPage_EntrySeamCopiedAtConstruction pins the SetBackupEntry seam:
// the page copies the wired entry at construction (not at call time), so the
// refetch rebuilds keep using whatever the cli package wired at init.
// Sequential on purpose — it mutates the package seam (no parallel test in
// this package touches it, and sequential tests never interleave).
func TestBackupPage_EntrySeamCopiedAtConstruction(t *testing.T) {
	called := false
	SetBackupEntry(BackupEntry{
		LoadConfig:     func(string) (*BackupConfig, error) { called = true; return nil, nil },
		ValidateValues: func(string, string) error { return nil },
		SaveConfig:     func(string, string, int, string) error { return nil },
		Create:         func(string, io.Writer, io.Writer) error { return nil },
		Verify:         func(string, string, io.Writer) error { return nil },
	})
	defer SetBackupEntry(BackupEntry{})
	bp := newBackupPage(filepath.Join(t.TempDir(), "backup.json"))
	if !bp.entryReady() {
		t.Fatal("construction must copy the wired entry")
	}
	bp.entry.LoadConfig("x")
	if !called {
		t.Fatal("the copied entry must be the wired one")
	}
}
