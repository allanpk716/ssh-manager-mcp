package webuisoftwareinventory

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
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
