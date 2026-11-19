package webui

import "testing"

func TestSafeUIReturnPathRejectsExternalAndEscapingValues(t *testing.T) {
	tests := []struct {
		name string
		path string
		want bool
	}{
		{name: "服务器详情", path: "/ui/servers/server-1?tab=software", want: true},
		{name: "授权详情", path: "/ui/authorization/profiles/profile-1", want: true},
		{name: "双斜杠", path: "/ui//example.com", want: false},
		{name: "反斜杠", path: `/ui/servers/\\example.com`, want: false},
		{name: "网页协议", path: "https://example.com/ui/servers/", want: false},
		{name: "脚本协议", path: "javascript://example.com", want: false},
		{name: "站外路径", path: "/external", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := safeReturnPath(test.path, "fallback", "/ui/")
			if test.want && got != test.path {
				t.Fatalf("safeReturnPath(%q) = %q，期望原路径", test.path, got)
			}
			if !test.want && got != "fallback" {
				t.Fatalf("safeReturnPath(%q) = %q，期望回退路径", test.path, got)
			}
		})
	}
}
