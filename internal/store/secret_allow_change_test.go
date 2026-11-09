package store

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
)

// mustPasswordServer 新增一台带密码凭据的服务器条目，供授权测试使用。
func mustPasswordServer(t *testing.T, s *Store, name string) string {
	t.Helper()
	credentialID, err := s.SetCredential(&models.Credential{
		Type: models.CredPassword, Secret: []byte("test-password"),
	})
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.AddServer(&models.Server{
		Name: name, Host: "192.0.2.10", Port: 22, User: "owner",
		AuthMethod: models.AuthPassword, CredentialID: credentialID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestChangeSecretAllowsForNamesSurvivesConcurrentDelete 覆盖审查第 5 项：
// 名字解析与执行必须放进同一个事务。命令入口解析大量名字期间，另一个存
// 储连接删除了其中一个机密项；结果要么按名字报错，要么按开始时的同一快
// 照保持无变化成功，不能退回标识符报错，也不能写出一半授权。
func TestChangeSecretAllowsForNamesSurvivesConcurrentDelete(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	key := make([]byte, 32)
	randRead(t, key)
	main, err := Open(dbPath, key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = main.Close() })
	credentialID, err := main.SetCredential(&models.Credential{
		Type: models.CredPassword, Secret: []byte("test-password"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := main.AddServer(&models.Server{
		Name: "web1", Host: "192.0.2.10", Port: 22, User: "owner",
		AuthMethod: models.AuthPassword, CredentialID: credentialID,
	}); err != nil {
		t.Fatal(err)
	}

	// 目标全部预先授权：删除发生后，正确实现会在同一快照里判定为无变化，
	// 不需要升级成写事务，也就不会被并发写打断。
	names := make([]string, 1, 801)
	names[0] = "GONE_TOKEN"
	for i := range 800 {
		names = append(names, fmt.Sprintf("FILLER_TOKEN_%04d", i))
	}
	for _, name := range names {
		if err := main.AddSecret(name, "", []byte("filler-secret-value"), CLISource()); err != nil {
			t.Fatal(err)
		}
	}
	if err := main.AllowSecrets(names, []string{"web1"}, CLISource()); err != nil {
		t.Fatal(err)
	}
	views, err := main.ListSecretsInScope(ScopeAllServers())
	if err != nil {
		t.Fatal(err)
	}
	goneID := ""
	for _, view := range views {
		if view.Name == "GONE_TOKEN" {
			goneID = view.ID
		}
	}
	if goneID == "" {
		t.Fatal("准备机密项 GONE_TOKEN 失败")
	}

	other, err := Open(dbPath, key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })

	baseline, err := main.QueryAudit(AuditFilter{OwnerOnly: true, Actions: []string{"secret-allow", "secret-disallow", "secret-allow-change"}})
	if err != nil {
		t.Fatal(err)
	}
	seenBefore := make(map[int64]bool, len(baseline))
	for _, row := range baseline {
		seenBefore[row.ID] = true
	}

	result := make(chan error, 1)
	go func() {
		result <- main.ChangeSecretAllowsForNames(names, []string{"web1"}, true, CLISource())
	}()
	// 等待名字解析进入中段：GONE_TOKEN 排在第一位，先被解析；填充名字让
	// 解析阶段足够长，删除稳定落在两个阶段之间或同一事务快照内。
	time.Sleep(3 * time.Millisecond)
	deleteErr := other.DeleteSecret("GONE_TOKEN", CLISource())
	if deleteErr != nil {
		t.Fatalf("并发删除机密项失败：%v", deleteErr)
	}

	err = <-result
	if err != nil && (strings.Contains(err.Error(), goneID) || !strings.Contains(err.Error(), "GONE_TOKEN")) {
		t.Fatalf("并发删除后按名字整体处理，不能退回标识符报错：%v", err)
	}
	rows, err := main.QueryAudit(AuditFilter{OwnerOnly: true, Actions: []string{"secret-allow", "secret-disallow", "secret-allow-change"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if !seenBefore[row.ID] && strings.Contains(row.Command, `"name":"GONE_TOKEN"`) {
			t.Fatalf("并发删除后不应为已删机密项写授权审计：%+v", row)
		}
	}
}
