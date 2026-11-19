package webuitopology

import (
	"errors"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
)

func TestTopologyWriterManagesLocationTreeThroughStore(t *testing.T) {
	st := openStoreForTest(t)
	writer := WriterFromStore(st)
	source := webui.TopologyWriteSource{RemoteAddress: "192.0.2.9"}

	message, err := writer.AddLocation("公司", "", "", source)
	if err != nil {
		t.Fatalf("新建根地点失败：%v", err)
	}
	if message != "已新建地点 公司" {
		t.Fatalf("新建地点提示错误：%q", message)
	}
	factory, err := writer.AddLocation("一厂区", "公司", "", source)
	if err != nil || factory != "已新建地点 公司 / 一厂区" {
		t.Fatalf("新建子地点失败：%v %q", err, factory)
	}
	if _, err := writer.AddLocation("3 号楼", "公司 / 一厂区", "", source); err != nil {
		t.Fatalf("新建三级地点失败：%v", err)
	}

	invalid := &webui.TopologyInvalidInputError{}
	if _, err := writer.EditLocation("公司", webui.TopologyLocationChanges{
		Parent: ptrString("公司 / 一厂区 / 3 号楼"),
	}, source); !errors.As(err, &invalid) {
		t.Fatalf("把上级移动到自己的下级应按校验错误拒绝：%v", err)
	}
	if _, err := writer.EditLocation("3 号楼", webui.TopologyLocationChanges{
		Name: ptrString("4 号楼"),
	}, source); err != nil {
		t.Fatalf("地点改名失败：%v", err)
	}
	if _, err := writer.DeleteLocation("公司", source); !errors.As(err, &invalid) ||
		!strings.Contains(err.Error(), "还有下级地点：一厂区") {
		t.Fatalf("删除有下级的地点应返回守卫原因：%v", err)
	}
	if _, err := writer.DeleteLocation("公司 / 一厂区 / 4 号楼", source); err != nil {
		t.Fatalf("删除叶子地点失败：%v", err)
	}

	sites, err := st.ListSites()
	if err != nil {
		t.Fatal(err)
	}
	for _, site := range sites {
		if site.Name == "4 号楼" {
			t.Fatalf("叶子地点应已删除：%+v", sites)
		}
	}
	rows, err := st.QueryAudit(store.AuditFilter{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	var sawWebUISiteAdd bool
	for _, row := range rows {
		if row.Action == "site.add" && row.Source == store.AuditSourceWebUI && row.SourceIP == "192.0.2.9" {
			sawWebUISiteAdd = true
		}
	}
	if !sawWebUISiteAdd {
		t.Fatalf("地点写入审计应记录 Web UI 来源与地址：%+v", rows)
	}
}

func TestTopologyWriterManagesNetworksThroughStore(t *testing.T) {
	st := newWriterTestStore(t)
	writer := WriterFromStore(st)
	source := webui.TopologyWriteSource{RemoteAddress: "192.0.2.9"}

	message, err := writer.AddManagedNetwork(webui.TopologyNetworkInput{
		Name: "新办公网", Type: "physical", Site: "家A",
		NetworkRange: "192.0.2.0/24", BandwidthMbps: 1000, Metered: true,
	}, source)
	if err != nil {
		t.Fatalf("新建物理局域网失败：%v", err)
	}
	if message != "已保存网络 新办公网（物理局域网）" {
		t.Fatalf("新建网络提示错误：%q", message)
	}
	if _, err := writer.AddManagedNetwork(webui.TopologyNetworkInput{
		Name: "新虚拟网", Type: "virtual", NetworkRange: "100.64.0.0/24",
	}, source); err != nil {
		t.Fatalf("新建虚拟局域网失败：%v", err)
	}

	invalid := &webui.TopologyInvalidInputError{}
	if _, err := writer.AddManagedNetwork(webui.TopologyNetworkInput{
		Name: "坏网络", Type: "virtual", Site: "家A",
	}, source); !errors.As(err, &invalid) {
		t.Fatalf("虚拟局域网带地点应按校验错误拒绝：%v", err)
	}
	if _, err := writer.AddManagedNetwork(webui.TopologyNetworkInput{
		Name: "坏带宽", Type: "physical", Site: "家A", BandwidthMbps: -1,
	}, source); !errors.As(err, &invalid) {
		t.Fatalf("负标称带宽应按校验错误拒绝：%v", err)
	}

	bandwidth := int64(2000)
	site := "机房"
	if _, err := writer.EditManagedNetwork("新办公网", webui.TopologyNetworkChanges{
		BandwidthMbps: &bandwidth, Site: &site,
	}, source); err != nil {
		t.Fatalf("编辑网络失败：%v", err)
	}
	networks, err := st.ListNetworks(store.NetworkFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, network := range networks {
		if network.Name == "新办公网" &&
			(network.BandwidthMbps != 2000 || network.SiteName != "机房" ||
				network.Type != models.NetworkPhysical || !network.Metered) {
			t.Fatalf("编辑后的网络字段错误：%+v", network)
		}
	}
}

func ptrString(value string) *string { return &value }

// TestTopologyWriterEditsDeviceSiteAndMovesServer 校验服务器详情页新增的两
// 个写入口都走存储层：机器地点修改与 devices move-server 等价操作。
func TestTopologyWriterEditsDeviceSiteAndMovesServer(t *testing.T) {
	st := newWriterTestStore(t)
	writer := WriterFromStore(st)
	source := webui.TopologyWriteSource{RemoteAddress: "192.0.2.9"}

	message, err := writer.EditDeviceSite("工作站", "机房", source)
	if err != nil || message != "已把设备 工作站 的地点改为 机房" {
		t.Fatalf("修改机器地点失败：%v %q", err, message)
	}
	device, err := st.GetDevice("工作站")
	if err != nil || device.SiteName != "机房" {
		t.Fatalf("机器地点应已写入存储：%+v %v", device, err)
	}

	added, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: "web", Host: "192.0.2.10", Port: 22, User: "root",
	}, &models.Credential{Type: models.CredPassword, Secret: []byte("secret")}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	move, err := writer.MoveServerToDevice(added.ServerID, "NAS", source)
	if err != nil {
		t.Fatalf("改挂服务器失败：%v", err)
	}
	if move.ServerName != "web" || move.ToDevice != "NAS" || move.FromDevice != "web" || move.AnchorNotice != "" {
		t.Fatalf("改挂结果不正确：%+v", move)
	}
	movedServers, err := st.ListServersForDevice("NAS")
	if err != nil {
		t.Fatal(err)
	}
	sawMove := false
	for _, row := range movedServers {
		if row.ID == added.ServerID {
			sawMove = true
		}
	}
	if !sawMove {
		t.Fatalf("改挂后目标机器应包含该服务器条目：%+v", movedServers)
	}
	rows, err := st.QueryAudit(store.AuditFilter{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	var sawWebUIDeviceEdit bool
	for _, row := range rows {
		if row.Action == "device.edit" && row.Source == store.AuditSourceWebUI {
			sawWebUIDeviceEdit = true
		}
	}
	if !sawWebUIDeviceEdit {
		t.Fatalf("机器地点写入审计应记录 Web UI 来源：%+v", rows)
	}
}
