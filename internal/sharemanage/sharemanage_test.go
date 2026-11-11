package sharemanage_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sharemanage"
	"ssh-manager-mcp/internal/sharemount"
	"ssh-manager-mcp/internal/sharemounttest"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

func TestUnmountRemovesLinuxConfigurationBeforeUnmount(t *testing.T) {
	fixture := newUnmountFixture(t, false)

	result, err := fixture.manager.Unmount(context.Background(), sharemanage.UnmountRequest{
		Share: "datasets", Server: "linux-web",
	})
	if err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	if result.Result != "success" || result.ServerName != "linux-web" || result.ShareName != "datasets" {
		t.Fatalf("结构化结果 = %+v", result)
	}
	state := fixture.linux.State()
	if state.Credential != "" || len(state.FstabLines) != 0 || state.Mounted {
		t.Fatalf("卸载后的 Linux 状态 = %+v", state)
	}
	if state.RevokedMarker != "" {
		t.Fatalf("服务器仍在可挂载范围内时不应写撤销标记：%q", state.RevokedMarker)
	}
	configIndex := historyIndex(state.History, "delete-configuration")
	unmountIndex := historyIndex(state.History, "unmount")
	if configIndex < 0 || unmountIndex < 0 || configIndex > unmountIndex {
		t.Fatalf("状态变化历史应先删配置再卸载：%v", state.History)
	}
	if !containsText(result.Notices, "服务器仍在可挂载范围内，agent 之后仍可能重新挂载；彻底撤下请使用 shares revoke") {
		t.Fatalf("缺少范围内卸载提示：%+v", result)
	}
	if len(state.Commands) != 2 {
		t.Fatalf("远端命令数量 = %d，期待识别系统与卸载两条：%v", len(state.Commands), state.Commands)
	}
	for _, forbidden := range []string{"mount-password", "mount-user", "192.0.2.10"} {
		if strings.Contains(state.Commands[1], forbidden) {
			t.Fatalf("固定卸载命令包含 %q：%s", forbidden, state.Commands[1])
		}
	}
}

func TestUnmountOutsideMountableRangeWritesRevokedMarker(t *testing.T) {
	fixture := newUnmountFixture(t, true)

	result, err := fixture.manager.Unmount(context.Background(), sharemanage.UnmountRequest{
		Share: "datasets", Server: "linux-web",
	})
	if err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	if result.Result != "success" || result.Version != 2 {
		t.Fatalf("结构化结果 = %+v", result)
	}
	state := fixture.linux.State()
	wantMarker := "sshmgr-share=datasets;sshmgr-version=2;sshmgr-state=revoked"
	if state.RevokedMarker != wantMarker {
		t.Fatalf("撤销标记 = %q，期待 %q", state.RevokedMarker, wantMarker)
	}
	if state.RevokedMarkerPath != "/etc/sshmgr/shares/datasets.revoked" {
		t.Fatalf("撤销标记路径 = %q", state.RevokedMarkerPath)
	}
	if containsText(result.Notices, "服务器仍在可挂载范围内") {
		t.Fatalf("范围外卸载不应给出范围内提示：%+v", result)
	}
	deleteIndex := historyIndex(state.History, "delete-configuration")
	markerIndex := historyIndex(state.History, "write-revoked-marker")
	unmountIndex := historyIndex(state.History, "unmount")
	if deleteIndex < 0 || markerIndex < 0 || unmountIndex < 0 ||
		deleteIndex > markerIndex || markerIndex > unmountIndex {
		t.Fatalf("状态变化历史应先删配置、写撤销标记、再卸载：%v", state.History)
	}
}

func TestUnmountDeletedShareCleansRemoteMarker(t *testing.T) {
	fixture := newUnmountFixture(t, false)
	deleted, err := fixture.store.DeleteSharedStorage("datasets", store.CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if deleted.Version != 1 {
		t.Fatalf("删除前共享版本号 = %d", deleted.Version)
	}

	result, err := fixture.manager.Unmount(context.Background(), sharemanage.UnmountRequest{
		Share: "datasets", Server: "linux-web",
	})
	if err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	if result.Result != "success" || result.Version != 2 {
		t.Fatalf("结构化结果 = %+v", result)
	}
	state := fixture.linux.State()
	if state.RevokedMarker != "sshmgr-share=datasets;sshmgr-version=2;sshmgr-state=revoked" {
		t.Fatalf("撤销标记 = %q", state.RevokedMarker)
	}
	if state.Credential != "" || len(state.FstabLines) != 0 || state.Mounted {
		t.Fatalf("卸载后的 Linux 状态 = %+v", state)
	}
}

func TestUnmountBusyLinuxWithoutForceKeepsMountAndListsProcesses(t *testing.T) {
	fixture := newUnmountFixture(t, false)
	fixture.linux.BusyProcesses = []sharemounttest.BusyProcess{
		{ProcessID: "1234", User: "worker", Command: "python"},
	}

	result, err := fixture.manager.Unmount(context.Background(), sharemanage.UnmountRequest{
		Share: "datasets", Server: "linux-web",
	})
	if err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	if result.Result != "busy" || len(result.BusyProcesses) != 1 || result.BusyProcesses[0].ProcessID != "1234" ||
		result.BusyProcesses[0].User != "worker" || result.BusyProcesses[0].Command != "python" {
		t.Fatalf("占用结果 = %+v", result)
	}
	state := fixture.linux.State()
	if !state.Mounted || state.Credential != "" || len(state.FstabLines) != 0 {
		t.Fatalf("占用时应保留挂载但删除配置：%+v", state)
	}
}

func TestUnmountCleansResidueAfterRevokeWithoutUnmount(t *testing.T) {
	fixture := newUnmountFixture(t, false)
	if _, err := fixture.manager.Revoke(context.Background(), sharemanage.RevokeRequest{
		Share: "datasets", Server: "linux-web",
	}); err != nil {
		t.Fatalf("撤销: %v", err)
	}

	report, err := fixture.manager.Residue(context.Background(), sharemanage.ResidueRequest{})
	if err != nil {
		t.Fatalf("第一次残留检查: %v", err)
	}
	row := residueResultFor(report, "linux-web")
	if row == nil || row.Status != sharemanage.ResidueStatusResidue ||
		len(row.Findings) != 1 || !row.Findings[0].HasMount ||
		row.Findings[0].HasConfigEntry || row.Findings[0].HasCredentialFile {
		t.Fatalf("撤销但保留挂载后的残留检查 = %+v", row)
	}

	unmount, err := fixture.manager.Unmount(context.Background(), sharemanage.UnmountRequest{
		Share: "datasets", Server: "linux-web",
	})
	if err != nil {
		t.Fatalf("清理保留挂载: %v", err)
	}
	if unmount.Result != "success" || fixture.linux.State().Mounted {
		t.Fatalf("清理结果 = %+v，服务器状态 = %+v", unmount, fixture.linux.State())
	}

	report, err = fixture.manager.Residue(context.Background(), sharemanage.ResidueRequest{})
	if err != nil {
		t.Fatalf("第二次残留检查: %v", err)
	}
	row = residueResultFor(report, "linux-web")
	if row == nil || row.Status != sharemanage.ResidueStatusMarkerOnly ||
		len(row.Findings) != 1 || row.Findings[0].HasMount {
		t.Fatalf("清理后的残留检查应只剩撤销标记：%+v", row)
	}

	_, err = fixture.manager.Unmount(context.Background(), sharemanage.UnmountRequest{
		Share: "datasets", Server: "linux-web",
	})
	if !errors.Is(err, sharemount.ErrUnmountNotManaged) {
		t.Fatalf("没有可清理内容时的卸载错误 = %v", err)
	}
}

func TestUnmountCleansLinuxCredentialFileWithoutConfiguration(t *testing.T) {
	fixture := newUnmountFixture(t, false)
	if _, err := fixture.manager.Revoke(context.Background(), sharemanage.RevokeRequest{
		Share: "datasets", Server: "linux-web",
	}); err != nil {
		t.Fatalf("撤销: %v", err)
	}
	fixture.linux.Credential = "username=mount-user\npassword=mount-password\n"
	fixture.linux.Mounted = false

	result, err := fixture.manager.Unmount(context.Background(), sharemanage.UnmountRequest{
		Share: "datasets", Server: "linux-web",
	})
	if err != nil {
		t.Fatalf("只剩凭据文件时卸载: %v", err)
	}
	state := fixture.linux.State()
	if result.Result != "success" || state.Credential != "" || state.Mounted {
		t.Fatalf("只剩凭据文件时的结果 = %+v，服务器状态 = %+v", result, state)
	}
}

func TestUnmountBusyLinuxWithForceUnmounts(t *testing.T) {
	fixture := newUnmountFixture(t, false)
	fixture.linux.BusyProcesses = []sharemounttest.BusyProcess{{ProcessID: "1234", User: "worker", Command: "python"}}
	fixture.linux.ForceSucceeds = true

	result, err := fixture.manager.Unmount(context.Background(), sharemanage.UnmountRequest{
		Share: "datasets", Server: "linux-web", Force: true,
	})
	if err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	if result.Result != "success" || !result.Force || fixture.linux.State().Mounted {
		t.Fatalf("强制卸载结果 = %+v，服务器状态 = %+v", result, fixture.linux.State())
	}
}

func TestUnmountUnavailableServerFailsImmediatelyWithoutStateChange(t *testing.T) {
	fixture := newUnmountFixture(t, false)
	before := fixture.linux.State()
	fixture.closeServer()

	result, err := fixture.manager.Unmount(context.Background(), sharemanage.UnmountRequest{
		Share: "datasets", Server: "linux-web",
	})
	if err == nil || result != nil {
		t.Fatalf("服务器连不上时立即失败，结果 = %+v，错误 = %v", result, err)
	}
	if !containsText([]string{err.Error()}, "SSH 会话执行失败") {
		t.Fatalf("错误应使用不含远端内容的连接失败说明：%v", err)
	}
	after := fixture.linux.State()
	if len(after.Commands) != len(before.Commands) || after.Mounted != before.Mounted ||
		after.Credential != before.Credential {
		t.Fatalf("连不上时服务器状态不应变化：before=%+v after=%+v", before, after)
	}
	rows, err := fixture.store.QueryAudit(store.AuditFilter{
		OwnerOnly: true, Actions: []string{"share-unmount"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != "error" {
		t.Fatalf("连不上时的 owner 审计 = %+v", rows)
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(rows[0].Command), &summary); err != nil {
		t.Fatal(err)
	}
	if summary["result"] != "ssh-error" || summary["version"] != float64(1) {
		t.Fatalf("连不上时的审计摘要 = %#v", summary)
	}
}

func TestUnmountWritesSafeOwnerAudit(t *testing.T) {
	fixture := newUnmountFixture(t, false)

	result, err := fixture.manager.Unmount(context.Background(), sharemanage.UnmountRequest{
		Share: "datasets", Server: "linux-web", Force: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Result != "success" {
		t.Fatalf("结构化结果 = %+v", result)
	}
	rows, err := fixture.store.QueryAudit(store.AuditFilter{
		OwnerOnly: true, Actions: []string{"share-unmount"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != "ok" {
		t.Fatalf("owner 审计 = %+v", rows)
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(rows[0].Command), &summary); err != nil {
		t.Fatal(err)
	}
	if summary["share"] != "datasets" || summary["server"] != "linux-web" ||
		summary["result"] != "success" || summary["version"] != float64(1) || summary["force"] != true {
		t.Fatalf("审计摘要 = %#v", summary)
	}
	auditText := rows[0].Command
	for _, secret := range []string{"mount-password", "mount-user", "192.0.2.10"} {
		if strings.Contains(auditText, secret) {
			t.Fatalf("审计泄露 %q：%s", secret, auditText)
		}
	}
}

func TestUnmountRemovesDarwinConfigurationBeforeUnmount(t *testing.T) {
	fixture := newDarwinUnmountFixture(t, false)

	result, err := fixture.manager.Unmount(context.Background(), sharemanage.UnmountRequest{
		Share: "datasets", Server: "darwin-web",
	})
	if err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	if result.Result != "success" || result.Version != 1 {
		t.Fatalf("结构化结果 = %+v", result)
	}
	state := fixture.darwin.State()
	if len(state.MapLines) != 0 || state.Mounted {
		t.Fatalf("卸载后的 macOS 状态 = %+v", state)
	}
	if state.RevokedMarker != "" {
		t.Fatalf("服务器仍在可挂载范围内时不应写撤销标记：%q", state.RevokedMarker)
	}
	deleteIndex := historyIndex(state.History, "delete-configuration")
	unmountIndex := historyIndex(state.History, "unmount")
	reloadIndex := historyIndex(state.History, "reload-automount")
	if deleteIndex < 0 || reloadIndex < 0 || unmountIndex < 0 ||
		deleteIndex > reloadIndex || reloadIndex > unmountIndex {
		t.Fatalf("状态变化历史应先删配置、重载 autofs、再卸载：%v", state.History)
	}
}

func TestUnmountDarwinOutsideRangeWritesRevokedMarker(t *testing.T) {
	fixture := newDarwinUnmountFixture(t, true)

	result, err := fixture.manager.Unmount(context.Background(), sharemanage.UnmountRequest{
		Share: "datasets", Server: "darwin-web",
	})
	if err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	if result.Result != "success" || result.Version != 2 {
		t.Fatalf("结构化结果 = %+v", result)
	}
	state := fixture.darwin.State()
	if state.RevokedMarker != "sshmgr-share=datasets;sshmgr-version=2;sshmgr-state=revoked" {
		t.Fatalf("撤销标记 = %q", state.RevokedMarker)
	}
	if state.RevokedMarkerPath != "/etc/sshmgr/datasets.revoked" {
		t.Fatalf("撤销标记路径 = %q", state.RevokedMarkerPath)
	}
}

func TestUnmountDeletedDarwinShareCleansRemoteMarker(t *testing.T) {
	fixture := newDarwinUnmountFixture(t, false)
	if _, err := fixture.store.DeleteSharedStorage("datasets", store.CLISource()); err != nil {
		t.Fatal(err)
	}

	result, err := fixture.manager.Unmount(context.Background(), sharemanage.UnmountRequest{
		Share: "datasets", Server: "darwin-web",
	})
	if err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	state := fixture.darwin.State()
	if result.Result != "success" || result.Version != 2 ||
		state.RevokedMarker != "sshmgr-share=datasets;sshmgr-version=2;sshmgr-state=revoked" ||
		len(state.MapLines) != 0 || state.Mounted {
		t.Fatalf("结构化结果 = %+v，macOS 状态 = %+v", result, state)
	}
}

func TestUnmountBusyDarwinWithAndWithoutForce(t *testing.T) {
	t.Run("不带强制", func(t *testing.T) {
		fixture := newDarwinUnmountFixture(t, false)
		fixture.darwin.BusyProcesses = []sharemounttest.BusyProcess{
			{ProcessID: "2345", User: "worker", Command: "tar"},
		}
		result, err := fixture.manager.Unmount(context.Background(), sharemanage.UnmountRequest{
			Share: "datasets", Server: "darwin-web",
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.Result != "busy" || len(result.BusyProcesses) != 1 || !fixture.darwin.State().Mounted {
			t.Fatalf("macOS 占用结果 = %+v，状态 = %+v", result, fixture.darwin.State())
		}
	})
	t.Run("强制", func(t *testing.T) {
		fixture := newDarwinUnmountFixture(t, false)
		fixture.darwin.BusyProcesses = []sharemounttest.BusyProcess{{ProcessID: "2345", User: "worker", Command: "tar"}}
		fixture.darwin.ForceSucceeds = true
		result, err := fixture.manager.Unmount(context.Background(), sharemanage.UnmountRequest{
			Share: "datasets", Server: "darwin-web", Force: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.Result != "success" || fixture.darwin.State().Mounted {
			t.Fatalf("macOS 强制卸载结果 = %+v，状态 = %+v", result, fixture.darwin.State())
		}
	})
}

func TestUnmountDarwinReloadsAutomountBeforeBusyUnmountAttempt(t *testing.T) {
	fixture := newDarwinUnmountFixture(t, false)
	fixture.darwin.BusyProcesses = []sharemounttest.BusyProcess{
		{ProcessID: "1234", User: "worker", Command: "python"},
	}

	result, err := fixture.manager.Unmount(context.Background(), sharemanage.UnmountRequest{
		Share: "datasets", Server: "darwin-web",
	})
	if err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	if result.Result != "busy" || !fixture.darwin.State().Mounted {
		t.Fatalf("被占用时的结果 = %+v，服务器状态 = %+v", result, fixture.darwin.State())
	}
	state := fixture.darwin.State()
	deleteIndex := historyIndex(state.History, "delete-configuration")
	reloadIndex := historyIndex(state.History, "reload-automount")
	unmountIndex := historyIndex(state.History, "unmount")
	if deleteIndex < 0 || reloadIndex < 0 || reloadIndex < deleteIndex || unmountIndex >= 0 {
		t.Fatalf("macOS 状态变化历史应先删映射、再重载 autofs、后尝试卸载：%v", state.History)
	}
}

func TestUnmountWaitsForSharedStorageLock(t *testing.T) {
	tests := []struct {
		name   string
		system string
		server string
	}{
		{name: "Linux", system: "linux", server: "linux-web"},
		{name: "macOS", system: "darwin", server: "darwin-web"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newSystemUnmountFixture(t, testCase.system, false)
			if testCase.system == "darwin" {
				fixture.darwin.LockHeld = true
			} else {
				fixture.linux.LockHeld = true
			}
			_, err := fixture.manager.Unmount(context.Background(), sharemanage.UnmountRequest{
				Share: "datasets", Server: testCase.server,
			})
			if !errors.Is(err, sharemount.ErrUnmountLockTimeout) {
				t.Fatalf("锁被占用时的错误 = %v", err)
			}
		})
	}
}

type unmountFixture struct {
	store       *store.Store
	manager     *sharemanage.Manager
	linux       *sharemounttest.LinuxServer
	darwin      *sharemounttest.DarwinServer
	closeServer func()
}

func newUnmountFixture(t *testing.T, outOfRange bool) unmountFixture {
	return newSystemUnmountFixture(t, "linux", outOfRange)
}

func newDarwinUnmountFixture(t *testing.T, outOfRange bool) unmountFixture {
	return newSystemUnmountFixture(t, "darwin", outOfRange)
}

func newSystemUnmountFixture(t *testing.T, system string, outOfRange bool) unmountFixture {
	t.Helper()
	databasePath := filepath.Join(t.TempDir(), "share-manage-test.db")
	masterKey, _ := store.GenerateMasterKey()
	st, err := store.Open(databasePath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	linux := &sharemounttest.LinuxServer{ShareName: "datasets"}
	darwin := &sharemounttest.DarwinServer{ShareName: "datasets"}
	var handler func(string, io.Reader) (string, string, int)
	targetDeviceName := "linux-device"
	targetServerName := "linux-web"
	if system == "darwin" {
		handler = darwin.Handler
		targetDeviceName = "darwin-device"
		targetServerName = "darwin-web"
	} else {
		handler = linux.Handler
	}
	addr, hostKey, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "login-pw", SudoPassword: "sudo-pw", Exec: handler,
	})
	t.Cleanup(cleanup)

	if _, err := st.AddDevice("NAS", "共享存储提供方", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice(targetDeviceName, "挂载目标", ""); err != nil {
		t.Fatal(err)
	}
	loginCredential := &models.Credential{Type: models.CredPassword, Secret: []byte("login-pw")}
	sudoCredential := &models.Credential{Type: models.CredPassword, Secret: []byte("sudo-pw")}
	server := &models.Server{
		Name: targetServerName, Host: addr[:strings.LastIndex(addr, ":")],
		Port: portOfAddr(addr), User: "deploy", ShellType: models.ShellPOSIX,
	}
	added, err := st.AddServerWithCredentialsToDevice(server, loginCredential, sudoCredential, targetDeviceName)
	if err != nil {
		t.Fatal(err)
	}
	server.ID = added.ServerID
	if err := st.SaveEntryPin(server, hostKey.Marshal()); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret("DATASETS_MOUNT_PASSWORD", "共享挂载密码", []byte("mount-password"), store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := st.AllowSecrets([]string{"DATASETS_MOUNT_PASSWORD"}, []string{targetServerName}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	addedShare, err := st.AddSharedStorage(store.SharedStorageInput{
		Name: "datasets", Device: "NAS", User: "mount-user",
		PasswordSecret: "DATASETS_MOUNT_PASSWORD", Drive: "Z",
	}, store.CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if outOfRange {
		if err := st.DisallowSecrets([]string{"DATASETS_MOUNT_PASSWORD"}, []string{targetServerName}, store.CLISource()); err != nil {
			t.Fatal(err)
		}
	}
	if system == "darwin" {
		darwin.SeedManagedConfiguration("192.0.2.10", addedShare.Version)
	} else {
		linux.ShareID = addedShare.ID
		linux.SeedManagedConfiguration("192.0.2.10", addedShare.Version)
	}

	return unmountFixture{
		store:       st,
		manager:     sharemanage.New(st),
		linux:       linux,
		darwin:      darwin,
		closeServer: cleanup,
	}
}

func historyIndex(values []string, want string) int {
	for index, value := range values {
		if value == want {
			return index
		}
	}
	return -1
}

func containsText(values []string, want string) bool {
	for _, value := range values {
		if strings.Contains(value, want) {
			return true
		}
	}
	return false
}

func portOfAddr(addr string) int {
	port, err := strconv.Atoi(addr[strings.LastIndex(addr, ":")+1:])
	if err != nil {
		panic(err)
	}
	return port
}
