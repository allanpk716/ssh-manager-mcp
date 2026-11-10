package sharemount

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

func TestDarwinPasswordUsesPercentEncodingForURLUserInfo(t *testing.T) {
	password := []byte(`mount-P@$$ 'quotes' "double" \ end`)
	want := `mount-P%40%24%24%20%27quotes%27%20%22double%22%20%5C%20end`
	if encoded := escapeDarwinPassword(password); encoded != want {
		t.Fatalf("网址编码后的挂载密码 = %q，期待 %q", encoded, want)
	}
}

func TestDarwinAutomountEntryContainsMarkerAndOneShareLine(t *testing.T) {
	share := &models.SharedStorage{Name: "datasets", MountUser: "mount-user", Version: 7}
	marker, entry := buildDarwinAutomountEntry(share, "192.0.2.10", "a%2Bb")
	wantEntry := "datasets -fstype=smbfs ://mount-user:a%2Bb@192.0.2.10/datasets"
	if marker != "# sshmgr-share=datasets;sshmgr-version=7" || entry != wantEntry {
		t.Fatalf("macOS 间接映射 = %q\n%q，期待标记行与 %q", marker, entry, wantEntry)
	}
}

func TestDarwinMountCommandPassesPOSIXShellSyntaxCheck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 没有 POSIX shell 解释器")
	}
	command := buildDarwinMountCommand(
		&models.SharedStorage{Name: "datasets", MountUser: "mount-user"},
		[]*Candidate{{Address: "192.0.2.10"}, {Address: "100.64.10.10"}},
	)
	if strings.Contains(command, "LaunchDaemon") {
		t.Fatalf("macOS 固定命令不能使用 LaunchDaemon：%s", command)
	}
	script := t.TempDir() + "/darwin-mount-command.sh"
	if err := os.WriteFile(script, []byte(command), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("/bin/sh", "-n", script).CombinedOutput(); err != nil {
		t.Fatalf("macOS 固定命令不是合法 POSIX shell 脚本：%v\n%s\n%s", err, output, command)
	}
}
