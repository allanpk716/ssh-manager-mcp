package dialplan

import (
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
)

// TestStoreSourceReadsCurrentTopology: 存储来源每次被询问时都从当前存储读
// 设备与网络成员；成员地址热更新后，同一次进程里的下一次询问立即看到新
// 地址。
func TestStoreSourceReadsCurrentTopology(t *testing.T) {
	vaultDir := t.TempDir()
	t.Setenv("SSHMGR_STORE", filepath.Join(vaultDir, "store.db"))
	if err := roles.Save(roles.State{Role: roles.RoleStandalone, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	masterKey, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(vaultDir, "store.db"), masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if _, err := st.AddSite("家A", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "家A局域网", Type: models.NetworkPhysical, SiteName: "家A"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("工作站", "", ""); err != nil {
		t.Fatal(err)
	}
	local, err := st.AddDevice("笔记本", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := roles.SetLocalDeviceID(local.ID); err != nil {
		t.Fatal(err)
	}
	serverResult, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: "工作站 SSH", Host: "198.51.100.10", Port: 22, User: "u",
	}, nil, nil, "工作站")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership("工作站", "家A局域网", "192.0.2.10"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership("笔记本", "家A局域网", "192.0.2.2"); err != nil {
		t.Fatal(err)
	}
	srv, err := st.GetServer(serverResult.ServerID)
	if err != nil {
		t.Fatal(err)
	}

	source := NewStoreSource(st, "")
	topology, err := source.ServerDialTopology(srv)
	if err != nil {
		t.Fatal(err)
	}
	if !topology.DialerKnown || len(topology.TargetMemberships) != 1 || topology.TargetMemberships[0].Address != "192.0.2.10" {
		t.Fatalf("topology = %+v, want the current target membership", topology)
	}

	if _, err := st.EditNetworkMembership("工作站", "家A局域网", "192.0.2.11"); err != nil {
		t.Fatal(err)
	}
	topology, err = source.ServerDialTopology(srv)
	if err != nil {
		t.Fatal(err)
	}
	if len(topology.TargetMemberships) != 1 || topology.TargetMemberships[0].Address != "192.0.2.11" {
		t.Fatalf("refreshed topology = %+v, want the edited address", topology)
	}
}
