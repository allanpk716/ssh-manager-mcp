package localaddr

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestLocalAndForeignTargets(t *testing.T) {
	for _, target := range []string{"https://127.0.0.1:7878", "http://[::1]:7878"} {
		if !ForeignTarget(target) {
			t.Fatalf("环回地址 %s 不应属于默认本机地址集合", target)
		}
	}
	if ips := NonLoopbackIPs(); len(ips) > 0 {
		if ForeignTarget(fmt.Sprintf("https://%s:7878", ips[0])) {
			t.Fatalf("本机非环回地址 %s 不应被判定为异常", ips[0])
		}
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		if ForeignTarget("https://"+host) || ForeignTarget(fmt.Sprintf("https://%s:7878", host)) {
			t.Fatalf("本机主机名 %s 不应被判定为异常", host)
		}
	}
	for _, target := range []string{"", "not a url", "https://[bad", "https://203.0.113.9:7878"} {
		if !ForeignTarget(target) {
			t.Fatalf("%q 应被判定为异常", target)
		}
	}
}

func TestRegisteredAddressesMatchHostPortAndIPv6(t *testing.T) {
	addresses, err := ParseRegisteredAddresses("Example.COM, 203.0.113.9:18178, [2001:db8::1], [2001:db8::2]:18178")
	if err != nil {
		t.Fatalf("合法登记地址应解析成功：%v", err)
	}
	if len(addresses) != 4 {
		t.Fatalf("登记地址数量=%d，应为 4：%+v", len(addresses), addresses)
	}

	cases := []struct {
		name      string
		targetURL string
		foreign   bool
	}{
		{name: "主机名大小写不敏感且任意端口", targetURL: "https://example.com:1", foreign: false},
		{name: "只写主机时目标不带端口也算", targetURL: "https://example.com", foreign: false},
		{name: "主机与端口都写时端口必须相同", targetURL: "https://203.0.113.9:18179", foreign: true},
		{name: "主机与端口完全一致", targetURL: "https://203.0.113.9:18178", foreign: false},
		{name: "IPv6 带方括号不带端口", targetURL: "https://[2001:db8:0:0::1]/ui", foreign: false},
		{name: "IPv6 带方括号带端口", targetURL: "https://[2001:0db8::2]:18178", foreign: false},
		{name: "IPv6 端口不同", targetURL: "https://[2001:db8::2]:18179", foreign: true},
		{name: "未登记 IPv6", targetURL: "https://[2001:db8::9]", foreign: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := ForeignTarget(testCase.targetURL, addresses...)
			if got != testCase.foreign {
				t.Fatalf("ForeignTarget(%q)=%v，应为 %v", testCase.targetURL, got, testCase.foreign)
			}
		})
	}
}

func TestParseRegisteredAddressesRejectsInvalidEntries(t *testing.T) {
	for _, value := range []string{
		"203.0.113.9:0", "203.0.113.9:65536", "203.0.113.9:notaport",
		"[2001:db8::1", "2001:db8::1]:18178", "https://example.com", "example.com/path",
		"example.com,,203.0.113.9", "example.com:18178:18179", " ",
		"127.0.0.1", "[::1]:7878", "0.0.0.0", "localhost:18178",
	} {
		if _, err := ParseRegisteredAddresses(value); err == nil || !strings.Contains(err.Error(), "serve.public_addresses") {
			t.Fatalf("非法登记地址 %q 应返回中文错误，实际 %v", value, err)
		}
	}
}
