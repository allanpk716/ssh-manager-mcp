package store

// 布局坐标存储只服务拓扑图。它必须随备份与导出的全量快照往返保留，
// 也随 profile 裁剪快照下发；只有布局坐标变化不改变客户端用来判断是否
// 重新拉取的快照修订号。

import (
	"encoding/json"
	"math"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
)

func TestDeviceLayoutCoordinatesSnapshotRoundTripAndClientDelivery(t *testing.T) {
	dir := t.TempDir()
	masterKey, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("generate master key: %v", err)
	}
	source, err := Open(filepath.Join(dir, "source.db"), masterKey)
	if err != nil {
		t.Fatalf("open source store: %v", err)
	}
	defer source.Close()
	profileID, err := source.AddProfile("client-profile")
	if err != nil {
		t.Fatal(err)
	}
	device, err := source.AddDevice("工作站", "", "")
	if err != nil {
		t.Fatal(err)
	}
	revisionBeforeLayout, err := source.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	// 本票没有命令行写入入口。测试按拓扑图未来落库的公开备份形态构造
	// 一行坐标，再观察导出与导入行为。
	if _, err := source.db.Exec(
		`INSERT INTO device_layout_coordinates(view_key,device_id,x,y,created_at,updated_at)
		 VALUES(?,?,?,?,?,?)`,
		"主视图", device.ID, 12.5, -3, 1000, 1001,
	); err != nil {
		t.Fatalf("seed device layout coordinate: %v", err)
	}
	revisionAfterLayout, err := source.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if revisionAfterLayout != revisionBeforeLayout {
		t.Fatalf("layout coordinates must not change the client snapshot revision: before=%d after=%d",
			revisionBeforeLayout, revisionAfterLayout)
	}

	snapshot, err := source.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	want := []SnapshotDeviceLayoutCoordinate{{
		ViewKey:   "主视图",
		DeviceID:  device.ID,
		X:         12.5,
		Y:         -3,
		CreatedAt: 1000,
		UpdatedAt: 1001,
	}}
	if !reflect.DeepEqual(snapshot.DeviceLayoutCoordinates, want) {
		t.Fatalf("full snapshot layout coordinates = %+v, want %+v",
			snapshot.DeviceLayoutCoordinates, want)
	}

	clientSnapshot, err := source.ExportSnapshotForProfile(profileID, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(clientSnapshot.DeviceLayoutCoordinates, want) {
		t.Fatalf("client snapshot layout coordinates = %+v, want %+v",
			clientSnapshot.DeviceLayoutCoordinates, want)
	}
	encoded, err := json.Marshal(clientSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"device_layout_coordinates"`) {
		t.Fatalf("client snapshot JSON must contain layout coordinates:\n%s", encoded)
	}

	targetKey, err := GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	target, err := Open(filepath.Join(dir, "target.db"), targetKey)
	if err != nil {
		t.Fatalf("open target store: %v", err)
	}
	defer target.Close()
	if err := target.ImportSnapshot(snapshot); err != nil {
		t.Fatalf("import snapshot: %v", err)
	}
	restored, err := target.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored.DeviceLayoutCoordinates, want) {
		t.Fatalf("layout coordinates changed after import: got %+v want %+v",
			restored.DeviceLayoutCoordinates, want)
	}
}

func TestListDeviceLayoutCoordinatesReturnsEveryViewSorted(t *testing.T) {
	dir := t.TempDir()
	masterKey, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("generate master key: %v", err)
	}
	st, err := Open(filepath.Join(dir, "store.db"), masterKey)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	first, err := st.AddDevice("工作站", "", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.AddDevice("NAS", "", "")
	if err != nil {
		t.Fatal(err)
	}
	rows := []struct {
		view   string
		device string
		x, y   float64
	}{
		{"physical", second.ID, 30, 40},
		{"overlay", first.ID, 1, 2},
		{"physical", first.ID, 10, 20},
		{"virtual", first.ID, -5, 7},
	}
	for _, row := range rows {
		if _, err := st.db.Exec(
			`INSERT INTO device_layout_coordinates(view_key,device_id,x,y,created_at,updated_at)
			 VALUES(?,?,?,?,1000,1001)`,
			row.view, row.device, row.x, row.y,
		); err != nil {
			t.Fatalf("seed coordinate %s/%s: %v", row.view, row.device, err)
		}
	}

	got, err := st.ListDeviceLayoutCoordinates()
	if err != nil {
		t.Fatalf("list layout coordinates: %v", err)
	}
	byKey := make(map[string]models.DeviceLayoutCoordinate, len(got))
	for _, coordinate := range got {
		key := coordinate.ViewKey + "/" + coordinate.DeviceID
		if _, exists := byKey[key]; exists {
			t.Fatalf("同一视图与设备出现重复坐标：%s", key)
		}
		byKey[key] = coordinate
	}
	wantCoordinates := []struct {
		view   string
		device string
		x, y   float64
	}{
		{"overlay", first.ID, 1, 2},
		{"physical", first.ID, 10, 20},
		{"physical", second.ID, 30, 40},
		{"virtual", first.ID, -5, 7},
	}
	for _, want := range wantCoordinates {
		coordinate, ok := byKey[want.view+"/"+want.device]
		if !ok {
			t.Fatalf("缺少坐标 %s/%s，实际：%+v", want.view, want.device, byKey)
		}
		if coordinate.X != want.x || coordinate.Y != want.y ||
			!coordinate.CreatedAt.Equal(time.Unix(1000, 0)) || !coordinate.UpdatedAt.Equal(time.Unix(1001, 0)) {
			t.Fatalf("坐标 %s/%s = %+v，want x=%v y=%v 创建时间=1000 更新时间=1001",
				want.view, want.device, coordinate, want.x, want.y)
		}
	}
}

func TestSaveViewLayoutReplacesOneViewAndWritesOneAuditRow(t *testing.T) {
	st := newTestStore(t)
	site, err := st.AddSite("办公室", "")
	if err != nil {
		t.Fatal(err)
	}
	network, err := st.AddNetwork(models.Network{Name: "办公局域网", Type: models.NetworkPhysical, SiteName: "办公室"})
	if err != nil {
		t.Fatal(err)
	}
	device, err := st.AddDevice("工作站", "", "")
	if err != nil {
		t.Fatal(err)
	}
	revisionBefore, err := st.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}

	input := ViewLayoutInput{
		Devices: []DeviceLayoutInput{{DeviceID: device.ID, X: 11.5, Y: -4}},
		Containers: []ContainerLayoutInput{
			{EntityType: LayoutEntitySite, EntityID: site.ID, X: 120, Y: 80},
			{EntityType: LayoutEntityNetwork, EntityID: network.ID, X: 240, Y: 260},
		},
	}
	if err := st.SaveViewLayout("physical", input, "127.0.0.1"); err != nil {
		t.Fatalf("保存物理视图布局失败：%v", err)
	}
	revisionAfter, err := st.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if revisionAfter != revisionBefore {
		t.Fatalf("布局保存不得推动快照修订号：保存前=%d 保存后=%d", revisionBefore, revisionAfter)
	}

	deviceCoordinates, err := st.ListDeviceLayoutCoordinates()
	if err != nil {
		t.Fatal(err)
	}
	if len(deviceCoordinates) != 1 || deviceCoordinates[0].ViewKey != "physical" ||
		deviceCoordinates[0].DeviceID != device.ID ||
		deviceCoordinates[0].X != 11.5 || deviceCoordinates[0].Y != -4 {
		t.Fatalf("保存后的设备坐标错误：%+v", deviceCoordinates)
	}
	containerCoordinates, err := st.ListContainerLayoutCoordinates()
	if err != nil {
		t.Fatal(err)
	}
	containerByKey := make(map[string]models.ContainerLayoutCoordinate, len(containerCoordinates))
	for _, coordinate := range containerCoordinates {
		key := coordinate.ViewKey + "/" + coordinate.EntityType + "/" + coordinate.EntityID
		containerByKey[key] = coordinate
	}
	savedSite := containerByKey["physical/"+LayoutEntitySite+"/"+site.ID]
	savedNetwork := containerByKey["physical/"+LayoutEntityNetwork+"/"+network.ID]
	if len(containerCoordinates) != 2 ||
		savedSite.X != 120 || savedSite.Y != 80 ||
		savedNetwork.X != 240 || savedNetwork.Y != 260 {
		t.Fatalf("保存后的容器坐标错误：%+v", containerCoordinates)
	}

	saveAudits := auditRowsByAction(t, st, "topology.layout.save")
	if len(saveAudits) != 1 {
		t.Fatalf("一次布局保存应写一条审计，实际 %d 条", len(saveAudits))
	}
	if saveAudits[0].Source != AuditSourceWebUI || saveAudits[0].SourceIP != "127.0.0.1" ||
		saveAudits[0].Status != "ok" {
		t.Fatalf("布局保存审计行的来源错误：%+v", saveAudits[0])
	}

	// 第二次保存同一个视图时整体替换：未出现的坐标行一起删除。
	replacement := ViewLayoutInput{Devices: []DeviceLayoutInput{{DeviceID: device.ID, X: -9, Y: 33}}}
	if err := st.SaveViewLayout("physical", replacement, "127.0.0.1"); err != nil {
		t.Fatalf("替换物理视图布局失败：%v", err)
	}
	containerCoordinates, err = st.ListContainerLayoutCoordinates()
	if err != nil {
		t.Fatal(err)
	}
	if len(containerCoordinates) != 0 {
		t.Fatalf("同一次保存应替换整个视图的容器坐标，残留 %+v", containerCoordinates)
	}

	// 其它视图互不影响；重置只清空指定视图。
	virtual := ViewLayoutInput{Devices: []DeviceLayoutInput{{DeviceID: device.ID, X: 5, Y: 6}}}
	if err := st.SaveViewLayout("virtual", virtual, "127.0.0.1"); err != nil {
		t.Fatalf("保存虚拟视图布局失败：%v", err)
	}
	if err := st.ResetViewLayout("physical", "127.0.0.1"); err != nil {
		t.Fatalf("重置物理视图布局失败：%v", err)
	}
	deviceCoordinates, err = st.ListDeviceLayoutCoordinates()
	if err != nil {
		t.Fatal(err)
	}
	if len(deviceCoordinates) != 1 || deviceCoordinates[0].ViewKey != "virtual" {
		t.Fatalf("重置物理视图后剩余坐标错误：%+v", deviceCoordinates)
	}
	resetAudits := auditRowsByAction(t, st, "topology.layout.reset")
	if len(resetAudits) != 1 || resetAudits[0].Source != AuditSourceWebUI ||
		resetAudits[0].SourceIP != "127.0.0.1" || resetAudits[0].Status != "ok" {
		t.Fatalf("布局重置审计行错误：%+v", resetAudits)
	}
	saveAudits = auditRowsByAction(t, st, "topology.layout.save")
	if len(saveAudits) != 3 {
		t.Fatalf("三次保存应各写一条审计，实际 %d 条", len(saveAudits))
	}
}

func TestSaveViewLayoutRejectsInvalidInput(t *testing.T) {
	st := newTestStore(t)
	site, err := st.AddSite("办公室", "")
	if err != nil {
		t.Fatal(err)
	}
	device, err := st.AddDevice("工作站", "", "")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		view  string
		input ViewLayoutInput
	}{
		{name: "未知视图", view: "主视图", input: ViewLayoutInput{}},
		{name: "不存在的设备", view: "physical", input: ViewLayoutInput{
			Devices: []DeviceLayoutInput{{DeviceID: "missing-device", X: 1, Y: 2}}}},
		{name: "重复设备", view: "physical", input: ViewLayoutInput{
			Devices: []DeviceLayoutInput{
				{DeviceID: device.ID, X: 1, Y: 2},
				{DeviceID: device.ID, X: 3, Y: 4},
			}}},
		{name: "不存在的站点", view: "physical", input: ViewLayoutInput{
			Containers: []ContainerLayoutInput{{EntityType: LayoutEntitySite, EntityID: "missing-site", X: 1, Y: 2}}}},
		{name: "不存在的网络", view: "physical", input: ViewLayoutInput{
			Containers: []ContainerLayoutInput{{EntityType: LayoutEntityNetwork, EntityID: "missing-network", X: 1, Y: 2}}}},
		{name: "非法实体类型", view: "physical", input: ViewLayoutInput{
			Containers: []ContainerLayoutInput{{EntityType: "device", EntityID: device.ID, X: 1, Y: 2}}}},
		{name: "重复容器", view: "physical", input: ViewLayoutInput{
			Containers: []ContainerLayoutInput{
				{EntityType: LayoutEntitySite, EntityID: site.ID, X: 1, Y: 2},
				{EntityType: LayoutEntitySite, EntityID: site.ID, X: 3, Y: 4},
			}}},
		{name: "非有限坐标", view: "physical", input: ViewLayoutInput{
			Devices: []DeviceLayoutInput{{DeviceID: device.ID, X: math.NaN(), Y: 2}}}},
		{name: "空设备标识", view: "physical", input: ViewLayoutInput{
			Devices: []DeviceLayoutInput{{X: 1, Y: 2}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before, err := st.SnapshotRevision()
			if err != nil {
				t.Fatal(err)
			}
			if err := st.SaveViewLayout(tc.view, tc.input, "127.0.0.1"); err == nil {
				t.Fatalf("非法输入 %s 应被拒绝", tc.name)
			}
			after, err := st.SnapshotRevision()
			if err != nil {
				t.Fatal(err)
			}
			if after != before {
				t.Fatalf("非法保存不得改变任何数据，修订号 %d → %d", before, after)
			}
		})
	}
}

func auditRowsByAction(t *testing.T, st *Store, action string) []AuditRow {
	t.Helper()
	rows, err := st.AuditRows(1000)
	if err != nil {
		t.Fatalf("读取审计失败：%v", err)
	}
	var matched []AuditRow
	for _, row := range rows {
		if row.Action == action {
			matched = append(matched, row)
		}
	}
	return matched
}

func TestContainerLayoutCoordinatesSnapshotRoundTripClientDeliveryAndCleanup(t *testing.T) {
	dir := t.TempDir()
	masterKey, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("generate master key: %v", err)
	}
	source, err := Open(filepath.Join(dir, "source.db"), masterKey)
	if err != nil {
		t.Fatalf("open source store: %v", err)
	}
	defer source.Close()
	profileID, err := source.AddProfile("client-profile")
	if err != nil {
		t.Fatal(err)
	}
	site, err := source.AddSite("办公室", "")
	if err != nil {
		t.Fatal(err)
	}
	homeNetwork, err := source.AddNetwork(models.Network{Name: "办公局域网", Type: models.NetworkPhysical, SiteName: "办公室"})
	if err != nil {
		t.Fatal(err)
	}
	virtualNetwork, err := source.AddNetwork(models.Network{Name: "虚拟网", Type: models.NetworkVirtual})
	if err != nil {
		t.Fatal(err)
	}
	device, err := source.AddDevice("工作站", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.AddNetworkMembership("工作站", "虚拟网", "100.64.0.20"); err != nil {
		t.Fatal(err)
	}
	layout := ViewLayoutInput{
		Devices: []DeviceLayoutInput{{DeviceID: device.ID, X: 12.5, Y: -3}},
		Containers: []ContainerLayoutInput{
			{EntityType: LayoutEntitySite, EntityID: site.ID, X: 20, Y: 30},
			{EntityType: LayoutEntityNetwork, EntityID: homeNetwork.ID, X: 40, Y: 50},
			{EntityType: LayoutEntityNetwork, EntityID: virtualNetwork.ID, X: 60, Y: 70},
		},
	}
	if err := source.SaveViewLayout("overlay", layout, "127.0.0.1"); err != nil {
		t.Fatalf("保存布局失败：%v", err)
	}

	snapshot, err := source.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	wantContainers := []SnapshotContainerLayoutCoordinate{
		{ViewKey: "overlay", EntityType: LayoutEntityNetwork, EntityID: homeNetwork.ID, X: 40, Y: 50},
		{ViewKey: "overlay", EntityType: LayoutEntityNetwork, EntityID: virtualNetwork.ID, X: 60, Y: 70},
		{ViewKey: "overlay", EntityType: LayoutEntitySite, EntityID: site.ID, X: 20, Y: 30},
	}
	// 快照段按视图、实体类型、实体标识排序；标识由存储生成，断言按键查找。
	snapshotByEntity := make(map[string]SnapshotContainerLayoutCoordinate, len(snapshot.ContainerLayoutCoordinates))
	for _, coordinate := range snapshot.ContainerLayoutCoordinates {
		key := coordinate.EntityType + "/" + coordinate.EntityID
		snapshotByEntity[key] = coordinate
	}
	if len(snapshotByEntity) != len(wantContainers) {
		t.Fatalf("全量快照容器坐标段 = %+v，want %+v",
			snapshot.ContainerLayoutCoordinates, wantContainers)
	}
	for i := range wantContainers {
		want := &wantContainers[i]
		got, ok := snapshotByEntity[want.EntityType+"/"+want.EntityID]
		if !ok || got.ViewKey != want.ViewKey || got.X != want.X || got.Y != want.Y ||
			got.CreatedAt <= 0 || got.UpdatedAt != got.CreatedAt {
			t.Fatalf("全量快照容器坐标 %s = %+v，want %+v",
				want.EntityType+"/"+want.EntityID, got, want)
		}
		want.CreatedAt, want.UpdatedAt = got.CreatedAt, got.UpdatedAt
	}
	sort.Slice(wantContainers, func(i, j int) bool {
		if wantContainers[i].EntityType != wantContainers[j].EntityType {
			return wantContainers[i].EntityType < wantContainers[j].EntityType
		}
		return wantContainers[i].EntityID < wantContainers[j].EntityID
	})
	clientSnapshot, err := source.ExportSnapshotForProfile(profileID, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(clientSnapshot.ContainerLayoutCoordinates, wantContainers) {
		t.Fatalf("客户端快照容器坐标段 = %+v，want %+v",
			clientSnapshot.ContainerLayoutCoordinates, wantContainers)
	}
	encoded, err := json.Marshal(clientSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"container_layout_coordinates"`) {
		t.Fatalf("客户端快照 JSON 应包含容器坐标段：\n%s", encoded)
	}

	targetKey, err := GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	target, err := Open(filepath.Join(dir, "target.db"), targetKey)
	if err != nil {
		t.Fatalf("open target store: %v", err)
	}
	defer target.Close()
	if err := target.ImportSnapshot(snapshot); err != nil {
		t.Fatalf("import snapshot: %v", err)
	}
	restored, err := target.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored.ContainerLayoutCoordinates, wantContainers) {
		t.Fatalf("导入后的容器坐标段 = %+v，want %+v",
			restored.ContainerLayoutCoordinates, wantContainers)
	}

	// 删除网络与站点时，它们的容器坐标随之删除；其它坐标保留。
	if _, err := target.DeleteNetwork("办公局域网"); err != nil {
		t.Fatal(err)
	}
	if _, err := target.DeleteNetwork("虚拟网"); err != nil {
		t.Fatal(err)
	}
	if err := target.DeleteSite("办公室"); err != nil {
		t.Fatal(err)
	}
	restored, err = target.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.ContainerLayoutCoordinates) != 0 {
		t.Fatalf("删除站点与网络后应清理容器坐标，实际 %+v",
			restored.ContainerLayoutCoordinates)
	}
	if len(restored.DeviceLayoutCoordinates) != 1 {
		t.Fatalf("设备坐标不应随网络删除被清理：%+v", restored.DeviceLayoutCoordinates)
	}

	// 删除设备时，该设备的坐标在全部视图里删除。
	if err := source.SaveViewLayout("physical", ViewLayoutInput{
		Devices: []DeviceLayoutInput{{DeviceID: device.ID, X: 1, Y: 2}},
	}, "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	preview, err := source.DeviceDeletePreview("工作站")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.DeleteDeviceCascading("工作站", preview); err != nil {
		t.Fatal(err)
	}
	deviceRows, err := source.ListDeviceLayoutCoordinates()
	if err != nil {
		t.Fatal(err)
	}
	if len(deviceRows) != 0 {
		t.Fatalf("删除设备后应清理它的全部坐标：%+v", deviceRows)
	}
}
