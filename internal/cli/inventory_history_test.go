package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// seedInventoryHistoryVault 直接在命令行测试的 vault 里准备两台设备与两
// 次实测：工作站产生变化历史，网络存储没有采集执行者。
func seedInventoryHistoryVault(t *testing.T) {
	t.Helper()
	dbPath, key := newSecretsEnv(t)
	st, err := store.Open(dbPath, key)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, deviceName := range []string{"网络存储", "工作站"} {
		if _, err := st.AddDevice(deviceName, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	result, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: "工作站入口", Host: "192.0.2.10", Port: 22, User: "owner",
		ShellType: models.ShellPOSIX,
	}, &models.Credential{Type: models.CredPassword, Secret: []byte("password")}, nil, "工作站")
	if err != nil {
		t.Fatal(err)
	}
	secondAt := time.Now().Add(-time.Hour)
	firstAt := secondAt.Add(-24 * time.Hour)
	st.NowFn = func() time.Time { return firstAt }
	if err := st.ReplaceSoftwareObservedItems("工作站", store.SoftwareObservedWrite{
		Items: []models.SoftwareObservedItem{{
			Category: models.SoftwareCategorySystemPackage, Name: "openssh-server",
			Version: "1:9.6p1-1",
			ListeningPorts: []models.SoftwareListeningPort{{
				Address: "0.0.0.0", Port: 22, Protocol: "tcp",
				Exposure: models.SoftwareExposureAllAddresses,
			}},
		}},
		ServerID: result.ServerID, CommandSetVersion: "linux-v1", SystemType: "ubuntu",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	st.NowFn = func() time.Time { return secondAt }
	if err := st.ReplaceSoftwareObservedItems("工作站", store.SoftwareObservedWrite{
		Items: []models.SoftwareObservedItem{{
			Category: models.SoftwareCategorySystemPackage, Name: "openssh-server",
			Version: "1:9.9p1-1",
			ListeningPorts: []models.SoftwareListeningPort{{
				Address: "127.0.0.1", Port: 22, Protocol: "tcp",
				Exposure: models.SoftwareExposureLocal,
			}},
		}},
		ServerID: result.ServerID, CommandSetVersion: "linux-v1", SystemType: "ubuntu",
		Incomplete: true,
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	st.NowFn = func() time.Time { return time.Now() }
	if err := st.RecordSoftwareCollectionFailure(
		"工作站", "连接超时", result.ServerID, false, store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
}

// TestInventoryHistoryCommand 覆盖变化历史命令的人读输出与 JSON Lines 输
// 出：时间倒序，四种变化种类、旧值与新值可见。
func TestInventoryHistoryCommand(t *testing.T) {
	seedInventoryHistoryVault(t)
	output := mustInventoryCli(t, "", "inventory", "history", "工作站")
	for _, want := range []string{
		"版本变化", "openssh-server", "1:9.6p1-1", "1:9.9p1-1",
		"暴露级别变化", "监听所有地址", "仅本机", "新增",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("变化历史输出缺少 %q：\n%s", want, output)
		}
	}
	if strings.Index(output, "版本变化") > strings.Index(output, "新增") {
		t.Fatalf("变化历史应按时间倒序输出：\n%s", output)
	}

	jsonOutput := mustInventoryCli(t, "", "inventory", "history", "工作站", "--json")
	lines := strings.Split(strings.TrimSpace(jsonOutput), "\n")
	if len(lines) < 3 {
		t.Fatalf("变化历史 --json 至少三行，得到 %d 行：\n%s", len(lines), jsonOutput)
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("变化历史 --json 第一行不是 JSON：%v\n%s", err, lines[0])
	}
	if first["device_name"] != "工作站" || first["name"] != "openssh-server" {
		t.Fatalf("变化历史 JSON 行 = %+v", first)
	}
	if _, ok := first["kind"].(string); !ok {
		t.Fatalf("变化历史 JSON 行缺少变化种类：%+v", first)
	}
}

// TestInventoryStatusCommand 覆盖采集状态命令：列出全部设备或单台设备，
// 显示采集执行者、上次成功、上次失败与原因、是否不完整、命令集版本与
// 是否清单过期。
func TestInventoryStatusCommand(t *testing.T) {
	seedInventoryHistoryVault(t)
	output := mustInventoryCli(t, "", "inventory", "status")
	for _, want := range []string{
		"工作站", "权威端", "工作站入口", "上次成功", "命令集版本 linux-v1", "未过期",
		"上次失败：", "连接超时", "上次结果不完整",
		"网络存储", "从未采集", "上次结果完整", "不受管设备不算过期",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("采集状态输出缺少 %q：\n%s", want, output)
		}
	}

	jsonOutput := mustInventoryCli(t, "", "inventory", "status", "工作站", "--json")
	var status map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(jsonOutput)), &status); err != nil {
		t.Fatalf("采集状态 --json 应输出一行 JSON：%v\n%s", err, jsonOutput)
	}
	if status["device_name"] != "工作站" || status["collector_label"] != "权威端" ||
		status["stale"] != false || status["never_collected"] != false {
		t.Fatalf("采集状态 JSON 行 = %+v", status)
	}
	if _, ok := status["last_success_at"].(string); !ok {
		t.Fatalf("采集状态 JSON 行缺少上次成功时间：%+v", status)
	}
}
