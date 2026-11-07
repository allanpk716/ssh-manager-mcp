package store

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/models"
)

func TestNetworkMembershipRegistrationOrderForNewMembers(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.AddSite("家A", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{
		Name: "家A局域网", Type: models.NetworkPhysical, SiteName: "家A",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "虚拟网", Type: models.NetworkVirtual}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("工作站", "", ""); err != nil {
		t.Fatal(err)
	}

	first, err := st.AddNetworkMembership("工作站", "虚拟网", "100.64.0.5")
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.AddNetworkMembership("工作站", "家A局域网", "192.168.1.20")
	if err != nil {
		t.Fatal(err)
	}
	if first.RegistrationOrder != 1 || second.RegistrationOrder != 2 {
		t.Fatalf("新增网络成员的登记序号错误：第1个=%d，第2个=%d", first.RegistrationOrder, second.RegistrationOrder)
	}

	ordered, err := st.ListNetworkMembershipsByRegistration("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if len(ordered) != 2 ||
		ordered[0].NetworkName != "虚拟网" ||
		ordered[1].NetworkName != "家A局域网" {
		t.Fatalf("按登记序号读取网络成员的结果错误：%+v", ordered)
	}
}

func TestNetworkMembershipRegistrationOrderMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	masterKey, err := GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := Open(path, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSite("家A", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{
		Name: "家A局域网", Type: models.NetworkPhysical, SiteName: "家A",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "虚拟网", Type: models.NetworkVirtual}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("工作站", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("笔记本", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership("笔记本", "家A局域网", "192.168.1.10"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership("工作站", "虚拟网", "100.64.0.5"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership("笔记本", "虚拟网", "100.64.0.10"); err != nil {
		t.Fatal(err)
	}
	revisionBeforeUpgrade, err := st.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`ALTER TABLE network_memberships DROP COLUMN registration_order`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	upgraded, err := Open(path, masterKey)
	if err != nil {
		t.Fatalf("升级缺少登记序号列的库失败：%v", err)
	}
	defer upgraded.Close()
	revisionAfterUpgrade, err := upgraded.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if revisionAfterUpgrade != revisionBeforeUpgrade {
		t.Fatalf("登记序号升级不应推进快照修订号：升级前=%d，升级后=%d",
			revisionBeforeUpgrade, revisionAfterUpgrade)
	}

	workstation, err := upgraded.ListNetworkMembershipsByRegistration("工作站")
	if err != nil {
		t.Fatal(err)
	}
	laptop, err := upgraded.ListNetworkMembershipsByRegistration("笔记本")
	if err != nil {
		t.Fatal(err)
	}
	if len(workstation) != 1 || workstation[0].RegistrationOrder != 2 {
		t.Fatalf("升级后工作站网络成员的登记序号错误：%+v", workstation)
	}
	if len(laptop) != 2 ||
		laptop[0].NetworkName != "家A局域网" || laptop[0].RegistrationOrder != 1 ||
		laptop[1].NetworkName != "虚拟网" || laptop[1].RegistrationOrder != 3 {
		t.Fatalf("升级后笔记本网络成员的登记序号错误：%+v", laptop)
	}
}

func TestNetworkMembershipRegistrationOrderSnapshot(t *testing.T) {
	sourceKey, err := GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	source, err := Open(filepath.Join(t.TempDir(), "source.db"), sourceKey)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	profileID, err := source.AddProfile("client-profile")
	if err != nil {
		t.Fatal(err)
	}
	clientTokenID, _, err := source.AddCacheToken("laptop", profileID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.AddSite("家A", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := source.AddNetwork(models.Network{
		Name: "家A局域网", Type: models.NetworkPhysical, SiteName: "家A",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := source.AddNetwork(models.Network{Name: "虚拟网", Type: models.NetworkVirtual}); err != nil {
		t.Fatal(err)
	}
	if _, err := source.AddDevice("笔记本", "", "laptop"); err != nil {
		t.Fatal(err)
	}
	if _, err := source.AddNetworkMembership("笔记本", "虚拟网", "100.64.0.5"); err != nil {
		t.Fatal(err)
	}
	if _, err := source.AddNetworkMembership("笔记本", "家A局域网", "192.168.1.20"); err != nil {
		t.Fatal(err)
	}

	clientSnapshot, err := source.ExportSnapshotForProfile(profileID, false, clientTokenID)
	if err != nil {
		t.Fatal(err)
	}
	ordersByAddress := make(map[string]int64)
	for _, membership := range clientSnapshot.NetworkMemberships {
		ordersByAddress[membership.Address] = membership.RegistrationOrder
	}
	if len(clientSnapshot.NetworkMemberships) != 2 ||
		ordersByAddress["100.64.0.5"] != 1 ||
		ordersByAddress["192.168.1.20"] != 2 {
		t.Fatalf("客户端快照没有携带登记序号：%+v", clientSnapshot.NetworkMemberships)
	}
	encoded, err := json.Marshal(clientSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte(`"registration_order"`)) {
		t.Fatalf("客户端快照 JSON 缺少登记序号字段：%s", encoded)
	}

	target, err := Open(filepath.Join(t.TempDir(), "target.db"), sourceKey)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if err := target.ImportSnapshotForClientCache(clientSnapshot); err != nil {
		t.Fatal(err)
	}
	restored, err := target.ListNetworkMembershipsByRegistration("笔记本")
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 2 ||
		restored[0].NetworkName != "虚拟网" || restored[0].RegistrationOrder != 1 ||
		restored[1].NetworkName != "家A局域网" || restored[1].RegistrationOrder != 2 {
		t.Fatalf("客户端缓存导入后登记序号改变：%+v", restored)
	}

	oldSnapshot := *clientSnapshot
	oldSnapshot.NetworkMemberships = append(
		make([]SnapshotNetworkMembership, 0, len(clientSnapshot.NetworkMemberships)),
		clientSnapshot.NetworkMemberships...,
	)
	for i := range oldSnapshot.NetworkMemberships {
		oldSnapshot.NetworkMemberships[i].RegistrationOrder = 0
	}
	oldTarget, err := Open(filepath.Join(t.TempDir(), "old-target.db"), sourceKey)
	if err != nil {
		t.Fatal(err)
	}
	defer oldTarget.Close()
	if err := oldTarget.ImportSnapshotForClientCache(&oldSnapshot); err != nil {
		t.Fatalf("导入缺少登记序号的旧快照失败：%v", err)
	}
	assigned, err := oldTarget.ListNetworkMembershipsByRegistration("笔记本")
	if err != nil {
		t.Fatal(err)
	}
	if len(assigned) != 2 ||
		assigned[0].Address != oldSnapshot.NetworkMemberships[0].Address ||
		assigned[0].RegistrationOrder != 1 ||
		assigned[1].Address != oldSnapshot.NetworkMemberships[1].Address ||
		assigned[1].RegistrationOrder != 2 {
		t.Fatalf("旧快照没有按出现顺序补登记序号：%+v", assigned)
	}
}
