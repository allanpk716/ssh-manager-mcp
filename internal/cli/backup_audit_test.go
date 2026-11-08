package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/store"
)

func latestBackupAuditRow(t *testing.T) store.AuditRow {
	t.Helper()
	st, err := store.Open(os.Getenv("SSHMGR_STORE"), mustDecodeHex(t, os.Getenv("SSHMGR_MASTERKEY_HEX")))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rows, err := st.AuditRows(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("应写入一条审计行，实际 %+v", rows)
	}
	return rows[0]
}

func TestBackupCreateWritesAuditWhenPreparationFails(t *testing.T) {
	t.Run("目录标记缺失", func(t *testing.T) {
		seedVaultForBackup(t)
		bdir := t.TempDir()
		if _, err := runCreate(t, bdir); err == nil {
			t.Fatal("目录标记缺失应失败")
		}
		row := latestBackupAuditRow(t)
		if row.Action != "backup.create" || row.Status != "error" ||
			row.Source != store.AuditSourceCLI || !strings.Contains(row.Command, `"result":"error"`) {
			t.Fatalf("目录标记缺失应写 backup.create 失败审计：%+v", row)
		}
	})

	t.Run("配置读取失败", func(t *testing.T) {
		seedVaultForBackup(t)
		bdir := t.TempDir()
		touchMarker(t, bdir)
		configPath := filepath.Join(t.TempDir(), "backup.json")
		if err := os.WriteFile(configPath, []byte(`{oops`), 0o600); err != nil {
			t.Fatal(err)
		}
		root := NewRootCmd()
		root.SetArgs([]string{"backup", "create", "--config", configPath})
		root.SetOut(&strings.Builder{})
		root.SetErr(&strings.Builder{})
		if err := root.Execute(); err == nil {
			t.Fatal("配置读取失败应返回错误")
		}
		row := latestBackupAuditRow(t)
		if row.Action != "backup.create" || row.Status != "error" ||
			!strings.Contains(row.Command, `"failure_kind":"config"`) {
			t.Fatalf("配置读取失败应写配置错误审计：%+v", row)
		}
	})

	t.Run("口令文件位于备份目录", func(t *testing.T) {
		seedVaultForBackup(t)
		bdir := t.TempDir()
		touchMarker(t, bdir)
		passFile := writePassFile(t, bdir, "secret-passphrase-content")
		if _, err := runCreate(t, bdir, "--passphrase-file", passFile); err == nil {
			t.Fatal("口令文件位于备份目录应失败")
		}
		row := latestBackupAuditRow(t)
		if row.Action != "backup.create" || row.Status != "error" ||
			strings.Contains(row.Command, "secret-passphrase-content") {
			t.Fatalf("口令文件位置失败应写审计且不包含口令内容：%+v", row)
		}
	})
}

func TestBackupVerifyPathWritesAudit(t *testing.T) {
	seedVaultForBackup(t)
	bdir := t.TempDir()
	touchMarker(t, bdir)
	if _, err := runCreate(t, bdir); err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(bdir, "vault-*.json"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("应生成一个明文备份：%v %v", matches, err)
	}
	backupPath := matches[0]
	name := filepath.Base(backupPath)

	if _, err := runVerifyArgs(t, backupPath); err != nil {
		t.Fatalf("按路径校验成功失败：%v", err)
	}
	row := latestBackupAuditRow(t)
	if row.Action != "backup.verify" || row.Status != "ok" ||
		row.Source != store.AuditSourceCLI || !strings.Contains(row.Command, name) ||
		!strings.Contains(row.Command, `"result":"ok"`) {
		t.Fatalf("按路径校验成功应写命令行审计：%+v", row)
	}

	if err := os.Remove(backupPath + ".sha256"); err != nil {
		t.Fatal(err)
	}
	if _, err := runVerifyArgs(t, backupPath); err == nil {
		t.Fatal("缺少校验和附文件应失败")
	}
	row = latestBackupAuditRow(t)
	if row.Action != "backup.verify" || row.Status != "error" ||
		!strings.Contains(row.Command, name) || !strings.Contains(row.Command, `"result":"error"`) {
		t.Fatalf("按路径校验失败也应写命令行审计：%+v", row)
	}
}
