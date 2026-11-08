// seedvault 是 Playwright 浏览器测试的造库工具：在指定目录里生成一个可
// 直接打开的单机 vault（两台服务器、一个 profile、一条初始授权），并写好
// 单机角色状态。测试进程用它准备临时加密库，再启动 `sshmgr ui`。
package main

import (
	"fmt"
	"os"
	"path/filepath"

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
	webID, err := st.AddServer(&models.Server{
		Name: "web-1", Host: "192.0.2.10", Port: 22, User: "owner",
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
	profileID, err := st.AddProfile("运维")
	if err != nil {
		return err
	}
	if err := st.GrantServers(profileID, []string{webID}); err != nil {
		return err
	}
	return roles.Save(roles.State{Role: roles.RoleStandalone, SetupComplete: true})
}
