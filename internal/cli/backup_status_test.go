package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/store"
)

// prepareBackupStatusEnv 准备一个临时 vault 环境与一份合法的 backup.json，
// 返回备份配置目录。
func prepareBackupStatusEnv(t *testing.T, passFile string) string {
	t.Helper()
	root := t.TempDir()
	withCliStoreEnv(t)
	t.Setenv("SSHMGR_VAULT_ROOT", root)
	vaultDir := filepath.Join(root, "ssh-manager")
	if err := os.MkdirAll(vaultDir, 0o700); err != nil {
		t.Fatal(err)
	}
	backupDir := filepath.Join(root, "backups")
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeBackupConfig(t, filepath.Join(vaultDir, "backup.json"), backupConfigJSON(backupDir, 5, passFile))
	return backupDir
}

func writeStatusBackupFile(t *testing.T, dir, name string, age time.Duration) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	modTime := time.Now().Add(-age)
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatal(err)
	}
}

func TestBackupStatusText(t *testing.T) {
	passFile := filepath.Join(t.TempDir(), "backup.pass")
	if err := os.WriteFile(passFile, []byte("pass"), 0o600); err != nil {
		t.Fatal(err)
	}
	backupDir := prepareBackupStatusEnv(t, passFile)
	writeStatusBackupFile(t, backupDir, "vault-20260101-000000.sme", 2*time.Hour)
	writeStatusBackupFile(t, backupDir, "vault-20260102-000000.sme", 26*time.Hour)

	out := runCli(t, "backup", "status")
	for _, want := range []string{
		"config: ok", "dir: " + backupDir, "keep: 5", "passphrase_file: " + passFile,
		"backups: 2", "latest: vault-20260102-000000.sme", "freshness: stale",
		"verify: not verified", "scheduled task:",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("status 输出缺少 %q：\n%s", want, out)
		}
	}
}

func TestBackupStatusJSON(t *testing.T) {
	passFile := filepath.Join(t.TempDir(), "backup.pass")
	if err := os.WriteFile(passFile, []byte("pass"), 0o600); err != nil {
		t.Fatal(err)
	}
	backupDir := prepareBackupStatusEnv(t, passFile)
	writeStatusBackupFile(t, backupDir, "vault-20260102-000000.sme", time.Hour)

	out := runCli(t, "backup", "status", "--json")
	var payload struct {
		Config struct {
			Dir            string `json:"dir"`
			Keep           int    `json:"keep"`
			PassphraseFile string `json:"passphrase_file"`
		} `json:"config"`
		Total  int    `json:"total"`
		Latest string `json:"latest"`
		Stale  bool   `json:"stale"`
		Verify *struct {
			File string `json:"file"`
			OK   bool   `json:"ok"`
		} `json:"verify"`
		TaskInstalled bool `json:"task_installed"`
		TaskProbed    bool `json:"task_probed"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("status --json 应输出合法 JSON：%v\n%s", err, out)
	}
	if payload.Config.Dir != backupDir || payload.Config.Keep != 5 ||
		payload.Config.PassphraseFile != passFile || payload.Total != 1 ||
		payload.Latest != "vault-20260102-000000.sme" || payload.Stale ||
		payload.Verify != nil || payload.TaskProbed != (runtime.GOOS == "windows") {
		t.Fatalf("status --json 内容不符：%s", out)
	}
}

func TestBackupStatusMissingConfig(t *testing.T) {
	withCliStoreEnv(t)
	root := t.TempDir()
	t.Setenv("SSHMGR_VAULT_ROOT", root)

	out := runCli(t, "backup", "status")
	if !strings.Contains(out, "config: missing") || !strings.Contains(out, filepath.Join(root, "ssh-manager", "backup.json")) {
		t.Fatalf("无配置时应给出引导信息：\n%s", out)
	}
}

func TestBackupVerifyLatest(t *testing.T) {
	passDir := t.TempDir()
	passFile := writePassFile(t, passDir, encPass1)
	backupDir := prepareBackupStatusEnv(t, passFile)
	touchMarker(t, backupDir)
	runCreate(t, backupDir, "--passphrase-file", passFile)

	out := runCli(t, "backup", "verify", "--latest")
	if !strings.Contains(out, "ok: vault-") || !strings.Contains(out, "(decrypted and json structurally valid)") {
		t.Fatalf("verify --latest 应输出成功结果：\n%s", out)
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
	if len(rows) != 1 || rows[0].Action != "backup.verify" || rows[0].Status != "ok" ||
		rows[0].Source != store.AuditSourceCLI || !strings.Contains(rows[0].Command, "vault-") {
		t.Fatalf("verify --latest 应写命令行来源的 backup.verify 审计：%+v", rows)
	}
}

func TestBackupVerifyLatestWithoutBackups(t *testing.T) {
	passFile := filepath.Join(t.TempDir(), "backup.pass")
	if err := os.WriteFile(passFile, []byte("pass"), 0o600); err != nil {
		t.Fatal(err)
	}
	prepareBackupStatusEnv(t, passFile)

	runCliErr(t, "backup", "verify", "--latest")
}
