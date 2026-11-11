package sharemanage_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sharemanage"
	"ssh-manager-mcp/internal/sharemount"
	"ssh-manager-mcp/internal/sharemounttest"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

func TestReselectRejectsServerOutsideMountableRange(t *testing.T) {
	fixture := newUnmountFixture(t, true)

	result, err := fixture.manager.Reselect(context.Background(), sharemanage.ReselectRequest{
		Share: "datasets", Server: "linux-web",
	})
	if !errors.Is(err, sharemanage.ErrReselectOutsideMountableRange) {
		t.Fatalf("范围外重选地址的错误 = %v", err)
	}
	if result != nil {
		t.Fatalf("范围外重选地址不应返回结果：%+v", result)
	}
}

func TestReselectReportsBestAddressUnchanged(t *testing.T) {
	for system, serverName := range map[string]string{"Linux": "linux-web", "macOS": "darwin-web"} {
		t.Run(system, func(t *testing.T) {
			fixture := newReselectFixture(t, system, "192.0.2.10")

			result, err := fixture.manager.Reselect(context.Background(), sharemanage.ReselectRequest{
				Share: "datasets", Server: serverName,
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.Result != "best-address-unchanged" || result.Version != 1 ||
				result.NetworkName != "办公物理局域网" || !result.Physical {
				t.Fatalf("结构化结果 = %+v", result)
			}
			if !containsText(result.Notices, "当前地址已是最优可达地址，未做改动") {
				t.Fatalf("缺少已是最优说明：%+v", result)
			}
			var history []string
			if system == "Linux" {
				history = fixture.linux.State().History
			} else {
				history = fixture.darwin.State().History
			}
			if len(history) != 0 {
				t.Fatalf("已是最优时不应改动远端状态：%v", history)
			}
		})
	}
}

func TestReselectSwitchesToPhysicalAddress(t *testing.T) {
	for system, serverName := range map[string]string{"Linux": "linux-web", "macOS": "darwin-web"} {
		t.Run(system, func(t *testing.T) {
			fixture := newReselectFixture(t, system, "100.64.10.10")

			result, err := fixture.manager.Reselect(context.Background(), sharemanage.ReselectRequest{
				Share: "datasets", Server: serverName,
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.Result != "success" || result.NetworkName != "办公物理局域网" || !result.Physical {
				t.Fatalf("结构化结果 = %+v", result)
			}
			if system == "Linux" {
				state := fixture.linux.State()
				if !state.Mounted || len(state.FstabLines) != 1 || !strings.Contains(state.FstabLines[0], "//192.0.2.10/datasets") {
					t.Fatalf("切换后的 Linux 状态 = %+v", state)
				}
				if state.Credential != "username=mount-user\npassword=mount-password\n" {
					t.Fatalf("切换后的 Linux 凭据文件 = %q", state.Credential)
				}
				assertHistoryOrder(t, state.History, "unmount", "write-configuration", "mount")
				assertPasswordOnlyInStandardInput(t, state.Commands, state.StandardInputs)
			} else {
				state := fixture.darwin.State()
				if !state.Mounted || len(state.MapLines) != 2 || !strings.Contains(state.MapLines[1], "@192.0.2.10/datasets") {
					t.Fatalf("切换后的 macOS 状态 = %+v", state)
				}
				assertHistoryOrder(t, state.History, "unmount", "write-configuration", "mount")
				assertPasswordOnlyInStandardInput(t, state.Commands, state.StandardInputs)
			}
		})
	}
}

func TestReselectKeepsOldConfigurationWhenBusyAndForceSwitches(t *testing.T) {
	for system, serverName := range map[string]string{"Linux": "linux-web", "macOS": "darwin-web"} {
		t.Run(system, func(t *testing.T) {
			fixture := newReselectFixture(t, system, "100.64.10.10")
			busy := []sharemount.BusyProcess{{
				ProcessID: "2345", User: "worker", Command: "tar",
			}}
			if system == "Linux" {
				fixture.linux.BusyProcesses = []sharemounttest.BusyProcess{{
					ProcessID: "2345", User: "worker", Command: "tar",
				}}
			} else {
				fixture.darwin.BusyProcesses = []sharemounttest.BusyProcess{{
					ProcessID: "2345", User: "worker", Command: "tar",
				}}
			}

			result, err := fixture.manager.Reselect(context.Background(), sharemanage.ReselectRequest{
				Share: "datasets", Server: serverName,
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.Result != "busy" || !reflect.DeepEqual(result.BusyProcesses, busy) {
				t.Fatalf("占用结果 = %+v", result)
			}
			if system == "Linux" {
				state := fixture.linux.State()
				if !state.Mounted || !strings.Contains(state.FstabLines[0], "//100.64.10.10/datasets") ||
					strings.Contains(state.Credential, "new") || len(state.History) != 0 {
					t.Fatalf("不带强制时不应改动 Linux 旧状态：%+v", state)
				}
			} else {
				state := fixture.darwin.State()
				if !state.Mounted || !strings.Contains(state.MapLines[1], "@100.64.10.10/datasets") ||
					len(state.History) != 0 {
					t.Fatalf("不带强制时不应改动 macOS 旧状态：%+v", state)
				}
			}

			if system == "Linux" {
				fixture.linux.ForceSucceeds = true
			} else {
				fixture.darwin.ForceSucceeds = true
			}
			forced, err := fixture.manager.Reselect(context.Background(), sharemanage.ReselectRequest{
				Share: "datasets", Server: serverName, Force: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			if forced.Result != "success" || forced.NetworkName != "办公物理局域网" || !forced.Physical {
				t.Fatalf("强制切换结果 = %+v", forced)
			}
			if system == "Linux" {
				state := fixture.linux.State()
				if !state.Mounted || !strings.Contains(state.FstabLines[0], "//192.0.2.10/datasets") {
					t.Fatalf("强制切换后的 Linux 状态 = %+v", state)
				}
			} else {
				state := fixture.darwin.State()
				if !state.Mounted || !strings.Contains(state.MapLines[1], "@192.0.2.10/datasets") {
					t.Fatalf("强制切换后的 macOS 状态 = %+v", state)
				}
			}
		})
	}
}

func TestReselectWritesSafeOwnerAudit(t *testing.T) {
	fixture := newReselectFixture(t, "Linux", "100.64.10.10")
	fixture.linux.ForceSucceeds = true

	result, err := fixture.manager.Reselect(context.Background(), sharemanage.ReselectRequest{
		Share: "datasets", Server: "linux-web", Force: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Result != "success" {
		t.Fatalf("结构化结果 = %+v", result)
	}
	rows, err := fixture.store.QueryAudit(store.AuditFilter{
		OwnerOnly: true, Actions: []string{"share-reselect"},
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
	for _, secret := range []string{"mount-password", "mount-user", "192.0.2.10", "100.64.10.10"} {
		if strings.Contains(auditText, secret) {
			t.Fatalf("审计泄露 %q：%s", secret, auditText)
		}
	}
}

func assertHistoryOrder(t *testing.T, history []string, wanted ...string) {
	t.Helper()
	previous := -1
	for _, want := range wanted {
		index := historyIndex(history, want)
		if index < 0 || index < previous {
			t.Fatalf("状态变化历史顺序不符：%v", history)
		}
		previous = index
	}
}

func assertPasswordOnlyInStandardInput(t *testing.T, commands []string, inputs [][]byte) {
	t.Helper()
	if len(commands) != len(inputs) {
		t.Fatalf("命令与标准输入数量不同：%d %d", len(commands), len(inputs))
	}
	for index, command := range commands {
		if strings.Contains(command, "mount-password") {
			t.Fatalf("第 %d 条远端命令泄露挂载密码：%s", index, command)
		}
	}
	found := false
	for _, input := range inputs {
		if bytes.Contains(input, []byte("mount-password\n")) {
			found = true
		}
	}
	if !found {
		t.Fatalf("挂载密码没有经标准输入送达：%v", inputs)
	}
}

type reselectFixture struct {
	store       *store.Store
	manager     *sharemanage.Manager
	linux       *sharemounttest.LinuxServer
	darwin      *sharemounttest.DarwinServer
	closeServer func()
}

func newReselectFixture(t *testing.T, system, currentAddress string) reselectFixture {
	t.Helper()
	databasePath := filepath.Join(t.TempDir(), "share-reselect-test.db")
	masterKey, _ := store.GenerateMasterKey()
	st, err := store.Open(databasePath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	linux := &sharemounttest.LinuxServer{
		ShareName: "datasets", ReachableAddresses: []string{"192.0.2.10", "100.64.10.10"},
	}
	darwin := &sharemounttest.DarwinServer{
		ShareName: "datasets", ReachableAddresses: []string{"192.0.2.10", "100.64.10.10"},
	}
	targetDeviceName := "linux-device"
	targetServerName := "linux-web"
	var handler func(string, io.Reader) (string, string, int)
	if system == "macOS" {
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
	if _, err := st.AddSite("办公室", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "办公物理局域网", Type: models.NetworkPhysical, SiteName: "办公室"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "办公虚拟局域网", Type: models.NetworkVirtual}); err != nil {
		t.Fatal(err)
	}
	for deviceName, memberships := range map[string][][2]string{
		"NAS":            {{"办公物理局域网", "192.0.2.10"}, {"办公虚拟局域网", "100.64.10.10"}},
		targetDeviceName: {{"办公物理局域网", "192.0.2.20"}, {"办公虚拟局域网", "100.64.10.20"}},
	} {
		for _, membership := range memberships {
			if _, err := st.AddNetworkMembership(deviceName, membership[0], membership[1]); err != nil {
				t.Fatal(err)
			}
		}
	}
	server := &models.Server{
		Name: targetServerName, Host: addr[:strings.LastIndex(addr, ":")],
		Port: portOfAddr(addr), User: "deploy", ShellType: models.ShellPOSIX,
	}
	loginCredential := &models.Credential{Type: models.CredPassword, Secret: []byte("login-pw")}
	sudoCredential := &models.Credential{Type: models.CredPassword, Secret: []byte("sudo-pw")}
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
	if system == "macOS" {
		darwin.SeedManagedConfiguration(currentAddress, addedShare.Version)
	} else {
		linux.ShareID = addedShare.ID
		linux.SeedManagedConfiguration(currentAddress, addedShare.Version)
	}
	return reselectFixture{
		store: st, manager: sharemanage.New(st), linux: linux, darwin: darwin, closeServer: cleanup,
	}
}

func TestReselectRejectsWindowsServer(t *testing.T) {
	fixture := newUnmountFixture(t, false)
	_, err := fixture.store.AddServer(&models.Server{
		Name: "windows-web", Host: "192.0.2.21", Port: 22, User: "deploy",
		AuthMethod: models.AuthPassword, ShellType: models.ShellPowerShell,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.AllowSecrets(
		[]string{"DATASETS_MOUNT_PASSWORD"}, []string{"windows-web"}, store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}

	result, err := fixture.manager.Reselect(context.Background(), sharemanage.ReselectRequest{
		Share: "datasets", Server: "windows-web",
	})
	if !errors.Is(err, sharemanage.ErrWindowsReselectUnsupported) {
		t.Fatalf("Windows 重选地址的错误 = %v", err)
	}
	if result != nil {
		t.Fatalf("Windows 重选地址不应返回结果：%+v", result)
	}
}
