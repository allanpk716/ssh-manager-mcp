package cli

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/softwareinventory"
	"ssh-manager-mcp/internal/store"
)

// seedInventoryObservedForExport 直接写入实测条目，供导出命令测试使用。
func seedInventoryObservedForExport(
	t *testing.T,
	items map[string][]models.SoftwareObservedItem,
) {
	t.Helper()
	st, err := store.Open(os.Getenv("SSHMGR_STORE"), mustDecodeHex(t, os.Getenv("SSHMGR_MASTERKEY_HEX")))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for device, rows := range items {
		if err := st.ReplaceSoftwareObservedItems(device, store.SoftwareObservedWrite{
			Items: rows, CommandSetVersion: "posix-v2", SystemType: "ubuntu",
		}, store.CLISource()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInventoryExportCycloneDXIncludesSystemPackages(t *testing.T) {
	newSecretsEnv(t)
	mustInventoryCli(t, "", "devices", "add", "gpu工作站")
	seedInventoryObservedForExport(t, map[string][]models.SoftwareObservedItem{
		"gpu工作站": {
			{Category: softwareinventory.CategorySystemPackage, Name: "libc6", Version: "2.39-0ubuntu8"},
			{Category: softwareinventory.CategoryServiceProgram, Name: "nginx", Version: "1.25.3"},
		},
	})

	output := mustInventoryCli(t, "", "inventory", "export", "--format", "cyclonedx")
	var document map[string]any
	if err := json.Unmarshal([]byte(output), &document); err != nil {
		t.Fatalf("导出不是合法 JSON：%v\n%s", err, output)
	}
	if document["bomFormat"] != "CycloneDX" || document["specVersion"] != "1.6" {
		t.Fatalf("CycloneDX 文档头 = %+v", document)
	}
	if !strings.Contains(output, `"libc6"`) || !strings.Contains(output, `"2.39-0ubuntu8"`) {
		t.Fatalf("全量导出缺少系统包：\n%s", output)
	}
	metadata, _ := document["metadata"].(map[string]any)
	profileValue := ""
	if properties, ok := metadata["properties"].([]any); ok {
		for _, property := range properties {
			propertyMap, _ := property.(map[string]any)
			if propertyMap["name"] == "sshmgr:profile-name" {
				profileValue, _ = propertyMap["value"].(string)
			}
		}
	}
	if profileValue != "owner" {
		t.Fatalf("本地导出缺少 owner 可见范围：\n%s", output)
	}
}

func TestInventoryExportRequiresCycloneDXFormat(t *testing.T) {
	newSecretsEnv(t)
	for _, args := range [][]string{
		{"inventory", "export"},
		{"inventory", "export", "--format", "spdx"},
	} {
		_, _, err := runInventoryRoot(t, "", args...)
		if err == nil {
			t.Fatalf("%v：导出格式必须显式写为 cyclonedx", args)
		}
	}
}

// readInventoryExportCSV 把命令输出的 CSV 用标准解析器读回；先断言字节顺
// 序标记与回车换行，避免只看整串文本。
func readInventoryExportCSV(t *testing.T, output string) [][]string {
	t.Helper()
	blob := []byte(output)
	if !bytes.HasPrefix(blob, []byte("\uFEFF")) {
		t.Fatalf("导出缺少 UTF-8 字节顺序标记：%q", blob)
	}
	if bytes.Contains(blob[3:], []byte("\n")) && !bytes.Contains(blob[3:], []byte("\r\n")) {
		t.Fatalf("导出换行必须是回车加换行：%q", blob)
	}
	rows, err := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(blob, []byte("\uFEFF")))).ReadAll()
	if err != nil {
		t.Fatalf("导出必须能用标准 CSV 解析器读回：%v\n%q", err, output)
	}
	if len(rows) == 0 {
		t.Fatalf("导出缺少表头：%q", output)
	}
	return rows
}

func TestInventoryExportCSVUsesCommandLineFilters(t *testing.T) {
	newSecretsEnv(t)
	st, err := store.Open(os.Getenv("SSHMGR_STORE"), mustDecodeHex(t, os.Getenv("SSHMGR_MASTERKEY_HEX")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSite("公司", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDeviceAtSite("工作站", "", "", "公司"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDeviceAtSite("分部工作站", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{
		Name: "总部物理局域网", Type: models.NetworkPhysical, SiteName: "公司",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership("工作站", "总部物理局域网", "192.0.2.10"); err != nil {
		t.Fatal(err)
	}
	seedInventoryObservedForExport(t, map[string][]models.SoftwareObservedItem{
		"工作站": {
			{
				Category: softwareinventory.CategoryServiceProgram,
				Name: `nginx,网页服务
含引号"特"`, Version: "1.25.3",
				ListeningPorts: []softwareinventory.ListeningPort{{
					Address: "0.0.0.0", Port: 80, Protocol: "tcp",
					Exposure: softwareinventory.ExposureAllAddresses,
				}},
			},
			{Category: softwareinventory.CategorySystemPackage, Name: "libc6", Version: "2.39-0ubuntu8"},
		},
		"分部工作站": {
			{Category: softwareinventory.CategoryServiceProgram, Name: "nginx", Version: "1.24.0"},
		},
	})
	st.Close()

	output := mustInventoryCli(t, "", "inventory", "export", "--format", "csv",
		"--name", "nginx", "--version", "1.25.3",
		"--site", "公司", "--network", "总部物理局域网")
	rows := readInventoryExportCSV(t, output)
	if strings.Join(rows[0], "|") != "设备|软件名|版本|缺版本|类别|监听端口|暴露级别|采集时间|待装安全更新" {
		t.Fatalf("CSV 表头 = %#v", rows[0])
	}
	if len(rows) != 2 || rows[1][0] != "工作站" ||
		rows[1][1] != "nginx,网页服务\n含引号\"特\"" || rows[1][2] != "1.25.3" ||
		rows[1][5] != "0.0.0.0:80/tcp" || rows[1][6] != "监听所有地址" {
		t.Fatalf("按条件导出的 CSV 行 = %#v", rows)
	}

	output = mustInventoryCli(t, "", "inventory", "export", "--format", "csv",
		"--device", "分部工作站")
	rows = readInventoryExportCSV(t, output)
	if len(rows) != 2 || rows[1][0] != "分部工作站" || rows[1][2] != "1.24.0" {
		t.Fatalf("单台设备导出的 CSV 行 = %#v", rows)
	}
}

func TestInventoryExportCSVWritesOutputFile(t *testing.T) {
	newSecretsEnv(t)
	mustInventoryCli(t, "", "devices", "add", "gpu工作站")
	seedInventoryObservedForExport(t, map[string][]models.SoftwareObservedItem{
		"gpu工作站": {
			{Category: softwareinventory.CategoryServiceProgram, Name: "nginx", Version: "1.25.3"},
		},
	})
	outputPath := filepath.Join(t.TempDir(), "software-inventory.csv")
	mustInventoryCli(t, "", "inventory", "export", "--format", "csv", "--output", outputPath)
	file, err := os.Open(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	blob, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	rows := readInventoryExportCSV(t, string(blob))
	if len(rows) != 2 || rows[1][1] != "nginx" {
		t.Fatalf("输出文件里的 CSV 行 = %#v", rows)
	}
}

func TestInventoryExportWritesOutputFile(t *testing.T) {
	newSecretsEnv(t)
	mustInventoryCli(t, "", "devices", "add", "gpu工作站")
	seedInventoryObservedForExport(t, map[string][]models.SoftwareObservedItem{
		"gpu工作站": {
			{Category: softwareinventory.CategoryServiceProgram, Name: "nginx", Version: "1.25.3"},
		},
	})
	outputPath := filepath.Join(t.TempDir(), "software-inventory.json")
	mustInventoryCli(t, "", "inventory", "export", "--format", "cyclonedx", "--output", outputPath)
	blob, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(blob), `"nginx"`) {
		t.Fatalf("输出文件缺少软件条目：\n%s", blob)
	}
}
