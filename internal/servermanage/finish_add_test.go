package servermanage

import (
	"errors"
	"testing"

	"ssh-manager-mcp/internal/models"
)

func TestFinishAddSkipDoesNotApplySelectedSite(t *testing.T) {
	m := newTestManager(t)
	physical, _, _, _ := seedAddTopology(t, m)
	other, err := m.st.AddSite("机房", "")
	if err != nil {
		t.Fatal(err)
	}
	result, err := m.Add(AddRequest{
		Server:      models.Server{Name: "skip-site", User: "deploy", Port: 22},
		SiteName:    "家A",
		Memberships: []NetworkMembershipInput{{Network: physical, Address: "192.168.1.51"}},
	}, CLISource())
	if err != nil {
		t.Fatal(err)
	}

	if err := m.FinishAdd(FinishAddRequest{
		ServerID: result.ServerID, SiteAction: "skip", SiteID: other.ID,
	}, WebUISource("127.0.0.1")); err != nil {
		t.Fatal(err)
	}
	device, err := m.st.GetDevice(result.DeviceName)
	if err != nil {
		t.Fatal(err)
	}
	if device.SiteName != "家A" {
		t.Fatalf("跳过地点不得改机器地点，实际 %q", device.SiteName)
	}
}

func TestFinishAddSelectValidatesSite(t *testing.T) {
	m := newTestManager(t)
	physical, _, profileID, _ := seedAddTopology(t, m)
	result, err := m.Add(AddRequest{
		Server:      models.Server{Name: "select-site", User: "deploy", Port: 22},
		Memberships: []NetworkMembershipInput{{Network: physical, Address: "192.168.1.52"}},
	}, CLISource())
	if err != nil {
		t.Fatal(err)
	}

	empty := m.FinishAdd(FinishAddRequest{ServerID: result.ServerID, SiteAction: "select"}, CLISource())
	var emptyField *FieldError
	if !errors.As(empty, &emptyField) || emptyField.Page != "请先选择一个地点。" {
		t.Fatalf("未选地点应返回表单错误，实际 %v", empty)
	}
	unknown := m.FinishAdd(FinishAddRequest{ServerID: result.ServerID, SiteAction: "select", SiteID: "site-missing"}, CLISource())
	if unknown == nil {
		t.Fatal("未知地点应返回错误")
	}

	sites, err := m.st.ListSites()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.FinishAdd(FinishAddRequest{
		ServerID: result.ServerID, SiteAction: "select", SiteID: sites[0].ID, ProfileIDs: []string{profileID},
	}, WebUISource("127.0.0.1")); err != nil {
		t.Fatal(err)
	}
	granted, err := m.st.ProfilesForServer(result.ServerID)
	if err != nil {
		t.Fatal(err)
	}
	if len(granted) != 1 || granted[0] != profileID {
		t.Fatalf("完成新增应写入 profile 授权，实际 %v", granted)
	}
}
