package ops

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vaultio"
)

func TestManagerSaveBackupConfigWritesChangedFieldsAndAudit(t *testing.T) {
	m, st, root := newBackupTestManager(t)
	backupDir := filepath.Join(root, "backups")
	passFile := filepath.Join(root, "backup.pass")
	newPassFile := filepath.Join(root, "new-backup.pass")
	writeBackupJSON(t, filepath.Join(root, "backup.json"), filepath.Join(root, "old-backups"), 5, passFile)

	err := m.SaveBackupConfig(BackupConfig{
		Dir: backupDir, Keep: 9, PassphraseFile: newPassFile,
	}, WebUISource("192.0.2.23"))
	if err != nil {
		t.Fatalf("首次通过运维模块修改配置应成功：%v", err)
	}
	got, configPath, err := m.ReadBackupConfig()
	if err != nil {
		t.Fatal(err)
	}
	if configPath != filepath.Join(root, "backup.json") || got.Dir != backupDir || got.Keep != 9 ||
		got.PassphraseFile != newPassFile {
		t.Fatalf("保存后的配置不符：path=%q config=%+v", configPath, got)
	}

	rows, err := st.AuditRows(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Action != "backup.config" || rows[0].Status != "ok" ||
		rows[0].Source != store.AuditSourceWebUI || rows[0].SourceIP != "192.0.2.23" {
		t.Fatalf("修改配置应写 Web UI 来源审计行：%+v", rows)
	}
	var summary struct {
		Dir            string `json:"dir"`
		Keep           int    `json:"keep"`
		PassphraseFile string `json:"passphrase_file"`
	}
	if err := json.Unmarshal([]byte(rows[0].Command), &summary); err != nil {
		t.Fatalf("配置审计摘要应是 JSON：%v（%s）", err, rows[0].Command)
	}
	if summary.Dir != backupDir || summary.Keep != 9 || summary.PassphraseFile != newPassFile {
		t.Fatalf("配置审计摘要应记录变化的字段与新值：%s；got=%+v want dir=%q pass=%q",
			rows[0].Command, summary, backupDir, newPassFile)
	}
	if strings.Contains(rows[0].Command, "correct-passphrase") {
		t.Fatalf("配置审计摘要不得包含口令内容：%s", rows[0].Command)
	}
}

func TestManagerSaveBackupConfigRejectsInvalidValues(t *testing.T) {
	m, _, root := newBackupTestManager(t)
	backupDir := filepath.Join(root, "backups")
	passFile := filepath.Join(root, "backup.pass")

	cases := []struct {
		name   string
		config BackupConfig
		field  string
	}{
		{
			name:   "备份目录不是绝对路径",
			config: BackupConfig{Dir: "backups", Keep: 7, PassphraseFile: passFile},
			field:  "dir",
		},
		{
			name:   "备份目录为空",
			config: BackupConfig{Dir: "", Keep: 7, PassphraseFile: passFile},
			field:  "dir",
		},
		{
			name:   "口令文件不是绝对路径",
			config: BackupConfig{Dir: backupDir, Keep: 7, PassphraseFile: "backup.pass"},
			field:  "passphrase_file",
		},
		{
			name:   "口令文件为空",
			config: BackupConfig{Dir: backupDir, Keep: 7, PassphraseFile: ""},
			field:  "passphrase_file",
		},
		{
			name:   "口令文件位于备份目录里",
			config: BackupConfig{Dir: backupDir, Keep: 7, PassphraseFile: filepath.Join(backupDir, "backup.pass")},
			field:  "passphrase_file",
		},
		{
			name:   "保留份数是负数",
			config: BackupConfig{Dir: backupDir, Keep: -1, PassphraseFile: passFile},
			field:  "keep",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := m.SaveBackupConfig(tc.config, CLISource())
			if err == nil {
				t.Fatal("非法配置应被拒绝")
			}
			if BackupConfigErrorField(err) != tc.field {
				t.Fatalf("错误应标明字段 %s，得到 %q：%v", tc.field, BackupConfigErrorField(err), err)
			}
			if _, err := os.Stat(filepath.Join(root, "backup.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("非法配置不应写入文件：%v", err)
			}
		})
	}
}

func TestManagerSaveBackupConfigChecksNewPassphraseAgainstLatestBackup(t *testing.T) {
	m, _, root := newBackupTestManager(t)
	backupDir := filepath.Join(root, "backups")
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	correctPassFile := filepath.Join(root, "correct.pass")
	wrongPassFile := filepath.Join(root, "wrong.pass")
	if err := os.WriteFile(correctPassFile, []byte("correct-passphrase"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wrongPassFile, []byte("wrong-passphrase"), 0o600); err != nil {
		t.Fatal(err)
	}
	blob, err := vaultio.Encrypt([]byte("correct-passphrase"), []byte(`{"version":1}`))
	if err != nil {
		t.Fatal(err)
	}
	backupPath := filepath.Join(backupDir, "vault-20260101-000000.sme")
	if err := os.WriteFile(backupPath, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	writeBackupJSON(t, filepath.Join(root, "backup.json"), backupDir, 5, correctPassFile)

	err = m.SaveBackupConfig(BackupConfig{
		Dir: backupDir, Keep: 6, PassphraseFile: wrongPassFile,
	}, WebUISource("192.0.2.23"))
	if err == nil {
		t.Fatal("新口令解不开现有最新备份时应拒绝")
	}
	if BackupConfigErrorField(err) != "passphrase_file" ||
		!strings.Contains(err.Error(), "新口令无法解开现有最新备份") {
		t.Fatalf("拒绝原因应指向口令文件并说明试解失败：%v", err)
	}
	got, _, err := m.ReadBackupConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got.PassphraseFile != correctPassFile || got.Keep != 5 {
		t.Fatalf("拒绝后不应改写原配置：%+v", got)
	}
}

func TestManagerSaveFirstBackupConfigChecksPassphraseAgainstExistingLatestBackup(t *testing.T) {
	m, _, root := newBackupTestManager(t)
	backupDir := filepath.Join(root, "backups")
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	correctPassFile := filepath.Join(root, "correct.pass")
	wrongPassFile := filepath.Join(root, "wrong.pass")
	if err := os.WriteFile(correctPassFile, []byte("correct-passphrase"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wrongPassFile, []byte("wrong-passphrase"), 0o600); err != nil {
		t.Fatal(err)
	}
	blob, err := vaultio.Encrypt([]byte("correct-passphrase"), []byte(`{"version":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, "vault-20260101-000000.sme"), blob, 0o600); err != nil {
		t.Fatal(err)
	}

	err = m.SaveBackupConfig(BackupConfig{
		Dir: backupDir, Keep: 5, PassphraseFile: wrongPassFile,
	}, CLISource())
	if err == nil || BackupConfigErrorField(err) != "passphrase_file" {
		t.Fatalf("首次配置也应对已有最新备份做口令试解：%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "backup.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("试解失败不应写入配置：%v", err)
	}
}
