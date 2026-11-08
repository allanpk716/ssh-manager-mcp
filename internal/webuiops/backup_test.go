package webuiops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/ops"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
)

func prepareBackupAdapter(t *testing.T) (webui.OpsStore, *store.Store, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("SSHMGR_VAULT_ROOT", root)
	key, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	vaultDir := filepath.Join(root, "ssh-manager")
	if err := os.MkdirAll(vaultDir, 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(vaultDir, "store.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	backupDir := filepath.Join(root, "backups")
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, ".ssh-manager-backup-marker"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	passFile := filepath.Join(root, "backup.pass")
	if err := os.WriteFile(passFile, []byte("adapter-pass"), 0o600); err != nil {
		t.Fatal(err)
	}
	configBody := `{"dir":` + quoteJSON(backupDir) + `,"keep":4,"passphrase_file":` + quoteJSON(passFile) + `}`
	if err := os.WriteFile(filepath.Join(vaultDir, "backup.json"), []byte(configBody), 0o600); err != nil {
		t.Fatal(err)
	}
	return FromManager(ops.New(st)), st, backupDir
}

func quoteJSON(value string) string {
	return `"` + strings.ReplaceAll(value, `\`, `\\`) + `"`
}

func TestManagerAdapterBackupStatusCreateAndVerify(t *testing.T) {
	adapter, st, backupDir := prepareBackupAdapter(t)

	status, err := adapter.BackupStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.ConfigError != "" || status.ConfigDir != backupDir || status.ConfigKeep != 4 ||
		status.TotalBackups != 0 || status.HasLatest {
		t.Fatalf("适配后的备份状态不符：%+v", status)
	}

	created, err := adapter.CreateBackup(webui.OpsWriteSource{RemoteAddress: "192.0.2.9"})
	if err != nil {
		t.Fatal(err)
	}
	if created.File == "" || created.Skipped {
		t.Fatalf("适配后的立即备份结果不符：%+v", created)
	}
	rows, err := st.AuditRows(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Action != "backup.create" || rows[0].Source != store.AuditSourceWebUI ||
		rows[0].SourceIP != "192.0.2.9" {
		t.Fatalf("适配器应把来源地址换算进审计行：%+v", rows)
	}

	verified, err := adapter.VerifyLatestBackup(webui.OpsWriteSource{RemoteAddress: "192.0.2.9"})
	if err != nil {
		t.Fatal(err)
	}
	if !verified.OK || verified.File != created.File {
		t.Fatalf("适配后的一键校验结果不符：%+v", verified)
	}

	status, err = adapter.BackupStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.TotalBackups != 1 || status.LatestBackup != created.File ||
		status.Verify == nil || !status.Verify.OK || !status.Verify.At.After(time.Now().Add(-time.Minute)) {
		t.Fatalf("备份后状态应显示新文件与校验结果：%+v", status)
	}
}

func TestManagerAdapterReadsAndSavesBackupConfig(t *testing.T) {
	adapter, st, backupDir := prepareBackupAdapter(t)
	newPassFile := filepath.Join(filepath.Dir(backupDir), "new-backup.pass")

	config, err := adapter.ReadBackupConfig()
	if err != nil {
		t.Fatal(err)
	}
	if config.Dir != backupDir || config.Keep != 4 {
		t.Fatalf("适配后的备份配置不符：%+v", config)
	}
	config.Keep = 6
	config.PassphraseFile = newPassFile
	if err := adapter.SaveBackupConfig(config, webui.OpsWriteSource{RemoteAddress: "192.0.2.9"}); err != nil {
		t.Fatalf("适配器应保存备份配置：%v", err)
	}
	got, err := adapter.ReadBackupConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got.Keep != 6 || got.PassphraseFile != newPassFile {
		t.Fatalf("保存后的备份配置不符：%+v", got)
	}
	rows, err := st.AuditRows(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Action != "backup.config" || rows[0].Source != store.AuditSourceWebUI ||
		rows[0].SourceIP != "192.0.2.9" || !strings.Contains(rows[0].Command, `"keep":6`) {
		t.Fatalf("保存配置的审计行来源与摘要不符：%+v", rows)
	}
}
