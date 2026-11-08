package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/proberesults"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
)

// TestProbeConnectivityUsesBridgeInstance: 桥进程所在实例由命令行固定在
// SSHMGR_MCP_INSTANCE；客户端实例在非默认实例下 self 解析与探测结果文件
// 都落在该实例目录。
func TestProbeConnectivityUsesBridgeInstance(t *testing.T) {
	configRoot := t.TempDir()
	t.Setenv("APPDATA", configRoot)
	t.Setenv("XDG_CONFIG_HOME", configRoot)
	t.Setenv("SSHMGR_CONFIG_ROOT", configRoot)
	t.Setenv("SSHMGR_MCP_INSTANCE", "laptop2")
	if err := roles.Save(roles.State{Role: roles.RoleClient, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	instanceDirectory := filepath.Join(configRoot, "ssh-manager", "instances", "laptop2")
	if err := os.MkdirAll(instanceDirectory, 0o700); err != nil {
		t.Fatal(err)
	}

	vaultDir := t.TempDir()
	t.Setenv("SSHMGR_STORE", filepath.Join(vaultDir, "store.db"))
	masterKey, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(vaultDir, "store.db"), masterKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.AddSite("家A", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "家A局域网", Type: models.NetworkPhysical, SiteName: "家A"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("NAS", "", ""); err != nil {
		t.Fatal(err)
	}
	laptop, err := st.AddDevice("笔记本", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, membership := range []struct {
		device, network, address string
	}{
		{"笔记本", "家A局域网", "127.0.0.1"},
		{"NAS", "家A局域网", "127.0.0.2"},
	} {
		if _, err := st.AddNetworkMembership(membership.device, membership.network, membership.address); err != nil {
			t.Fatal(err)
		}
	}
	metaBlob, err := json.Marshal(map[string]string{"device_id": laptop.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instanceDirectory, "cache.meta.json"), metaBlob, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSHMGR_SSH_ADDRESS_TIMEOUT", "250ms")

	_, err = ProbeConnectivityForProfile(context.Background(), st, "proj-probe", "profile-probe",
		"self", []string{"NAS:1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	observations, err := proberesults.NewFileProbeResults(
		filepath.Join(instanceDirectory, "probe-results.json")).List(laptop.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 1 {
		t.Fatalf("observations = %+v, want one in the named instance directory", observations)
	}
}
