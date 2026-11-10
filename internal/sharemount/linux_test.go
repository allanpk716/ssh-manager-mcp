package sharemount

import (
	"os"
	"os/exec"
	"runtime"
	"testing"

	"ssh-manager-mcp/internal/models"
)

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
