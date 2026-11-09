package mcpserver

import (
	"bytes"
	"encoding/json"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// TestListServersOmitsSoftwareInventory 钉住软件清单与 list_servers 的边界：
// 即使同一台设备上已有声明，服务器列表输出也不携带任何清单字段或条目。
func TestListServersOmitsSoftwareInventory(t *testing.T) {
	st := newStore(t)
	credentialID, err := st.SetCredential(&models.Credential{
		Type: models.CredPassword, Secret: []byte("test-password"),
	})
	if err != nil {
		t.Fatal(err)
	}
	device, err := st.AddDevice("工作站", "", "")
	if err != nil {
		t.Fatal(err)
	}
	added, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: "gpu", Host: "192.0.2.10", Port: 22, User: "owner",
		AuthMethod: models.AuthPassword, CredentialID: credentialID,
	}, nil, nil, "工作站")
	if err != nil {
		t.Fatal(err)
	}
	serverID := added.ServerID
	if _, err := st.AddSoftwareDeclaration(models.SoftwareDeclaration{
		Device: device.Name, Category: models.SoftwareCategoryServiceProgram,
		Name: "secret-software-name", Version: "1.0",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	profileID, err := st.AddProfile("运维")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{serverID}); err != nil {
		t.Fatal(err)
	}

	servers, err := ListServersForProfile(st, profileID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(ListServersOutput{Servers: servers})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("secret-software-name")) ||
		bytes.Contains(encoded, []byte("software")) ||
		bytes.Contains(encoded, []byte("inventory")) {
		t.Fatalf("list_servers 输出携带了软件清单：%s", encoded)
	}
}
