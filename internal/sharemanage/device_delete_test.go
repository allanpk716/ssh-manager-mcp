package sharemanage_test

import (
	"context"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sharemanage"
	"ssh-manager-mcp/internal/sharemounttest"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

func TestDeleteDeviceCascadingRevokesLinuxAndDarwinServers(t *testing.T) {
	fixture := newDeviceDeleteFixture(t)
	preview, err := fixture.store.DeviceDeletePreview("NAS")
	if err != nil {
		t.Fatal(err)
	}

	result, err := fixture.manager.DeleteDeviceCascading(context.Background(), sharemanage.DeviceDeleteCascadingRequest{
		Device: "NAS", Confirmed: preview,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.DeviceName != "NAS" {
		t.Fatalf("删除设备结果 = %+v", result)
	}
	if _, err := fixture.store.GetDevice("NAS"); err == nil {
		t.Fatal("设备删除事务提交后提供方设备仍存在")
	}
	if _, err := fixture.store.GetSharedStorage("datasets"); err == nil {
		t.Fatal("设备删除事务提交后共享存储登记仍存在")
	}

	linuxResult := deviceDeleteResultFor(t, result, "linux-web")
	darwinResult := deviceDeleteResultFor(t, result, "darwin-web")
	if linuxResult.Result != "success" || linuxResult.System != "Linux" ||
		linuxResult.Version != fixture.shareVersion+1 {
		t.Fatalf("Linux 删除设备级联结果 = %+v", linuxResult)
	}
	if darwinResult.Result != "success" || darwinResult.System != "Darwin" ||
		darwinResult.Version != fixture.shareVersion+1 {
		t.Fatalf("macOS 删除设备级联结果 = %+v", darwinResult)
	}

	wantMarker := "sshmgr-share=datasets;sshmgr-version=" +
		strconv.FormatInt(fixture.shareVersion+1, 10) + ";sshmgr-state=revoked"
	linuxState := fixture.linux.State()
	if linuxState.Credential != "" || len(linuxState.FstabLines) != 0 || linuxState.Mounted ||
		linuxState.RevokedMarker != wantMarker {
		t.Fatalf("删除设备后的 Linux 状态 = %+v，期待撤销标记 %q", linuxState, wantMarker)
	}
	darwinState := fixture.darwin.State()
	if len(darwinState.MapLines) != 0 || len(darwinState.AutoMasterLines) != 0 ||
		darwinState.Mounted || darwinState.RevokedMarker != wantMarker {
		t.Fatalf("删除设备后的 macOS 状态 = %+v，期待撤销标记 %q", darwinState, wantMarker)
	}
}

func TestDeleteDeviceCascadingReportsBusyAndUnavailableServersWithoutRollback(t *testing.T) {
	fixture := newDeviceDeleteFixture(t)
	fixture.closeLinux()
	fixture.darwin.BusyProcesses = []sharemounttest.BusyProcess{
		{ProcessID: "1234", User: "worker", Command: "python"},
	}
	linuxBefore := fixture.linux.State()
	preview, err := fixture.store.DeviceDeletePreview("NAS")
	if err != nil {
		t.Fatal(err)
	}

	result, err := fixture.manager.DeleteDeviceCascading(context.Background(), sharemanage.DeviceDeleteCascadingRequest{
		Device: "NAS", Confirmed: preview,
	})
	if err != nil {
		t.Fatalf("远端失败不应让已提交的设备删除返回错误：%v", err)
	}
	if _, err := fixture.store.GetDevice("NAS"); err == nil {
		t.Fatal("远端失败不应回滚设备删除")
	}
	if _, err := fixture.store.GetSharedStorage("datasets"); err == nil {
		t.Fatal("远端失败不应回滚共享存储删除")
	}

	linuxResult := deviceDeleteResultFor(t, result, "linux-web")
	if !linuxResult.Failed || linuxResult.Result != "ssh-error" {
		t.Fatalf("连不上服务器的结果 = %+v", linuxResult)
	}
	linuxAfter := fixture.linux.State()
	if len(linuxAfter.Commands) != len(linuxBefore.Commands) ||
		linuxAfter.Mounted != linuxBefore.Mounted ||
		linuxAfter.Credential != linuxBefore.Credential {
		t.Fatalf("连不上时 Linux 状态不应变化：before=%+v after=%+v", linuxBefore, linuxAfter)
	}

	darwinResult := deviceDeleteResultFor(t, result, "darwin-web")
	if !darwinResult.Failed || darwinResult.Result != "busy" ||
		len(darwinResult.BusyProcesses) != 1 || darwinResult.BusyProcesses[0].ProcessID != "1234" {
		t.Fatalf("被占用 macOS 服务器的结果 = %+v", darwinResult)
	}
	darwinState := fixture.darwin.State()
	wantMarker := "sshmgr-share=datasets;sshmgr-version=" +
		strconv.FormatInt(fixture.shareVersion+1, 10) + ";sshmgr-state=revoked"
	if !darwinState.Mounted || len(darwinState.MapLines) != 0 ||
		darwinState.RevokedMarker != wantMarker {
		t.Fatalf("占用但不强制时应保留挂载并删除配置：%+v", darwinState)
	}
	if !containsText(result.Notices, "shares residue") ||
		!containsText(result.Notices, "shares unmount --force") {
		t.Fatalf("远端失败汇总提示 = %+v", result.Notices)
	}
}

func TestDeleteDeviceCascadingStillDialsServerEntryDeletedWithDevice(t *testing.T) {
	fixture := newDeviceDeleteFixture(t)
	if _, err := fixture.store.MoveServerToDevice("linux-web", "NAS"); err != nil {
		t.Fatal(err)
	}
	preview, err := fixture.store.DeviceDeletePreview("NAS")
	if err != nil {
		t.Fatal(err)
	}

	result, err := fixture.manager.DeleteDeviceCascading(context.Background(), sharemanage.DeviceDeleteCascadingRequest{
		Device: "NAS", Confirmed: preview,
	})
	if err != nil {
		t.Fatal(err)
	}
	linuxResult := deviceDeleteResultFor(t, result, "linux-web")
	if linuxResult.Result != "success" || linuxResult.System != "Linux" {
		t.Fatalf("随设备删除的服务器条目也应完成远端撤销：%+v", linuxResult)
	}
	if state := fixture.linux.State(); state.Mounted || state.Credential != "" {
		t.Fatalf("随设备删除的服务器条目远端状态 = %+v", state)
	}
}

type deviceDeleteFixture struct {
	store        *store.Store
	manager      *sharemanage.Manager
	linux        *sharemounttest.LinuxServer
	darwin       *sharemounttest.DarwinServer
	closeLinux   func()
	closeDarwin  func()
	shareVersion int64
}

func newDeviceDeleteFixture(t *testing.T) deviceDeleteFixture {
	t.Helper()
	databasePath := filepath.Join(t.TempDir(), "device-delete-test.db")
	masterKey, _ := store.GenerateMasterKey()
	st, err := store.Open(databasePath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	linux := &sharemounttest.LinuxServer{ShareName: "datasets"}
	darwin := &sharemounttest.DarwinServer{ShareName: "datasets"}
	_, closeLinux := addDeviceDeleteTarget(t, st, "linux-device", "linux-web", linux.Handler)
	_, closeDarwin := addDeviceDeleteTarget(t, st, "darwin-device", "darwin-web", darwin.Handler)

	if _, err := st.AddDevice("NAS", "共享存储提供方", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("windows-device", "挂载目标", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret("DATASETS_MOUNT_PASSWORD", "共享挂载密码", []byte("mount-password"), store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSharedStorage(store.SharedStorageInput{
		Name: "datasets", Device: "NAS", User: "mount-user",
		PasswordSecret: "DATASETS_MOUNT_PASSWORD", Drive: "Z",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	windows := &models.Server{
		Name: "windows-web", Host: "127.0.0.1", Port: 1, User: "deploy",
		ShellType: models.ShellPowerShell,
	}
	if _, err := st.AddServerWithCredentialsToDevice(
		windows, nil, nil, "windows-device",
	); err != nil {
		t.Fatal(err)
	}
	if err := st.AllowSecrets(
		[]string{"DATASETS_MOUNT_PASSWORD"},
		[]string{"linux-web", "darwin-web", "windows-web"},
		store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	share, err := st.GetSharedStorage("datasets")
	if err != nil {
		t.Fatal(err)
	}
	linux.ShareID = share.ID
	linux.SeedManagedConfiguration("192.0.2.10", share.Version)
	darwin.SeedManagedConfiguration("192.0.2.20", share.Version)

	return deviceDeleteFixture{
		store: st, manager: sharemanage.New(st), linux: linux, darwin: darwin,
		closeLinux: closeLinux, closeDarwin: closeDarwin, shareVersion: share.Version,
	}
}

func addDeviceDeleteTarget(
	t *testing.T, st *store.Store, deviceName, serverName string,
	handler func(string, io.Reader) (string, string, int),
) (*models.Server, func()) {
	t.Helper()
	if _, err := st.AddDevice(deviceName, "挂载目标", ""); err != nil {
		t.Fatal(err)
	}
	addr, hostKey, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "login-pw", SudoPassword: "sudo-pw", Exec: handler,
	})
	server := &models.Server{
		Name: serverName, Host: addr[:strings.LastIndex(addr, ":")],
		Port: portOfAddr(addr), User: "deploy", ShellType: models.ShellPOSIX,
	}
	loginCredential := &models.Credential{Type: models.CredPassword, Secret: []byte("login-pw")}
	sudoCredential := &models.Credential{Type: models.CredPassword, Secret: []byte("sudo-pw")}
	added, err := st.AddServerWithCredentialsToDevice(server, loginCredential, sudoCredential, deviceName)
	if err != nil {
		t.Fatal(err)
	}
	server.ID = added.ServerID
	if err := st.SaveEntryPin(server, hostKey.Marshal()); err != nil {
		t.Fatal(err)
	}
	return server, cleanup
}

func deviceDeleteResultFor(
	t *testing.T, result *sharemanage.DeviceDeleteCascadingResult, serverName string,
) sharemanage.DeviceDeleteShareServerResult {
	t.Helper()
	for _, item := range result.Results {
		if item.ServerName == serverName {
			return item
		}
	}
	t.Fatalf("删除设备级联结果里没有服务器 %s：%+v", serverName, result)
	return sharemanage.DeviceDeleteShareServerResult{}
}
