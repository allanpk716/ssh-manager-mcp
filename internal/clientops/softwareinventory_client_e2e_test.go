package clientops

// 票 #187 的进程内端到端冒烟：真实测试 SSH 服务器回放 Ubuntu 夹具，客
// 户端实例用本地缓存里的 SSH 凭据采集，经权威端上交后可在线查询，审计落
// 在权威端；客户端快照本身不携带软件清单。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
	"ssh-manager-mcp/internal/vaultio"
)

func TestSoftwareInventoryClientCollectorEndToEndWithTestSSHServer(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("..", "softwareinventory", "testdata", "posix", "ubuntu2404-full.txt"))
	if err != nil {
		t.Fatal(err)
	}
	addr, hostKey, stopSSH := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec: func(string, io.Reader) (string, string, int) {
			return string(fixture), "", 0
		},
	})
	defer stopSSH()
	host := addr[:strings.LastIndex(addr, ":")]
	port, err := strconv.Atoi(addr[strings.LastIndex(addr, ":")+1:])
	if err != nil {
		t.Fatal(err)
	}

	authorityKey, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "authority.db"), authorityKey)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.AddDevice("gpu-设备", "", ""); err != nil {
		t.Fatal(err)
	}
	credential := &models.Credential{Type: models.CredPassword, Secret: []byte("pw")}
	added, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: "gpu", Host: host, Port: port, User: "u",
		ShellType: models.ShellPOSIX,
	}, credential, nil, "gpu-设备")
	if err != nil {
		t.Fatal(err)
	}
	server, err := st.GetServer(added.ServerID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveEntryPin(server, hostKey.Marshal()); err != nil {
		t.Fatal(err)
	}
	profileID, err := st.AddProfile("采集 profile")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{server.ID}); err != nil {
		t.Fatal(err)
	}
	tokenID, token, err := st.AddCacheToken("laptop", profileID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSoftwareCollectionCollector("gpu-设备", "laptop", server.ID, store.CLISource()); err != nil {
		t.Fatal(err)
	}

	runner, err := mcpserver.NewServeRunner(st)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	authority := httptest.NewTLSServer(runner.HTTPHandler())
	defer authority.Close()
	pin := mcpserver.SPKIFingerprint(authority.Certificate())

	cacheDir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DIR", cacheDir)
	memDEK := withDEK(t)
	snapshot, err := st.ExportSnapshotForProfile(profileID, false, tokenID)
	if err != nil {
		t.Fatal(err)
	}
	dek := dekOf(t, memDEK)
	encrypted, err := vaultio.EncryptWithKey(dek, mustJSON(t, snapshot))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "cache.bin"), encrypted, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteCacheCredFor("", &CacheCred{
		URL: authority.URL, Token: token, Pin: pin,
	}); err != nil {
		t.Fatal(err)
	}

	report, err := CollectSoftwareInventoryForInstance(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if report.Due != 1 || report.Succeeded != 1 || report.Failed != 0 {
		t.Fatalf("客户端端到端采集报告 = %+v", report)
	}
	items, err := st.SoftwareObservedItemsForDevice("gpu-设备")
	if err != nil || len(items) == 0 {
		t.Fatalf("权威端没有收到实测条目：%d %v", len(items), err)
	}
	audit, err := st.QueryAudit(store.AuditFilter{Actions: []string{"software-collect"}})
	if err != nil || len(audit) != 1 {
		t.Fatalf("权威端采集审计行数 = %d err=%v", len(audit), err)
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(audit[0].Command), &summary); err != nil {
		t.Fatal(err)
	}
	if summary["collector"] != "laptop" || summary["result"] != "ok" {
		t.Fatalf("权威端采集审计 = %v", summary)
	}
	if len(snapshot.SoftwareObservedItems) != 0 || len(snapshot.SoftwareCollectionStatuses) != 0 {
		t.Fatal("客户端快照不应携带软件清单或采集状态")
	}

	forwarder, err := NewSoftwareInventoryForwarder(CacheCred{
		URL: authority.URL, Token: token, Pin: pin,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer forwarder.Close()
	result, err := forwarder.QuerySoftwareInventory(mcpserver.QuerySoftwareInventoryInput{
		Device: "gpu-设备", IncludeSystemPackages: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Devices) != 1 || result.Devices[0].DeviceName != "gpu-设备" ||
		!strings.Contains(fmt.Sprint(result.Devices[0].Items), "openssh-server") {
		t.Fatalf("客户端在线查询权威端结果 = %+v", result.Devices)
	}
	authority.Close()
	_, err = forwarder.QuerySoftwareInventory(mcpserver.QuerySoftwareInventoryInput{Device: "gpu-设备"})
	if !IsSoftwareInventoryUnreachable(err) || !errors.Is(err, err) {
		t.Fatalf("权威端停机后的查询错误 = %v", err)
	}
}
