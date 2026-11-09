package cli

// 票 #185：客户端命令行的软件清单查看与搜索走权威端在线接口。权威端不
// 可达时输出固定错误文本并使用专门退出码；本地没有任何软件清单数据，
// 不存在回退路径。

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/softwareinventory"
	"ssh-manager-mcp/internal/store"
)

// newInventoryClientFixture 造一个在内存里的权威端（含一台可见设备与一条
// 声明）和一个客户端缓存目录（cache.auth.json 指向该权威端）。返回的关
// 闭函数用于模拟权威端停机。
func newInventoryClientFixture(t *testing.T) (stop func()) {
	t.Helper()
	vaultDirectory := t.TempDir()
	masterKey, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(vaultDirectory, "authority.db"), masterKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
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
	profileID, err := st.AddProfile("inventory-profile")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{added.ServerID}); err != nil {
		t.Fatal(err)
	}
	if _, err := softwareinventory.New(st).AddDeclaration(softwareinventory.Declaration{
		Device: "gpu-设备", Category: softwareinventory.CategoryServiceProgram,
		Name: "nginx", Version: "1.25.3",
		ListeningPorts: []softwareinventory.ListeningPort{{
			Address: "0.0.0.0", Port: 80, Protocol: "tcp",
			Exposure: softwareinventory.ExposureAllAddresses,
		}},
	}, softwareinventory.CLISource()); err != nil {
		t.Fatal(err)
	}
	runner, err := mcpserver.NewServeRunner(st)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runner.Close)
	server := httptest.NewTLSServer(runner.HTTPHandler())
	t.Cleanup(server.Close)
	_, token, err := st.AddCacheToken("laptop", profileID)
	if err != nil {
		t.Fatal(err)
	}
	pin := mcpserver.SPKIFingerprint(server.Certificate())
	cacheDirectory := t.TempDir()
	credential := `{"url":"` + server.URL + `","token":"` + token + `","pin":"` + pin + `"}`
	if err := os.WriteFile(filepath.Join(cacheDirectory, "cache.auth.json"), []byte(credential), 0o600); err != nil {
		t.Fatal(err)
	}
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         filepath.Join(t.TempDir(), "absent.db"),
		"SSHMGR_MASTERKEY_HEX": "",
		"SSHMGR_FILEKEY_PATH":  filepath.Join(t.TempDir(), "absent.key"),
		"SSHMGR_CACHE_DIR":     cacheDirectory,
	})
	return server.Close
}

func TestInventoryClientViewsAndSearchesOnline(t *testing.T) {
	stop := newInventoryClientFixture(t)
	defer stop()

	view := mustInventoryCli(t, "", "inventory", "view", "gpu-设备")
	for _, want := range []string{"gpu-设备", "nginx", "1.25.3", "声明", "0.0.0.0:80/tcp", "监听所有地址"} {
		if !strings.Contains(view, want) {
			t.Fatalf("客户端查看缺少 %q：\n%s", want, view)
		}
	}
	search := mustInventoryCli(t, "", "inventory", "search", "NGINX")
	if !strings.Contains(search, "gpu-设备") || !strings.Contains(search, "nginx") {
		t.Fatalf("客户端搜索结果：\n%s", search)
	}
}

func TestInventoryClientUnreachableMapsDedicatedExitCode(t *testing.T) {
	stop := newInventoryClientFixture(t)
	stop() // 权威端停机

	for _, args := range [][]string{
		{"inventory", "view", "gpu-设备"},
		{"inventory", "search", "nginx"},
	} {
		_, _, err := runInventoryRoot(t, "", args...)
		if err == nil {
			t.Fatalf("%v：权威端不可达时应报错", args)
		}
		if err.Error() != clientops.SoftwareInventoryUnreachableText {
			t.Fatalf("%v：错误文本不是固定契约：%v", args, err)
		}
		if code := ExitCodeFor(err); code != 4 {
			t.Fatalf("%v：退出码 = %d，应为 4", args, code)
		}
	}
}
