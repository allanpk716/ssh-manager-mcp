// seedclient 是软件清单浏览器测试的造客户端工具：在指定目录里生成一个可
// 解密的本地快照缓存，并写好客户端角色状态。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
	if strings.TrimSpace(os.Getenv("SSHMGR_E2E_TOPOLOGY")) == "1" {
		snapshot.Sites = []store.SnapshotSite{{ID: "site-home", Name: "家A", Note: "物理场所"}}
		snapshot.Networks = []store.SnapshotNetwork{
			{ID: "net-home", Name: "家A局域网", Type: "physical", SiteID: "site-home",
				CIDR: "192.168.1.0/24", BandwidthMbps: 1000, Metered: true},
			{ID: "net-virtual", Name: "虚拟网", Type: "virtual", Note: "跨站点"},
		}
		snapshot.Devices = []store.SnapshotDevice{
			{ID: "device-server", Name: "客户端服务器设备"},
			{ID: "device-laptop", Name: "笔记本", ClientTokenID: "client-token", ClientName: "laptop-client"},
			{ID: "device-nas", Name: "NAS"},
		}
		snapshot.Servers[0].DeviceID = "device-server"
		snapshot.NetworkMemberships = []store.SnapshotNetworkMembership{
			{DeviceID: "device-server", NetworkID: "net-home", Address: "192.168.1.20", RegistrationOrder: 1},
			{DeviceID: "device-laptop", NetworkID: "net-virtual", Address: "100.64.0.9", RegistrationOrder: 2},
			{DeviceID: "device-nas", NetworkID: "net-home", Address: "192.168.1.30", RegistrationOrder: 3},
		}
		snapshot.DeviceLayoutCoordinates = []store.SnapshotDeviceLayoutCoordinate{
			{ViewKey: "physical", DeviceID: "device-server", X: 12.5, Y: -3, CreatedAt: 1000, UpdatedAt: 1001},
		}
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
	if strings.TrimSpace(os.Getenv("SSHMGR_E2E_TOPOLOGY")) == "1" {
		meta, err := json.Marshal(map[string]string{"device_id": "device-laptop"})
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(cacheDir, "cache.meta.json"), meta, 0o600); err != nil {
			return err
		}
	}
	return roles.Save(roles.State{Role: roles.RoleClient, SetupComplete: true})
}
