package secretmanage

import (
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// newTestManager 打开一个全新的临时加密 vault，并返回挂在它上面的机密项
// 管理模块与存储句柄。存储句柄只用于准备服务器条目和读回审计行。
func newTestManager(t *testing.T) (*Manager, *store.Store) {
	t.Helper()
	key, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return New(st), st
}

// mustServer 新增一台带密码凭据的服务器条目，返回标识符与名字。
func mustServer(t *testing.T, st *store.Store, name string) string {
	t.Helper()
	credentialID, err := st.SetCredential(&models.Credential{
		Type: models.CredPassword, Secret: []byte("test-password"),
	})
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.AddServer(&models.Server{
		Name: name, Host: "192.0.2.10", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: credentialID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestWriteOperationsRecordSource 验证每个机密项写操作的审计来源：命令行来
// 源不带地址，Web UI 来源带来源地址；所有摘要都不包含值。
func TestWriteOperationsRecordSource(t *testing.T) {
	m, st := newTestManager(t)
	web1 := mustServer(t, st, "web1")
	web2 := mustServer(t, st, "web2")

	if err := m.AddSecret("DEPLOY_TOKEN", "部署令牌", "HF_TOKEN", []byte("web-add-secret-value"), WebUISource("192.0.2.7")); err != nil {
		t.Fatal(err)
	}
	if err := m.SetSecretValue("DEPLOY_TOKEN", []byte("cli-set-secret-value"), CLISource()); err != nil {
		t.Fatal(err)
	}
	note := "生产部署令牌"
	environmentName := "MODEL_TOKEN"
	if err := m.EditMetadata("DEPLOY_TOKEN", MetadataEdit{Note: &note, EnvironmentName: &environmentName}, WebUISource("192.0.2.8")); err != nil {
		t.Fatal(err)
	}
	if err := m.AllowSecrets([]string{"DEPLOY_TOKEN"}, []string{"web1", "web2"}, CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := m.DisallowSecrets([]string{"DEPLOY_TOKEN"}, []string{"web1"}, WebUISource("192.0.2.9")); err != nil {
		t.Fatal(err)
	}
	if err := m.AddSecret("OLD_TOKEN", "待删除", "", []byte("web-add-old-secret-value"), WebUISource("192.0.2.7")); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteSecret("OLD_TOKEN", CLISource()); err != nil {
		t.Fatal(err)
	}

	rows, err := st.QueryAudit(store.AuditFilter{OwnerOnly: true, Actions: []string{
		"secret-add", "secret-set", "secret-edit", "secret-env",
		"secret-allow", "secret-disallow", "secret-rm",
	}})
	if err != nil {
		t.Fatal(err)
	}
	wantSource := map[string]struct {
		source  string
		address string
	}{
		"secret-add":      {store.AuditSourceWebUI, "192.0.2.7"},
		"secret-env":      {store.AuditSourceWebUI, "192.0.2.8"},
		"secret-set":      {store.AuditSourceCLI, ""},
		"secret-edit":     {store.AuditSourceWebUI, "192.0.2.8"},
		"secret-allow":    {store.AuditSourceCLI, ""},
		"secret-disallow": {store.AuditSourceWebUI, "192.0.2.9"},
		"secret-rm":       {store.AuditSourceCLI, ""},
	}
	seen := make(map[string]bool)
	for _, row := range rows {
		expected, exists := wantSource[row.Action]
		if row.Action == "secret-env" && strings.Contains(row.Command, `"after":"HF_TOKEN"`) {
			expected = struct {
				source  string
				address string
			}{store.AuditSourceWebUI, "192.0.2.7"}
		}
		if !exists {
			t.Fatalf("未预期的机密项审计动作 %q：%+v", row.Action, row)
		}
		if row.Status != "ok" {
			t.Fatalf("动作 %s 的审计状态 = %q，应为 ok", row.Action, row.Status)
		}
		if row.Source != expected.source || row.SourceIP != expected.address {
			t.Fatalf("动作 %s 的来源 = %q/%q，应为 %q/%q", row.Action, row.Source, row.SourceIP, expected.source, expected.address)
		}
		if strings.Contains(row.Command, "secret-value") {
			t.Fatalf("动作 %s 的审计摘要包含值：%s", row.Action, row.Command)
		}
		seen[row.Action] = true
	}
	for action := range wantSource {
		if !seen[action] {
			t.Fatalf("缺少动作 %q 的审计行，实际行：%+v", action, rows)
		}
	}

	views, err := m.List(ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Name != "DEPLOY_TOKEN" || views[0].ID == "" {
		t.Fatalf("列表结果 = %+v，应只剩带标识符的 DEPLOY_TOKEN", views)
	}
	if len(views[0].AllowedServers) != 1 || views[0].AllowedServers[0] != "web2" {
		t.Fatalf("可写入服务器 = %v，应只剩 web2", views[0].AllowedServers)
	}
	if views[0].EnvironmentName != "MODEL_TOKEN" || views[0].Note != "生产部署令牌" {
		t.Fatalf("元数据 = %+v", views[0])
	}
	if strings.Join(views[0].AllowedServers, ",") == web1 || views[0].ID == web2 {
		t.Fatal("列表不应把服务器标识符当成机密项标识符")
	}
}
