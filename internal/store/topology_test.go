package store

import (
	"errors"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

func TestAddNetworkRejectsInvalidNetworkRange(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.AddSite("家A", ""); err != nil {
		t.Fatal(err)
	}

	for _, input := range []struct {
		networkRange string
		wantMessage  string
	}{
		{networkRange: "192.168.100.0", wantMessage: "网段"},
		{networkRange: "not-a-network", wantMessage: "网段"},
		{networkRange: "192.168.100.1/24", wantMessage: "主机位"},
	} {
		t.Run(input.networkRange, func(t *testing.T) {
			_, err := st.AddNetwork(models.Network{
				Name: "坏网段", Type: models.NetworkPhysical, SiteName: "家A",
				NetworkRange: input.networkRange,
			})

			var validation *TopologyValidationError
			if !errors.As(err, &validation) {
				t.Fatalf("非法网段应返回可直接展示的校验错误：%v", err)
			}
			if !strings.Contains(validation.Message, input.wantMessage) {
				t.Fatalf("错误说明应包含 %q，实际：%q", input.wantMessage, validation.Message)
			}
		})
	}
}

func TestEditNetworkRejectsInvalidNetworkRange(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.AddSite("家A", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{
		Name: "家A局域网", Type: models.NetworkPhysical, SiteName: "家A",
		NetworkRange: "192.168.100.0/24",
	}); err != nil {
		t.Fatal(err)
	}

	for _, networkRange := range []string{"192.168.100.0", "not-a-network", "192.168.100.1/24"} {
		t.Run(networkRange, func(t *testing.T) {
			_, err := st.EditNetwork("家A局域网", NetworkChanges{NetworkRange: &networkRange})

			var validation *TopologyValidationError
			if !errors.As(err, &validation) {
				t.Fatalf("非法网段应返回可直接展示的校验错误：%v", err)
			}
			networks, listErr := st.ListNetworks(NetworkFilter{})
			if listErr != nil {
				t.Fatal(listErr)
			}
			if len(networks) != 1 || networks[0].NetworkRange != "192.168.100.0/24" {
				t.Fatalf("非法编辑不能改变已登记网段：%+v", networks)
			}
		})
	}
}

func TestNetworkRangeAcceptsEmptyValidAndDuplicateRanges(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.AddSite("家A", ""); err != nil {
		t.Fatal(err)
	}

	first, err := st.AddNetwork(models.Network{
		Name: "家A局域网", Type: models.NetworkPhysical, SiteName: "家A",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.NetworkRange != "" {
		t.Fatalf("留空网段应表示没有登记网段：%q", first.NetworkRange)
	}

	ipv4Range := "192.168.100.0/24"
	first, err = st.EditNetwork(first.Name, NetworkChanges{NetworkRange: &ipv4Range})
	if err != nil {
		t.Fatal(err)
	}
	if first.NetworkRange != ipv4Range {
		t.Fatalf("合法 IPv4 网段应保存：%q", first.NetworkRange)
	}

	if _, err := st.AddNetwork(models.Network{
		Name: "备用网", Type: models.NetworkPhysical, SiteName: "家A", NetworkRange: ipv4Range,
	}); err != nil {
		t.Fatalf("不同网络登记相同网段应允许：%v", err)
	}
	if _, err := st.AddNetwork(models.Network{
		Name: "虚拟网", Type: models.NetworkVirtual, NetworkRange: "2001:db8::/32",
	}); err != nil {
		t.Fatalf("合法 IPv6 网段应保存：%v", err)
	}

	emptyRange := ""
	first, err = st.EditNetwork(first.Name, NetworkChanges{NetworkRange: &emptyRange})
	if err != nil {
		t.Fatal(err)
	}
	if first.NetworkRange != "" {
		t.Fatalf("编辑时传空字符串应清空网段：%q", first.NetworkRange)
	}
}
