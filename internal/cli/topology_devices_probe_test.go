package cli

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/proberesults"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vaultio"
)

func startProbeListener(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	return listener.Addr().(*net.TCPAddr).Port
}

func seedProbeTopology(t *testing.T, st *store.Store) {
	t.Helper()
	if _, err := st.AddSite("家A", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "家A局域网", Type: models.NetworkPhysical, SiteName: "家A"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "NetBird", Type: models.NetworkVirtual}); err != nil {
		t.Fatal(err)
	}
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
	for _, membership := range []struct {
		device, network, address string
	}{
		{"NAS", "家A局域网", "127.0.0.1"},
		{"NAS", "NetBird", "::1"},
		{"笔记本", "家A局域网", "127.0.0.2"},
		{"笔记本", "NetBird", "::2"},
	} {
		if _, err := st.AddNetworkMembership(membership.device, membership.network, membership.address); err != nil {
			t.Fatal(err)
		}
	}
}

// TestDevicesProbeStandaloneWritesAuditAndResults: 单机部署上 `devices
// probe` 输出与探测服务相同的文本，审计来源记为命令行，探测结果写进本机
// 状态目录。
func TestDevicesProbeStandaloneWritesAuditAndResults(t *testing.T) {
	path, masterKey := withCliStoreEnv(t)
	configRoot := t.TempDir()
	withEnv(t, map[string]string{
		"APPDATA":            configRoot,
		"XDG_CONFIG_HOME":    configRoot,
		"SSHMGR_CONFIG_ROOT": configRoot,
	})
	if err := roles.Save(roles.State{Role: roles.RoleStandalone, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(path, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	seedProbeTopology(t, st)
	laptopID := ""
	devices, _ := st.ListDevices(store.DeviceFilter{})
	for _, device := range devices {
		if device.Name == "笔记本" {
			laptopID = device.ID
		}
	}
	st.Close()
	openPort := startProbeListener(t)

	out := runCli(t, "devices", "probe", "--from", "self", fmt.Sprintf("NAS:%d", openPort))
	if !strings.Contains(out, fmt.Sprintf("NAS:%d  NetBird 第1个地址  ✓", openPort)) {
		t.Fatalf("probe output missing the successful line:\n%s", out)
	}
	if strings.Contains(out, "127.0.0.1") || strings.Contains(out, "::1") {
		t.Fatalf("probe output must not expose addresses:\n%s", out)
	}

	auditStore, err := store.Open(path, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer auditStore.Close()
	rows, err := auditStore.AuditRows(1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("audit rows = %+v %v", rows, err)
	}
	if rows[0].Action != "probe-connectivity" || rows[0].Source != store.AuditSourceCLI || rows[0].Status != "ok" {
		t.Fatalf("audit row = %+v", rows[0])
	}
	observations, err := proberesults.NewFileProbeResults(filepath.Join(filepath.Dir(path), "probe-results.json")).List(laptopID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 2 {
		t.Fatalf("observations = %+v, want both addresses", observations)
	}
}

// seedProbeClientCache 写一个客户端实例缓存：本机设备是「笔记本」，目标
// 设备「NAS」的家A局域网地址指向本机回环地址，NetBird 地址指向 IPv6 回环
// 地址。
func seedProbeClientCache(t *testing.T) string {
	t.Helper()
	cacheDir := t.TempDir()
	sourceDir := t.TempDir()
	masterKey, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.Open(filepath.Join(sourceDir, "source.db"), masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	profileID, err := source.AddProfile("客户端探测")
	if err != nil {
		t.Fatal(err)
	}
	laptopTokenID, _, err := source.AddCacheToken("laptop", profileID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.AddSite("家A", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := source.AddNetwork(models.Network{Name: "家A局域网", Type: models.NetworkPhysical, SiteName: "家A"}); err != nil {
		t.Fatal(err)
	}
	if _, err := source.AddNetwork(models.Network{Name: "NetBird", Type: models.NetworkVirtual}); err != nil {
		t.Fatal(err)
	}
	if _, err := source.AddDevice("NAS", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := source.AddDevice("笔记本", "", "laptop"); err != nil {
		t.Fatal(err)
	}
	for _, membership := range []struct {
		device, network, address string
	}{
		{"NAS", "家A局域网", "127.0.0.1"},
		{"NAS", "NetBird", "::1"},
		{"笔记本", "家A局域网", "127.0.0.2"},
		{"笔记本", "NetBird", "::2"},
	} {
		if _, err := source.AddNetworkMembership(membership.device, membership.network, membership.address); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := source.ExportSnapshotForProfile(profileID, true, laptopTokenID)
	if err != nil {
		t.Fatal(err)
	}
	laptopDevice, err := source.GetDevice("笔记本")
	if err != nil || laptopDevice == nil {
		t.Fatalf("read laptop device: %v %v", laptopDevice, err)
	}
	metaBlob, err := json.Marshal(map[string]string{"device_id": laptopDevice.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "cache.meta.json"), metaBlob, 0o600); err != nil {
		t.Fatal(err)
	}
	plaintext, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	cacheKey, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	blob, err := vaultio.EncryptWithKey(cacheKey, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "cache.bin"), blob, 0o600); err != nil {
		t.Fatal(err)
	}
	authBlob, err := json.Marshal(clientops.CacheCred{
		URL: "https://serve.example.internal:7878", Token: "code-laptop",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "cache.auth.json"), authBlob, 0o600); err != nil {
		t.Fatal(err)
	}
	memoryKeychain := withDEK(t)
	if err := memoryKeychain.Set(cacheKey); err != nil {
		t.Fatal(err)
	}
	configRoot := t.TempDir()
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         filepath.Join(t.TempDir(), "no-vault.db"),
		"SSHMGR_CACHE_DIR":     cacheDir,
		"SSHMGR_MASTERKEY_HEX": "",
		"SSHMGR_FILEKEY_PATH":  filepath.Join(t.TempDir(), "no-such-master.key"),
		"APPDATA":              configRoot,
		"XDG_CONFIG_HOME":      configRoot,
		"SSHMGR_CONFIG_ROOT":   configRoot,
	})
	if err := roles.Save(roles.State{Role: roles.RoleClient, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	return cacheDir
}

// TestDevicesProbeClientInstanceWritesSidecarAudit: 客户端实例从本地加密快照
// 取拓扑执行探测，审计写进该实例的旁车审计文件，来源记为命令行。
func TestDevicesProbeClientInstanceWritesSidecarAudit(t *testing.T) {
	cacheDir := seedProbeClientCache(t)
	openPort := startProbeListener(t)

	out := runCli(t, "devices", "probe", "--from", "self", fmt.Sprintf("NAS:%d", openPort))
	if !strings.Contains(out, fmt.Sprintf("NAS:%d  NetBird 第1个地址  ✓", openPort)) {
		t.Fatalf("client probe output missing the successful line:\n%s", out)
	}
	auditBlob, err := os.ReadFile(filepath.Join(cacheDir, "cache-audit.log"))
	if err != nil {
		t.Fatalf("read sidecar audit: %v", err)
	}
	line := strings.TrimSpace(string(auditBlob))
	var row map[string]any
	if err := json.Unmarshal([]byte(line), &row); err != nil {
		t.Fatalf("sidecar audit line is not JSON: %q", line)
	}
	if row["action"] != "probe-connectivity" || row["source"] != store.AuditSourceCLI {
		t.Fatalf("sidecar audit row = %v", row)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "probe-results.json")); err != nil {
		t.Fatalf("probe results file missing in the instance directory: %v", err)
	}
}
