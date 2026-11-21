package softwareinventory

import (
	"bytes"
	"encoding/csv"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
)

// readCSVForTest 去掉字节顺序标记后用标准解析器读回导出的 CSV。
func readCSVForTest(t *testing.T, blob []byte) [][]string {
	t.Helper()
	if !bytes.HasPrefix(blob, []byte("\uFEFF")) {
		t.Fatalf("CSV 缺少 UTF-8 字节顺序标记：%q", blob)
	}
	if bytes.Contains(blob[3:], []byte("\n")) && !bytes.Contains(blob[3:], []byte("\r\n")) {
		t.Fatalf("CSV 换行必须是回车加换行：%q", blob)
	}
	reader := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(blob, []byte("\uFEFF"))))
	rows, err := reader.ReadAll()
	if err != nil {
		t.Fatalf("CSV 必须能用标准解析器读回：%v\n%q", err, blob)
	}
	return rows
}

func TestMarshalCSVProducesFlatSoftwareTable(t *testing.T) {
	collectedAt := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	var noTime *time.Time
	blob, err := MarshalCSV(QueryResult{Devices: []QueryDeviceResult{{
		DeviceID: "device-1", DeviceName: "gpu工作站",
		Items: []QueryItem{
			{
				Category: CategoryServiceProgram, CategoryLabel: CategoryServiceProgram.Label(),
				Name: `nginx,网页服务
含引号"特"`, Version: "1.25.3",
				ListeningPorts: []ListeningPort{
					{Address: "0.0.0.0", Port: 80, Protocol: "tcp", Exposure: ExposureAllAddresses},
					{Address: "192.0.2.10", Port: 8443, Protocol: "tcp", Exposure: ExposureSpecificAddress},
				},
				CollectedAt: &collectedAt,
			},
			{
				Category: CategorySystemPackage, CategoryLabel: CategorySystemPackage.Label(),
				Name: "libc6", Version: "2.39-0ubuntu8", CollectedAt: noTime,
			},
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}

	rows := readCSVForTest(t, blob)
	wantHeader := []string{"设备", "软件名", "版本", "缺版本", "类别", "监听端口", "暴露级别", "采集时间"}
	if len(rows) != 2 {
		t.Fatalf("CSV 行数 = %d（含表头应为 2，没有监听端口的系统包不导出）：\n%q", len(rows), blob)
	}
	if strings.Join(rows[0], "|") != strings.Join(wantHeader, "|") {
		t.Fatalf("表头 = %v，想要 %v", rows[0], wantHeader)
	}
	wantFirst := []string{
		"gpu工作站", "nginx,网页服务\n含引号\"特\"", "1.25.3", "", "监听程序",
		"0.0.0.0:80/tcp;192.0.2.10:8443/tcp", "监听所有地址;监听特定地址",
		"2026-11-01T08:00:00Z",
	}
	if strings.Join(rows[1], "|") != strings.Join(wantFirst, "|") {
		t.Fatalf("多端口软件行 = %#v，想要 %#v", rows[1], wantFirst)
	}
}

// TestMarshalCSVExportsFocusListOnlyWithMissingVersionColumn 钉住平铺总表
// 的两个重点清单口径：即使查询结果带着系统包（例如按名称搜索时自动包
// 含），没有监听端口的系统包也不导出；版本为空的条目在「缺版本」列标
// 「是」。
func TestMarshalCSVExportsFocusListOnlyWithMissingVersionColumn(t *testing.T) {
	manager, st := newTestManager(t)
	mustDevice(t, st, "工作站", true)
	mustObservedItems(t, st, "工作站",
		models.SoftwareObservedItem{
			Category: CategorySystemPackage, Name: "nginx", Version: "1.25.3",
			ListeningPorts: []models.SoftwareListeningPort{{
				Address: "0.0.0.0", Port: 80, Protocol: "tcp",
				Exposure: models.SoftwareExposureAllAddresses,
			}},
		},
		models.SoftwareObservedItem{Category: CategorySystemPackage, Name: "libc6", Version: "2.39-0ubuntu8"},
		models.SoftwareObservedItem{Category: CategoryUnidentified, Name: "unknown-gateway"},
	)

	result, err := manager.Query(QueryOptions{
		Name: "", IncludeSystemPackages: true, Visibility: AllDevicesVisible(),
	})
	if err != nil {
		t.Fatal(err)
	}
	blob, err := MarshalCSV(result)
	if err != nil {
		t.Fatal(err)
	}
	rows := readCSVForTest(t, blob)
	if len(rows) != 3 {
		t.Fatalf("CSV 行数 = %d（含表头应为 3：表头、nginx、缺版本网关）：\n%q", len(rows), blob)
	}
	for _, row := range rows[1:] {
		if row[1] == "libc6" {
			t.Fatalf("没有监听端口的系统包不应出现在 CSV 里：%#v", row)
		}
	}
	if rows[1][1] != "nginx" || rows[1][3] != "" {
		t.Fatalf("有版本的 nginx 行缺版本列应为空：%#v", rows[1])
	}
	if rows[2][1] != "unknown-gateway" || rows[2][3] != "是" {
		t.Fatalf("版本为空的条目缺版本列应为「是」：%#v", rows[2])
	}
}

func TestMarshalCSVUsesSharedDisplayCategory(t *testing.T) {
	blob, err := MarshalCSV(QueryResult{Devices: []QueryDeviceResult{{
		DeviceName: "工作站",
		Items: []QueryItem{
			{
				Category: CategoryOperatingSystem, CategoryLabel: CategoryOperatingSystem.Label(),
				Name: "Ubuntu", Version: "24.04.1 LTS",
			},
			{
				Category: CategoryDriverFirmware, CategoryLabel: CategoryDriverFirmware.Label(),
				Name: "Linux 内核", Version: "6.8.0-49-generic",
			},
			{
				Category: CategoryDriverFirmware, CategoryLabel: CategoryDriverFirmware.Label(),
				Name: "NVIDIA 显卡驱动", Version: "550.120",
			},
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	rows := readCSVForTest(t, blob)
	if len(rows) != 4 {
		t.Fatalf("CSV 应有一条表头和三条软件条目，得到 %d 行", len(rows))
	}
	got := []string{rows[1][4], rows[2][4], rows[3][4]}
	want := []string{"操作系统与内核", "操作系统与内核", "驱动与固件"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("CSV 类别 = %v，想要 %v", got, want)
	}
}

func TestMarshalCSVRowsMatchQueryFiltersAndSingleDevice(t *testing.T) {
	manager, st := newTestManager(t)
	mustDevice(t, st, "工作站", true)
	mustDevice(t, st, "分部工作站", true)
	mustObservedItems(t, st, "工作站",
		models.SoftwareObservedItem{
			Category: CategoryServiceProgram, Name: "nginx", Version: "1.25.3",
		},
		models.SoftwareObservedItem{
			Category: CategorySystemPackage, Name: "libc6", Version: "2.39-0ubuntu8",
		},
	)
	mustObservedItems(t, st, "分部工作站",
		models.SoftwareObservedItem{
			Category: CategoryServiceProgram, Name: "nginx", Version: "1.24.0",
		},
	)

	filtered, err := manager.Query(QueryOptions{
		Name: "nginx", ExactVersion: "1.25.3", IncludeSystemPackages: true,
		Visibility: AllDevicesVisible(),
	})
	if err != nil {
		t.Fatal(err)
	}
	blob, err := MarshalCSV(filtered)
	if err != nil {
		t.Fatal(err)
	}
	rows := readCSVForTest(t, blob)
	if len(rows) != 2 || rows[1][0] != "工作站" || rows[1][1] != "nginx" {
		t.Fatalf("筛选导出行数或内容不对：%#v", rows)
	}

	single, err := manager.Query(QueryOptions{
		Device: "分部工作站", IncludeSystemPackages: true, Visibility: AllDevicesVisible(),
	})
	if err != nil {
		t.Fatal(err)
	}
	blob, err = MarshalCSV(single)
	if err != nil {
		t.Fatal(err)
	}
	rows = readCSVForTest(t, blob)
	if len(rows) != 2 || rows[1][0] != "分部工作站" || rows[1][2] != "1.24.0" {
		t.Fatalf("单台设备导出行数或内容不对：%#v", rows)
	}
}
