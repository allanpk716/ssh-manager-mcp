package softwareinventory

import (
	"testing"

	"ssh-manager-mcp/internal/models"
)

// TestUnifiedQueryOutputsObservedOnlyAndKeepsDeclarations 覆盖查询收口：
// 只有实测、没有声明的设备能查到；声明、认领合成与「对不上」不进入使用
// 者可见结果，但生效声明保留在存储里。
func TestUnifiedQueryOutputsObservedOnlyAndKeepsDeclarations(t *testing.T) {
	manager, st := newTestManager(t)
	mustDevice(t, st, "受管甲", true)
	mustDevice(t, st, "只有实测", true)
	mustDevice(t, st, "不受管设备", false)

	obsolete := models.SoftwareObservedItem{
		Category: CategoryServiceProgram, Name: "旧服务", Version: "1.0",
	}
	mustObservedItems(t, st, "受管甲", obsolete)
	observed, err := st.SoftwareObservedItemsForDevice("受管甲")
	if err != nil || len(observed) != 1 {
		t.Fatalf("准备受管甲实测条目失败：%v %+v", err, observed)
	}
	obsoleteKey := observed[0].StableKey

	declarations := []struct {
		device string
		name   string
		claim  string
	}{
		{device: "受管甲", name: "没有对应的声明"},
		{device: "受管甲", name: "已认领后消失的声明", claim: obsoleteKey},
		{device: "不受管设备", name: "网络存储声明"},
	}
	for _, declaration := range declarations {
		id, err := manager.AddDeclaration(Declaration{
			Device: declaration.device, Category: CategoryServiceProgram,
			Name: declaration.name, Version: "1.0",
		}, CLISource())
		if err != nil {
			t.Fatal(err)
		}
		if declaration.claim != "" {
			if err := manager.ClaimDeclaration(id, declaration.claim, CLISource()); err != nil {
				t.Fatal(err)
			}
		}
	}
	mustObservedItems(t, st, "受管甲")
	mustObservedItems(t, st, "只有实测", models.SoftwareObservedItem{
		Category: CategoryServiceProgram, Name: "observed-only", Version: "2.0",
	})

	result, err := manager.Query(QueryOptions{Visibility: AllDevicesVisible()})
	if err != nil {
		t.Fatal(err)
	}
	if findQueryDevice(result, "只有实测") == nil ||
		len(findQueryDevice(result, "只有实测").Items) != 1 ||
		findQueryDevice(result, "只有实测").Items[0].Source != SourceObserved {
		t.Fatalf("只有实测的设备应能被统一查询：%+v", result.Devices)
	}
	if item := findQueryItem(result, "没有对应的声明"); item != nil {
		t.Fatalf("声明不应进入使用者可见查询：%+v", item)
	}
	if item := findQueryItem(result, "已认领后消失的声明"); item != nil {
		t.Fatalf("认领的实测消失后，声明不应进入查询：%+v", item)
	}
	if item := findQueryItem(result, "网络存储声明"); item != nil {
		t.Fatalf("不受管设备声明不应进入查询：%+v", item)
	}
	storedDeclarations, err := st.SearchSoftwareDeclarations("", AllDevicesVisible())
	if err != nil {
		t.Fatal(err)
	}
	if len(storedDeclarations) != 3 {
		t.Fatalf("三条生效声明应保留在存储里：%+v", storedDeclarations)
	}
}
