package store

// 布局坐标存储只服务拓扑图。它必须随备份与导出的全量快照往返保留，
// 也随 profile 裁剪快照下发；只有布局坐标变化不改变客户端用来判断是否
// 重新拉取的快照修订号。

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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
