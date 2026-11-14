package sharemount

import (
	"errors"
	"testing"
)

func TestSharedStorageMarkerRoundTrip(t *testing.T) {
	marker := sharedStorageMarkerOptions("data,set", 7)
	if marker != "sshmgr-share=data%2Cset;sshmgr-version=7" {
		t.Fatalf("共享存储标记 = %q", marker)
	}

	parsed, err := parseSharedStorageMarkerOptions(marker)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Share != "data,set" || parsed.Version != 7 {
		t.Fatalf("解析后的共享存储标记 = %+v", parsed)
	}
}

func TestLinuxSharedStorageFstabMarkerUsesUserspaceOptions(t *testing.T) {
	marker := linuxSharedStorageFstabMarkerOptions("data,set", 7)
	if marker != "x-sshmgr-share=data%2Cset,x-sshmgr-version=7" {
		t.Fatalf("Linux fstab 标记 = %q", marker)
	}
}

func TestSharedStorageMarkerRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name   string
		marker string
	}{
		{name: "缺少共享名", marker: "sshmgr-version=7"},
		{name: "缺少版本号", marker: "sshmgr-share=datasets"},
		{name: "版本号不是整数", marker: "sshmgr-share=datasets;sshmgr-version=seven"},
		{name: "共享名不是百分号编码", marker: "sshmgr-share=data,set;sshmgr-version=7"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := parseSharedStorageMarkerOptions(testCase.marker)
			if err == nil {
				t.Fatalf("应拒绝 %q", testCase.marker)
			}
			if !errors.Is(err, ErrInvalidSharedStorageMarker) {
				t.Fatalf("错误类型 = %v", err)
			}
		})
	}
}
