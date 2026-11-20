package networkrange

import (
	"reflect"
	"testing"

	"ssh-manager-mcp/internal/models"
)

func suggestionNames(suggestions []Suggestion) []string {
	if len(suggestions) == 0 {
		return nil
	}
	names := make([]string, 0, len(suggestions))
	for _, suggestion := range suggestions {
		names = append(names, suggestion.NetworkName)
	}
	return names
}

func TestSuggestionsMatchesIPv4AndIPv6(t *testing.T) {
	sites := []*models.Site{
		{ID: "site-home", Name: "家A"},
		{ID: "site-room", ParentID: "site-home", Name: "房间"},
	}
	networks := []Network{
		{Name: "家A-办公网", Range: "192.168.100.0/24", SiteID: "site-home", Physical: true},
		{Name: "IPv6 虚拟网", Range: "2001:db8:1::/64"},
	}

	got := Suggestions(Input{
		Address: "192.168.100.41", CurrentNetwork: "other", DeviceSiteID: "site-room",
		Networks: networks, Sites: sites,
	})
	if want := []Suggestion{{NetworkName: "家A-办公网", NetworkRange: "192.168.100.0/24"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("IPv4 地址应提示下级地点也被覆盖的网络，得到 %+v，应为 %+v", got, want)
	}

	got = Suggestions(Input{
		Address: "2001:DB8:1::41", CurrentNetwork: "other",
		Networks: networks, Sites: sites,
	})
	if want := []Suggestion{{NetworkName: "IPv6 虚拟网", NetworkRange: "2001:db8:1::/64"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("IPv6 地址应提示虚拟局域网，得到 %+v，应为 %+v", got, want)
	}
}

func TestSuggestionsFiltersAddressAndRange(t *testing.T) {
	sites := []*models.Site{{ID: "site-home", Name: "家A"}}
	cases := []struct {
		name    string
		address string
		current string
		network Network
		want    []string
	}{
		{name: "域名地址不提示", address: "example.com", current: "other",
			network: Network{Name: "家A-办公网", Range: "192.168.100.0/24", SiteID: "site-home", Physical: true}, want: nil},
		{name: "网络未登记网段不提示", address: "192.168.100.41", current: "other",
			network: Network{Name: "家A-办公网", SiteID: "site-home", Physical: true}, want: nil},
		{name: "网段主机位不为零不提示", address: "192.168.100.41", current: "other",
			network: Network{Name: "家A-办公网", Range: "192.168.100.1/24", SiteID: "site-home", Physical: true}, want: nil},
		{name: "非法网段不提示", address: "192.168.100.41", current: "other",
			network: Network{Name: "家A-办公网", Range: "192.168.100.0/33", SiteID: "site-home", Physical: true}, want: nil},
		{name: "地址家族不同不提示", address: "2001:db8::41", current: "other",
			network: Network{Name: "家A-办公网", Range: "192.168.100.0/24", SiteID: "site-home", Physical: true}, want: nil},
		{name: "当前行已属于的网络不重复提示", address: "192.168.100.41", current: "家A-当前网",
			network: Network{Name: "家A-当前网", Range: "192.168.100.0/24", SiteID: "site-home", Physical: true}, want: nil},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := suggestionNames(Suggestions(Input{
				Address: testCase.address, CurrentNetwork: testCase.current,
				DeviceSiteID: "site-home", Networks: []Network{testCase.network}, Sites: sites,
			}))
			if !reflect.DeepEqual(got, testCase.want) {
				t.Fatalf("提示网络 = %v，应为 %v", got, testCase.want)
			}
		})
	}
}

func TestSuggestionsRequiresCoveredLocationForPhysicalNetwork(t *testing.T) {
	sites := []*models.Site{
		{ID: "site-home", Name: "家A"},
		{ID: "site-room", ParentID: "site-home", Name: "房间"},
		{ID: "site-office", Name: "办公室"},
	}
	networks := []Network{
		{Name: "家A-办公网", Range: "192.168.100.0/24", SiteID: "site-home", Physical: true},
		{Name: "办公室同网段", Range: "192.168.100.0/24", SiteID: "site-office", Physical: true},
		{Name: "虚拟网", Range: "192.168.100.0/24"},
	}
	cases := []struct {
		name         string
		deviceSiteID string
		want         []string
	}{
		{name: "地点在覆盖地点内", deviceSiteID: "site-room", want: []string{"家A-办公网", "虚拟网"}},
		{name: "地点不在覆盖范围内", deviceSiteID: "site-office", want: []string{"办公室同网段", "虚拟网"}},
		{name: "设备未登记地点", deviceSiteID: "", want: []string{"虚拟网"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := suggestionNames(Suggestions(Input{
				Address: "192.168.100.41", CurrentNetwork: "other",
				DeviceSiteID: testCase.deviceSiteID, Networks: networks, Sites: sites,
			}))
			if !reflect.DeepEqual(got, testCase.want) {
				t.Fatalf("提示网络 = %v，应为 %v", got, testCase.want)
			}
		})
	}
}

func TestSuggestionsListsEveryDuplicateRangeCandidate(t *testing.T) {
	sites := []*models.Site{{ID: "site-home", Name: "家A"}}
	networks := []Network{
		{Name: "家A-办公网", Range: "192.168.100.0/24", SiteID: "site-home", Physical: true},
		{Name: "家A-备用网", Range: "192.168.100.0/24", SiteID: "site-home", Physical: true},
	}

	got := Suggestions(Input{
		Address: "192.168.100.41", CurrentNetwork: "other", DeviceSiteID: "site-home",
		Networks: networks, Sites: sites,
	})
	if len(got) != 2 || got[0].NetworkName != "家A-办公网" || got[1].NetworkName != "家A-备用网" {
		t.Fatalf("重复网段应逐个列出候选，得到 %+v", got)
	}
}
