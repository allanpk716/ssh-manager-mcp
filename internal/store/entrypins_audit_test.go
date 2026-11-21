package store

import (
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// TestEntryLifecycle_PinDeleteAuditEmptyHostWritesDash：删除条目时随删的
// 锚审计行对空主机条目写 host=-；主机非空条目照旧记录兜底地址。
func TestEntryLifecycle_PinDeleteAuditEmptyHostWritesDash(t *testing.T) {
	s := newTestStore(t)
	bare := addEntryForTest(t, s, "bare", "", 22)
	hosted := addEntryForTest(t, s, "hosted", "198.51.100.7", 22)
	key, _ := pinHostKey(t)
	if err := s.SaveEntryPin(&models.Server{ID: bare, Port: 22}, key); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveEntryPin(&models.Server{ID: hosted, Host: "198.51.100.7", Port: 22}, key); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteServerCascading(bare); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteServerCascading(hosted); err != nil {
		t.Fatal(err)
	}
	deletes, err := s.QueryAudit(AuditFilter{Actions: []string{"pin-delete"}})
	if err != nil {
		t.Fatal(err)
	}
	commands := map[string]string{}
	for _, row := range deletes {
		commands[row.ServerID] = row.Command
	}
	if !strings.Contains(commands[bare], "host=- ") {
		t.Fatalf("空主机条目的删除锚审计行应为 host=-：%s", commands[bare])
	}
	if !strings.Contains(commands[hosted], "host=198.51.100.7:22 ") {
		t.Fatalf("主机非空条目的删除锚审计行应照旧记录兜底地址：%s", commands[hosted])
	}
}
