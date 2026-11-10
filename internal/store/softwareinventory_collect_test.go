package store

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
)

func newCollectTestStore(t *testing.T) *Store {
	t.Helper()
	key, err := GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := Open(filepath.Join(t.TempDir(), "vault.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func addCollectServer(t *testing.T, st *Store, deviceName, serverName string) *models.Server {
	t.Helper()
	var device *models.Device
	if existing, getErr := st.GetDevice(deviceName); getErr == nil {
		device = existing
	} else {
		added, addErr := st.AddDevice(deviceName, "", "")
		if addErr != nil {
			t.Fatal(addErr)
		}
		device = added
	}
	result, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: serverName, Host: "192.0.2.10", Port: 22, User: "owner",
		ShellType: models.ShellPOSIX,
	}, &models.Credential{
		Type: models.CredPassword, Secret: []byte("test-password"),
	}, nil, deviceName)
	if err != nil {
		t.Fatal(err)
	}
	if result.ServerID == "" || result.Device.ID != device.ID {
		t.Fatalf("服务器条目没有挂到设备上：%+v", result)
	}
	server, err := st.GetServer(result.ServerID)
	if err != nil {
		t.Fatal(err)
	}
	return server
}

// TestSoftwareCollectSudoSetting 验证 sudo 采集开关存在软件清单自己的设
// 置表里、以服务器条目为键、默认关闭、每次修改写审计。
func TestSoftwareCollectSudoSetting(t *testing.T) {
	st := newCollectTestStore(t)
	server := addCollectServer(t, st, "工作站", "工作站入口")

	enabled, err := st.SoftwareCollectSudoEnabled()
	if err != nil {
		t.Fatal(err)
	}
	if enabled[server.ID] {
		t.Fatal("sudo 采集开关默认关闭")
	}
	if err := st.SetSoftwareCollectSudo(server.Name, true, CLISource()); err != nil {
		t.Fatal(err)
	}
	enabled, err = st.SoftwareCollectSudoEnabled()
	if err != nil {
		t.Fatal(err)
	}
	if !enabled[server.ID] {
		t.Fatal("打开开关后应能读到开启状态")
	}
	if err := st.SetSoftwareCollectSudo(server.ID, false, CLISource()); err != nil {
		t.Fatal(err)
	}
	enabled, err = st.SoftwareCollectSudoEnabled()
	if err != nil {
		t.Fatal(err)
	}
	if enabled[server.ID] {
		t.Fatal("开关应能关闭")
	}
	rows, err := st.QueryAudit(AuditFilter{Actions: []string{"software-collect-sudo"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("sudo 开关每次修改都应写审计，得到 %d 行", len(rows))
	}
	// 审计按编号倒序返回：rows[0] 是最后一次（关闭），rows[1] 是打开。
	var summary map[string]any
	if err := json.Unmarshal([]byte(rows[1].Command), &summary); err != nil {
		t.Fatal(err)
	}
	if summary["server"] != "工作站入口" || summary["sudo"] != true {
		t.Fatalf("sudo 开关审计摘要 = %v", summary)
	}
}

// TestSoftwareObservedItemsReplaceAndStatus 验证成功采集整体替换实测条目
// 与监听端口，并更新采集状态；失败只记录时间与原因，保留上一次结果。
func TestSoftwareObservedItemsReplaceAndStatus(t *testing.T) {
	st := newCollectTestStore(t)
	now := time.Unix(1770000000, 0)
	st.NowFn = func() time.Time { return now }
	server := addCollectServer(t, st, "工作站", "工作站入口")

	first := []models.SoftwareObservedItem{
		{
			Category: models.SoftwareCategorySystemPackage, Name: "openssh-server",
			Version: "1:9.6p1-3ubuntu13.20", Architecture: "amd64",
			PackageURL: "pkg:deb/ubuntu/openssh-server@1:9.6p1-3ubuntu13.20?arch=amd64",
			ListeningPorts: []models.SoftwareListeningPort{{
				Address: "0.0.0.0", Port: 22, Protocol: "tcp",
				Exposure: models.SoftwareExposureAllAddresses,
			}},
		},
		{
			Category: models.SoftwareCategoryUnidentified, Name: "python",
			ExecutablePath: "/opt/app/python", ProcessName: "python",
			ListeningPorts: []models.SoftwareListeningPort{{
				Address: "127.0.0.1", Port: 8765, Protocol: "tcp",
				Exposure: models.SoftwareExposureLocal,
			}},
		},
	}
	if err := st.ReplaceSoftwareObservedItems("工作站", SoftwareObservedWrite{
		Items: first, ServerID: server.ID, CommandSetVersion: "posix-v2",
		SystemType: "ubuntu",
	}, CLISource()); err != nil {
		t.Fatal(err)
	}
	items, err := st.SoftwareObservedItemsForDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].StableKey == "" || items[0].CollectedAt != now ||
		len(items[0].ListeningPorts) != 1 {
		t.Fatalf("第一次采集的实测条目 = %+v", items)
	}
	status, err := st.SoftwareCollectionStatusForDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if status.LastSuccessAt != now || status.LastIncomplete || status.CommandSetVersion != "posix-v2" ||
		status.SystemType != "ubuntu" || status.LastServerID != server.ID || status.Collector.IsClient() {
		t.Fatalf("采集状态 = %+v", status)
	}

	// 第二次成功采集整体替换：旧条目消失，新条目只有一条。
	if err := st.ReplaceSoftwareObservedItems("工作站", SoftwareObservedWrite{
		Items: []models.SoftwareObservedItem{{
			Category: models.SoftwareCategorySystemPackage, Name: "nginx", Version: "1.24.0",
		}},
		ServerID: server.ID, CommandSetVersion: "posix-v2", SystemType: "ubuntu", Incomplete: true,
	}, CLISource()); err != nil {
		t.Fatal(err)
	}
	items, err = st.SoftwareObservedItemsForDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Name != "nginx" {
		t.Fatalf("成功采集应整体替换实测条目：%+v", items)
	}
	status, err = st.SoftwareCollectionStatusForDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if !status.LastIncomplete {
		t.Fatalf("不完整标志应写入状态：%+v", status)
	}

	// 失败：条目保留，失败时间与原因进入状态。
	failureAt := now.Add(time.Hour)
	st.NowFn = func() time.Time { return failureAt }
	if err := st.RecordSoftwareCollectionFailure("工作站", "连接超时", server.ID, false, CLISource()); err != nil {
		t.Fatal(err)
	}
	items, err = st.SoftwareObservedItemsForDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Name != "nginx" {
		t.Fatalf("失败应保留上一次结果：%+v", items)
	}
	status, err = st.SoftwareCollectionStatusForDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if status.LastFailureAt != failureAt || status.LastFailureReason != "连接超时" ||
		status.LastSuccessAt != now {
		t.Fatalf("失败状态 = %+v", status)
	}
	rows, err := st.QueryAudit(AuditFilter{Actions: []string{"software-collect"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("每次采集应写一行审计，得到 %d 行", len(rows))
	}
	// 审计按编号倒序返回：最新一行是失败那次。
	var summary map[string]any
	if err := json.Unmarshal([]byte(rows[0].Command), &summary); err != nil {
		t.Fatal(err)
	}
	if summary["device"] != "工作站" || summary["server"] != "工作站入口" ||
		summary["collector"] != "" || summary["result"] != "failed" {
		t.Fatalf("采集审计摘要 = %v", summary)
	}
}

// TestFullServersForDevice 验证领域模块能按设备拿到完整服务器条目（包含
// shell 类型与 sudo 凭据信息），并按条目名字排序。
func TestFullServersForDevice(t *testing.T) {
	st := newCollectTestStore(t)
	addCollectServer(t, st, "工作站", "乙入口")
	addCollectServer(t, st, "工作站", "甲入口")
	servers, err := st.FullServersForDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	// SQLite 按字节序排序中文名，这里只钉住数量与关键字段，顺序语义在
	// 领域模块的条目选择测试里用 ASCII 名字验证。
	if len(servers) != 2 ||
		servers[0].ShellType != models.ShellPOSIX || servers[0].CredentialID == "" ||
		servers[1].CredentialID == "" {
		t.Fatalf("按设备取完整条目 = %+v / %+v", *servers[0], *servers[1])
	}
	if _, err := st.FullServersForDevice("不存在的设备"); err == nil {
		t.Fatal("设备不存在应返回错误")
	}
}
