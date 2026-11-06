package store

import (
	"testing"

	"ssh-manager-mcp/internal/models"
)

func TestListDevicesFiltersNamesCaseInsensitively(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.AddSite("Office", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{
		Name: "Lab", Type: models.NetworkPhysical, SiteName: "Office",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("Workstation", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership("Workstation", "Lab", "192.0.2.10"); err != nil {
		t.Fatal(err)
	}

	bySite, err := st.ListDevices(DeviceFilter{Site: "office"})
	if err != nil {
		t.Fatal(err)
	}
	if len(bySite) != 1 || bySite[0].Name != "Workstation" {
		t.Fatalf("site filter must compare names case-insensitively, got: %+v", bySite)
	}
	byNetwork, err := st.ListDevices(DeviceFilter{Network: "lab"})
	if err != nil {
		t.Fatal(err)
	}
	if len(byNetwork) != 1 || byNetwork[0].Name != "Workstation" {
		t.Fatalf("network filter must compare names case-insensitively, got: %+v", byNetwork)
	}
}
