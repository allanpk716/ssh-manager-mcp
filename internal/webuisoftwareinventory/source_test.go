package webuisoftwareinventory

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/softwareinventory"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
)

func newTestStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	key, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "vault.db")
	st, err := store.Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, path
}

func addTestDevice(t *testing.T, st *store.Store, name string) {
	t.Helper()
	if _, err := st.AddDevice(name, "", ""); err != nil {
		t.Fatal(err)
	}
	credentialID, err := st.SetCredential(&models.Credential{
		Type: models.CredPassword, Secret: []byte("test-password"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: name + "-ssh", Host: "192.0.2.10", Port: 22, User: "owner",
		AuthMethod: models.AuthPassword, CredentialID: credentialID,
	}, nil, nil, name); err != nil {
		t.Fatal(err)
	}
}

// TestSourceBuildsDeviceViewFromUnifiedQuery 验证适配层把统一查询、采集
// 状态与变化历史合成 Web UI 页面数据，并把系统包整理成默认折叠分组。
func TestSourceBuildsDeviceViewFromUnifiedQuery(t *testing.T) {
	st, _ := newTestStore(t)
	addTestDevice(t, st, "工作站")
	manager := softwareinventory.New(st)
	confirmedAt := time.Unix(1770000000, 0)
	st.NowFn = func() time.Time { return confirmedAt }
	if _, err := manager.AddDeclaration(softwareinventory.Declaration{
		Device: "工作站", Category: softwareinventory.CategoryServiceProgram,
		Name: "自建网关", Version: "1.2.3+build.7",
		ListeningPorts: []softwareinventory.ListeningPort{{
			Address: "10.0.0.5", Port: 8443, Protocol: "tcp",
			Exposure: softwareinventory.ExposureLocalAreaNetwork,
		}},
	}, softwareinventory.CLISource()); err != nil {
		t.Fatal(err)
	}
	server, err := st.GetServerByName("工作站-ssh")
	if err != nil || server == nil {
		t.Fatalf("取服务器条目失败：%v %+v", err, server)
	}
	firstAt := confirmedAt.Add(time.Hour)
	st.NowFn = func() time.Time { return firstAt }
	if err := st.ReplaceSoftwareObservedItems("工作站", store.SoftwareObservedWrite{
		Items: []models.SoftwareObservedItem{
			{
				Category: models.SoftwareCategoryContainerImage, Name: "postgres", Version: "16.4",
				BelongsTo: "Docker", ContainerTag: "16-alpine", ContainerDigest: "sha256:abcdef",
				ContainerRunning: true, ListeningPorts: []models.SoftwareListeningPort{{
					Address: "0.0.0.0", Port: 5432, Protocol: "tcp", Exposure: models.SoftwareExposureAllAddresses,
				}},
			},
			{Category: models.SoftwareCategorySystemPackage, Name: "curl", Version: "8.8.0-1"},
		},
		ServerID: server.ID, CommandSetVersion: softwareinventory.POSIXCommandSetVersion, SystemType: "ubuntu",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	secondAt := firstAt.Add(24 * time.Hour)
	st.NowFn = func() time.Time { return secondAt }
	if err := st.ReplaceSoftwareObservedItems("工作站", store.SoftwareObservedWrite{
		Items: []models.SoftwareObservedItem{
			{
				Category: models.SoftwareCategoryContainerImage, Name: "postgres", Version: "16.4",
				BelongsTo: "Docker", ContainerTag: "16-alpine", ContainerDigest: "sha256:abcdef",
				ContainerRunning: true, ListeningPorts: []models.SoftwareListeningPort{{
					Address: "0.0.0.0", Port: 5432, Protocol: "tcp", Exposure: models.SoftwareExposureAllAddresses,
				}},
			},
			{Category: models.SoftwareCategorySystemPackage, Name: "curl", Version: "8.9.1-1"},
		},
		ServerID: server.ID, CommandSetVersion: "linux-v1", SystemType: "ubuntu",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	st.NowFn = func() time.Time { return secondAt.Add(time.Second) }
	source := FromOpener(func() (*store.Store, func(), error) { return st, func() {}, nil })

	device, err := source.SoftwareInventoryDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if device.Name != "工作站" || !device.Status.Managed || device.Status.CollectorLabel != "权威端" ||
		device.Status.LastSuccessAt == nil || device.Status.LastSuccessAt.Unix() != secondAt.Unix() {
		t.Fatalf("设备页面数据或采集状态不符：%+v", device)
	}
	if len(device.Groups) != 3 || device.Groups[0].CategoryLabel != "服务程序" ||
		device.Groups[1].CategoryLabel != "容器镜像" || device.Groups[2].CategoryLabel != "系统包" {
		t.Fatalf("设备页面类别分组不符：%+v", device.Groups)
	}
	declared := device.Groups[0].Items[0]
	if declared.Version != "1.2.3+build.7" || declared.Source != "声明" ||
		declared.ConfirmedAt == nil || declared.ConfirmedAt.Unix() != confirmedAt.Unix() ||
		len(declared.ListeningPorts) != 1 || declared.ListeningPorts[0].ExposureLabel != "局域网" {
		t.Fatalf("声明条目页面数据不符：%+v", declared)
	}
	container := device.Groups[1].Items[0]
	if container.Version != "16.4" || container.Source != "实测" || container.BelongsTo != "Docker" ||
		container.ContainerTag != "16-alpine" || container.ContainerDigest != "sha256:abcdef" ||
		!container.ContainerRunning || container.ListeningPorts[0].ExposureLabel != "监听所有地址" {
		t.Fatalf("容器镜像条目页面数据不符：%+v", container)
	}
	if !device.Groups[2].SystemPackages || device.Groups[2].SystemPackageCount != 1 ||
		len(device.Groups[2].Items) != 1 || device.Groups[2].Items[0].Version != "8.9.1-1" {
		t.Fatalf("系统包分组应保留全量数据并给出数量：%+v", device.Groups[2])
	}
	var versionChange *webui.SoftwareInventoryChange
	for i := range device.History {
		if device.History[i].KindLabel == "版本变化" && device.History[i].Name == "curl" {
			versionChange = &device.History[i]
		}
	}
	if versionChange == nil || versionChange.OldValue != "8.8.0-1" || versionChange.NewValue != "8.9.1-1" {
		t.Fatalf("变化历史页面数据不符：%+v", device.History)
	}

	if _, err := source.SoftwareInventoryDevice("不存在"); err == nil {
		t.Fatal("不存在的设备应返回错误")
	} else {
		var missing *webui.SoftwareInventoryDeviceNotFoundError
		if !errors.As(err, &missing) {
			t.Fatalf("不存在的设备应返回页面层可识别的错误：%v", err)
		}
	}
}

// TestSourceRequestsCollectionForClientCollector 验证客户端实例作为采集
// 执行者时，立即采集只登记请求并写 Web UI 来源审计。
func TestSourceRequestsCollectionForClientCollector(t *testing.T) {
	st, path := newTestStore(t)
	addTestDevice(t, st, "工作站")
	firstAt := time.Unix(1770000000, 0)
	st.NowFn = func() time.Time { return firstAt }
	server, err := st.GetServerByName("工作站-ssh")
	if err != nil || server == nil {
		t.Fatalf("取服务器条目失败：%v %+v", err, server)
	}
	if err := st.ReplaceSoftwareObservedItems("工作站", store.SoftwareObservedWrite{
		Items: []models.SoftwareObservedItem{{
			Category: models.SoftwareCategoryOperatingSystem, Name: "Ubuntu", Version: "22.04.5",
		}}, ServerID: server.ID, CommandSetVersion: softwareinventory.POSIXCommandSetVersion, SystemType: "ubuntu",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(
		`UPDATE software_collection_status SET collector='laptop-client' WHERE device_id=?`, server.DeviceID,
	); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	source := FromOpener(func() (*store.Store, func(), error) { return st, func() {}, nil })

	result, err := source.RequestSoftwareInventoryCollection(
		context.Background(), "工作站",
		webui.SoftwareInventoryWriteSource{RemoteAddress: "198.51.100.9"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Requested {
		t.Fatalf("客户端采集执行者应只登记请求：%+v", result)
	}
	statuses, err := softwareinventory.New(st).CollectionStatuses(softwareinventory.AllDevicesVisible(), "工作站")
	if err != nil || len(statuses) != 1 {
		t.Fatalf("读取采集状态失败：%v %+v", err, statuses)
	}
	if !statuses[0].PendingImmediate || statuses[0].Collector != "laptop-client" {
		t.Fatalf("立即采集请求应保持客户端采集执行者并置为待执行：%+v", statuses[0])
	}
	auditRows, err := st.QueryAudit(store.AuditFilter{
		Actions: []string{"software-collect-request"}, Source: store.AuditSourceWebUI,
		SourceIP: "198.51.100.9", Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(auditRows) != 1 {
		t.Fatalf("立即采集请求应写一条 Web UI 来源审计：%+v", auditRows)
	}
}

// TestSourceBuildsTodosFromDomainResult 验证待办页适配层直接使用领域模块
// 的三类待办结果，字段不经过第二套计算。
func TestSourceBuildsTodosFromDomainResult(t *testing.T) {
	st, _ := newTestStore(t)
	addTestDevice(t, st, "工作站")
	server, err := st.GetServerByName("工作站-ssh")
	if err != nil || server == nil {
		t.Fatalf("取服务器条目失败：%v %+v", err, server)
	}
	if err := st.ReplaceSoftwareObservedItems("工作站", store.SoftwareObservedWrite{
		Items: []models.SoftwareObservedItem{{
			StableKey: "unidentified-8443", Category: models.SoftwareCategoryUnidentified,
			Name: "unknown-gateway", ProcessName: "gateway", ExecutablePath: "/opt/gateway/gateway",
			ListeningPorts: []models.SoftwareListeningPort{{
				Address: "0.0.0.0", Port: 8443, Protocol: "tcp",
				Exposure: models.SoftwareExposureAllAddresses,
			}},
		}},
		ServerID: server.ID, CommandSetVersion: "linux-v1", SystemType: "ubuntu",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	source := FromOpener(func() (*store.Store, func(), error) { return st, func() {}, nil })

	todos, err := source.SoftwareInventoryTodos()
	if err != nil {
		t.Fatal(err)
	}
	if len(todos.PendingExposure) != 1 || todos.PendingExposure[0].DeviceName != "工作站" ||
		len(todos.PendingExposure[0].Ports) != 1 ||
		todos.PendingExposure[0].Ports[0].ObservedStableKey != "unidentified-8443" ||
		todos.PendingExposure[0].Ports[0].ExposureLabel != "监听所有地址" {
		t.Fatalf("待确认暴露页面数据不符：%+v", todos.PendingExposure)
	}
	if len(todos.UnidentifiedProcesses) != 1 ||
		todos.UnidentifiedProcesses[0].ProcessName != "gateway" ||
		todos.UnidentifiedProcesses[0].ExecutablePath != "/opt/gateway/gateway" ||
		len(todos.UnidentifiedProcesses[0].ListeningPorts) != 1 {
		t.Fatalf("未识别进程页面数据不符：%+v", todos.UnidentifiedProcesses)
	}
	if len(todos.Mismatches) != 0 || len(todos.StaleDevices) != 0 {
		t.Fatalf("没有对应数据的待办不应凭空出现：%+v", todos)
	}
}

// TestSourceAddsDeclarationAndClaimsObservedItem 验证声明表单先走领域模块
// 新增，再用实测条目稳定键认领，并把 Web UI 来源写入两条审计。
func TestSourceAddsDeclarationAndClaimsObservedItem(t *testing.T) {
	st, _ := newTestStore(t)
	addTestDevice(t, st, "工作站")
	server, err := st.GetServerByName("工作站-ssh")
	if err != nil || server == nil {
		t.Fatalf("取服务器条目失败：%v %+v", err, server)
	}
	if err := st.ReplaceSoftwareObservedItems("工作站", store.SoftwareObservedWrite{
		Items: []models.SoftwareObservedItem{{
			StableKey: "unidentified-8443", Category: models.SoftwareCategoryUnidentified,
			Name: "unknown-gateway", ExecutablePath: "/opt/gateway/gateway",
			ListeningPorts: []models.SoftwareListeningPort{{
				Address: "0.0.0.0", Port: 8443, Protocol: "tcp",
				Exposure: models.SoftwareExposureAllAddresses,
			}},
		}}, ServerID: server.ID, CommandSetVersion: "linux-v1", SystemType: "ubuntu",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	source := FromOpener(func() (*store.Store, func(), error) { return st, func() {}, nil })

	id, err := source.AddSoftwareInventoryDeclaration(webui.SoftwareInventoryDeclarationForm{
		Device: "工作站", Category: "service_program", Name: "业务网关", Version: "2.1.0",
		ListeningPorts: []webui.SoftwareInventoryFormPort{{
			Address: "0.0.0.0", Port: 8443, Protocol: "tcp",
			Exposure: "virtual_local_area_network",
		}},
		ObservedStableKey: "unidentified-8443",
	}, webui.SoftwareInventoryWriteSource{RemoteAddress: "198.51.100.9"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := softwareinventory.New(st).Query(softwareinventory.QueryOptions{
		Device: "工作站", Name: "业务网关", Visibility: softwareinventory.AllDevicesVisible(),
		IncludeSystemPackages: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Devices) != 1 || len(result.Devices[0].Items) != 1 ||
		result.Devices[0].Items[0].Source != softwareinventory.SourceCombined {
		t.Fatalf("新增并认领后应合成一条软件条目：%+v", result.Devices)
	}
	rows, err := st.QueryAudit(store.AuditFilter{
		Actions: []string{"software-declaration-add", "software-declaration-claim"},
		Source:  store.AuditSourceWebUI, SourceIP: "198.51.100.9", Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]bool{}
	for _, row := range rows {
		actions[row.Action] = true
	}
	if len(rows) != 2 || !actions["software-declaration-add"] || !actions["software-declaration-claim"] {
		t.Fatalf("新增与认领应各写一条 Web UI 来源审计：%+v", rows)
	}
	if id == "" {
		t.Fatal("新增声明应返回声明标识符")
	}
}

// TestSourceEditsClaimsAndDeletesDeclaration 验证修改、认领与删除都调用软
// 件清单领域模块，并统一记录 Web UI 加来源地址的审计。
func TestSourceEditsClaimsAndDeletesDeclaration(t *testing.T) {
	st, _ := newTestStore(t)
	addTestDevice(t, st, "工作站")
	manager := softwareinventory.New(st)
	id, err := manager.AddDeclaration(softwareinventory.Declaration{
		Device: "工作站", Category: softwareinventory.CategoryServiceProgram,
		Name: "业务网关", Version: "2.1.0",
	}, softwareinventory.CLISource())
	if err != nil {
		t.Fatal(err)
	}
	source := FromOpener(func() (*store.Store, func(), error) { return st, func() {}, nil })
	form := webui.SoftwareInventoryDeclarationForm{
		ID: id, Device: "工作站", Category: "service_program",
		Name: "业务网关", Version: "2.2.0", PackageURL: "pkg:generic/gateway@2.2.0",
		ListeningPorts: []webui.SoftwareInventoryFormPort{{
			Address: "0.0.0.0", Port: 8443, Protocol: "tcp", Exposure: "public_network",
		}},
	}
	if err := source.EditSoftwareInventoryDeclaration(
		id, form, webui.SoftwareInventoryWriteSource{RemoteAddress: "192.0.2.7"},
	); err != nil {
		t.Fatal(err)
	}
	if err := source.ClaimSoftwareInventoryDeclaration(
		id, "observed-1", webui.SoftwareInventoryWriteSource{RemoteAddress: "192.0.2.7"},
	); err == nil {
		t.Fatal("认领不存在的实测条目应失败")
	}
	declaration, err := source.SoftwareInventoryDeclaration(id)
	if err != nil {
		t.Fatal(err)
	}
	if declaration.Version != "2.2.0" || declaration.PackageURL != "pkg:generic/gateway@2.2.0" ||
		len(declaration.ListeningPorts) != 1 || declaration.ListeningPorts[0].Exposure != "public_network" {
		t.Fatalf("修改后的声明页面数据不符：%+v", declaration)
	}
	if err := source.DeleteSoftwareInventoryDeclaration(
		id, webui.SoftwareInventoryWriteSource{RemoteAddress: "192.0.2.7"},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := source.SoftwareInventoryDeclaration(id); err == nil {
		t.Fatal("删除后的声明不应再可读")
	}
	rows, err := st.QueryAudit(store.AuditFilter{
		Actions: []string{"software-declaration-edit", "software-declaration-rm"},
		Source:  store.AuditSourceWebUI, SourceIP: "192.0.2.7", Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]bool{}
	for _, row := range rows {
		actions[row.Action] = true
	}
	if len(rows) != 2 || !actions["software-declaration-edit"] || !actions["software-declaration-rm"] {
		t.Fatalf("修改与删除应各写一条 Web UI 来源审计：%+v", rows)
	}
}

// TestSourceReviewsDeclarationDrafts 覆盖草稿列出、按原样确认、修改后确
// 认与驳回；确认进入生效声明，驳回后消失，全部写审计为 Web UI 来源。
func TestSourceReviewsDeclarationDrafts(t *testing.T) {
	st, _ := newTestStore(t)
	addTestDevice(t, st, "工作站")
	manager := softwareinventory.New(st)
	ids, err := manager.SubmitDeclarationDrafts([]models.SoftwareDeclarationDraft{
		{
			SoftwareDeclaration: models.SoftwareDeclaration{
				Device: "工作站", Category: softwareinventory.CategoryServiceProgram,
				Name: "业务网关", Version: "2.1.0",
			},
		},
		{
			SoftwareDeclaration: models.SoftwareDeclaration{
				Device: "工作站", Category: softwareinventory.CategoryServiceProgram,
				Name: "旧网关", Version: "1.0",
			},
		},
		{
			SoftwareDeclaration: models.SoftwareDeclaration{
				Device: "工作站", Category: softwareinventory.CategoryServiceProgram,
				Name: "驳回网关", Version: "0.9",
			},
		},
	}, softwareinventory.AllDevicesVisible(), softwareinventory.AgentSource())
	if err != nil || len(ids) != 3 {
		t.Fatalf("提交三条草稿失败：%v %v", err, ids)
	}
	source := FromOpener(func() (*store.Store, func(), error) { return st, func() {}, nil })

	drafts, err := source.SoftwareInventoryDrafts()
	if err != nil {
		t.Fatal(err)
	}
	if len(drafts) != 3 || drafts[0].ID != ids[0] || drafts[0].Device != "工作站" ||
		drafts[0].CategoryLabel != "服务程序" || drafts[0].Name != "业务网关" {
		t.Fatalf("草稿页面数据不符：%+v", drafts)
	}
	if err := source.ConfirmSoftwareInventoryDraft(
		ids[0], nil, webui.SoftwareInventoryWriteSource{RemoteAddress: "192.0.2.7"},
	); err != nil {
		t.Fatal(err)
	}
	modified := webui.SoftwareInventoryDeclarationForm{
		ID: ids[1], Device: "工作站", Category: "service_program",
		Name: "旧网关", Version: "1.1.0",
		ListeningPorts: []webui.SoftwareInventoryFormPort{{
			Address: "0.0.0.0", Port: 9090, Protocol: "tcp", Exposure: "local_area_network",
		}},
	}
	if err := source.ConfirmSoftwareInventoryDraft(
		ids[1], &modified, webui.SoftwareInventoryWriteSource{RemoteAddress: "192.0.2.7"},
	); err != nil {
		t.Fatal(err)
	}
	if err := source.RejectSoftwareInventoryDraft(
		ids[2], webui.SoftwareInventoryWriteSource{RemoteAddress: "192.0.2.7"},
	); err != nil {
		t.Fatal(err)
	}
	result, err := manager.Query(softwareinventory.QueryOptions{
		Device: "工作站", Visibility: softwareinventory.AllDevicesVisible(), IncludeSystemPackages: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Devices) != 1 || len(result.Devices[0].Items) != 2 {
		t.Fatalf("两条确认草稿应进入生效清单：%+v", result.Devices)
	}
	remaining, err := source.SoftwareInventoryDrafts()
	if err != nil || len(remaining) != 0 {
		t.Fatalf("确认与驳回后的草稿都应消失：%v %+v", err, remaining)
	}
	rows, err := st.QueryAudit(store.AuditFilter{
		Actions: []string{"software-declaration-confirm", "software-declaration-confirm-edit", "software-declaration-reject"},
		Source:  store.AuditSourceWebUI, SourceIP: "192.0.2.7", Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("草稿确认、修改后确认与驳回应各写一条 Web UI 来源审计：%+v", rows)
	}
}

// TestSourceCollectorSettingsAndSudo 验证采集设置读取、登记校验错误透
// 出、成功登记与 sudo 开关写入 Web UI 来源审计。
func TestSourceCollectorSettingsAndSudo(t *testing.T) {
	st, _ := newTestStore(t)
	addTestDevice(t, st, "工作站")
	server, err := st.GetServerByName("工作站-ssh")
	if err != nil || server == nil {
		t.Fatalf("取服务器条目失败：%v %+v", err, server)
	}
	profileID, err := st.AddProfile("运维")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{server.ID}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.AddCacheToken("laptop-client", profileID); err != nil {
		t.Fatal(err)
	}
	source := FromOpener(func() (*store.Store, func(), error) { return st, func() {}, nil })

	settings, err := source.SoftwareInventorySettings()
	if err != nil {
		t.Fatal(err)
	}
	if len(settings.Devices) != 1 || settings.Devices[0].Name != "工作站" ||
		len(settings.Servers) != 1 || settings.Servers[0].Name != server.Name ||
		len(settings.ClientInstances) != 1 || settings.ClientInstances[0].Name != "laptop-client" ||
		settings.ClientInstances[0].ProfileName != "运维" {
		t.Fatalf("采集设置页面数据不符：%+v", settings)
	}
	err = source.SetSoftwareInventoryCollector(webui.SoftwareInventoryCollectorForm{
		Device: "工作站", Collector: "不存在的客户端", ServerEntry: server.Name,
	}, webui.SoftwareInventoryWriteSource{RemoteAddress: "192.0.2.7"})
	var formErr *webui.SoftwareInventoryFormError
	if !errors.As(err, &formErr) || !strings.Contains(formErr.Message, "不是有效的活动设备码") {
		t.Fatalf("采集执行者登记校验错误应可在页面展示：%v", err)
	}
	if err := source.SetSoftwareInventoryCollector(webui.SoftwareInventoryCollectorForm{
		Device: "工作站", Collector: "laptop-client", ServerEntry: server.Name,
	}, webui.SoftwareInventoryWriteSource{RemoteAddress: "192.0.2.7"}); err != nil {
		t.Fatal(err)
	}
	if err := source.SetSoftwareInventorySudo(
		server.Name, true, webui.SoftwareInventoryWriteSource{RemoteAddress: "192.0.2.7"},
	); err != nil {
		t.Fatal(err)
	}
	settings, err = source.SoftwareInventorySettings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.Servers[0].SudoEnabled != true {
		t.Fatalf("sudo 开关打开后的设置页面数据不符：%+v", settings.Servers[0])
	}
	rows, err := st.QueryAudit(store.AuditFilter{
		Actions: []string{"software-collect-collector", "software-collect-sudo"},
		Source:  store.AuditSourceWebUI, SourceIP: "192.0.2.7", Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("采集执行者与 sudo 开关应各写一条 Web UI 来源审计：%+v", rows)
	}
}
