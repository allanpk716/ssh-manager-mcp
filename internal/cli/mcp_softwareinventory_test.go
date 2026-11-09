package cli

// 票 #185 的桥接验证：mcp --cache 构建的客户端缓存面上，软件清单查询工
// 具把权威端不可达映射为转发器的固定错误文本（真实转发器注入，不是替身）。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

func TestCacheBrokerSoftwareInventoryToolReportsUnreachable(t *testing.T) {
	// 先起一个 TLS 服务拿到证书固定值与地址，再关掉：转发器拿着有效配置
	// 访问一台已停机的权威端。
	authority := httptest.NewTLSServer(http.NotFoundHandler())
	pin := mcpserver.SPKIFingerprint(authority.Certificate())
	authority.Close()

	vaultDirectory := t.TempDir()
	masterKey, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(vaultDirectory, "cache.db"), masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	credentialID, err := st.SetCredential(&models.Credential{
		Type: models.CredPassword, Secret: []byte("pw"),
	})
	if err != nil {
		t.Fatal(err)
	}
	serverID, err := st.AddServer(&models.Server{
		Name: "gpu", Host: "192.0.2.10", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: credentialID,
	})
	if err != nil {
		t.Fatal(err)
	}
	profileID, err := st.AddProfile("team-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{serverID}); err != nil {
		t.Fatal(err)
	}
	_, token, err := st.AddProject("proj", profileID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := st.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}

	forwarder, err := clientops.NewSoftwareInventoryForwarder(clientops.CacheCred{
		URL: authority.URL, Token: "device-code:" + pin, Pin: pin,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer forwarder.Close()
	broker, tunnels, tasks, cleanup, err := mcpserver.NewCacheBroker(
		token, snapshot, filepath.Join(t.TempDir(), "audit.log"), nil, nil,
		"laptop", nil, forwarder,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	defer tunnels.CloseAll()
	defer tasks.CloseAll()

	clientSide, serverSide := mcp.NewInMemoryTransports()
	serverSession, err := broker.Connect(context.Background(), serverSide, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "v0"}, nil)
	clientSession, err := client.Connect(context.Background(), clientSide, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()

	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "query_software_inventory", Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatal("权威端不可达时工具必须返回 IsError")
	}
	var text string
	for _, content := range result.Content {
		if textContent, ok := content.(*mcp.TextContent); ok {
			text += textContent.Text
		}
	}
	if text != clientops.SoftwareInventoryUnreachableText {
		t.Fatalf("工具错误文本不是固定契约：\n得到：%q\n想要：%q", text, clientops.SoftwareInventoryUnreachableText)
	}
}
