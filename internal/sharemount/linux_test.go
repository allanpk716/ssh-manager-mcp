package sharemount

import (
	"os"
	"os/exec"
	"runtime"
	"testing"

	"ssh-manager-mcp/internal/models"
)

func TestLinuxFstabLineContainsMachineReadableMarker(t *testing.T) {
	share := &models.SharedStorage{ID: "share-001", Name: "datasets"}
	line := buildLinuxFstabLine(share, "192.0.2.10", "2.1", sharedStorageMarkerOptions("datasets", 7))

	want := "//192.0.2.10/datasets /mnt/sshmgr/datasets cifs " +
		"credentials=/etc/sshmgr/shares/share-001.credentials,vers=2.1," +
		"sshmgr-share=datasets,sshmgr-version=7," +
		"_netdev,nofail,x-systemd.automount,x-systemd.mount-timeout=10 0 0"
	if line != want {
		t.Fatalf("fstab 配置行 =\n%s\n期待\n%s", line, want)
	}

	entry, err := parseLinuxFstabLine(line)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Address != "192.0.2.10" || entry.ProtocolVersion != "2.1" ||
		entry.Marker.Share != "datasets" || entry.Marker.Version != 7 {
		t.Fatalf("解析后的 fstab 配置 = %+v", entry)
	}
}

func TestLinuxMountCommandPassesPOSIXShellSyntaxCheck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 没有 POSIX shell 解释器")
	}
	command := buildLinuxMountCommand(&models.SharedStorage{ID: "share-001", Name: "datasets"}, []*Candidate{
		{Address: "192.0.2.10"}, {Address: "100.64.10.10"},
	})
	script := t.TempDir() + "/mount-command.sh"
	if err := os.WriteFile(script, []byte(command), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("/bin/sh", "-n", script).CombinedOutput(); err != nil {
		t.Fatalf("Linux 固定命令不是合法 POSIX shell 脚本：%v\n%s\n%s", err, output, command)
	}
}

func TestSafeMountShareNameRejectsConfigurationFieldBreakers(t *testing.T) {
	for _, name := range []string{"", ".", "..", "data set", "data\tset", "data/set", `data\set`, "data\x00set"} {
		if safeMountShareName(name) {
			t.Fatalf("共享名 %q 不能直接作为 fstab 字段使用", name)
		}
	}
	if !safeMountShareName("datasets") {
		t.Fatal("普通共享名应可用")
	}
}
