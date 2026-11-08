package webuiservers

import (
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/servermanage"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
)

func newAdapterManager(t *testing.T) (*servermanage.Manager, *store.Store) {
	t.Helper()
	key, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return servermanage.New(st), st
}

// TestWriterAdapterEditsAndAuditsWebUISource 用真实 vault 验证适配器：编辑走
// 服务器管理模块，审计来源为 Web UI 并带来源地址。
func TestWriterAdapterEditsAndAuditsWebUISource(t *testing.T) {
	manager, st := newAdapterManager(t)
	id, err := st.AddServerWithCredentials(&models.Server{
		Name: "web", Host: "192.0.2.10", Port: 22, User: "root",
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	writer := WriterFromManager(manager)

	detail, err := FromManager(manager).ServerDetail("", "web")
	if err != nil || detail == nil || detail.ID != id {
		t.Fatalf("详情适配不正确：%+v %v", detail, err)
	}
	role := "网站"
	result, conflict, err := writer.EditServer(id, webui.ServerEditForm{
		Role: &role, ExpectedRevision: detail.Revision,
	}, webui.ServerWriteSource{RemoteAddress: "192.0.2.7"})
	if err != nil || conflict != nil {
		t.Fatalf("编辑失败：%v conflict=%+v", err, conflict)
	}
	if result.Detail.Role != "网站" {
		t.Fatalf("编辑结果不正确：%+v", result.Detail)
	}

	stale := detail.Revision
	_, conflict, err = writer.EditServer(id, webui.ServerEditForm{
		Role: &role, ExpectedRevision: stale,
	}, webui.ServerWriteSource{RemoteAddress: "192.0.2.7"})
	if err != nil || conflict == nil || conflict.Current.Role != "网站" {
		t.Fatalf("修订号冲突应返回当前值：%v %+v", err, conflict)
	}

	rows, err := st.QueryAudit(store.AuditFilter{Actions: []string{"server.edit"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("应有两条编辑审计（成功与冲突失败），实际 %d", len(rows))
	}
	for _, row := range rows {
		if row.Source != store.AuditSourceWebUI || row.SourceIP != "192.0.2.7" {
			t.Fatalf("审计来源不正确：%+v", row)
		}
		if strings.Contains(row.Command, "网站") {
			t.Fatalf("审计摘要不得包含字段值：%s", row.Command)
		}
	}
}
