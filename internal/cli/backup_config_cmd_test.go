package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/store"
)

func TestBackupConfigShow(t *testing.T) {
	passFile := filepath.Join(t.TempDir(), "backup.pass")
	if err := os.WriteFile(passFile, []byte("pass"), 0o600); err != nil {
		t.Fatal(err)
	}
	backupDir := prepareBackupStatusEnv(t, passFile)

	out := runCli(t, "backup", "config", "show")
	for _, want := range []string{
		"config: " + filepath.Join(os.Getenv("SSHMGR_VAULT_ROOT"), "ssh-manager", "backup.json"),
		"dir: " + backupDir, "keep: 5", "passphrase_file: " + passFile,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("backup config show 输出缺少 %q：\n%s", want, out)
		}
	}
}

func TestBackupConfigSetWritesConfigAndAudit(t *testing.T) {
	passFile := filepath.Join(t.TempDir(), "backup.pass")
	if err := os.WriteFile(passFile, []byte("pass"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldBackupDir := prepareBackupStatusEnv(t, passFile)
	newBackupDir := filepath.Join(filepath.Dir(oldBackupDir), "new-backups")

	out := runCli(t, "backup", "config", "set", "--dir", newBackupDir,
		"--keep", "9", "--passphrase-file", passFile)
	if !strings.Contains(out, "saved backup config") {
		t.Fatalf("backup config set 应输出保存结果：\n%s", out)
	}
	got := runCli(t, "backup", "config", "show")
	if !strings.Contains(got, "dir: "+newBackupDir) || !strings.Contains(got, "keep: 9") {
		t.Fatalf("设置后的备份配置不符：\n%s", got)
	}

	st, err := store.Open(os.Getenv("SSHMGR_STORE"), mustDecodeHex(t, os.Getenv("SSHMGR_MASTERKEY_HEX")))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rows, err := st.AuditRows(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Action != "backup.config" || rows[0].Status != "ok" ||
		rows[0].Source != store.AuditSourceCLI || !strings.Contains(rows[0].Command, `"dir":"`+newBackupDir+`"`) {
		t.Fatalf("设置备份配置应写命令行来源审计行：%+v", rows)
	}
}

func TestBackupConfigSetRejectsInvalidValue(t *testing.T) {
	passFile := filepath.Join(t.TempDir(), "backup.pass")
	if err := os.WriteFile(passFile, []byte("pass"), 0o600); err != nil {
		t.Fatal(err)
	}
	backupDir := prepareBackupStatusEnv(t, passFile)
	configPath := filepath.Join(os.Getenv("SSHMGR_VAULT_ROOT"), "ssh-manager", "backup.json")
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}

	out := runCliErr(t, "backup", "config", "set", "--dir", "relative-backups",
		"--keep", "9", "--passphrase-file", passFile)
	if !strings.Contains(out, `"dir" must be an absolute path`) {
		t.Fatalf("非法备份目录应报字段错误：\n%s", out)
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) || backupDir == "" {
		t.Fatal("非法值不应改写备份配置")
	}
}
