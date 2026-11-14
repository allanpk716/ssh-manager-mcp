package sharemanage_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sharemanage"
	"ssh-manager-mcp/internal/sharemount"
	"ssh-manager-mcp/internal/sharemounttest"
	"ssh-manager-mcp/internal/store"
)

func TestRevokeLinuxWithoutUnmountDeletesConfigurationAndKeepsMount(t *testing.T) {
	fixture := newUnmountFixture(t, false)
	revisionBefore, err := fixture.store.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}

	result, err := fixture.manager.Revoke(context.Background(), sharemanage.RevokeRequest{
		Share: "datasets", Server: "linux-web",
	})
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if result.Result != "success" || result.Version != 2 || !result.MountPreserved {
		t.Fatalf("结构化结果 = %+v", result)
	}
	share, err := fixture.store.GetSharedStorage("datasets")
	if err != nil {
		t.Fatal(err)
	}
	if share.Version != 2 {
		t.Fatalf("撤销后共享存储版本号 = %d，期待 2", share.Version)
	}
	for _, server := range share.MountableServers {
		if server.Name == "linux-web" {
			t.Fatalf("撤销后可挂载服务器仍包含 linux-web：%+v", share.MountableServers)
		}
	}
	revisionAfter, err := fixture.store.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if revisionAfter <= revisionBefore {
		t.Fatalf("快照修订号未前进：before=%d after=%d", revisionBefore, revisionAfter)
	}
	state := fixture.linux.State()
	if state.Credential != "" || len(state.FstabLines) != 0 || !state.Mounted {
		t.Fatalf("不带 --unmount 时应删配置但保留挂载：%+v", state)
	}
	if state.RevokedMarker != "sshmgr-share=datasets;sshmgr-version=2;sshmgr-state=revoked" {
		t.Fatalf("撤销标记 = %q", state.RevokedMarker)
	}
	if historyIndex(state.History, "unmount") >= 0 {
		t.Fatalf("不带 --unmount 时不应卸载：%v", state.History)
	}

	rows, err := fixture.store.QueryAudit(store.AuditFilter{
		OwnerOnly: true, Actions: []string{"share-revoke"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != "ok" {
		t.Fatalf("共享撤销 owner 审计 = %+v", rows)
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(rows[0].Command), &summary); err != nil {
		t.Fatal(err)
	}
	if summary["share"] != "datasets" || summary["server"] != "linux-web" ||
		summary["version"] != float64(2) || summary["unmount"] != false ||
		summary["password_changed"] != false {
		t.Fatalf("共享撤销审计摘要 = %#v", summary)
	}
	if strings.Contains(rows[0].Command, "mount-password") ||
		strings.Contains(rows[0].Command, "mount-user") ||
		strings.Contains(rows[0].Command, "192.0.2.10") {
		t.Fatalf("共享撤销审计泄露敏感信息：%s", rows[0].Command)
	}
}

// TestRevokeOutOfScopeServerOnlyCleansRemote 覆盖已移出可挂载范围的服务
// 器：命令行与 Web UI 共用这段逻辑，只清理远端，不再修改登记，共享存
// 储版本号与变更说明保持不变。
func TestRevokeOutOfScopeServerOnlyCleansRemote(t *testing.T) {
	fixture := newUnmountFixture(t, true)
	shareBefore, err := fixture.store.GetSharedStorage("datasets")
	if err != nil {
		t.Fatal(err)
	}
	if shareBefore.Version != 2 {
		t.Fatalf("准备阶段共享存储版本号 = %d，期待 2", shareBefore.Version)
	}

	result, err := fixture.manager.Revoke(context.Background(), sharemanage.RevokeRequest{
		Share: "datasets", Server: "linux-web", Unmount: true,
	})
	if err != nil {
		t.Fatalf("已移出范围的服务器撤销不应失败：%v", err)
	}
	if result.Result != "success" || result.Version != 2 || result.ScopeChanged ||
		result.MountPreserved || !result.Unmount {
		t.Fatalf("已移出范围的结构化结果 = %+v", result)
	}
	shareAfter, err := fixture.store.GetSharedStorage("datasets")
	if err != nil {
		t.Fatal(err)
	}
	if shareAfter.Version != shareBefore.Version ||
		shareAfter.ChangeNote != shareBefore.ChangeNote ||
		len(shareAfter.MountableServers) != 0 {
		t.Fatalf("已移出范围的撤销不应修改登记：before=%+v after=%+v", shareBefore, shareAfter)
	}
	state := fixture.linux.State()
	if state.Mounted || len(state.FstabLines) != 0 || state.Credential != "" {
		t.Fatalf("已移出范围的远端清理不完整：%+v", state)
	}
	if state.RevokedMarker != "sshmgr-share=datasets;sshmgr-version=2;sshmgr-state=revoked" {
		t.Fatalf("已移出范围的撤销标记 = %q", state.RevokedMarker)
	}
	revokeRows, err := fixture.store.QueryAudit(store.AuditFilter{
		OwnerOnly: true, Actions: []string{"share-revoke"},
	})
	if err != nil || len(revokeRows) != 0 {
		t.Fatalf("已移出范围的撤销不应写登记撤销审计：%v %+v", err, revokeRows)
	}
	remoteRows, err := fixture.store.QueryAudit(store.AuditFilter{
		OwnerOnly: true, Actions: []string{"share-revoke-remote"},
	})
	if err != nil || len(remoteRows) != 1 || remoteRows[0].Status != "ok" {
		t.Fatalf("已移出范围的远端撤销审计 = %v %+v", err, remoteRows)
	}
}

func TestRevokeNeverMountedServerWritesMarkerAndBlocksMountShare(t *testing.T) {
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
			targetDeviceName := "linux-device"
			if testCase.system == "darwin" {
				targetDeviceName = "darwin-device"
			}
			if _, err := fixture.store.AddSite("办公室", ""); err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.store.AddNetwork(models.Network{
				Name: "办公物理局域网", Type: models.NetworkPhysical, SiteName: "办公室",
			}); err != nil {
				t.Fatal(err)
			}
			for deviceName, address := range map[string]string{
				"NAS": "192.0.2.10", targetDeviceName: "192.0.2.20",
			} {
				if _, err := fixture.store.AddNetworkMembership(
					deviceName, "办公物理局域网", address,
				); err != nil {
					t.Fatal(err)
				}
			}
			shareBefore, err := fixture.store.GetSharedStorage("datasets")
			if err != nil {
				t.Fatal(err)
			}
			if testCase.system == "darwin" {
				fixture.darwin.MapLines = nil
				fixture.darwin.AutoMasterLines = nil
				fixture.darwin.Mounted = false
				fixture.darwin.ReachableAddresses = []string{"192.0.2.10"}
			} else {
				fixture.linux.FstabLines = nil
				fixture.linux.Credential = ""
				fixture.linux.Mounted = false
				fixture.linux.ReachableAddresses = []string{"192.0.2.10"}
			}

			result, err := fixture.manager.Revoke(context.Background(), sharemanage.RevokeRequest{
				Share: "datasets", Server: testCase.server,
			})
			if err != nil {
				t.Fatalf("从未挂载的服务器撤销不应失败：%v", err)
			}
			if result.Result != "success" || result.Version != shareBefore.Version+1 {
				t.Fatalf("从未挂载的服务器撤销结果 = %+v", result)
			}
			var stateMarker string
			if testCase.system == "darwin" {
				stateMarker = fixture.darwin.State().RevokedMarker
			} else {
				stateMarker = fixture.linux.State().RevokedMarker
			}
			wantMarker := fmt.Sprintf(
				"sshmgr-share=datasets;sshmgr-version=%d;sshmgr-state=revoked",
				shareBefore.Version+1,
			)
			if stateMarker != wantMarker {
				t.Fatalf("撤销标记 = %q，期待 %q", stateMarker, wantMarker)
			}

			share, err := fixture.store.GetSharedStorage("datasets")
			if err != nil {
				t.Fatal(err)
			}
			server, err := fixture.store.ResolveServer(testCase.server)
			if err != nil || server == nil {
				t.Fatalf("解析服务器失败：%v %+v", err, server)
			}
			_, mountErr := (&sharemount.Service{Store: fixture.store}).Mount(context.Background(), sharemount.Request{
				Share: share, Server: server, Password: []byte("mount-password"),
			})
			var mountFailure *sharemount.MountFailure
			if !errors.As(mountErr, &mountFailure) ||
				mountFailure.Class != sharemount.MountClassShareRevoked {
				t.Fatalf("撤销后 mount_share 错误 = %v", mountErr)
			}
		})
	}
}

func TestRevokeLinuxWithUnmountForceCleansRemoteAndRecordsFlags(t *testing.T) {
	fixture := newUnmountFixture(t, false)
	fixture.linux.BusyProcesses = []sharemounttest.BusyProcess{
		{ProcessID: "1234", User: "worker", Command: "python"},
	}
	fixture.linux.ForceSucceeds = true

	result, err := fixture.manager.Revoke(context.Background(), sharemanage.RevokeRequest{
		Share: "datasets", Server: "linux-web", Unmount: true, Force: true,
	})
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if result.Result != "success" || !result.Unmount || !result.Force || result.MountPreserved {
		t.Fatalf("结构化结果 = %+v", result)
	}
	state := fixture.linux.State()
	if state.Credential != "" || len(state.FstabLines) != 0 || state.Mounted {
		t.Fatalf("撤销后的 Linux 状态 = %+v", state)
	}
	if state.RevokedMarker != "sshmgr-share=datasets;sshmgr-version=2;sshmgr-state=revoked" {
		t.Fatalf("撤销标记 = %q", state.RevokedMarker)
	}

	rows, err := fixture.store.QueryAudit(store.AuditFilter{
		OwnerOnly: true, Actions: []string{"share-revoke"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("共享撤销 owner 审计行数 = %d", len(rows))
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(rows[0].Command), &summary); err != nil {
		t.Fatal(err)
	}
	if summary["unmount"] != true || summary["force"] != true {
		t.Fatalf("共享撤销审计应记录实际选项：%#v", summary)
	}
}

func TestRevokeWritesRemoteResultAudit(t *testing.T) {
	t.Run("远端成功", func(t *testing.T) {
		fixture := newUnmountFixture(t, false)
		if _, err := fixture.manager.Revoke(context.Background(), sharemanage.RevokeRequest{
			Share: "datasets", Server: "linux-web", Unmount: true,
		}); err != nil {
			t.Fatalf("Revoke: %v", err)
		}
		rows, err := fixture.store.QueryAudit(store.AuditFilter{
			OwnerOnly: true, Actions: []string{"share-revoke-remote"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].Status != "ok" {
			t.Fatalf("远端撤销 owner 审计 = %+v", rows)
		}
		var summary map[string]any
		if err := json.Unmarshal([]byte(rows[0].Command), &summary); err != nil {
			t.Fatal(err)
		}
		if summary["share"] != "datasets" || summary["server"] != "linux-web" ||
			summary["result"] != "success" || summary["version"] != float64(2) ||
			summary["unmount"] != true {
			t.Fatalf("远端撤销审计摘要 = %#v", summary)
		}
		if strings.Contains(rows[0].Command, "mount-password") ||
			strings.Contains(rows[0].Command, "mount-user") ||
			strings.Contains(rows[0].Command, "192.0.2.10") {
			t.Fatalf("远端撤销审计泄露敏感信息：%s", rows[0].Command)
		}
	})
	t.Run("远端失败", func(t *testing.T) {
		fixture := newUnmountFixture(t, false)
		fixture.closeServer()
		_, _ = fixture.manager.Revoke(context.Background(), sharemanage.RevokeRequest{
			Share: "datasets", Server: "linux-web",
		})
		rows, err := fixture.store.QueryAudit(store.AuditFilter{
			OwnerOnly: true, Actions: []string{"share-revoke-remote"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].Status != "error" {
			t.Fatalf("远端失败的 owner 审计 = %+v", rows)
		}
		var summary map[string]any
		if err := json.Unmarshal([]byte(rows[0].Command), &summary); err != nil {
			t.Fatal(err)
		}
		if summary["result"] != "ssh-error" || summary["version"] != float64(2) {
			t.Fatalf("远端失败审计摘要 = %#v", summary)
		}
	})
}

func TestRevokeWithNewPasswordRecordsRemainingServersAndKeepsRemoteClean(t *testing.T) {
	fixture := newUnmountFixture(t, false)
	if _, err := fixture.store.AddServer(&models.Server{
		Name: "linux-worker", Host: "192.0.2.11", Port: 22, User: "deploy",
		AuthMethod: models.AuthPassword,
	}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.AllowSecrets(
		[]string{"DATASETS_MOUNT_PASSWORD"}, []string{"linux-worker"}, store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}

	result, err := fixture.manager.Revoke(context.Background(), sharemanage.RevokeRequest{
		Share: "datasets", Server: "linux-web", NewPassword: []byte("second-mount-password"),
	})
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if !result.PasswordChanged || result.Version != 3 {
		t.Fatalf("撤销加换密码结果 = %+v", result)
	}
	if len(result.RemainingMountServers) != 1 ||
		result.RemainingMountServers[0] != "linux-worker" {
		t.Fatalf("仍可挂载的 Linux 与 macOS 服务器 = %v", result.RemainingMountServers)
	}
	if !containsText(result.Notices, "桌面上手动添加的共享连接也需要更换密码") {
		t.Fatalf("缺少桌面连接提醒：%+v", result)
	}
	state := fixture.linux.State()
	if strings.Contains(state.RevokedMarker, "second-mount-password") ||
		strings.Contains(strings.Join(state.Commands, "\n"), "second-mount-password") {
		t.Fatalf("远端撤销标记或命令泄露新密码：%+v", state)
	}

	rows, err := fixture.store.QueryAudit(store.AuditFilter{
		OwnerOnly: true, Actions: []string{"share-revoke"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || strings.Contains(rows[0].Command, "second-mount-password") {
		t.Fatalf("共享撤销审计应只记录是否换密码：%+v", rows)
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(rows[0].Command), &summary); err != nil {
		t.Fatal(err)
	}
	if summary["password_changed"] != true {
		t.Fatalf("共享撤销审计摘要应记录换密码：%#v", summary)
	}
}

func TestRevokeUnavailableServerKeepsRegistrationAndPointsToResidue(t *testing.T) {
	fixture := newUnmountFixture(t, false)
	before := fixture.linux.State()
	fixture.closeServer()

	result, err := fixture.manager.Revoke(context.Background(), sharemanage.RevokeRequest{
		Share: "datasets", Server: "linux-web",
	})
	if err == nil || result == nil {
		t.Fatalf("服务器连不上时应返回结构化失败结果，结果 = %+v，错误 = %v", result, err)
	}
	if result.Result != "ssh-error" {
		t.Fatalf("远端失败分类 = %q", result.Result)
	}
	if !containsText(result.Notices, "登记变更已保留") ||
		!containsText(result.Notices, "shares residue") ||
		!containsText(result.Notices, "shares unmount") {
		t.Fatalf("远端失败提示 = %+v", result.Notices)
	}
	if !strings.Contains(err.Error(), "shares residue") ||
		!strings.Contains(err.Error(), "登记变更已保留") {
		t.Fatalf("错误应说明登记保留并指向 shares residue：%v", err)
	}
	share, err := fixture.store.GetSharedStorage("datasets")
	if err != nil {
		t.Fatal(err)
	}
	if share.Version != 2 || len(share.MountableServers) != 0 {
		t.Fatalf("连不上时登记变更应保留：%+v", share)
	}
	after := fixture.linux.State()
	if len(after.Commands) != len(before.Commands) || after.Credential != before.Credential ||
		after.Mounted != before.Mounted || after.RevokedMarker != before.RevokedMarker {
		t.Fatalf("连不上时远端状态不应变化：before=%+v after=%+v", before, after)
	}
}

func TestRevokeStopsWhenRemoteConfigurationVersionIsNewer(t *testing.T) {
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
				fixture.darwin.MapLines = nil
				fixture.darwin.AutoMasterLines = nil
				fixture.darwin.Mounted = false
				fixture.darwin.SeedManagedConfiguration("192.0.2.10", 3)
			} else {
				fixture.linux.FstabLines = nil
				fixture.linux.Credential = ""
				fixture.linux.Mounted = false
				fixture.linux.SeedManagedConfiguration("192.0.2.10", 3)
			}

			_, err := fixture.manager.Revoke(context.Background(), sharemanage.RevokeRequest{
				Share: "datasets", Server: testCase.server,
			})
			if !errors.Is(err, sharemount.ErrUnmountRemoteVersionNewer) {
				t.Fatalf("远端版本更新时的错误 = %v", err)
			}
			if testCase.system == "darwin" {
				state := fixture.darwin.State()
				if len(state.MapLines) != 2 || !state.Mounted || state.RevokedMarker != "" {
					t.Fatalf("远端版本更新时 macOS 状态不应改动：%+v", state)
				}
			} else {
				state := fixture.linux.State()
				if len(state.FstabLines) != 1 || state.Credential == "" ||
					!state.Mounted || state.RevokedMarker != "" {
					t.Fatalf("远端版本更新时 Linux 状态不应改动：%+v", state)
				}
			}
		})
	}
}

func TestRevokeDarwinDeletesConfigurationAndKeepsMountWithoutUnmount(t *testing.T) {
	fixture := newDarwinUnmountFixture(t, false)

	result, err := fixture.manager.Revoke(context.Background(), sharemanage.RevokeRequest{
		Share: "datasets", Server: "darwin-web",
	})
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if result.Result != "success" || result.Version != 2 || !result.MountPreserved {
		t.Fatalf("结构化结果 = %+v", result)
	}
	state := fixture.darwin.State()
	if len(state.MapLines) != 0 || len(state.AutoMasterLines) != 0 || !state.Mounted {
		t.Fatalf("不带 --unmount 时应删 macOS 配置但保留挂载：%+v", state)
	}
	if state.RevokedMarker != "sshmgr-share=datasets;sshmgr-version=2;sshmgr-state=revoked" {
		t.Fatalf("撤销标记 = %q", state.RevokedMarker)
	}
	if historyIndex(state.History, "unmount") >= 0 {
		t.Fatalf("不带 --unmount 时不应卸载：%v", state.History)
	}
}

func TestRevokeBusyServerRespectsForceOption(t *testing.T) {
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
			t.Run("占用时不强制", func(t *testing.T) {
				fixture := newSystemUnmountFixture(t, testCase.system, false)
				busy := []sharemounttest.BusyProcess{{ProcessID: "1234", User: "worker", Command: "python"}}
				if testCase.system == "darwin" {
					fixture.darwin.BusyProcesses = busy
				} else {
					fixture.linux.BusyProcesses = busy
				}
				result, err := fixture.manager.Revoke(context.Background(), sharemanage.RevokeRequest{
					Share: "datasets", Server: testCase.server, Unmount: true,
				})
				if err != nil {
					t.Fatal(err)
				}
				if result.Result != "busy" || len(result.BusyProcesses) != 1 ||
					result.BusyProcesses[0].ProcessID != "1234" {
					t.Fatalf("占用结果 = %+v", result)
				}
				if testCase.system == "darwin" {
					if !fixture.darwin.State().Mounted {
						t.Fatal("占用时不强制应保留挂载")
					}
				} else if !fixture.linux.State().Mounted {
					t.Fatal("占用时不强制应保留挂载")
				}
			})
			t.Run("占用时强制", func(t *testing.T) {
				fixture := newSystemUnmountFixture(t, testCase.system, false)
				if testCase.system == "darwin" {
					fixture.darwin.BusyProcesses = []sharemounttest.BusyProcess{{ProcessID: "1234", User: "worker", Command: "python"}}
					fixture.darwin.ForceSucceeds = true
				} else {
					fixture.linux.BusyProcesses = []sharemounttest.BusyProcess{{ProcessID: "1234", User: "worker", Command: "python"}}
					fixture.linux.ForceSucceeds = true
				}
				result, err := fixture.manager.Revoke(context.Background(), sharemanage.RevokeRequest{
					Share: "datasets", Server: testCase.server, Unmount: true, Force: true,
				})
				if err != nil {
					t.Fatal(err)
				}
				if result.Result != "success" {
					t.Fatalf("强制结果 = %+v", result)
				}
				if testCase.system == "darwin" {
					if fixture.darwin.State().Mounted {
						t.Fatal("强制后应卸载")
					}
				} else if fixture.linux.State().Mounted {
					t.Fatal("强制后应卸载")
				}
			})
		})
	}
}

func TestRevokeRemoteIsSeparateReusableEntryPoint(t *testing.T) {
	fixture := newUnmountFixture(t, false)
	server, err := fixture.store.ResolveServer("linux-web")
	if err != nil || server == nil {
		t.Fatalf("解析服务器失败：%v %+v", err, server)
	}

	result, err := fixture.manager.RevokeRemote(context.Background(), sharemanage.RevokeRemoteRequest{
		Server: server, ShareName: "datasets", Version: 2,
	})
	if err != nil {
		t.Fatalf("RevokeRemote: %v", err)
	}
	if result.Result != "success" || result.Version != 2 {
		t.Fatalf("远端撤销结果 = %+v", result)
	}
	state := fixture.linux.State()
	if state.Credential != "" || len(state.FstabLines) != 0 || !state.Mounted {
		t.Fatalf("远端撤销应删配置并保留挂载：%+v", state)
	}
	if state.RevokedMarker != "sshmgr-share=datasets;sshmgr-version=2;sshmgr-state=revoked" {
		t.Fatalf("撤销标记 = %q", state.RevokedMarker)
	}
}
