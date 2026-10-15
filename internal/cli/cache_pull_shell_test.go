package cli

// 命令策略·服务器 shell 类型字段(ticket #14)随 cache pull 到达客户端的
// 端到端测试:权威端 serve 下发的裁剪快照(ExportSnapshotForProfile)携带
// 字段 → cache pull 落盘 cache.bin → 客户端经 LoadCacheSnapshotFor 读回 →
// 载入只读镜像库后 GetServerByName 读到同一标注。写法沿用
// cache_test.go 的 standUpServe + TestCachePull_WritesEncryptedCacheAndMeta。

import (
	"bytes"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// standUpServeWithShell spins a ServeRunner over a store seeded with one
// PowerShell-marked server; returns the plaintext URL + a valid device code
// bound to a profile granting that server.
func standUpServeWithShell(t *testing.T) (url, cacheToken string) {
	t.Helper()
	dir := t.TempDir()
	mk, _ := store.GenerateMasterKey()
	st, err := store.Open(filepath.Join(dir, "serve.db"), mk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	srvID, err := st.AddServer(&models.Server{
		Name: "winbox", Host: "192.0.2.10", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: cid,
		ShellType: models.ShellPowerShell,
	})
	if err != nil {
		t.Fatal(err)
	}
	profID, err := st.AddProfile("team-shell")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profID, []string{srvID}); err != nil {
		t.Fatal(err)
	}
	r, err := mcpserver.NewServeRunner(st)
	if err != nil {
		t.Fatalf("NewServeRunner: %v", err)
	}
	t.Cleanup(r.Close)
	srv := httptest.NewServer(r.HTTPHandler())
	t.Cleanup(srv.Close)
	_, code, err := st.AddCacheToken("laptop-shell", profID)
	if err != nil {
		t.Fatal(err)
	}
	return srv.URL, code
}

// mk0 生成镜像库用的主密钥(store.Open 需要)。
func mk0(t *testing.T) []byte {
	t.Helper()
	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	return mk
}

// TestCachePull_CarriesShellType: pull 之后客户端读回的快照带 shell 类型,
// 载入后的镜像库里 GetServerByName 读到 PowerShell(客户端执行侧据此选
// 拆解语法)。
func TestCachePull_CarriesShellType(t *testing.T) {
	url, code := standUpServeWithShell(t)
	withDEK(t)
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": cacheDir})

	root := NewRootCmd()
	root.SetArgs([]string{"cache", "pull", "--url", url, "--token", code, "--allow-plaintext"})
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	if err := root.Execute(); err != nil {
		t.Fatalf("cache pull: %v (out=%s)", err, out.String())
	}

	snap, err := clientops.LoadCacheSnapshotFor("")
	if err != nil {
		t.Fatalf("LoadCacheSnapshotFor: %v", err)
	}
	found := false
	for _, sv := range snap.Servers {
		if sv.Name == "winbox" {
			found = true
			if sv.ShellType != "powershell" {
				t.Fatalf("pulled snapshot winbox ShellType = %q, want powershell", sv.ShellType)
			}
		}
	}
	if !found {
		t.Fatalf("pulled snapshot missing winbox: %+v", snap.Servers)
	}

	// 载入镜像库(桥·缓存形态的装配 guts),字段原样可读。
	hyd, err := store.Open(filepath.Join(t.TempDir(), "hyd.db"), mk0(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hyd.Close() })
	if err := hyd.ImportSnapshot(snap); err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	got, err := hyd.GetServerByName("winbox")
	if err != nil || got == nil {
		t.Fatalf("hydrated winbox: %v %v", got, err)
	}
	if got.ShellType != models.ShellPowerShell {
		t.Fatalf("hydrated winbox ShellType = %q, want powershell", got.ShellType)
	}
}
