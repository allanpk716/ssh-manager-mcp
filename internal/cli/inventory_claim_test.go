package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/softwareinventory"
	"ssh-manager-mcp/internal/store"
)

// TestInventoryClaimAndUnclaimCLI 覆盖命令行认领与取消认领未识别进程，
// 并确认两次操作都写入命令行来源的审计。
func TestInventoryClaimAndUnclaimCLI(t *testing.T) {
	dbPath, masterKey := newSecretsEnv(t)
	st, err := store.Open(dbPath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("工作站", "", ""); err != nil {
		t.Fatal(err)
	}
	credentialID, err := st.SetCredential(&models.Credential{
		Type: models.CredPassword, Secret: []byte("password"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: "工作站-ssh", Host: "192.0.2.10", Port: 22, User: "owner",
		AuthMethod: models.AuthPassword, CredentialID: credentialID,
	}, nil, nil, "工作站"); err != nil {
		t.Fatal(err)
	}
	observed := models.SoftwareObservedItem{
		Category: softwareinventory.CategoryUnidentified, Name: "未识别进程",
		Version:        "0.9",
		ExecutablePath: "/opt/custom-service/service", ProcessName: "service",
		ListeningPorts: []softwareinventory.ListeningPort{{
			Address: "0.0.0.0", Port: 8443, Protocol: "tcp",
			Exposure: softwareinventory.ExposureAllAddresses,
		}},
		CollectedAt: time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC),
	}
	if err := st.ReplaceSoftwareObservedItems("工作站", store.SoftwareObservedWrite{
		Items:             []models.SoftwareObservedItem{observed},
		CommandSetVersion: "posix-v2", SystemType: "ubuntu",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	storedObserved, err := st.SoftwareObservedItemsForDevice("工作站")
	if err != nil || len(storedObserved) != 1 {
		t.Fatalf("准备实测条目失败：%v %+v", err, storedObserved)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	addOutput := mustInventoryCli(t, "", "inventory", "add", "工作站",
		"--category", "service_program", "--name", "自定义网站服务", "--version", "1.0",
		"--listen", "0.0.0.0,8443,tcp,public_network")
	declarationID := inventoryDeclarationID(t, addOutput)
	claimOutput := mustInventoryCli(t, "", "inventory", "claim", declarationID,
		"--observed-key", storedObserved[0].StableKey)
	if !strings.Contains(claimOutput, declarationID) || !strings.Contains(claimOutput, "已认领") {
		t.Fatalf("认领输出 = %q", claimOutput)
	}
	mustInventoryCli(t, "", "inventory", "edit", declarationID, "--version", "1.1")
	view := mustInventoryCli(t, "", "inventory", "view", "工作站")
	if !strings.Contains(view, "自定义网站服务 0.9（来源：两者合成）") ||
		!strings.Contains(view, "声明版本不一致：1.1") ||
		strings.Contains(view, "未识别进程  ") {
		t.Fatalf("认领后的查看结果应只显示合成条目：\n%s", view)
	}

	unclaimOutput := mustInventoryCli(t, "", "inventory", "unclaim", declarationID)
	if !strings.Contains(unclaimOutput, "已取消认领") {
		t.Fatalf("取消认领输出 = %q", unclaimOutput)
	}
	view = mustInventoryCli(t, "", "inventory", "view", "工作站")
	if !strings.Contains(view, "自定义网站服务 1.1（来源：声明）") ||
		!strings.Contains(view, "对不上：声明没有认领实测条目") ||
		!strings.Contains(view, "未识别进程") {
		t.Fatalf("取消认领后应重新显示声明与实测条目：\n%s", view)
	}

	todo := mustInventoryCli(t, "", "inventory", "todo")
	for _, want := range []string{
		"待确认暴露", "工作站", "8443", "未识别进程", "/opt/custom-service/service",
		"对不上与清单过期", "自定义网站服务",
	} {
		if !strings.Contains(todo, want) {
			t.Fatalf("待办输出缺少 %q：\n%s", want, todo)
		}
	}
	jsonTodo := mustInventoryCli(t, "", "inventory", "todo", "--json")
	var parsed softwareinventory.TodoResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(jsonTodo)), &parsed); err != nil {
		t.Fatalf("todo --json 应输出一行 JSON：%v\n%s", err, jsonTodo)
	}
	if len(parsed.UnidentifiedProcesses) != 1 ||
		parsed.UnidentifiedProcesses[0].ObservedStableKey != storedObserved[0].StableKey {
		t.Fatalf("todo JSON 的未识别进程 = %+v", parsed.UnidentifiedProcesses)
	}

	st, err = store.Open(dbPath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rows, err := st.QueryAudit(store.AuditFilter{Actions: []string{
		"software-declaration-claim", "software-declaration-unclaim",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("认领与取消认领审计数量 = %d：%+v", len(rows), rows)
	}
	for _, row := range rows {
		if row.Source != store.AuditSourceCLI || row.Status != "ok" {
			t.Fatalf("认领审计来源不正确：%+v", row)
		}
	}
}
