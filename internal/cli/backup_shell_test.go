package cli

// 命令策略·服务器 shell 类型字段(ticket #14)的备份与恢复往返:明文 JSON
// 备份(默认形态,不经口令加密层)→ 恢复统一走 import → 字段保持。
// 用 SSHMGR_FILEKEY_PATH 指向不存在的文件隔离出测试自己的主密钥
// (newHintEnv 的既有做法),否则开发机上的真实 master.key 会以第一优先
// 级顶掉 SSHMGR_MASTERKEY_HEX,备份的取凭据解密直接失败(本机已知的
// TestBackup* 环境性失败即此因)。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

func TestBackupRestoreKeepsShellType(t *testing.T) {
	dir := t.TempDir()
	dbA := filepath.Join(dir, "a.db")
	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	// 隔离主密钥解析:文件层指向不存在的路径 → 环境变量层生效。
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         dbA,
		"SSHMGR_MASTERKEY_HEX": hexEncode(mk),
		"SSHMGR_FILEKEY_PATH":  filepath.Join(dir, "no-such-master.key"),
	})
	st, err := store.Open(dbA, mk)
	if err != nil {
		t.Fatal(err)
	}
	cid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	if _, err := st.AddServer(&models.Server{
		Name: "winbox", Host: "192.0.2.10", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: cid,
		ShellType: models.ShellPowerShell,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddServer(&models.Server{
		Name: "nix", Host: "192.0.2.11", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: cid,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// 备份(明文 JSON 形态)。
	bdir := filepath.Join(dir, "backups")
	if err := os.MkdirAll(bdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bdir, ".ssh-manager-backup-marker"), []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	runCaptured(t, "backup", "create", "--dir", bdir)
	matches, err := filepath.Glob(filepath.Join(bdir, "vault-*.json"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected 1 backup json, got %v (err %v)", matches, err)
	}

	// 恢复统一走 import:指向另一个空库(不同主密钥,同一隔离的文件层路径)。
	dbB := filepath.Join(dir, "b.db")
	mk2, _ := store.GenerateMasterKey()
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         dbB,
		"SSHMGR_MASTERKEY_HEX": hexEncode(mk2),
		"SSHMGR_FILEKEY_PATH":  filepath.Join(dir, "no-such-master.key"),
	})
	runCaptured(t, "import", matches[0])

	st2, err := store.Open(dbB, mk2)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	for name, want := range map[string]models.ShellType{
		"winbox": models.ShellPowerShell,
		"nix":    models.ShellPOSIX, // 未标注 → 默认 POSIX,备份恢复后同样保持
	} {
		got, err := st2.GetServerByName(name)
		if err != nil || got == nil {
			t.Fatalf("restored %s: %v %v", name, got, err)
		}
		if got.ShellType != want {
			t.Fatalf("restored %s ShellType = %q, want %q", name, got.ShellType, want)
		}
	}
}

// TestBackupJSONCarriesShellTypeField 钉备份文件本身的形态:明文 JSON 里的
// 服务器对象带 shell_type 键(快照层的序列化面)。
func TestBackupJSONCarriesShellTypeField(t *testing.T) {
	newHintEnv(t)
	runCaptured(t, "servers", "add", "--name", "ps", "--host", "h", "--user", "u", "--shell", "powershell")
	bdir := t.TempDir()
	if err := os.WriteFile(filepath.Join(bdir, ".ssh-manager-backup-marker"), []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	runCaptured(t, "backup", "create", "--dir", bdir)
	matches, err := filepath.Glob(filepath.Join(bdir, "vault-*.json"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected 1 backup json, got %v (err %v)", matches, err)
	}
	blob, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(blob), "\"shell_type\": \"powershell\"") {
		t.Fatalf("backup json missing shell_type field:\n%s", string(blob)[:min(400, len(blob))])
	}
}
