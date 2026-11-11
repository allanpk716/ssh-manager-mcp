package clientfeatures

import "testing"

func TestClientVersionLabel(t *testing.T) {
	injected := []Feature{
		{Name: "机密项", SinceVersion: "v2.0.0"},
		{Name: "网络拓扑", SinceVersion: "v3.0.0"},
		{Name: "共享存储", SinceVersion: "v4.0.0"},
	}
	tests := []struct {
		name          string
		clientVersion string
		features      []Feature
		want          string
	}{
		{
			name:          "未声明版本列出全部功能",
			clientVersion: "",
			features:      injected,
			want:          "旧版客户端:不支持机密项、网络拓扑、共享存储",
		},
		{
			name:          "低于全部起始版本",
			clientVersion: "1.9.0",
			features:      injected,
			want:          "1.9.0（旧版客户端:不支持机密项、网络拓扑、共享存储）",
		},
		{
			name:          "介于两个起始版本之间",
			clientVersion: "v2.5.0",
			features:      injected,
			want:          "v2.5.0（旧版客户端:不支持网络拓扑、共享存储）",
		},
		{
			name:          "等于最后一个起始版本",
			clientVersion: "4.0.0",
			features:      injected,
			want:          "4.0.0",
		},
		{
			name:          "高于全部起始版本",
			clientVersion: "v5.1.2",
			features:      injected,
			want:          "v5.1.2",
		},
		{
			name:          "开发构建不判为旧版",
			clientVersion: "dev",
			features:      injected,
			want:          "dev",
		},
		{
			name:          "预发布版本按语义版本比较",
			clientVersion: "v2.0.0-rc.1",
			features:      injected,
			want:          "v2.0.0-rc.1（旧版客户端:不支持机密项、网络拓扑、共享存储）",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ClientVersionLabelWithFeatures(test.clientVersion, test.features)
			if got != test.want {
				t.Fatalf("展示文字 = %q，期待 %q", got, test.want)
			}
		})
	}
}

func TestDefaultFeaturesAreCentralOrderedConfiguration(t *testing.T) {
	want := []Feature{
		{Name: "机密项", SinceVersion: "v0.22.0"},
		{Name: "网络拓扑", SinceVersion: "v0.22.0"},
		{Name: "共享存储", SinceVersion: "v0.22.0"},
	}
	if len(DefaultFeatures) != len(want) {
		t.Fatalf("默认功能表 = %#v，期待 %#v", DefaultFeatures, want)
	}
	for i := range want {
		if DefaultFeatures[i] != want[i] {
			t.Fatalf("默认功能表第 %d 项 = %#v，期待 %#v", i, DefaultFeatures[i], want[i])
		}
	}
	if got := ClientVersionLabel(""); got != "旧版客户端:不支持机密项、网络拓扑、共享存储" {
		t.Fatalf("默认表展示文字 = %q", got)
	}
}
