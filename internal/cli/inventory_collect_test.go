package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/softwareinventory"
	"ssh-manager-mcp/internal/store"
)

// TestInventoryCollectAndSudoCLI 覆盖立即采集与 sudo 采集开关的命令行入
// 口：远程执行以回放函数注入，其余路径与生产一致。
func TestInventoryCollectAndSudoCLI(t *testing.T) {
	newSecretsEnv(t)
	st, err := store.Open(os.Getenv("SSHMGR_STORE"), mustDecodeHex(t, os.Getenv("SSHMGR_MASTERKEY_HEX")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("工作站", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: "entry-a", Host: "192.0.2.10", Port: 22, User: "owner",
		ShellType: models.ShellPOSIX,
	}, &models.Credential{Type: models.CredPassword, Secret: []byte("password")}, nil, "工作站"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	fixture, err := os.ReadFile(filepath.Join("..", "softwareinventory", "testdata", "posix", "ubuntu2404-full.txt"))
	if err != nil {
		t.Fatal(err)
	}
	previous := inventoryCollectService
	inventoryCollectService = func(s *store.Store) *softwareinventory.Service {
		return &softwareinventory.Service{
			Store: s,
			RemoteExec: func(_ context.Context, _ *models.Server, _ models.ShellType, _ string, _ bool) (softwareinventory.RemoteExecResult, error) {
				return softwareinventory.RemoteExecResult{Stdout: string(fixture)}, nil
			},
		}
	}
	t.Cleanup(func() { inventoryCollectService = previous })

	output := mustInventoryCli(t, "", "inventory", "collect", "--device", "工作站")
	for _, want := range []string{"工作站", "entry-a", "posix-v2", "ubuntu", "系统包", "1041", "未识别进程", "3"} {
		if !strings.Contains(output, want) {
			t.Fatalf("采集输出缺少 %q：\n%s", want, output)
		}
	}
	if strings.Contains(output, "不完整") {
		t.Fatalf("完整采集不应标为不完整：\n%s", output)
	}

	jsonOutput := mustInventoryCli(t, "", "inventory", "collect", "--device", "工作站", "--entry", "entry-a", "--json")
	var report map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(jsonOutput)), &report); err != nil {
		t.Fatalf("collect --json 应输出一行 JSON：%v\n%s", err, jsonOutput)
	}
	if report["device_name"] != "工作站" || report["server_entry_name"] != "entry-a" ||
		report["system_type"] != "ubuntu" || report["command_set_version"] != "posix-v2" {
		t.Fatalf("collect JSON = %+v", report)
	}

	sudoOutput := mustInventoryCli(t, "", "inventory", "sudo", "entry-a", "--enable")
	if !strings.Contains(sudoOutput, "entry-a") || !strings.Contains(sudoOutput, "已打开") {
		t.Fatalf("sudo 开关输出 = %q", sudoOutput)
	}
	sudoOutput = mustInventoryCli(t, "", "inventory", "sudo", "entry-a", "--disable")
	if !strings.Contains(sudoOutput, "已关闭") {
		t.Fatalf("sudo 开关关闭输出 = %q", sudoOutput)
	}
	if _, _, err := runInventoryRoot(t, "", "inventory", "sudo", "entry-a"); err == nil {
		t.Fatal("sudo 命令必须显式给出 --enable 或 --disable")
	}
}
