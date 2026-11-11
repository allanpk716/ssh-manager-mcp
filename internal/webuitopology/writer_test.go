package webuitopology

// 网络拓扑写入口适配器的测试：断言只看公开接口返回的提示、错误分类与存
// 储里的网络成员、网络与审计行，全部写操作必须与命令行走同一套存储语义。

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
)

// openStoreForTest 在临时目录里打开一个全新的加密存储。
func openStoreForTest(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("SSHMGR_STORE", filepath.Join(dir, "store.db"))
	masterKey, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatalf("生成主密钥失败：%v", err)
	}
	st, err := store.Open(filepath.Join(dir, "store.db"), masterKey)
	if err != nil {
		t.Fatalf("打开存储失败：%v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func newWriterTestStore(t *testing.T) *store.Store {
	t.Helper()
	st := openStoreForTest(t)
	if _, err := st.AddSite("家A", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSite("机房", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "家A局域网", Type: models.NetworkPhysical, SiteName: "家A"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "机房局域网", Type: models.NetworkPhysical, SiteName: "机房"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "虚拟网", Type: models.NetworkVirtual}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("工作站", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("NAS", "", ""); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestTopologyWriterDelegatesToStoreSemantics(t *testing.T) {
	st := newWriterTestStore(t)
	writer := WriterFromStore(st)

	message, err := writer.AddNetworkMembership("工作站", "家A局域网", "192.168.1.20")
	if err != nil {
		t.Fatalf("新增网络成员失败：%v", err)
	}
	if message != "已把设备 工作站 加入网络 家A局域网：192.168.1.20" {
		t.Fatalf("新增网络成员提示与命令行不一致：%q", message)
	}

	message, err = writer.EditNetworkMembership("工作站", "家A局域网", "192.168.1.21")
	if err != nil {
		t.Fatalf("修改网络成员地址失败：%v", err)
	}
	if message != "已修改设备 工作站 在网络 家A局域网 的地址：192.168.1.21" {
		t.Fatalf("修改网络成员地址提示与命令行不一致：%q", message)
	}

	message, err = writer.AddNetworkMembership("工作站", "虚拟网", "100.64.0.20")
	if err != nil {
		t.Fatal(err)
	}
	message, err = writer.MoveNetworkMembership("工作站", "家A局域网", "机房局域网", "10.0.0.20")
	if err != nil {
		t.Fatalf("移动网络成员失败：%v", err)
	}
	if !strings.Contains(message, "工作站") || !strings.Contains(message, "机房局域网") || !strings.Contains(message, "10.0.0.20") {
		t.Fatalf("移动网络成员提示应说明设备、目标网络与新地址：%q", message)
	}

	message, err = writer.RemoveNetworkMembership("工作站", "虚拟网")
	if err != nil {
		t.Fatalf("退出网络成员失败：%v", err)
	}
	if message != "已把设备 工作站 移出网络 虚拟网" {
		t.Fatalf("退出网络成员提示与命令行不一致：%q", message)
	}

	message, err = writer.AddPhysicalNetwork("新办公楼局域网", "机房")
	if err != nil {
		t.Fatalf("新建物理局域网失败：%v", err)
	}
	if message != "已登记网络 新办公楼局域网（物理局域网）" {
		t.Fatalf("新建物理局域网提示与命令行不一致：%q", message)
	}
	networks, err := st.ListNetworks(store.NetworkFilter{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, network := range networks {
		if network.Name == "新办公楼局域网" && network.Type == models.NetworkPhysical && network.SiteName == "机房" {
			found = true
		}
	}
	if !found {
		t.Fatalf("新建物理局域网应按站点归属登记：%+v", networks)
	}

	if _, err := writer.AddNetworkMembership("工作站", "家A局域网", "192.168.1.22"); err != nil {
		t.Fatal(err)
	}
	message, err = writer.RemovePhysicalNetworkMemberships("工作站")
	if err != nil {
		t.Fatalf("删除全部物理局域网成员失败：%v", err)
	}
	if !strings.Contains(message, "工作站") || !strings.Contains(message, "2") {
		t.Fatalf("删除全部物理局域网成员提示应说明设备与数量：%q", message)
	}
	memberships, err := st.ListNetworkMemberships("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if len(memberships) != 0 {
		t.Fatalf("工作站应已没有网络成员：%+v", memberships)
	}

	actions := map[string]int{}
	rows, err := st.QueryAudit(store.AuditFilter{Actions: []string{
		"device.member-add", "device.member-edit", "device.member-rm", "network.add",
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		actions[row.Action] += 1
	}
	if actions["device.member-add"] < 2 || actions["device.member-edit"] != 1 || actions["device.member-rm"] < 2 || actions["network.add"] < 1 {
		t.Fatalf("全部写路径都应写既有拓扑审计：%+v", actions)
	}
}

func TestTopologyWriterClassifiesNameNotFoundAndValidationErrors(t *testing.T) {
	st := newWriterTestStore(t)
	writer := WriterFromStore(st)

	_, err := writer.AddNetworkMembership("不存在的设备", "家A局域网", "192.168.1.20")
	var notFound *webui.TopologyNameNotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("设备不存在应映射为名称不存在错误：%v", err)
	}
	if notFound.Message == "" || !strings.Contains(notFound.Message, "不存在的设备") {
		t.Fatalf("名称不存在错误应保留存储层的可读提示：%q", notFound.Message)
	}

	_, err = writer.AddNetworkMembership("工作站", "不存在的网络", "192.168.1.20")
	if !errors.As(err, &notFound) {
		t.Fatalf("网络不存在应映射为名称不存在错误：%v", err)
	}

	if _, err := writer.AddNetworkMembership("工作站", "家A局域网", "192.168.1.20"); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.AddNetworkMembership("NAS", "家A局域网", "192.168.1.20"); err == nil {
		t.Fatal("地址冲突应返回错误")
	} else if !strings.Contains(err.Error(), "已被设备 工作站 占用") {
		t.Fatalf("地址冲突提示应指出占用设备：%v", err)
	} else if errors.As(err, &notFound) {
		t.Fatalf("地址冲突不应按名称不存在分类：%v", err)
	}

	_, err = writer.RemoveNetworkMembership("工作站", "不存在的网络")
	if !errors.As(err, &notFound) {
		t.Fatalf("退出不存在的网络应映射为名称不存在错误：%v", err)
	}
	_, err = writer.RemovePhysicalNetworkMemberships("不存在的设备")
	if !errors.As(err, &notFound) {
		t.Fatalf("删除全部物理成员时设备不存在应映射为名称不存在错误：%v", err)
	}
	_, err = writer.AddPhysicalNetwork("家A局域网", "家A")
	if err == nil || errors.As(err, &notFound) {
		t.Fatalf("网络名重复应按校验错误返回：%v", err)
	}
	_, err = writer.AddPhysicalNetwork("全新局域网", "不存在的站点")
	if !errors.As(err, &notFound) {
		t.Fatalf("站点不存在应映射为名称不存在错误：%v", err)
	}
}

func TestTopologyWriterFromOpenerOpensStorePerRequest(t *testing.T) {
	st := newWriterTestStore(t)
	opened := 0
	writer := WriterFromOpener(func() (*store.Store, func(), error) {
		opened += 1
		return st, func() {}, nil
	})
	if _, err := writer.AddNetworkMembership("工作站", "家A局域网", "192.168.1.20"); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.RemoveNetworkMembership("工作站", "家A局域网"); err != nil {
		t.Fatal(err)
	}
	if opened != 2 {
		t.Fatalf("每次写请求都应重新打开存储，实际 %d 次", opened)
	}
}
