package cli

import (
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/softwareinventory"
	"ssh-manager-mcp/internal/store"
)

// TestInventoryViewAndSearchIncludeObservedOnlyItems 命令行的查看与搜索也
// 走实测加声明的统一查询；没有任何声明的软件能被查到。
func TestInventoryViewAndSearchIncludeObservedOnlyItems(t *testing.T) {
	dbPath, masterKey := newSecretsEnv(t)
	st, err := store.Open(dbPath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("工作站", "", ""); err != nil {
		t.Fatal(err)
	}
	item := models.SoftwareObservedItem{
		Category: softwareinventory.CategoryServiceProgram,
		Name:     "observed-only", Version: "2.0",
		CollectedAt: time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC),
	}
	if err := st.ReplaceSoftwareObservedItems("工作站", store.SoftwareObservedWrite{
		Items:             []models.SoftwareObservedItem{item},
		CommandSetVersion: "posix-v2", SystemType: "ubuntu",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	view := mustInventoryCli(t, "", "inventory", "view", "工作站")
	if !strings.Contains(view, "observed-only 2.0（来源：实测）") {
		t.Fatalf("按设备查看应包含只有实测的条目：\n%s", view)
	}
	search := mustInventoryCli(t, "", "inventory", "search", "OBSERVED")
	if !strings.Contains(search, "工作站") || !strings.Contains(search, "来源：实测") {
		t.Fatalf("跨设备搜索应包含只有实测的条目：\n%s", search)
	}
}
