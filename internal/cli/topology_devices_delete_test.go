package cli

import (
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

func TestDevicesRemovePreviewSeparatesRemoteUnmountAndWindowsSnapshotSync(t *testing.T) {
	dbPath, masterKey := withCliStoreEnv(t)
	seedDeviceDeleteSharedStorage(t, dbPath, masterKey)
	st, err := store.Open(dbPath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.MoveServerToDevice("linux-web", "NAS"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	out := runCli(t, "devices", "rm", "NAS", "--yes")
	for _, want := range []string{
		"共享存储：",
		"datasets",
		"Linux 与 macOS 服务器（将删除配置并卸载）：",
		"linux-web（所属设备 NAS，系统类型 Linux 或 macOS）",
		"随设备一起删除，本次不会清理；如需清理，先运行 shares unmount datasets linux-web",
		"Windows 服务器（将随快照同步停止注入）：",
		"windows-web（所属设备 windows-device，系统类型 Windows）",
		"datasets / linux-web：未清理；服务器条目随设备一起删除，删除后无法再连接",
		"已删除设备 NAS",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("删除共享存储提供方设备输出缺少 %q：\n%s", want, out)
		}
	}
	if strings.Contains(out, "删除这些共享存储登记不会去服务器上卸载") {
		t.Fatalf("删除预览不应再说不会卸载：\n%s", out)
	}
	errText := runCliErr(t, "devices", "show", "NAS")
	if !strings.Contains(errText, "设备不存在") {
		t.Fatalf("远端清理失败不应回滚设备删除：%s", errText)
	}
}

func seedDeviceDeleteSharedStorage(t *testing.T, dbPath string, masterKey []byte) {
	t.Helper()
	st, err := store.Open(dbPath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, item := range []struct{ name, description string }{
		{name: "NAS", description: "共享存储提供方"},
		{name: "linux-device", description: "挂载目标"},
		{name: "windows-device", description: "挂载目标"},
	} {
		if _, err := st.AddDevice(item.name, item.description, ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []struct {
		server    *models.Server
		device    string
		shellType models.ShellType
	}{
		{
			server: &models.Server{
				Name: "linux-web", Host: "127.0.0.1", Port: 1, User: "deploy",
			}, device: "linux-device", shellType: models.ShellPOSIX,
		},
		{
			server: &models.Server{
				Name: "windows-web", Host: "127.0.0.1", Port: 1, User: "deploy",
			}, device: "windows-device", shellType: models.ShellPowerShell,
		},
	} {
		item.server.ShellType = item.shellType
		if _, err := st.AddServerWithCredentialsToDevice(item.server, nil, nil, item.device); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.AddSecret(
		"DATASETS_MOUNT_PASSWORD", "共享挂载密码", []byte("mount-password"), store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSharedStorage(store.SharedStorageInput{
		Name: "datasets", Device: "NAS", User: "mount-user",
		PasswordSecret: "DATASETS_MOUNT_PASSWORD", Drive: "Z",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := st.AllowSecrets(
		[]string{"DATASETS_MOUNT_PASSWORD"}, []string{"linux-web", "windows-web"}, store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
}

// TestDevicesRemovePreviewListsDeliveryRecords 设备删除预览列出该设备全部
// 服务器条目的下发记录；确认删除后记录随条目消失，输出不含机密项值。
func TestDevicesRemovePreviewListsDeliveryRecords(t *testing.T) {
	dbPath, masterKey := withCliStoreEnv(t)
	st, err := store.Open(dbPath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("工作站", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret("API_TOKEN", "接口令牌", []byte("device-secret-value-9f21"), store.CLISource()); err != nil {
		t.Fatal(err)
	}
	profileID, err := st.AddProfile("agents")
	if err != nil {
		t.Fatal(err)
	}
	seed := func(name, path string) {
		t.Helper()
		added, err := st.AddServerWithCredentialsToDevice(&models.Server{
			Name: name, Host: "127.0.0.1", Port: 1, User: "deploy",
		}, nil, nil, "工作站")
		if err != nil {
			t.Fatal(err)
		}
		if err := st.GrantServers(profileID, []string{added.ServerID}); err != nil {
			t.Fatal(err)
		}
		if err := st.AllowSecrets([]string{"API_TOKEN"}, []string{name}, store.CLISource()); err != nil {
			t.Fatal(err)
		}
		if _, err := st.RecordSecretDelivery(store.SecretDeliveryRecordInput{
			ServerID: added.ServerID, Path: path,
			Template:   []byte("token: {{sshmgr-secret:API_TOKEN}}"),
			FileSHA256: strings.Repeat("5", 64), WrittenAt: time.Unix(1760001000, 0),
			Secrets:              []store.SecretDeliverySecret{{Name: "API_TOKEN", ValueVersion: 1}},
			InitiatingDeviceName: "office-laptop", InitiatingInstanceName: "agent-a",
			AuthorizationProfileID: profileID,
		}, store.CLISource()); err != nil {
			t.Fatal(err)
		}
	}
	seed("work-a", "/etc/app/a.yml")
	seed("work-b", "/etc/app/b.yml")
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	out := runCli(t, "devices", "rm", "工作站", "--yes")
	for _, want := range []string{
		"会随之删除的下发记录：",
		"work-a：/etc/app/a.yml（机密项 API_TOKEN）",
		"work-b：/etc/app/b.yml（机密项 API_TOKEN）",
		"已删除设备 工作站",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("设备删除预览缺少 %q：\n%s", want, out)
		}
	}
	if strings.Contains(out, "device-secret-value-9f21") {
		t.Fatalf("设备删除预览不得出现机密项值：\n%s", out)
	}
	after, err := store.Open(dbPath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer after.Close()
	records, err := after.SecretDeliveryRecords(store.SecretDeliveryRecordFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("设备删除后下发记录应随服务器条目删除：%+v", records)
	}
}
