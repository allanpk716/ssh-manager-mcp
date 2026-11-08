package store

import (
	"reflect"
	"testing"

	"ssh-manager-mcp/internal/models"
)

func TestAllVirtualNetworkAddressesReturnsOnlyVirtualMembers(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	if _, err := s.AddSite("办公室", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddNetwork(models.Network{Name: "物理局域网", Type: models.NetworkPhysical, SiteName: "办公室"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddNetwork(models.Network{Name: "虚拟局域网", Type: models.NetworkVirtual}); err != nil {
		t.Fatal(err)
	}
	for _, membership := range []struct {
		device  string
		network string
		address string
	}{
		{"设备一", "虚拟局域网", "100.64.10.5"},
		{"设备二", "物理局域网", "192.0.2.10"},
		{"设备四", "虚拟局域网", "100::1"},
	} {
		if _, err := s.AddDevice(membership.device, "", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddNetworkMembership(membership.device, membership.network, membership.address); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.AllVirtualNetworkAddresses()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"100.64.10.5", "100::1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("虚拟局域网成员地址 = %v，应为 %v", got, want)
	}
}
