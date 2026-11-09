// seedvault 是 Playwright 浏览器测试的造库工具：在指定目录里生成一个可
// 直接打开的单机 vault（两台服务器、一个 profile、一条初始授权），并写好
// 单机角色状态。测试进程用它准备临时加密库，再启动 `sshmgr ui`。
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "用法：seedvault <临时库目录>")
		os.Exit(2)
	}
	if err := seed(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func seed(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	vaultDir := filepath.Join(dir, "ssh-manager")
	if err := os.MkdirAll(vaultDir, 0o700); err != nil {
		return err
	}
	backupDir := filepath.Join(dir, "backups")
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(backupDir, ".ssh-manager-backup-marker"), nil, 0o600); err != nil {
		return err
	}
	passPath := filepath.Join(dir, "backup.pass")
	if err := os.WriteFile(passPath, []byte("browser-test-backup-pass"), 0o600); err != nil {
		return err
	}
	key, err := store.GenerateMasterKey()
	if err != nil {
		return err
	}
	keyPath := filepath.Join(dir, "master.key.plain")
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(dir, "store.db"), key)
	if err != nil {
		return err
	}
	defer st.Close()
	credentialID, err := st.SetCredential(&models.Credential{
		Type: models.CredPassword, Secret: []byte("browser-test-password"),
	})
	if err != nil {
		return err
	}
	device, err := st.AddDevice("工作站", "浏览器测试设备", "")
	if err != nil {
		return err
	}
	webID, err := st.AddServer(&models.Server{
		Name: "web-1", Host: "192.0.2.10", Port: 22, User: "owner", Role: "运维网站",
		DeviceID:   device.ID,
		AuthMethod: models.AuthPassword, CredentialID: credentialID,
	})
	if err != nil {
		return err
	}
	if _, err := st.AddServer(&models.Server{
		Name: "db-1", Host: "192.0.2.11", Port: 22, User: "owner",
		AuthMethod: models.AuthPassword, CredentialID: credentialID,
	}); err != nil {
		return err
	}
	oldID, err := st.AddServerWithCredentials(&models.Server{
		Name: "old-1", Host: "192.0.2.12", Port: 22, User: "owner", Role: "待删除",
	}, &models.Credential{
		Type: models.CredPassword, Secret: []byte("browser-test-old-password"),
	}, nil)
	if err != nil {
		return err
	}
	profileID, err := st.AddProfile("运维")
	if err != nil {
		return err
	}
	if err := st.GrantServers(profileID, []string{webID, oldID}); err != nil {
		return err
	}
	if err := st.AddSecretWithEnvironmentName("MATRIX_TOKEN", "浏览器测试机密项", []byte("browser-test-secret-value"), "MATRIX_TOKEN_FILE", store.CLISource()); err != nil {
		return err
	}
	if err := st.AllowSecrets([]string{"MATRIX_TOKEN"}, []string{"db-1"}, store.CLISource()); err != nil {
		return err
	}
	if err := st.ReplaceSoftwareObservedItems("工作站", store.SoftwareObservedWrite{
		Items: []models.SoftwareObservedItem{
			{Category: models.SoftwareCategoryOperatingSystem, Name: "Ubuntu", Version: "24.04.1 LTS"},
			{Category: models.SoftwareCategorySystemPackage, Name: "base-files", Version: "13.5ubuntu3"},
			{Category: models.SoftwareCategorySystemPackage, Name: "zlib1g", Version: "1:1.3.dfsg-3.1ubuntu1"},
		},
		ServerID: webID, CommandSetVersion: "linux-v1", SystemType: "ubuntu",
	}, store.CLISource()); err != nil {
		return err
	}
	for i := 0; i < 120; i++ {
		if err := st.WriteAudit(store.AuditRow{
			TS: time.Now().Add(-time.Duration(i) * time.Minute), Action: "e2e.audit",
			Command: fmt.Sprintf("浏览器测试审计记录 %d", i+1), Status: "ok",
			Source: store.AuditSourceCLI,
		}); err != nil {
			return err
		}
	}
	return roles.Save(roles.State{Role: roles.RoleStandalone, SetupComplete: true})
}
