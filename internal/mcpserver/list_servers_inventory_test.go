package mcpserver

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"
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

// TestListServersOutputStructureUnchanged 钉住 list_servers 的输出结构
// （票 #185 验收标准）：软件清单功能不得给这个工具增删任何字段。
func TestListServersOutputStructureUnchanged(t *testing.T) {
	st := newStore(t)
	credentialID, err := st.SetCredential(&models.Credential{
		Type: models.CredPassword, Secret: []byte("test-password"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddServer(&models.Server{
		Name: "gpu", Host: "192.0.2.10", Port: 22, User: "owner",
		AuthMethod: models.AuthPassword, CredentialID: credentialID,
		Role: "edge", Tags: []string{"prod"},
	}); err != nil {
		t.Fatal(err)
	}
	profileID, err := st.AddProfile("结构测试")
	if err != nil {
		t.Fatal(err)
	}
	added, err := st.GetServerByName("gpu")
	if err != nil || added == nil {
		t.Fatalf("取服务器条目失败：%v %+v", err, added)
	}
	if err := st.GrantServers(profileID, []string{added.ID}); err != nil {
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
	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &wrapper); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reflect.ValueOf(mapKeys(wrapper)).Interface(), []string{"servers"}) {
		t.Fatalf("顶层字段 = %v，应只有 servers", mapKeys(wrapper))
	}
	var serverList []map[string]json.RawMessage
	if err := json.Unmarshal(wrapper["servers"], &serverList); err != nil {
		t.Fatal(err)
	}
	if len(serverList) != 1 {
		t.Fatalf("服务器条目数量 = %d", len(serverList))
	}
	wantKeys := []string{ // 与 mapKeys 一致，按字母序排列。
		"caveats", "description", "device", "hardware", "has_sudo", "host", "id",
		"location", "name", "networks", "revision", "role", "services", "site",
		"tags", "user",
	}
	if got := mapKeys(serverList[0]); !reflect.DeepEqual(got, wantKeys) {
		t.Fatalf("服务器条目字段 = %v，应保持既有结构 %v", got, wantKeys)
	}
}

// mapKeys 按序列化顺序取出 JSON 对象的字段名。
func mapKeys[V any](object map[string]V) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
