package clientops

// 转发器与权威端处理器的进程内端到端：同一份 JSON 字段在两端往返。真实
// 的设备码认证与 profile 可见范围由权威端测试覆盖，这里钉住两端契约不
// 漂移。

import (
	"net/http/httptest"
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/softwareinventory"
	"ssh-manager-mcp/internal/store"
)

func TestSoftwareInventoryForwarderAgainstServeHandler(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "serve.db"), mustClientopsMasterKey(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.AddDevice("gpu-设备", "", ""); err != nil {
		t.Fatal(err)
	}
	credentialID, err := st.SetCredential(&models.Credential{
		Type: models.CredPassword, Secret: []byte("pw"),
	})
	if err != nil {
		t.Fatal(err)
	}
	added, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: "gpu", Host: "192.0.2.10", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: credentialID,
	}, nil, nil, "gpu-设备")
	if err != nil {
		t.Fatal(err)
	}
	profileID, err := st.AddProfile("team-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{added.ServerID}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSite("总部", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSiteWithParent("机房", "", "总部"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{
		Name: "总部物理局域网", Type: models.NetworkPhysical, SiteName: "总部",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{
		Name: "跨地点虚拟局域网", Type: models.NetworkVirtual,
	}); err != nil {
		t.Fatal(err)
	}
	gpuSite := "机房"
	if _, err := st.EditDevice("gpu-设备", store.DeviceChanges{Site: &gpuSite}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership("gpu-设备", "总部物理局域网", "192.0.2.20"); err != nil {
		t.Fatal(err)
	}
	if _, err := softwareinventory.New(st).AddDeclaration(softwareinventory.Declaration{
		Device: "gpu-设备", Category: softwareinventory.CategoryServiceProgram,
		Name: "nginx", Version: "1.25.3",
	}, softwareinventory.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceSoftwareObservedItems("gpu-设备", store.SoftwareObservedWrite{
		Items: []models.SoftwareObservedItem{
			{Category: softwareinventory.CategorySystemPackage, Name: "openssl", Version: "3.0.2-0ubuntu1.18"},
			{Category: softwareinventory.CategorySystemPackage, Name: "openssl-tools", Version: "3.0.13-0ubuntu3.4"},
		},
		CommandSetVersion: "posix-v2", SystemType: "ubuntu",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	runner, err := mcpserver.NewServeRunner(st)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	serve := httptest.NewTLSServer(runner.HTTPHandler())
	defer serve.Close()
	_, token, err := st.AddCacheToken("laptop", profileID)
	if err != nil {
		t.Fatal(err)
	}
	pin := mcpserver.SPKIFingerprint(serve.Certificate())
	forwarder, err := NewSoftwareInventoryForwarder(CacheCred{
		URL: serve.URL, Token: token + ":" + pin, Pin: pin,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer forwarder.Close()

	result, err := forwarder.QuerySoftwareInventory(mcpserver.QuerySoftwareInventoryInput{
		Name: "openssl", ExactVersion: " 3.0.2-0ubuntu1.18 ",
		Site: "总部", Network: "总部物理局域网",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Devices) != 1 || result.Devices[0].DeviceName != "gpu-设备" ||
		len(result.Devices[0].Items) != 1 || result.Devices[0].Items[0].Version != "3.0.2-0ubuntu1.18" ||
		result.Devices[0].Items[0].Source != softwareinventory.SourceObserved ||
		result.Devices[0].Collection.NeverCollected {
		t.Fatalf("端到端查询结果 = %+v", result)
	}

	virtual, err := forwarder.QuerySoftwareInventory(mcpserver.QuerySoftwareInventoryInput{
		Name: "openssl", ExactVersion: "3.0.2-0ubuntu1.18",
		Site: "总部", Network: "跨地点虚拟局域网",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(virtual.Devices) != 0 {
		t.Fatalf("设备不是虚拟局域网成员时不应返回：%+v", virtual.Devices)
	}

	// 另一个未授权设备码：403 分支文本。
	otherProfile, err := st.AddProfile("team-b")
	if err != nil {
		t.Fatal(err)
	}
	_, otherToken, err := st.AddCacheToken("phone", otherProfile)
	if err != nil {
		t.Fatal(err)
	}
	otherForwarder, err := NewSoftwareInventoryForwarder(CacheCred{
		URL: serve.URL, Token: otherToken + ":" + pin, Pin: pin,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer otherForwarder.Close()
	_, err = otherForwarder.QuerySoftwareInventory(mcpserver.QuerySoftwareInventoryInput{
		Device: "gpu-设备",
	})
	if err == nil || err.Error() != softwareInventoryForbiddenText {
		t.Fatalf("范围外设备应返回 403 分支文本：%v", err)
	}
}

// mustClientopsMasterKey 造一个测试主密钥。
func mustClientopsMasterKey(t *testing.T) []byte {
	t.Helper()
	key, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	return key
}
