package webuiops

import (
	"path/filepath"
	"testing"
	"time"

	"ssh-manager-mcp/internal/ops"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
)

func TestManagerAdapterQueriesAudit(t *testing.T) {
	key, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	wantTime := time.Unix(1800000000, 0)
	if err := st.WriteAudit(store.AuditRow{
		TS: wantTime, Action: "admin.login", Status: "ok",
		Source: store.AuditSourceWebUI, SourceIP: "192.0.2.23",
	}); err != nil {
		t.Fatal(err)
	}

	var source webui.OpsStore = FromManager(ops.New(st))
	rows, err := source.QueryAudit(webui.OpsAuditQuery{
		Source: store.AuditSourceWebUI, SourceIP: "192.0.2.23", Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID == 0 || rows[0].Action != "admin.login" ||
		!rows[0].TS.Equal(wantTime) || rows[0].Source != store.AuditSourceWebUI {
		t.Fatalf("适配后的审计行不完整：%+v", rows)
	}
}

func TestManagerAdapterFromOpenerOpensStorePerCall(t *testing.T) {
	key, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	opens := 0
	adapter := FromOpener(func() (*ops.Manager, func(), error) {
		opens++
		return ops.New(st), func() {}, nil
	})
	rows, err := adapter.QueryAudit(webui.OpsAuditQuery{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 || opens != 1 {
		t.Fatalf("按需打开的适配器应能查询审计：rows=%+v opens=%d", rows, opens)
	}
}
