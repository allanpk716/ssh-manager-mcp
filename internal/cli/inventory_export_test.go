package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInventoryExportCycloneDXIncludesSystemPackages(t *testing.T) {
	newSecretsEnv(t)
	mustInventoryCli(t, "", "devices", "add", "gpu工作站")
	mustInventoryCli(t, "", "inventory", "add", "gpu工作站",
		"--category", "system_package", "--name", "libc6", "--version", "2.39-0ubuntu8")
	mustInventoryCli(t, "", "inventory", "add", "gpu工作站",
		"--category", "service_program", "--name", "nginx", "--version", "1.25.3")

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

func TestInventoryExportWritesOutputFile(t *testing.T) {
	newSecretsEnv(t)
	mustInventoryCli(t, "", "devices", "add", "gpu工作站")
	mustInventoryCli(t, "", "inventory", "add", "gpu工作站",
		"--category", "service_program", "--name", "nginx", "--version", "1.25.3")
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
