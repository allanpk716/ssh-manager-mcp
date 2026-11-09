// seedclient 是软件清单浏览器测试的造客户端工具：在指定目录里生成一个可
// 解密的本地快照缓存，并写好客户端角色状态。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vaultio"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "用法：seedclient <临时目录>")
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
	cacheDir := filepath.Join(dir, "cache")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return err
	}
	key, err := store.GenerateMasterKey()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "cache-dek.key"), key, 0o600); err != nil {
		return err
	}
	snapshot := store.Snapshot{
		Version: 1,
		Servers: []store.SnapshotServer{{
			ID: "client-server", Name: "客户端服务器", User: "owner",
			Host: "192.0.2.30", Port: 22, CredentialID: "credential",
			AuthMethod: string(models.AuthPassword), Role: "浏览器测试",
		}},
	}
	body, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	encrypted, err := vaultio.EncryptWithKey(key, body)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "cache.bin"), encrypted, 0o600); err != nil {
		return err
	}
	return roles.Save(roles.State{Role: roles.RoleClient, SetupComplete: true})
}
