package discoveryname

import "testing"

func TestDisplayNameUsesConfiguredHostnameThenFallback(t *testing.T) {
	cases := []struct {
		configured string
		hostname   string
		want       string
	}{
		{"nuc10", "other-host", "nuc10"},
		{"my nuc.10_x-y", "", "my nuc.10_x-y"},
		{"中文节点", "", "中文节点"},
		{"bad!name", "MY-NUC", "MY-NUC"},
		{"", "host-1", "host-1"},
		{"!", "192.168.1.10", "192.168.1.10"},
		{"", "", "sshmgr"},
		{"!!!", "###", "sshmgr"},
		{"!!!", "-_-", "sshmgr"},
		{"!!!", "--my.nuc--", "my.nuc--"},
		{"!", "01234567890123456789012345678901234", "01234567890123456789012345678901"},
		{"01234567890123456789012345678901234", "h", "h"},
	}
	for _, test := range cases {
		if got := DisplayName(test.configured, test.hostname); got != test.want {
			t.Errorf("DisplayName(%q, %q) = %q, want %q",
				test.configured, test.hostname, got, test.want)
		}
	}
}
