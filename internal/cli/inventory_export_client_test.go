package cli

import (
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
