package cli

import (
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// TestInventoryCollectorRegistration 覆盖采集执行者登记命令：成功输出实例
// 与条目；profile 不包含条目时拒绝并说明原因。
func TestInventoryCollectorRegistration(t *testing.T) {
	newSecretsEnv(t)
	st, err := openUnlockedStore()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("工作站", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: "工作站入口", Host: "192.0.2.10", Port: 22, User: "owner",
	}, &models.Credential{Type: models.CredPassword, Secret: []byte("pw")}, nil, "工作站"); err != nil {
		t.Fatal(err)
	}
	profile, err := st.AddProfile("采集 profile")
	if err != nil {
		t.Fatal(err)
	}
	server, err := st.GetServerByName("工作站入口")
	if err != nil || server == nil {
		t.Fatalf("取服务器条目失败：%v %+v", err, server)
	}
	if err := st.GrantServers(profile, []string{server.ID}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.AddCacheToken("laptop", profile); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	out := mustInventoryCli(t, "", "inventory", "collector", "工作站", "--client", "laptop", "--entry", "工作站入口")
	for _, want := range []string{"laptop", "工作站入口", "已登记"} {
		if !strings.Contains(out, want) {
			t.Fatalf("登记输出缺少 %q：\n%s", want, out)
		}
	}
	_, _, err = runInventoryRoot(t, "", "inventory", "collector", "工作站", "--client", "别的实例")
	if err == nil || !strings.Contains(err.Error(), "不是有效的活动设备码") {
		t.Fatalf("设备码不存在时应拒绝并说明原因，实际：%v", err)
	}
}

// TestInventoryImmediateCollectForClientOnlyRegisters 覆盖由客户端实例采集
// 的设备：权威端立即采集只登记请求，不在权威端本机执行 SSH。
func TestInventoryImmediateCollectForClientOnlyRegisters(t *testing.T) {
	newSecretsEnv(t)
	st, err := openUnlockedStore()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("工作站", "", ""); err != nil {
		t.Fatal(err)
	}
	added, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: "工作站入口", Host: "192.0.2.10", Port: 22, User: "owner",
	}, &models.Credential{Type: models.CredPassword, Secret: []byte("pw")}, nil, "工作站")
	if err != nil {
		t.Fatal(err)
	}
	profile, err := st.AddProfile("采集 profile")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profile, []string{added.ServerID}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.AddCacheToken("laptop", profile); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSoftwareCollectionCollector("工作站", "laptop", added.ServerID, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	out := mustInventoryCli(t, "", "inventory", "collect", "--device", "工作站")
	for _, want := range []string{"已请求", "等待客户端实例 laptop 执行"} {
		if !strings.Contains(out, want) {
			t.Fatalf("客户端采集设备的立即采集输出缺少 %q：\n%s", want, out)
		}
	}
}
