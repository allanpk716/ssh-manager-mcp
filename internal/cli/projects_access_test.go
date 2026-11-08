package cli

import (
	"bytes"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/access"
	"ssh-manager-mcp/internal/store"
)

// TestProjectsAddWritesAuditAndUnchangedOutput `projects add` 经访问管理
// 模块签发：输出形状与收模块前一致（一次性令牌加 `.mcp.json` 片段），
// 并补写 `project.add` 审计行。
func TestProjectsAddWritesAuditAndUnchangedOutput(t *testing.T) {
	dbPath, mk := withCliStoreEnv(t)
	s, err := store.Open(dbPath, mk)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddProfile("dev"); err != nil {
		t.Fatal(err)
	}
	s.Close()

	out := runCli(t, "projects", "add", "agent", "--profile", "dev")
	if !strings.HasPrefix(out, "Token (shown once): ") {
		t.Fatalf("输出应仍以一次性令牌开头：%q", out)
	}
	token := strings.TrimSpace(strings.TrimPrefix(strings.SplitN(out, "\n", 2)[0], "Token (shown once): "))
	if token == "" {
		t.Fatalf("输出里没解析到令牌：%q", out)
	}
	want := "Token (shown once): " + token + "\n\n.mcp.json snippet:\n" + access.ProjectTokenSnippet(token) + "\n"
	if out != want {
		t.Fatalf("输出 = %q，应与收模块前一致：%q", out, want)
	}
	if !bytes.Contains([]byte(out), []byte(token)) {
		t.Fatalf("片段应代入令牌：%q", out)
	}

	s, err = store.Open(dbPath, mk)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.QueryAudit(store.AuditFilter{Actions: []string{"project.add"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("project.add 审计行数量 = %d，应为 1：%+v", len(rows), rows)
	}
	r := rows[0]
	if r.Status != "ok" || r.Source != store.AuditSourceCLI || r.SourceIP != "" {
		t.Fatalf("project.add 审计行 = %+v，应为命令行来源的成功行", r)
	}
	if strings.Contains(r.Command, token) || r.ProjectID == "" {
		t.Fatalf("project.add 审计行 = %+v，摘要不得含令牌且应挂项目标识符", r)
	}
}
