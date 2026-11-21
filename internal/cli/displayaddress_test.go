package cli

import (
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
)

// seedPreferredAddressVault 准备一个单机 vault：本机设备「笔记本」与目标设
// 备「工作站」同在一个物理局域网，工作站上挂一台空主机条目与一台主机非空
// 条目。返回直接打开的存储句柄与两台条目的标识符。
func seedPreferredAddressVault(t *testing.T) (*store.Store, string, string) {
	t.Helper()
	configRoot := t.TempDir()
	t.Setenv("APPDATA", configRoot)
	t.Setenv("XDG_CONFIG_HOME", configRoot)
	t.Setenv("SSHMGR_CONFIG_ROOT", configRoot)
	dbPath, masterKey := withCliStoreEnv(t)
	if err := roles.Save(roles.State{Role: roles.RoleStandalone, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dbPath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.AddSite("家A", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "家A局域网", Type: models.NetworkPhysical, SiteName: "家A"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("工作站", "", ""); err != nil {
		t.Fatal(err)
	}
	local, err := st.AddDevice("笔记本", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := roles.SetLocalDeviceID(local.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership("工作站", "家A局域网", "192.0.2.5"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership("笔记本", "家A局域网", "192.0.2.99"); err != nil {
		t.Fatal(err)
	}
	credential := &models.Credential{Type: models.CredPassword, Secret: []byte("pw")}
	bare, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: "裸条目", Host: "", Port: 22, User: "ops", AuthMethod: models.AuthPassword,
	}, credential, nil, "工作站")
	if err != nil {
		t.Fatal(err)
	}
	hosted, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: "实主机", Host: "198.51.100.7", Port: 2222, User: "ops", AuthMethod: models.AuthPassword,
	}, credential, nil, "工作站")
	if err != nil {
		t.Fatal(err)
	}
	return st, bare.ServerID, hosted.ServerID
}

// TestServersLsEmptyHostShowsPreferredAddress：空主机条目在 servers ls 里显
// 示本机与目标设备共同物理局域网的首选成员地址；主机非空条目照旧显示条目
// 主机；任何行都不出现「user@:」这样的空主机假地址。
func TestServersLsEmptyHostShowsPreferredAddress(t *testing.T) {
	seedPreferredAddressVault(t)

	out := runCli(t, "servers", "ls")
	if !strings.Contains(out, "ops@192.0.2.5:22") {
		t.Fatalf("空主机条目应显示首选网络地址 ops@192.0.2.5:22：\n%s", out)
	}
	if !strings.Contains(out, "ops@198.51.100.7:2222") {
		t.Fatalf("主机非空条目显示应保持不变：\n%s", out)
	}
	if strings.Contains(out, "@:") {
		t.Fatalf("输出不得出现空主机假地址 user@:：\n%s", out)
	}
}

// TestServerDetailConnectionLineEmptyHost：server <name> 的「连接：」行对空
// 主机条目显示首选网络地址，主机非空条目照旧。
func TestServerDetailConnectionLineEmptyHost(t *testing.T) {
	seedPreferredAddressVault(t)

	out := runCli(t, "servers", "show", "裸条目")
	if !strings.Contains(out, "连接：ops@192.0.2.5:22\n") {
		t.Fatalf("详情「连接：」行应显示首选网络地址：\n%s", out)
	}
	out = runCli(t, "servers", "show", "实主机")
	if !strings.Contains(out, "连接：ops@198.51.100.7:2222\n") {
		t.Fatalf("主机非空条目的「连接：」行应保持不变：\n%s", out)
	}
}

// TestProjectsMemberListEmptyHost：projects 的成员清单对空主机条目显示首选
// 网络地址。
func TestProjectsMemberListEmptyHost(t *testing.T) {
	st, bareID, hostedID := seedPreferredAddressVault(t)
	profileID, err := st.AddProfile("运维")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.AddProject("默认", profileID); err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{bareID, hostedID}); err != nil {
		t.Fatal(err)
	}

	out := runCli(t, "projects", "show", "默认")
	if !strings.Contains(out, "ops@192.0.2.5:22") {
		t.Fatalf("projects 成员清单应显示首选网络地址：\n%s", out)
	}
	if !strings.Contains(out, "ops@198.51.100.7:2222") {
		t.Fatalf("主机非空成员显示应保持不变：\n%s", out)
	}
	if strings.Contains(out, "@:") {
		t.Fatalf("输出不得出现空主机假地址 user@:：\n%s", out)
	}
}

// TestServersLsEmptyHostWithoutCandidateDegenerates：本机设备未知（角色未
// 设置的 vault）时没有局域网候选，空主机条目退化显示「user (port N)」。
func TestServersLsEmptyHostWithoutCandidateDegenerates(t *testing.T) {
	configRoot := t.TempDir()
	t.Setenv("APPDATA", configRoot)
	t.Setenv("XDG_CONFIG_HOME", configRoot)
	t.Setenv("SSHMGR_CONFIG_ROOT", configRoot)
	dbPath, masterKey := withCliStoreEnv(t)
	st, err := store.Open(dbPath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	credential := &models.Credential{Type: models.CredPassword, Secret: []byte("pw")}
	if _, err := st.AddServerWithCredentials(&models.Server{
		Name: "孤立条目", Host: "", Port: 2200, User: "ops", AuthMethod: models.AuthPassword,
	}, credential, nil); err != nil {
		t.Fatal(err)
	}

	out := runCli(t, "servers", "ls")
	if !strings.Contains(out, "ops (port 2200)") {
		t.Fatalf("无候选的空主机条目应退化显示 ops (port 2200)：\n%s", out)
	}
	if strings.Contains(out, "@:") {
		t.Fatalf("输出不得出现空主机假地址 user@:：\n%s", out)
	}

	detail := runCli(t, "servers", "show", "孤立条目")
	if !strings.Contains(detail, "连接：ops (port 2200)\n") {
		t.Fatalf("详情「连接：」行应退化显示 ops (port 2200)：\n%s", detail)
	}
}
