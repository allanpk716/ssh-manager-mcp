package cli

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/clientops"
)

func TestInventoryClientExportsCycloneDXOnline(t *testing.T) {
	stop := newInventoryClientFixture(t)
	defer stop()

	output := mustInventoryCli(t, "", "inventory", "export", "--format", "cyclonedx")
	var document map[string]any
	if err := json.Unmarshal([]byte(output), &document); err != nil {
		t.Fatalf("客户端导出不是合法 JSON：%v\n%s", err, output)
	}
	if !strings.Contains(output, `"gpu-设备"`) || !strings.Contains(output, `"nginx"`) ||
		!strings.Contains(output, `"libc6"`) {
		t.Fatalf("客户端导出缺少权威端在线查询结果或系统包：\n%s", output)
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
	if profileValue != "inventory-profile" {
		t.Fatalf("客户端导出的 profile 名 = %q，应为 inventory-profile", profileValue)
	}
}

func TestInventoryClientExportsCSVOnline(t *testing.T) {
	stop := newInventoryClientFixture(t)
	defer stop()

	output := mustInventoryCli(t, "", "inventory", "export", "--format", "csv",
		"--name", "nginx", "--version", "1.25.3",
		"--site", "总部", "--network", "总部物理局域网")
	blob := []byte(output)
	if !bytes.HasPrefix(blob, []byte("\uFEFF")) {
		t.Fatalf("客户端导出缺少 UTF-8 字节顺序标记：%q", blob)
	}
	rows, err := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(blob, []byte("\uFEFF")))).ReadAll()
	if err != nil {
		t.Fatalf("客户端导出不是合法 CSV：%v\n%s", err, output)
	}
	if len(rows) != 2 || rows[1][0] != "gpu-设备" || rows[1][1] != "nginx" ||
		rows[1][2] != "1.25.3" || rows[1][5] != "0.0.0.0:80/tcp" {
		t.Fatalf("客户端按条件导出的 CSV 行 = %#v", rows)
	}
}

func TestInventoryClientExportUnreachableUsesDedicatedExitCode(t *testing.T) {
	stop := newInventoryClientFixture(t)
	stop()

	_, _, err := runInventoryRoot(t, "", "inventory", "export", "--format", "cyclonedx")
	if err == nil {
		t.Fatal("权威端停机时导出应报错")
	}
	if err.Error() != clientops.SoftwareInventoryUnreachableText {
		t.Fatalf("导出错误文本不是固定契约：%v", err)
	}
	if code := ExitCodeFor(err); code != 4 {
		t.Fatalf("导出权威端不可达退出码 = %d，应为 4", code)
	}
}
