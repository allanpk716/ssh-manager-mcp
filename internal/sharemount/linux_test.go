package sharemount

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// exitLiteral 匹配脚本源码里写死的非零退出码；退出码必须来自 Go 常量。
var exitLiteral = regexp.MustCompile(`exit\s+[1-9]`)

func TestLinuxMountCommandPassesPOSIXShellSyntaxCheck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 没有 POSIX shell 解释器")
	}
	command := buildLinuxMountCommand(&models.Server{User: "deploy"}, &models.SharedStorage{ID: "share-001", Name: "datasets"}, []*Candidate{
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

func TestLinuxMountCommandWritesUserspaceMarkerOptions(t *testing.T) {
	command := buildLinuxMountCommand(&models.Server{User: "deploy"}, &models.SharedStorage{
		ID: "share-001", Name: "data,set", Version: 7,
	}, []*Candidate{{Address: "192.0.2.10"}})

	if !strings.Contains(command, "x-sshmgr-share=data%2Cset,x-sshmgr-version=7") {
		t.Fatalf("Linux 固定命令应写入 x- 前缀管理标记：\n%s", command)
	}
	if strings.Contains(command, ",sshmgr-share=") || strings.Contains(command, ",sshmgr-version=") {
		t.Fatalf("Linux 固定命令不能写入会被传给 CIFS 内核的旧管理标记：\n%s", command)
	}
}

func TestLinuxMountCommandMapsFilesToSSHLoginUser(t *testing.T) {
	server := &models.Server{User: "deploy"}
	share := &models.SharedStorage{
		ID: "share-001", Name: "datasets", Version: 7, MountUser: "mount-user",
	}
	command := buildLinuxMountCommand(server, share, []*Candidate{{Address: "192.0.2.10"}})
	for _, needed := range []string{
		"sshmgr_login_user='deploy'",
		`id -u -- "$sshmgr_login_user"`,
		`id -g -- "$sshmgr_login_user"`,
		`credentials=%s,vers=%s,%s,%s,_netdev,nofail,x-systemd.automount,x-systemd.mount-timeout=10 0 0`,
		`"$sshmgr_owner_options" "$sshmgr_fstab_marker"`,
		`"vers=$sshmgr_selected_version,credentials=$sshmgr_credential_candidate,$sshmgr_owner_options"`,
		`"vers=$sshmgr_version,credentials=$sshmgr_credential_candidate,$sshmgr_owner_options"`,
	} {
		if !strings.Contains(command, needed) {
			t.Fatalf("Linux 固定挂载命令缺少 %q：%s", needed, command)
		}
	}
}

func TestLinuxMountCommandFailsWhenSSHLoginUserIsMissing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 没有 POSIX shell 解释器")
	}
	command := buildLinuxMountCommand(
		&models.Server{User: "missing-user"},
		&models.SharedStorage{ID: "share-001", Name: "datasets", Version: 7},
		[]*Candidate{{Address: "192.0.2.10"}},
	)
	start := strings.Index(command, "\nsshmgr_login_user=")
	end := strings.Index(command[start:], "\nsshmgr_is_mounted() {")
	if start < 0 || end < 0 {
		t.Fatalf("Linux 固定挂载命令缺少登录用户解析片段：%s", command)
	}
	binaryDirectory := t.TempDir()
	if err := os.WriteFile(filepath.Join(binaryDirectory, "id"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := `set -eu
PATH='` + binaryDirectory + `'
sshmgr_exit_login_user_missing=` + strconv.Itoa(linuxExitLoginUserMissing) + "\n" +
		command[start:start+end] + "\n"
	output, err := exec.Command(posixShellForTest(t), "-c", script).CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != linuxExitLoginUserMissing {
		t.Fatalf("登录用户不存在时退出结果 = %v，期待 %d；输出：\n%s\n脚本：\n%s",
			err, linuxExitLoginUserMissing, output, script)
	}
	if !strings.Contains(string(output), linuxMountMarker+" login-user-missing") {
		t.Fatalf("登录用户不存在时的失败输出不明确：%q", output)
	}
}

func TestLinuxMountOutputOwnershipMappingNotice(t *testing.T) {
	action, selectedIndex, _, ownershipAdded, err := parseMountCommandOutput(
		linuxMountMarker+" result=update-success selected=2 ownership-mapping=added\n",
		linuxMountMarker, "Linux",
	)
	if err != nil || action != "update-success" || selectedIndex != 2 || !ownershipAdded {
		t.Fatalf("属主映射输出解析结果 = action %q selected %d added %v err %v",
			action, selectedIndex, ownershipAdded, err)
	}
}

func TestLinuxMountSupplementsOwnerMappingWithoutChangingWorkingAddress(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 没有 POSIX shell 解释器")
	}
	server := &models.Server{User: "deploy"}
	share := &models.SharedStorage{
		ID: "share-001", Name: "datasets", Version: 7, MountUser: "mount-user",
	}
	candidates := []*Candidate{{Address: "192.0.2.10"}, {Address: "100.64.10.10"}}
	command := buildLinuxMountCommand(server, share, candidates)
	ownershipStart := strings.Index(command, "\nsshmgr_login_user=")
	ownershipEnd := strings.Index(command[ownershipStart:], "\nsshmgr_is_mounted() {")
	buildStart := strings.Index(command, "\nsshmgr_build_fstab_line() {")
	buildEnd := strings.Index(command[buildStart:], "\nsshmgr_build_fstab_line\n")
	if ownershipStart < 0 || ownershipEnd < 0 || buildStart < 0 || buildEnd < 0 {
		t.Fatalf("Linux 固定挂载命令缺少属主映射或 fstab 生成片段：%s", command)
	}
	binaryDirectory := t.TempDir()
	idScript := "#!/bin/sh\ncase \"$1\" in -u) echo 1005 ;; -g) echo 1006 ;; *) exit 1 ;; esac\n"
	if err := os.WriteFile(filepath.Join(binaryDirectory, "id"), []byte(idScript), 0o755); err != nil {
		t.Fatal(err)
	}
	script := `set -eu
PATH='` + binaryDirectory + `'
sshmgr_exit_login_user_missing=` + strconv.Itoa(linuxExitLoginUserMissing) + `
share_name='datasets'
credential_file='/etc/sshmgr/shares/share-001.credentials'
mount_point='/mnt/sshmgr/datasets'
sshmgr_expected_share='datasets'
sshmgr_managed_count=1
sshmgr_existing_address='100.64.10.10'
sshmgr_existing_version='3.1.1'
sshmgr_existing_uid=''
sshmgr_existing_gid=''
sshmgr_fstab_marker='` + linuxSharedStorageFstabMarkerOptions("datasets", 7) + `'
candidate_1_address='192.0.2.10'
candidate_2_address='100.64.10.10'
` + candidateAddressSelectionScript(candidates, true) + "\n" +
		command[ownershipStart:ownershipStart+ownershipEnd] + "\n" +
		command[buildStart:buildStart+buildEnd+len("\nsshmgr_build_fstab_line\n")] + "\n" +
		`printf '%s\n' "$sshmgr_fstab_line"
printf 'supplement=%s\n' "$sshmgr_ownership_mapping_added"
`
	output, err := exec.Command(posixShellForTest(t), "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("补充属主映射片段执行失败：%v\n%s\n脚本：\n%s", err, output, script)
	}
	if !strings.Contains(string(output), "//100.64.10.10/datasets ") ||
		!strings.Contains(string(output), "uid=1005,gid=1006") ||
		strings.Contains(string(output), "//192.0.2.10/datasets ") {
		t.Fatalf("补充属主映射不应改写仍连通地址：%q\n脚本：\n%s", output, script)
	}
	if !strings.Contains(string(output), "supplement=added") {
		t.Fatalf("补充属主映射时缺少明确结论：%q", output)
	}
}

func TestLinuxMountExitCodesHaveExplanationsAndClasses(t *testing.T) {
	command := buildLinuxMountCommand(&models.Server{User: "deploy"}, &models.SharedStorage{ID: "share-001", Name: "datasets"}, []*Candidate{
		{Address: "192.0.2.10"},
	})
	validClasses := map[string]bool{
		MountClassMissingSoftware: true, MountClassProtocolFailed: true,
		MountClassUnmountFailed: true, MountClassVerifyFailed: true,
		MountClassLockTimeout: true, MountClassRemoteVersionNewer: true,
		MountClassError: true,
	}
	for _, code := range []int{
		linuxExitMissingCIFS, linuxExitProtocolFailed, linuxExitTrialUnmountFailed,
		linuxExitVerifyFailed, linuxExitLockTimeout, linuxExitRemoteNewer, linuxExitInvalidFstab,
		linuxExitLoginUserMissing,
	} {
		if !strings.Contains(command, "exit "+strconv.Itoa(code)) &&
			!strings.Contains(command, "="+strconv.Itoa(code)+"\n") {
			t.Fatalf("Linux 固定命令应使用常量插值退出码 %d：%s", code, command)
		}
		err := linuxMountExitCodeError(code)
		if err == nil {
			t.Fatalf("Linux 退出码 %d 没有中文说明", code)
		}
		var mountFailure *MountFailure
		if !errors.As(err, &mountFailure) || !validClasses[mountFailure.Class] {
			t.Fatalf("Linux 退出码 %d 的错误 = %v，分类不在允许集合内", code, err)
		}
	}
	source, err := os.ReadFile("linux.go")
	if err != nil {
		t.Fatal(err)
	}
	if exitLiteral.Find(source) != nil {
		t.Fatalf("Linux 固定命令源码不能写非零数字退出码字面量：%s", exitLiteral.Find(source))
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
