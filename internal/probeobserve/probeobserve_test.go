package probeobserve

import (
	"os"
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/proberesults"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
)

func withStandaloneRole(t *testing.T) string {
	t.Helper()
	vaultDir := t.TempDir()
	t.Setenv("SSHMGR_STORE", filepath.Join(vaultDir, "store.db"))
	if err := roles.Save(roles.State{Role: roles.RoleStandalone, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	return vaultDir
}

// TestPathForPlacesFileByRole: 客户端实例的探测结果文件放实例目录，单机
// 部署与权威端放本机状态目录（role.json 所在目录），与成功地址缓存同类。
func TestPathForPlacesFileByRole(t *testing.T) {
	vaultDir := withStandaloneRole(t)
	path, ok, err := PathFor("")
	if err != nil || !ok {
		t.Fatalf("PathFor(standalone) = %q %v %v, want a writable path", path, ok, err)
	}
	if filepath.Dir(path) != vaultDir || filepath.Base(path) != "probe-results.json" {
		t.Fatalf("standalone path = %q, want %s/probe-results.json", path, vaultDir)
	}

	configRoot := t.TempDir()
	t.Setenv("SSHMGR_STORE", filepath.Join(t.TempDir(), "absent-store.db"))
	t.Setenv("APPDATA", configRoot)
	t.Setenv("XDG_CONFIG_HOME", configRoot)
	t.Setenv("SSHMGR_CONFIG_ROOT", configRoot)
	t.Setenv("SSHMGR_CACHE_DIR", "")
	if err := roles.Save(roles.State{Role: roles.RoleClient, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	path, ok, err = PathFor("laptop")
	if err != nil || !ok {
		t.Fatalf("PathFor(client) = %q %v %v, want a writable path", path, ok, err)
	}
	if filepath.Base(filepath.Dir(path)) != "laptop" {
		t.Fatalf("client path = %q, want inside the laptop instance directory", path)
	}
}

// TestDialObserverRecordsTopologyAttempts: 观察者把一次候选地址观察写进探测
// 结果文件：起点设备取当前实例登记的本机设备，目标设备取服务器条目关联
// 的设备，端口取条目端口。
func TestDialObserverRecordsTopologyAttempts(t *testing.T) {
	vaultDir := withStandaloneRole(t)
	masterKey, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(vaultDir, "store.db"), masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.AddDevice("NAS", "", ""); err != nil {
		t.Fatal(err)
	}
	laptop, err := st.AddDevice("笔记本", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := roles.SetLocalDeviceID(laptop.ID); err != nil {
		t.Fatal(err)
	}
	serverResult, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: "NAS SSH", Host: "192.0.2.10", Port: 22, User: "u",
	}, nil, nil, "NAS")
	if err != nil {
		t.Fatal(err)
	}
	server, err := st.GetServer(serverResult.ServerID)
	if err != nil {
		t.Fatal(err)
	}

	path, ok, err := PathFor("")
	if err != nil || !ok {
		t.Fatalf("PathFor = %q %v %v", path, ok, err)
	}
	observer := NewDialObserver(st, "", path)
	observer.ObserveDialAttempt(server, sshbroker.DialAddress{
		Address: "192.0.2.10", NetworkName: "家A局域网", NetworkSequence: 1,
		Source: sshbroker.DialAddressPhysical,
	}, sshbroker.DialAttemptOutcome{
		OK: false, Category: models.ProbeErrorTimeout, Duration: 1500_000_000,
	})

	results := proberesults.NewFileProbeResults(path)
	got, err := results.List(laptop.ID, server.DeviceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("observations = %+v, want one record", got)
	}
	if got[0].OriginDeviceID != laptop.ID || got[0].TargetDeviceID != server.DeviceID ||
		got[0].Network != "家A局域网" || got[0].Address != "192.0.2.10" ||
		got[0].Port != 22 || got[0].Category != models.ProbeErrorTimeout ||
		got[0].Source != proberesults.SourceDial {
		t.Fatalf("observation = %+v, want the dial attempt under the local device origin", got[0])
	}
}

// TestDialObserverSkipsUnknownLocalDevice: 当前实例没有登记设备时不写任何
// 记录，也不报错——真实 SSH 连接照常进行。
func TestDialObserverSkipsUnknownLocalDevice(t *testing.T) {
	vaultDir := withStandaloneRole(t)
	path := filepath.Join(vaultDir, "probe-results.json")
	observer := NewDialObserver(nil, "", path)
	observer.ObserveDialAttempt(&models.Server{ID: "srv-a", DeviceID: "device-target", Port: 22},
		sshbroker.DialAddress{Address: "192.0.2.10", NetworkName: "家A局域网", Source: sshbroker.DialAddressPhysical},
		sshbroker.DialAttemptOutcome{OK: true})
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("no observation must be written when the local device is unknown, stat err = %v", err)
	}
}
