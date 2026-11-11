package cli

import (
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

func TestDevicesRemovePreviewSeparatesRemoteUnmountAndWindowsSnapshotSync(t *testing.T) {
	dbPath, masterKey := withCliStoreEnv(t)
	seedDeviceDeleteSharedStorage(t, dbPath, masterKey)

	out := runCli(t, "devices", "rm", "NAS", "--yes")
	for _, want := range []string{
		"共享存储：",
		"datasets",
		"Linux 与 macOS 服务器（将删除配置并卸载）：",
		"linux-web（所属设备 linux-device，系统类型 Linux 或 macOS）",
		"Windows 服务器（将随快照同步停止注入）：",
		"windows-web（所属设备 windows-device，系统类型 Windows）",
		"datasets / linux-web：连不上；请用 shares residue 检查残留",
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
