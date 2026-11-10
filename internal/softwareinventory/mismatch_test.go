package softwareinventory

import (
	"testing"

	"ssh-manager-mcp/internal/models"
)

// TestUnifiedQueryCoversObservedOnlyAndMismatchRules 覆盖统一查询的合成基
// 线：只有实测、没有声明的设备能查到；受管设备上的未匹配声明标为对不上；
// 不受管设备上的声明不算对不上。
func TestUnifiedQueryCoversObservedOnlyAndMismatchRules(t *testing.T) {
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
	got := map[string]map[string]QueryItem{}
	for _, device := range result.Devices {
		got[device.DeviceName] = map[string]QueryItem{}
		for _, item := range device.Items {
			got[device.DeviceName][item.Name] = item
		}
	}
	if len(got["只有实测"]) != 1 || got["只有实测"]["observed-only"].Source != SourceObserved {
		t.Fatalf("只有实测的设备应能被统一查询：%+v", result.Devices)
	}
	if item := got["受管甲"]["没有对应的声明"]; item.Source != SourceDeclared || !item.Mismatch {
		t.Fatalf("受管设备未认领到实测的声明 = %+v", item)
	}
	if item := got["受管甲"]["已认领后消失的声明"]; item.Source != SourceDeclared || !item.Mismatch {
		t.Fatalf("认领的实测消失后的声明 = %+v", item)
	}
	if item := got["不受管设备"]["网络存储声明"]; item.Source != SourceDeclared || item.Mismatch {
		t.Fatalf("不受管设备上的声明不应标为对不上：%+v", item)
	}
}
