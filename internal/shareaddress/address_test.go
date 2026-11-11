package shareaddress

import (
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// TestParseAcceptsWindowsAndURLWritings 覆盖共享地址的两种人常用写法、
// 共享名与主机名大小写等价，以及正反斜杠分隔符等价。
func TestParseAcceptsWindowsAndURLWritings(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  Address
	}{
		{
			name:  "反斜杠写法",
			input: `\\NAS.example\Datasets\Reports\2026.bin`,
			want:  Address{Host: "nas.example", ShareName: "datasets", SubPath: "Reports/2026.bin"},
		},
		{
			name:  "统一资源地址写法",
			input: "smb://nas.example/Datasets/Reports/2026.bin",
			want:  Address{Host: "nas.example", ShareName: "datasets", SubPath: "Reports/2026.bin"},
		},
		{
			name:  "大小写与分隔符混用",
			input: `\\Nas.Example/DATASETS\Reports/2026.bin`,
			want:  Address{Host: "nas.example", ShareName: "datasets", SubPath: "Reports/2026.bin"},
		},
		{
			name:  "共享根目录",
			input: `\\Nas.Example\Datasets\`,
			want:  Address{Host: "nas.example", ShareName: "datasets"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := Parse(test.input)
			if err != nil {
				t.Fatalf("解析共享地址失败：%v", err)
			}
			if got != test.want {
				t.Fatalf("解析结果 = %+v，期待 %+v", got, test.want)
			}
		})
	}
}

// TestParseRejectsRelativePathSegments 保证共享地址始终指称共享存储内部
// 的路径，不能借「当前目录」或「父目录」走出共享根。
func TestParseRejectsRelativePathSegments(t *testing.T) {
	for _, input := range []string{
		`\\nas.example\datasets\..\secret.txt`,
		`\\nas.example\datasets\.\secret.txt`,
		"smb://nas.example/datasets/reports/../../secret.txt",
	} {
		if _, err := Parse(input); err == nil ||
			!strings.Contains(err.Error(), "共享地址的子路径不能包含「.」或「..」") {
			t.Fatalf("共享地址 %q 的错误 = %v", input, err)
		}
	}
}

// TestResolveMatchesProviderAddressAndReportsAmbiguity 覆盖共享地址按提供
// 方设备的任一网络成员地址识别、无匹配与多候选拒绝。
func TestResolveMatchesProviderAddressAndReportsAmbiguity(t *testing.T) {
	shares := []*models.SharedStorage{
		{
			Name: "Datasets", DeviceName: "NAS一楼", CandidateAddresses: []string{"192.0.2.10", "100.64.0.10", "198.51.100.10"},
		},
		{
			Name: "Datasets", DeviceName: "NAS二楼", CandidateAddresses: []string{"198.51.100.10"},
		},
		{Name: "Backups", DeviceName: "备份设备", CandidateAddresses: []string{"198.51.100.20"}},
	}

	resolved, err := Resolve(`\\100.64.0.10\DATASETS\Reports`, shares)
	if err != nil {
		t.Fatalf("按提供方设备网络成员地址识别失败：%v", err)
	}
	if resolved.Share.DeviceName != "NAS一楼" || resolved.Address.SubPath != "Reports" {
		t.Fatalf("识别结果 = %+v", resolved)
	}

	if _, err := Resolve(`\\192.0.2.99\Datasets\Reports`, shares); err == nil ||
		!strings.Contains(err.Error(), "没有匹配当前会话可见的共享存储") {
		t.Fatalf("无匹配错误 = %v", err)
	}

	_, err = Resolve(`\\198.51.100.10\datasets\Reports`, shares)
	if err == nil || !strings.Contains(err.Error(), "NAS一楼：Datasets") ||
		!strings.Contains(err.Error(), "NAS二楼：Datasets") {
		t.Fatalf("多候选错误应列出提供方设备名与共享名：%v", err)
	}
}

// TestResolvedPathOnServerCoversMountAgreements 覆盖 Linux、macOS 与
// Windows 的挂载点约定翻译。
func TestResolvedPathOnServerCoversMountAgreements(t *testing.T) {
	resolved := &Resolved{
		Address: Address{SubPath: "Reports/2026.bin"},
		Share:   &models.SharedStorage{Name: "Datasets", Drive: "Z"},
	}
	tests := []struct {
		name      string
		shellType models.ShellType
		want      string
	}{
		{name: "Linux", shellType: models.ShellPOSIX, want: "/mnt/sshmgr/Datasets/Reports/2026.bin"},
		{name: "macOS", shellType: models.ShellPOSIX, want: "/mnt/sshmgr/Datasets/Reports/2026.bin"},
		{name: "Windows PowerShell", shellType: models.ShellPowerShell, want: `Z:\Reports\2026.bin`},
		{name: "Windows cmd", shellType: models.ShellWindowsCmd, want: `Z:\Reports\2026.bin`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := resolved.PathOnServer(test.shellType)
			if got != test.want {
				t.Fatalf("实际路径 = %q，期待 %q", got, test.want)
			}
		})
	}
}
