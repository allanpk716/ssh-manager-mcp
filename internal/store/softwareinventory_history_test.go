package store

import (
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
)

// softwareHistoryChange 把一条历史转换成便于断言的形状。
type softwareHistoryChange struct {
	Kind     models.SoftwareChangeKind
	Name     string
	OldValue string
	NewValue string
}

func historyKinds(rows []models.SoftwareChangeEntry) map[string]softwareHistoryChange {
	out := map[string]softwareHistoryChange{}
	for _, row := range rows {
		out[string(row.Kind)+"/"+row.Name] = softwareHistoryChange{
			Kind: row.Kind, Name: row.Name, OldValue: row.OldValue, NewValue: row.NewValue,
		}
	}
	return out
}

// observedWriteWithItems 组一次成功采集的落库输入。
func observedWriteWithItems(serverID string, incomplete bool, items ...models.SoftwareObservedItem) SoftwareObservedWrite {
	return SoftwareObservedWrite{
		Items: items, ServerID: serverID, CommandSetVersion: "linux-v1",
		SystemType: "ubuntu", Incomplete: incomplete,
	}
}

// TestSoftwareChangeHistoryRecordsAllKinds 覆盖两次成功采集之间的四种变
// 化：新增、移除、版本变化、暴露级别变化。
func TestSoftwareChangeHistoryRecordsAllKinds(t *testing.T) {
	st := newCollectTestStore(t)
	firstAt := time.Unix(1770000000, 0)
	secondAt := firstAt.Add(24 * time.Hour)
	st.NowFn = func() time.Time { return firstAt }
	server := addCollectServer(t, st, "工作站", "工作站入口")

	first := []models.SoftwareObservedItem{
		{
			Category: models.SoftwareCategorySystemPackage, Name: "openssh-server",
			Version: "1:9.6p1-3ubuntu13.20",
			ListeningPorts: []models.SoftwareListeningPort{{
				Address: "0.0.0.0", Port: 22, Protocol: "tcp",
				Exposure: models.SoftwareExposureAllAddresses,
			}},
		},
		{
			Category: models.SoftwareCategorySystemPackage, Name: "python3",
			Version: "3.12.3-1",
		},
	}
	if err := st.ReplaceSoftwareObservedItems(
		"工作站", observedWriteWithItems(server.ID, false, first...), CLISource()); err != nil {
		t.Fatal(err)
	}

	st.NowFn = func() time.Time { return secondAt }
	second := []models.SoftwareObservedItem{
		{
			Category: models.SoftwareCategorySystemPackage, Name: "openssh-server",
			Version: "1:9.9p1-3ubuntu13.20",
			ListeningPorts: []models.SoftwareListeningPort{{
				Address: "127.0.0.1", Port: 22, Protocol: "tcp",
				Exposure: models.SoftwareExposureLocal,
			}},
		},
		{
			Category: models.SoftwareCategorySystemPackage, Name: "nginx",
			Version: "1.24.0",
		},
	}
	if err := st.ReplaceSoftwareObservedItems(
		"工作站", observedWriteWithItems(server.ID, false, second...), CLISource()); err != nil {
		t.Fatal(err)
	}

	rows, err := st.SoftwareChangeHistoryForDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	changes := historyKinds(rows)
	if got := changes["added/nginx"]; got.Kind != models.SoftwareChangeAdded ||
		got.NewValue != "1.24.0" || got.OldValue != "" {
		t.Fatalf("新增历史 = %+v", got)
	}
	if got := changes["removed/python3"]; got.Kind != models.SoftwareChangeRemoved ||
		got.OldValue != "3.12.3-1" || got.NewValue != "" {
		t.Fatalf("移除历史 = %+v", got)
	}
	if got := changes["version_changed/openssh-server"]; got.Kind != models.SoftwareChangeVersionChanged ||
		got.OldValue != "1:9.6p1-3ubuntu13.20" || got.NewValue != "1:9.9p1-3ubuntu13.20" {
		t.Fatalf("版本变化历史 = %+v", got)
	}
	if got := changes["exposure_changed/openssh-server"]; got.Kind != models.SoftwareChangeExposureChanged ||
		got.OldValue != string(models.SoftwareExposureAllAddresses) ||
		got.NewValue != string(models.SoftwareExposureLocal) {
		t.Fatalf("暴露级别变化历史 = %+v", got)
	}

	// 历史按时间倒序：第二次采集的变化必须排在第一次采集之前。
	for _, row := range rows {
		if row.OccurredAt.Equal(secondAt) {
			break
		}
		if row.OccurredAt.After(secondAt) {
			t.Fatalf("变化历史未按时间倒序：%+v", rows)
		}
		t.Fatalf("第二次采集之前还有更早时间的变化历史行：%+v", rows)
	}
	var firstWriteRow models.SoftwareChangeEntry
	for _, row := range rows {
		if row.OccurredAt.Equal(firstAt) {
			firstWriteRow = row
			break
		}
	}
	if firstWriteRow.DeviceName != "工作站" || firstWriteRow.DeviceID == "" ||
		firstWriteRow.StableKey == "" || firstWriteRow.Category == "" {
		t.Fatalf("历史行缺少设备或条目标识字段：%+v", firstWriteRow)
	}
}

// TestSoftwareChangeHistoryIncompleteSkipsRemovals 钉住不完整采集的边界：
// 没采到的条目不能被误记为「移除」。
func TestSoftwareChangeHistoryIncompleteSkipsRemovals(t *testing.T) {
	st := newCollectTestStore(t)
	now := time.Unix(1770000000, 0)
	st.NowFn = func() time.Time { return now }
	server := addCollectServer(t, st, "工作站", "工作站入口")
	first := []models.SoftwareObservedItem{
		{Category: models.SoftwareCategorySystemPackage, Name: "openssh-server", Version: "1:9.6p1-1"},
		{Category: models.SoftwareCategorySystemPackage, Name: "python3", Version: "3.12.3-1"},
	}
	if err := st.ReplaceSoftwareObservedItems(
		"工作站", observedWriteWithItems(server.ID, false, first...), CLISource()); err != nil {
		t.Fatal(err)
	}
	second := []models.SoftwareObservedItem{
		{Category: models.SoftwareCategorySystemPackage, Name: "openssh-server", Version: "1:9.6p1-2"},
	}
	if err := st.ReplaceSoftwareObservedItems(
		"工作站", observedWriteWithItems(server.ID, true, second...), CLISource()); err != nil {
		t.Fatal(err)
	}

	rows, err := st.SoftwareChangeHistoryForDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Kind == models.SoftwareChangeRemoved {
			t.Fatalf("不完整采集不得产生移除历史：%+v", rows)
		}
	}
	foundVersionChange := false
	for _, row := range rows {
		if row.Kind == models.SoftwareChangeVersionChanged && row.Name == "openssh-server" {
			foundVersionChange = true
		}
	}
	if !foundVersionChange {
		t.Fatalf("不完整采集仍应记录已经看到的版本变化：%+v", rows)
	}
}

// TestSoftwareChangeHistoryRetentionBoundary 用固定时钟钉住一年保留期边界：
// 恰好满一年的行保留，早于一年的行在写入采集结果的同一事务里清理。
func TestSoftwareChangeHistoryRetentionBoundary(t *testing.T) {
	st := newCollectTestStore(t)
	base := time.Unix(1770000000, 0)
	server := addCollectServer(t, st, "工作站", "工作站入口")

	tooOld := base.Add(-365 * 24 * time.Hour).Add(-time.Second)
	st.NowFn = func() time.Time { return tooOld }
	if err := st.ReplaceSoftwareObservedItems("工作站", observedWriteWithItems(server.ID, false,
		models.SoftwareObservedItem{
			Category: models.SoftwareCategorySystemPackage, Name: "old-package", Version: "1",
		},
	), CLISource()); err != nil {
		t.Fatal(err)
	}
	exactlyOneYear := base.Add(-365 * 24 * time.Hour)
	st.NowFn = func() time.Time { return exactlyOneYear }
	if err := st.ReplaceSoftwareObservedItems("工作站", observedWriteWithItems(server.ID, false,
		models.SoftwareObservedItem{
			Category: models.SoftwareCategorySystemPackage, Name: "old-package", Version: "1",
		},
		models.SoftwareObservedItem{
			Category: models.SoftwareCategorySystemPackage, Name: "boundary-package", Version: "1",
		},
	), CLISource()); err != nil {
		t.Fatal(err)
	}
	st.NowFn = func() time.Time { return base }
	if err := st.ReplaceSoftwareObservedItems("工作站", observedWriteWithItems(server.ID, false,
		models.SoftwareObservedItem{
			Category: models.SoftwareCategorySystemPackage, Name: "old-package", Version: "1",
		},
		models.SoftwareObservedItem{
			Category: models.SoftwareCategorySystemPackage, Name: "boundary-package", Version: "1",
		},
		models.SoftwareObservedItem{
			Category: models.SoftwareCategorySystemPackage, Name: "fresh-package", Version: "1",
		},
	), CLISource()); err != nil {
		t.Fatal(err)
	}

	rows, err := st.SoftwareChangeHistoryForDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.OccurredAt.Before(exactlyOneYear) {
			t.Fatalf("超过一年的变化历史应被清理：%+v", row)
		}
	}
	sawBoundary := false
	for _, row := range rows {
		if row.OccurredAt.Equal(exactlyOneYear) {
			sawBoundary = true
		}
	}
	if !sawBoundary {
		t.Fatalf("恰好一年的变化历史不应清理：%+v", rows)
	}
}
