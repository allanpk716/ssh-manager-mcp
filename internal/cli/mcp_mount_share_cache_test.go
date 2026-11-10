package cli

import (
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

// cacheLinuxMountServer 模拟一台可被挂载的 Linux 服务器（客户端缓存面测试
// 专用；权威端工具面的版本见 internal/mcpserver/mount_share_test.go）。
type cacheLinuxMountServer struct {
	shareID       string
	credentialFmt string
}

func (f *cacheLinuxMountServer) handler(command string, stdin io.Reader) (string, string, int) {
	stdinBytes, _ := io.ReadAll(stdin)
	switch {
	case strings.TrimSpace(command) == "uname -s":
		return "Linux\n", "", 0
	case strings.Contains(command, "sshmgr_probe_one "):
		return "SSHMGR-PROBE 0 ok 2 \n", "", 0
	case strings.Contains(command, "SSHMGR-LINUX-MOUNT"):
		f.credentialFmt = "username=mount-user\npassword=" + string(stdinBytes)
		return "SSHMGR-LINUX-MOUNT ok\n", "", 0
	default:
		return "", "unexpected command: " + command, 90
	}
}

// seedCacheMountTarget 在权威端补齐共享存储挂载所需的登记：提供方与目标
// 设备、共同网络、带 sudo 的目标服务器、挂载密码机密项与共享登记。
func seedCacheMountTarget(t *testing.T, fx *cacheFwdFixture) (targetID string, fake *cacheLinuxMountServer) {
	t.Helper()
	fake = &cacheLinuxMountServer{}
	target, hostKey, targetCleanup := testsshd.Start(t, testsshd.Options{
		Password: "login-pw", SudoPassword: "sudo-pw", Exec: fake.handler,
	})
	t.Cleanup(targetCleanup)

	if _, err := fx.serve.AddDevice("NAS", "共享存储提供方", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.serve.AddDevice("linux-device", "挂载目标", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.serve.AddSite("办公室", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.serve.AddNetwork(models.Network{Name: "办公物理局域网", Type: models.NetworkPhysical, SiteName: "办公室"}); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.serve.AddNetwork(models.Network{Name: "办公虚拟局域网", Type: models.NetworkVirtual}); err != nil {
		t.Fatal(err)
	}
	for _, membership := range []struct {
		device  string
		network string
		address string
	}{
		{"NAS", "办公物理局域网", "192.0.2.10"},
		{"NAS", "办公虚拟局域网", "100.64.10.10"},
		{"linux-device", "办公物理局域网", "192.0.2.20"},
		{"linux-device", "办公虚拟局域网", "100.64.10.20"},
	} {
		if _, err := fx.serve.AddNetworkMembership(membership.device, membership.network, membership.address); err != nil {
			t.Fatal(err)
		}
	}
	loginCredential := &models.Credential{Type: models.CredPassword, Secret: []byte("login-pw")}
	sudoCredential := &models.Credential{Type: models.CredPassword, Secret: []byte("sudo-pw")}
	added, err := fx.serve.AddServerWithCredentialsToDevice(&models.Server{
		Name: "linux-web", Host: target[:strings.LastIndex(target, ":")],
		Port: portFromAddr(target), User: "deploy", ShellType: models.ShellPOSIX,
	}, loginCredential, sudoCredential, "linux-device")
	if err != nil {
		t.Fatal(err)
	}
	if err := fx.serve.GrantServers(fx.profID, []string{added.ServerID}); err != nil {
		t.Fatal(err)
	}
	// owner 已在权威端锚定目标主机密钥；客户端快照因此带着既有锚拨号，
	// 不触发「首次锚定只能经虚拟局域网」的限制。
	pinnedServer, err := fx.serve.GetServer(added.ServerID)
	if err != nil {
		t.Fatal(err)
	}
	if err := fx.serve.SaveEntryPin(pinnedServer, hostKey.Marshal()); err != nil {
		t.Fatal(err)
	}
	if err := fx.serve.AddSecret(
		"DATASETS_MOUNT_PASSWORD", "共享挂载密码", []byte("first-mount-password"), store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	if err := fx.serve.AllowSecrets(
		[]string{"DATASETS_MOUNT_PASSWORD"}, []string{"linux-web"}, store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.serve.AddSharedStorage(store.SharedStorageInput{
		Name: "datasets", Device: "NAS", User: "mount-user",
		PasswordSecret: "DATASETS_MOUNT_PASSWORD", Drive: "Z",
		Directories: []models.SharedStorageDirectory{
			{Purpose: "数据集", Path: "datasets"},
		},
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	share, err := fx.serve.GetSharedStorage("datasets")
	if err != nil {
		t.Fatal(err)
	}
	fake.shareID = share.ID
	return added.ServerID, fake
}

// TestMountShareCacheE2E_SyncsStaleSnapshotBeforeMount：真实权威端、真实挂载
// 前同步器与假 Linux 目标服务器的端到端验收：客户端快照落后时，mount_share
// 先查修订号、再全量同步，然后用换值后的挂载密码完成挂载。
func TestMountShareCacheE2E_SyncsStaleSnapshotBeforeMount(t *testing.T) {
	fx := newCacheFwdFixture(t)
	targetID, fake := seedCacheMountTarget(t, fx)
	setUpPollableCache(t, fx)

	// owner 在权威端换挂载密码：共享版本与快照修订号前进，客户端落后。
	if err := fx.serve.SetSecretValue(
		"DATASETS_MOUNT_PASSWORD", []byte("rotated-mount-password"), store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}

	snap, err := clientops.LoadCacheSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	rel := clientops.NewCacheReloader(time.Hour)
	syncer, err := clientops.NewMountShareSnapshotSyncerFor("")
	if err != nil {
		t.Fatal(err)
	}
	broker, tunnels, tasks, _, cleanupBroker, err := mcpserver.NewCacheBrokerWithRefresher(
		fx.projToken, snap, filepath.Join(t.TempDir(), "mount-share-audit.jsonl"), rel.Check,
		clientops.ForwardingHostKeys(mustForwarder(t, fx.cred)), "laptop", nil, syncer,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanupBroker)
	t.Cleanup(func() { tunnels.CloseAll(); tasks.CloseAll() })
	session := attachMCPClient(t, broker)

	versionQueries := fx.versionCount()
	snapshotPulls := fx.snapshotCount()
	texts := callToolTexts(t, session, "mount_share", map[string]any{
		"share": "datasets", "server_id": targetID,
	})
	if len(texts) == 0 {
		t.Fatal("mount_share 没有返回文本")
	}
	output := map[string]any{}
	if err := json.Unmarshal([]byte(texts[len(texts)-1]), &output); err != nil {
		t.Fatalf("解析 mount_share 输出失败：%v\n%s", err, strings.Join(texts, "\n"))
	}
	if output["version"] != float64(2) {
		t.Fatalf("挂载应使用同步后的共享版本 2，实际输出：%#v", output)
	}
	if want := "username=mount-user\npassword=rotated-mount-password\n"; fake.credentialFmt != want {
		t.Fatalf("目标服务器凭据文件 = %q，期待 %q", fake.credentialFmt, want)
	}
	if got := fx.versionCount() - versionQueries; got != 1 {
		t.Fatalf("挂载前修订号查询次数 = %d，应为 1", got)
	}
	if got := fx.snapshotCount() - snapshotPulls; got != 1 {
		t.Fatalf("挂载前全量同步次数 = %d，应为 1", got)
	}
	for _, text := range texts {
		if strings.Contains(text, "first-mount-password") || strings.Contains(text, "rotated-mount-password") ||
			strings.Contains(text, "192.0.2.10") {
			t.Fatalf("工具结果泄漏密码或地址：%s", text)
		}
	}
}
