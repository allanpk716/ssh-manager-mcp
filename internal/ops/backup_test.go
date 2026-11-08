package ops

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vaultio"
)

// newBackupTestManager 返回接上了临时备份配置路径的运维模块与存储句柄；
// 返回的根目录同时用来放备份目录与口令文件。
func newBackupTestManager(t *testing.T) (*Manager, *store.Store, string) {
	t.Helper()
	m, st := newTestManager(t)
	root := t.TempDir()
	configPath := filepath.Join(root, "backup.json")
	m.backupConfigPath = func() (string, error) { return configPath, nil }
	return m, st, root
}

// writeBackupJSON 写一份合法的备份配置；口令文件放在备份目录外。
func writeBackupJSON(t *testing.T, path, dir string, keep int, passFile string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"dir": dir, "keep": keep, "passphrase_file": passFile,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeSMEBackup 写一个空内容的备份文件；状态只看文件名与修改时间。
func writeSMEBackup(t *testing.T, dir, name string, modTime time.Time) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatal(err)
	}
}

func TestBackupStatusWithoutConfigShowsGuide(t *testing.T) {
	m, _, root := newBackupTestManager(t)

	status, err := m.BackupStatus()
	if err != nil {
		t.Fatalf("无配置不是读取错误：%v", err)
	}
	if !status.ConfigMissing || status.ConfigError == "" || status.Config != nil {
		t.Fatalf("无配置应进入引导状态：missing=%v err=%q config=%+v",
			status.ConfigMissing, status.ConfigError, status.Config)
	}
	if status.ConfigPath != filepath.Join(root, "backup.json") {
		t.Fatalf("状态应指出配置文件位置：%q", status.ConfigPath)
	}
	if status.Total != 0 || status.HasLatest || status.Stale || status.Verify != nil {
		t.Fatalf("无配置时不应统计备份：%+v", status)
	}
}

func TestBackupStatusWithBrokenConfigShowsError(t *testing.T) {
	m, _, root := newBackupTestManager(t)
	if err := os.WriteFile(filepath.Join(root, "backup.json"), []byte(`{oops`), 0o600); err != nil {
		t.Fatal(err)
	}

	status, err := m.BackupStatus()
	if err != nil {
		t.Fatalf("配置有误不是读取错误：%v", err)
	}
	if status.ConfigMissing || status.ConfigError == "" || status.Config != nil {
		t.Fatalf("配置有误应保留具体错误：missing=%v err=%q config=%+v",
			status.ConfigMissing, status.ConfigError, status.Config)
	}
}

func TestBackupStatusSelectsLatestByFileName(t *testing.T) {
	m, _, root := newBackupTestManager(t)
	dir := filepath.Join(root, "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeBackupJSON(t, filepath.Join(root, "backup.json"), dir, 7, filepath.Join(root, "backup.pass"))

	now := time.Unix(1800000000, 0)
	olderName := "vault-20260101-000000.sme"
	newerName := "vault-20260203-000000.sme"
	// 故意让文件名更旧的备份有更新的修改时间：选取必须按文件名，不按修改时间。
	writeSMEBackup(t, dir, olderName, now.Add(-time.Minute))
	writeSMEBackup(t, dir, newerName, now.Add(-2*time.Hour))
	m.now = func() time.Time { return now }

	status, err := m.BackupStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.Config == nil || status.Config.Dir != dir || status.Config.Keep != 7 ||
		status.Config.PassphraseFile != filepath.Join(root, "backup.pass") {
		t.Fatalf("状态应带回配置：%+v", status.Config)
	}
	if status.Total != 2 || !status.HasLatest || status.LatestName != newerName {
		t.Fatalf("最新备份应按文件名选取：total=%d latest=%q", status.Total, status.LatestName)
	}
	wantTime := now.Add(-2 * time.Hour)
	if !status.LatestTime.Equal(wantTime) {
		t.Fatalf("最新备份的显示时间应为修改时间：%v，得 %v", wantTime, status.LatestTime)
	}
	if status.Stale {
		t.Fatalf("2 小时内的备份不应过期：%+v", status)
	}
}

func TestBackupStatusMarksStaleAfter25Hours(t *testing.T) {
	m, _, root := newBackupTestManager(t)
	dir := filepath.Join(root, "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeBackupJSON(t, filepath.Join(root, "backup.json"), dir, 7, filepath.Join(root, "backup.pass"))

	now := time.Unix(1800000000, 0)
	writeSMEBackup(t, dir, "vault-20260101-000000.sme", now.Add(-26*time.Hour))
	m.now = func() time.Time { return now }

	status, err := m.BackupStatus()
	if err != nil {
		t.Fatal(err)
	}
	if !status.Stale {
		t.Fatalf("26 小时未备份应标记过期：%+v", status)
	}
}

func TestBackupStatusWithoutBackups(t *testing.T) {
	m, _, root := newBackupTestManager(t)
	dir := filepath.Join(root, "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeBackupJSON(t, filepath.Join(root, "backup.json"), dir, 7, filepath.Join(root, "backup.pass"))

	status, err := m.BackupStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.Total != 0 || status.HasLatest || status.LatestName != "" || status.Stale {
		t.Fatalf("尚无备份时应显示空状态：%+v", status)
	}
}

func TestBackupStatusProbesScheduleTask(t *testing.T) {
	m, _, _ := newBackupTestManager(t)
	m.backupScheduleProbe = func() (installed, probed bool) { return false, true }

	status, err := m.BackupStatus()
	if err != nil {
		t.Fatal(err)
	}
	if !status.TaskProbed || status.TaskInstalled {
		t.Fatalf("计划任务探测结果应进入状态：probed=%v installed=%v",
			status.TaskProbed, status.TaskInstalled)
	}
}

func writeBackupAudit(t *testing.T, st *store.Store, at time.Time, action, file, result, failureKind string) {
	t.Helper()
	summary := map[string]any{"file": file, "result": result}
	if failureKind != "" {
		summary["failure_kind"] = failureKind
	}
	if err := st.WriteAudit(store.AuditRow{
		TS: at, Action: action, Command: store.MarshalAuditSummary(summary),
		Status: map[bool]string{true: "ok", false: "error"}[result == "ok"],
		Source: store.AuditSourceWebUI, SourceIP: "192.0.2.23",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestBackupStatusReadsLatestVerifyResultFromAudit(t *testing.T) {
	m, st, root := newBackupTestManager(t)
	dir := filepath.Join(root, "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "backup.json")
	writeBackupJSON(t, configPath, dir, 7, filepath.Join(root, "backup.pass"))
	m.backupConfigPath = func() (string, error) { return configPath, nil }

	now := time.Unix(1800000000, 0)
	m.now = func() time.Time { return now }
	writeSMEBackup(t, dir, "vault-20260101-000000.sme", now.Add(-time.Hour))

	// 先有一条旧文件的失败校验，再有一条当前文件的成功校验：状态应取当前文件最近一条。
	writeBackupAudit(t, st, now.Add(-50*time.Minute), "backup.verify",
		"vault-20251231-000000.sme", "error", "passphrase")
	writeBackupAudit(t, st, now.Add(-40*time.Minute), "backup.verify",
		"vault-20260101-000000.sme", "ok", "")

	status, err := m.BackupStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.Verify == nil || !status.Verify.OK || status.Verify.File != "vault-20260101-000000.sme" ||
		!status.Verify.At.Equal(now.Add(-40*time.Minute)) {
		t.Fatalf("状态应显示当前最新文件最近一次校验成功：%+v", status.Verify)
	}

	// 新备份出现后，旧校验记录不再命中，状态变为尚未校验。
	writeSMEBackup(t, dir, "vault-20260102-000000.sme", now.Add(-time.Minute))
	status, err = m.BackupStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.Verify != nil {
		t.Fatalf("最新文件变化后应显示尚未校验：%+v", status.Verify)
	}
}

func TestBackupStatusTreatsCreateAuditAsVerified(t *testing.T) {
	m, st, root := newBackupTestManager(t)
	dir := filepath.Join(root, "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "backup.json")
	writeBackupJSON(t, configPath, dir, 7, filepath.Join(root, "backup.pass"))
	m.backupConfigPath = func() (string, error) { return configPath, nil }

	now := time.Unix(1800000000, 0)
	m.now = func() time.Time { return now }
	writeSMEBackup(t, dir, "vault-20260101-000000.sme", now.Add(-time.Hour))
	writeBackupAudit(t, st, now.Add(-30*time.Minute), "backup.create",
		"vault-20260101-000000.sme", "ok", "")

	status, err := m.BackupStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.Verify == nil || !status.Verify.OK {
		t.Fatalf("立即备份已回读校验，应算作一次成功校验：%+v", status.Verify)
	}
}

func TestBackupStatusShowsVerifyFailureKind(t *testing.T) {
	m, st, root := newBackupTestManager(t)
	dir := filepath.Join(root, "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "backup.json")
	writeBackupJSON(t, configPath, dir, 7, filepath.Join(root, "backup.pass"))
	m.backupConfigPath = func() (string, error) { return configPath, nil }

	now := time.Unix(1800000000, 0)
	m.now = func() time.Time { return now }
	writeSMEBackup(t, dir, "vault-20260101-000000.sme", now.Add(-time.Hour))
	writeBackupAudit(t, st, now.Add(-30*time.Minute), "backup.verify",
		"vault-20260101-000000.sme", "error", "corrupt")

	status, err := m.BackupStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.Verify == nil || status.Verify.OK || status.Verify.FailureKind != BackupFailureCorrupt {
		t.Fatalf("校验失败应带上失败类别：%+v", status.Verify)
	}
}

// prepareVerifiedBackupEnv 准备一份合法备份配置、口令文件和一个加密备份
// 文件，返回可覆盖的口令与备份文件路径。
func prepareVerifiedBackupEnv(t *testing.T) (*Manager, *store.Store, string, string, string) {
	t.Helper()
	m, st, root := newBackupTestManager(t)
	dir := filepath.Join(root, "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	passFile := filepath.Join(root, "backup.pass")
	if err := os.WriteFile(passFile, []byte("correct-passphrase"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "backup.json")
	writeBackupJSON(t, configPath, dir, 7, passFile)
	m.backupConfigPath = func() (string, error) { return configPath, nil }

	blob, err := vaultio.Encrypt([]byte("correct-passphrase"), []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	backupPath := filepath.Join(dir, "vault-20260101-000000.sme")
	if err := os.WriteFile(backupPath, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	return m, st, passFile, backupPath, dir
}

func TestVerifyLatestBackupSuccessWritesAudit(t *testing.T) {
	m, st, passFile, _, _ := prepareVerifiedBackupEnv(t)

	result, err := m.VerifyLatestBackup(WebUISource("192.0.2.23"))
	if err != nil {
		t.Fatalf("校验最新备份应成功：%v", err)
	}
	if !result.OK || result.File != "vault-20260101-000000.sme" || result.FailureKind != "" {
		t.Fatalf("校验结果不符：%+v", result)
	}

	rows, err := st.AuditRows(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Action != "backup.verify" || rows[0].Status != "ok" ||
		rows[0].Source != store.AuditSourceWebUI || rows[0].SourceIP != "192.0.2.23" {
		t.Fatalf("校验应写 Web UI 来源的审计行：%+v", rows)
	}
	if !strings.Contains(rows[0].Command, "vault-20260101-000000.sme") || !strings.Contains(rows[0].Command, `"result":"ok"`) {
		t.Fatalf("校验摘要应包含文件名与结果：%s", rows[0].Command)
	}
	if strings.Contains(rows[0].Command, "correct-passphrase") || strings.Contains(rows[0].Command, passFile) {
		t.Fatalf("校验摘要不得包含口令内容或口令文件路径：%s", rows[0].Command)
	}
}

func TestVerifyLatestBackupClassifiesFailures(t *testing.T) {
	cases := []struct {
		name string
		mut  func(t *testing.T, passFile, backupPath string)
		kind string
	}{
		{
			name: "口令不符",
			mut: func(t *testing.T, passFile, _ string) {
				t.Helper()
				if err := os.WriteFile(passFile, []byte("wrong-passphrase"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			kind: BackupFailurePassphrase,
		},
		{
			name: "文件不完整",
			mut: func(t *testing.T, _, backupPath string) {
				t.Helper()
				if err := os.WriteFile(backupPath, []byte("SSHMGRV1short"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			kind: BackupFailureIncomplete,
		},
		{
			name: "文件损坏",
			mut: func(t *testing.T, _, backupPath string) {
				t.Helper()
				if err := os.WriteFile(backupPath, []byte("this blob is long enough but has a wrong header"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			kind: BackupFailureCorrupt,
		},
		{
			name: "解密后内容损坏",
			mut: func(t *testing.T, _, backupPath string) {
				t.Helper()
				blob, err := vaultio.Encrypt([]byte("correct-passphrase"), []byte("not-json"))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(backupPath, blob, 0o600); err != nil {
					t.Fatal(err)
				}
			},
			kind: BackupFailureCorrupt,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, st, passFile, backupPath, _ := prepareVerifiedBackupEnv(t)
			tc.mut(t, passFile, backupPath)

			result, err := m.VerifyLatestBackup(CLISource())
			if err == nil {
				t.Fatalf("%s 应返回错误", tc.name)
			}
			if result.OK || result.FailureKind != tc.kind {
				t.Fatalf("%s 的失败类别应为 %s：%+v", tc.name, tc.kind, result)
			}
			rows, err := st.AuditRows(1)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0].Action != "backup.verify" || rows[0].Status != "error" ||
				!strings.Contains(rows[0].Command, tc.kind) || rows[0].Source != store.AuditSourceCLI {
				t.Fatalf("%s 的审计行应记录失败类别：%+v", tc.name, rows)
			}

			// 失败结果应反映到状态区。
			status, err := m.BackupStatus()
			if err != nil {
				t.Fatal(err)
			}
			if status.Verify == nil || status.Verify.OK || status.Verify.FailureKind != tc.kind {
				t.Fatalf("%s 后状态应显示失败：%+v", tc.name, status.Verify)
			}
		})
	}
}

func TestVerifyLatestBackupWithoutBackupsOrConfig(t *testing.T) {
	m, st, root := newBackupTestManager(t)
	dir := filepath.Join(root, "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "backup.json")
	writeBackupJSON(t, configPath, dir, 7, filepath.Join(root, "backup.pass"))
	m.backupConfigPath = func() (string, error) { return configPath, nil }

	if _, err := m.VerifyLatestBackup(CLISource()); err == nil {
		t.Fatal("尚无备份时应返回错误")
	}
	rows, err := st.AuditRows(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Action != "backup.verify" || rows[0].Status != "error" ||
		!strings.Contains(rows[0].Command, `"failure_kind":"no_backup"`) {
		t.Fatalf("无备份应写失败审计：%+v", rows)
	}

	m.backupConfigPath = func() (string, error) { return filepath.Join(root, "missing.json"), nil }
	if _, err := m.VerifyLatestBackup(CLISource()); err == nil {
		t.Fatal("配置缺失时应返回错误")
	}
}

func TestVerifyLatestBackupPartialFailuresWriteAudit(t *testing.T) {
	t.Run("配置错误", func(t *testing.T) {
		m, st, root := newBackupTestManager(t)
		if err := os.WriteFile(filepath.Join(root, "backup.json"), []byte(`{oops`), 0o600); err != nil {
			t.Fatal(err)
		}

		_, err := m.VerifyLatestBackup(CLISource())
		if err == nil {
			t.Fatal("配置有误应返回错误")
		}
		rows, err := st.AuditRows(1)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].Action != "backup.verify" || rows[0].Status != "error" ||
			!strings.Contains(rows[0].Command, `"result":"error"`) ||
			!strings.Contains(rows[0].Command, `"failure_kind":"config"`) {
			t.Fatalf("配置错误应写失败类别为 config 的审计：%+v", rows)
		}
	})

	t.Run("无备份", func(t *testing.T) {
		m, st, root := newBackupTestManager(t)
		dir := filepath.Join(root, "backups")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		writeBackupJSON(t, filepath.Join(root, "backup.json"), dir, 7, filepath.Join(root, "backup.pass"))

		_, err := m.VerifyLatestBackup(WebUISource("192.0.2.23"))
		if err == nil {
			t.Fatal("没有任何备份应返回错误")
		}
		rows, err := st.AuditRows(1)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].Action != "backup.verify" || rows[0].Status != "error" ||
			rows[0].Source != store.AuditSourceWebUI || rows[0].SourceIP != "192.0.2.23" ||
			!strings.Contains(rows[0].Command, `"result":"error"`) ||
			!strings.Contains(rows[0].Command, `"failure_kind":"no_backup"`) {
			t.Fatalf("无备份应写失败类别为 no_backup 的审计：%+v", rows)
		}
	})
}

// prepareCreateBackupEnv 准备带标记文件的备份目录、口令文件与备份配置。
func prepareCreateBackupEnv(t *testing.T, keep int) (*Manager, *store.Store, string, string) {
	t.Helper()
	m, st, root := newBackupTestManager(t)
	dir := filepath.Join(root, "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".ssh-manager-backup-marker"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	passFile := filepath.Join(root, "backup.pass")
	if err := os.WriteFile(passFile, []byte("correct-passphrase"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "backup.json")
	writeBackupJSON(t, configPath, dir, keep, passFile)
	m.backupConfigPath = func() (string, error) { return configPath, nil }
	return m, st, dir, passFile
}

func TestCreateBackupWritesEncryptedFileAndAudit(t *testing.T) {
	m, st, dir, passFile := prepareCreateBackupEnv(t, 7)
	if _, err := st.AddServer(&models.Server{
		Name: "web-1", Host: "192.0.2.10", Port: 22, User: "owner",
	}); err != nil {
		t.Fatal(err)
	}

	result, err := m.CreateBackup(WebUISource("192.0.2.23"))
	if err != nil {
		t.Fatalf("立即备份应成功：%v", err)
	}
	if result.File == "" || result.Skipped {
		t.Fatalf("备份结果应带文件名：%+v", result)
	}
	blob, err := os.ReadFile(filepath.Join(dir, result.File))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := vaultio.Decrypt([]byte("correct-passphrase"), blob)
	if err != nil {
		t.Fatalf("新备份应能用配置口令解密：%v", err)
	}
	if !strings.Contains(string(plain), "web-1") {
		t.Fatalf("新备份应包含 vault 快照内容：%s", plain)
	}

	rows, err := st.AuditRows(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Action != "backup.create" || rows[0].Status != "ok" ||
		rows[0].Source != store.AuditSourceWebUI || rows[0].SourceIP != "192.0.2.23" ||
		!strings.Contains(rows[0].Command, result.File) {
		t.Fatalf("立即备份应写审计行：%+v %s", rows, result.File)
	}
	if strings.Contains(rows[0].Command, "correct-passphrase") || strings.Contains(rows[0].Command, passFile) {
		t.Fatalf("备份摘要不得包含口令内容或口令文件路径：%s", rows[0].Command)
	}

	status, err := m.BackupStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.Total != 1 || status.LatestName != result.File {
		t.Fatalf("状态应显示新备份：%+v", status)
	}
	// 立即备份写入时已回读校验，应算作一次成功校验。
	if status.Verify == nil || !status.Verify.OK {
		t.Fatalf("立即备份后状态应显示已校验：%+v", status.Verify)
	}
}

func TestCreateBackupConfigFailureWritesAudit(t *testing.T) {
	m, st, root := newBackupTestManager(t)
	if err := os.WriteFile(filepath.Join(root, "backup.json"), []byte(`{oops`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := m.CreateBackup(WebUISource("192.0.2.23"))
	if err == nil {
		t.Fatal("配置有误应拒绝立即备份")
	}
	rows, err := st.AuditRows(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Action != "backup.create" || rows[0].Status != "error" ||
		rows[0].Source != store.AuditSourceWebUI || rows[0].SourceIP != "192.0.2.23" ||
		!strings.Contains(rows[0].Command, `"result":"error"`) ||
		!strings.Contains(rows[0].Command, `"failure_kind":"config"`) {
		t.Fatalf("配置错误应写失败类别为 config 的立即备份审计：%+v", rows)
	}
}

func TestCreateBackupRotatesByKeep(t *testing.T) {
	m, _, dir, _ := prepareCreateBackupEnv(t, 2)
	now := time.Unix(1800000000, 0)
	m.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		if _, err := m.CreateBackup(CLISource()); err != nil {
			t.Fatalf("第 %d 次备份失败：%v", i+1, err)
		}
		now = now.Add(2 * time.Second)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "vault-*.sme"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 {
		t.Fatalf("keep=2 时应只保留两份备份：%v", matches)
	}
}

func TestCreateBackupRefusesWrongPassphraseLineage(t *testing.T) {
	m, st, dir, _ := prepareCreateBackupEnv(t, 7)
	blob, err := vaultio.Encrypt([]byte("older-passphrase"), []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "vault-20251231-000000.sme"), blob, 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := m.CreateBackup(CLISource())
	if err == nil {
		t.Fatal("口令试解失败时应拒绝写入")
	}
	if result.File != "" {
		t.Fatalf("失败时不应报告文件名：%+v", result)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "vault-*.sme"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("失败时不应写入新备份：%v", matches)
	}
	rows, err := st.AuditRows(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Action != "backup.create" || rows[0].Status != "error" {
		t.Fatalf("失败的立即备份也应写审计：%+v", rows)
	}
}

func TestCreateBackupSkipsWhenConcurrent(t *testing.T) {
	m, _, dir, _ := prepareCreateBackupEnv(t, 7)
	lock, err := AcquireBackupLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()

	result, err := m.CreateBackup(CLISource())
	if err != nil {
		t.Fatalf("并发备份应返回跳过而不是错误：%v", err)
	}
	if !result.Skipped || result.File != "" {
		t.Fatalf("并发备份应标记跳过：%+v", result)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "vault-*.sme"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("并发跳过时不应写备份：%v", matches)
	}
}

func TestCreateBackupRefusesWithoutMarker(t *testing.T) {
	m, _, dir, _ := prepareCreateBackupEnv(t, 7)
	if err := os.Remove(filepath.Join(dir, ".ssh-manager-backup-marker")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateBackup(CLISource()); err == nil {
		t.Fatal("缺少标记文件时应拒绝写入")
	}
	matches, err := filepath.Glob(filepath.Join(dir, "vault-*.sme"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("缺少标记文件时不应写备份：%v", matches)
	}
}

func TestBackupPostWriteVerifyEncryptedTamperAndWrongPass(t *testing.T) {
	blob, err := vaultio.Encrypt([]byte("pass-alpha"), []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "vault-20260101-000000.sme")
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := verifyEncryptedWritten(path, []byte("pass-alpha")); err != nil {
		t.Fatalf("完好的备份应通过回读校验：%v", err)
	}
	tampered := append([]byte(nil), blob...)
	tampered[len(tampered)-1] ^= 0xFF
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyEncryptedWritten(path, []byte("pass-alpha")); err == nil {
		t.Fatal("被篡改的备份必须无法通过回读校验")
	}
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyEncryptedWritten(path, []byte("pass-beta")); err == nil {
		t.Fatal("口令不符必须无法通过回读校验")
	}
}

func TestBackupStatusWorksWithSettingInputsManager(t *testing.T) {
	// 权威端与新版单机入口用 NewWithSettingInputs 构造运维模块；备份接缝
	// 必须同样有默认值，不能只在 New 里填。
	m, _, root := newBackupTestManager(t)
	configured := NewWithSettingInputs(m.st, SettingInputs{})
	configured.backupConfigPath = func() (string, error) {
		return filepath.Join(root, "missing-backup.json"), nil
	}
	status, err := configured.BackupStatus()
	if err != nil {
		t.Fatal(err)
	}
	if !status.ConfigMissing || status.ConfigPath == "" {
		t.Fatalf("带设置输入的运维模块也应能读取备份状态：%+v", status)
	}
}
