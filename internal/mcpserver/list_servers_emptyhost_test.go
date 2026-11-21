package mcpserver

import (
	"testing"

	"ssh-manager-mcp/internal/models"
)

// TestListServersEmptyHostExposedHostIsEmptyString：MCP list_servers 对空主
// 机条目在 ExposeHost 打开时照旧返回原始主机，即空串；不新增字段。
func TestListServersEmptyHostExposedHostIsEmptyString(t *testing.T) {
	st := newStore(t)
	bare, _ := st.AddServer(&models.Server{
		Name: "bare", Host: "", Port: 22, User: "u", AuthMethod: models.AuthPassword,
		CredentialID: mustCred(t, st), ExposeHost: true,
	})
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{bare})

	got, err := ListServersForProfile(st, pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("应列出一台条目，实际 %d", len(got))
	}
	if got[0].Host != "" {
		t.Fatalf("空主机条目的 host 字段应为空串，实际 %q", got[0].Host)
	}
}
