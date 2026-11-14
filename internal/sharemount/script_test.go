package sharemount

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

func TestLinuxFstabParserAcceptsCurrentAndLegacyMarkers(t *testing.T) {
	shell := posixShellForTest(t)
	script := `set -eu
` + linuxFstabOptionParserScript() + `
sshmgr_parse_options "$1"
printf 'share=%s version=%s protocol=%s\n' "$sshmgr_option_share" "$sshmgr_option_version" "$sshmgr_option_vers"
`
	for name, options := range map[string]string{
		"当前格式": "credentials=/etc/sshmgr/shares/share.credentials,vers=3.1.1,x-sshmgr-share=datasets,x-sshmgr-version=7,_netdev",
		"旧格式":  "credentials=/etc/sshmgr/shares/share.credentials,vers=2.0,sshmgr-share=datasets,sshmgr-version=6,_netdev",
	} {
		t.Run(name, func(t *testing.T) {
			output, err := exec.Command(shell, "-c", script, "fstab-scan", options).CombinedOutput()
			if err != nil {
				t.Fatalf("解析失败：%v\n%s\n脚本：\n%s", err, output, script)
			}
			want := "share=datasets version="
			if name == "当前格式" {
				want += "7"
			} else {
				want += "6"
			}
			if !strings.Contains(string(output), want) {
				t.Fatalf("解析结果 = %q，应包含 %q", string(output), want)
			}
		})
	}
}

func TestLinuxFixedScriptsScanSampleFstabUnderPOSIXShell(t *testing.T) {
	shell := posixShellForTest(t)
	directory := t.TempDir()
	fstabPath := filepath.Join(directory, "fstab")
	procMountsPath := filepath.Join(directory, "proc-mounts")
	legacyLine := "//192.0.2.10/datasets /mnt/sshmgr/datasets cifs credentials=/etc/sshmgr/shares/share.credentials,vers=3.1.1,sshmgr-share=datasets,sshmgr-version=6,_netdev,nofail,x-systemd.automount,x-systemd.mount-timeout=10 0 0"
	currentLine := "//192.0.2.10/archive /mnt/sshmgr/archive cifs credentials=/etc/sshmgr/shares/archive.credentials,vers=3.0,x-sshmgr-share=archive,x-sshmgr-version=7,_netdev,nofail,x-systemd.automount,x-systemd.mount-timeout=10 0 0"
	ordinaryCIFSLine := "//owner.example/other /mnt/other cifs credentials=/etc/owner.credentials,vers=3.1.1 0 0"
	nonCIFSLine := "UUID=11111111-2222-3333-4444-555555555555 /backup ext4 defaults 0 2"
	sample := strings.Join([]string{legacyLine, currentLine, ordinaryCIFSLine, nonCIFSLine, ""}, "\n")
	if err := os.WriteFile(fstabPath, []byte(sample), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(procMountsPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("现场检测", func(t *testing.T) {
		command := buildLinuxShareCheckCommand([]*models.SharedStorage{{Name: "datasets"}})
		command = strings.ReplaceAll(command, "/etc/fstab", fstabPath)
		command = strings.ReplaceAll(command, "/proc/mounts", procMountsPath)
		output, err := exec.Command(shell, "-c", command).CombinedOutput()
		if err != nil {
			t.Fatalf("固定检测脚本执行失败：%v\n%s\n脚本：\n%s", err, output, command)
		}
		if !strings.Contains(string(output), "managed=yes share=datasets version=6") {
			t.Fatalf("固定检测脚本没有识别 sshmgr 管理行：%q\n脚本：\n%s", string(output), command)
		}
	})

	t.Run("只读枚举", func(t *testing.T) {
		command := buildLinuxEnumerateCommand()
		command = strings.ReplaceAll(command, "/etc/fstab", fstabPath)
		command = strings.ReplaceAll(command, "/proc/mounts", procMountsPath)
		command = strings.ReplaceAll(command, "/etc/sshmgr/shares/*.revoked", filepath.Join(directory, "*.revoked"))
		output, err := exec.Command(shell, "-c", command).CombinedOutput()
		if err != nil {
			t.Fatalf("固定枚举脚本执行失败：%v\n%s\n脚本：\n%s", err, output, command)
		}
		for _, want := range []string{
			"SSHMGR-LINUX-ENUM entry=datasets version=6",
			"SSHMGR-LINUX-ENUM entry=archive version=7",
		} {
			if !strings.Contains(string(output), want) {
				t.Fatalf("固定枚举脚本输出缺少 %q：%q\n脚本：\n%s", want, string(output), command)
			}
		}
		if strings.Contains(string(output), "entry=other") {
			t.Fatalf("固定枚举脚本把 owner 手写 CIFS 行当作 sshmgr 条目：%q", string(output))
		}
	})
}

func TestLinuxMountAndUnmountScanLegacyManagedFstab(t *testing.T) {
	shell := posixShellForTest(t)
	share := &models.SharedStorage{ID: "share-001", Name: "datasets", Version: 7}
	legacyLine := "//192.0.2.10/datasets /mnt/sshmgr/datasets cifs credentials=/etc/sshmgr/shares/share-001.credentials,vers=3.1.1,sshmgr-share=datasets,sshmgr-version=6,_netdev,nofail,x-systemd.automount,x-systemd.mount-timeout=10 0 0"
	fstabPath := filepath.Join(t.TempDir(), "fstab")
	if err := os.WriteFile(fstabPath, []byte(legacyLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name          string
		command       string
		checksContent bool
	}{
		{
			name:    "固定挂载命令",
			command: buildLinuxMountCommand(share, []*Candidate{{Address: "192.0.2.10"}}),
		},
		{
			name:          "固定卸载命令",
			command:       buildLinuxUnmountCommand(UnmountRequest{ShareName: "datasets", ShareID: "share-001", Version: 7}),
			checksContent: true,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			scan := fixedLinuxFstabScanForTest(t, testCase.command)
			scan = strings.ReplaceAll(scan, "/etc/fstab", fstabPath)
			script := `set -eu
mount_point='/mnt/sshmgr/datasets'
sshmgr_expected_share='datasets'
sshmgr_expected_version='7'
sshmgr_exit_invalid_fstab='89'
sshmgr_expected_credential='/etc/sshmgr/shares/share-001.credentials'
sshmgr_credential_file=''
` + scan + `
printf 'managed=%s remote_version=%s credential=%s\n' "$sshmgr_managed_count" "$sshmgr_remote_version" "$sshmgr_credential_file"
`
			output, err := exec.Command(shell, "-c", script).CombinedOutput()
			if err != nil {
				t.Fatalf("旧格式识别失败：%v\n%s\n脚本：\n%s", err, output, script)
			}
			if !strings.Contains(string(output), "managed=1 remote_version=6") {
				t.Fatalf("固定命令没有识别旧格式 fstab 标记：%q\n脚本：\n%s", string(output), script)
			}
			if testCase.checksContent &&
				!strings.Contains(string(output), "credential=/etc/sshmgr/shares/share-001.credentials") {
				t.Fatalf("固定卸载命令没有识别旧格式凭据路径：%q", string(output))
			}
		})
	}
}

func fixedLinuxFstabScanForTest(t *testing.T, command string) string {
	t.Helper()
	parser := linuxFstabOptionParserScript()
	start := strings.Index(command, parser)
	if start < 0 {
		t.Fatalf("固定命令缺少 fstab 解析函数：\n%s", command)
	}
	endMarker := "done < /etc/fstab\n"
	relativeEnd := strings.Index(command[start:], endMarker)
	if relativeEnd < 0 {
		t.Fatalf("固定命令缺少 fstab 扫描循环：\n%s", command)
	}
	return command[start : start+relativeEnd+len(endMarker)]
}

func TestDarwinMountTableScriptsAcceptFirmlinkPaths(t *testing.T) {
	shell := posixShellForTest(t)
	directory := t.TempDir()
	mapPath := filepath.Join(directory, "auto_share")
	if err := os.WriteFile(mapPath, []byte(
		"# sshmgr-share=datasets;sshmgr-version=7\ndatasets -fstype=smbfs ://mount-user:password@192.0.2.10/datasets\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	mountFunction := `mount() {
  printf 'smbfs@//192.0.2.10/datasets on /System/Volumes/Data/mnt/sshmgr/datasets (smbfs, nodev, nosuid, read-only, noowners, mounted by allan)\n'
}
`

	t.Run("挂载表核验片段", func(t *testing.T) {
		command := `set -eu
mount_point='/mnt/sshmgr/datasets'
` + mountFunction + mountTableCheckScript(CheckSystemDarwin) + `printf 'mounted=%s\n' "$sshmgr_mounted"
`
		output, err := exec.Command(shell, "-c", command).CombinedOutput()
		if err != nil {
			t.Fatalf("核验脚本执行失败：%v\n%s\n脚本：\n%s", err, output, command)
		}
		if !strings.Contains(string(output), "mounted=yes") {
			t.Fatalf("firmlink 挂载点应判定为已挂载：%q\n脚本：\n%s", string(output), command)
		}
	})

	t.Run("只读枚举脚本", func(t *testing.T) {
		command := buildDarwinEnumerateCommand()
		command = strings.ReplaceAll(command, "/etc/sshmgr/auto_share", mapPath)
		command = strings.ReplaceAll(command, "/etc/sshmgr/*.revoked", filepath.Join(directory, "*.revoked"))
		command = mountFunction + command
		output, err := exec.Command(shell, "-c", command).CombinedOutput()
		if err != nil {
			t.Fatalf("固定枚举脚本执行失败：%v\n%s\n脚本：\n%s", err, output, command)
		}
		if !strings.Contains(string(output), "SSHMGR-DARWIN-ENUM mount=datasets") {
			t.Fatalf("固定枚举脚本应识别 firmlink 挂载点：%q\n脚本：\n%s", string(output), command)
		}
	})

	t.Run("现场检测脚本", func(t *testing.T) {
		autoMasterPath := filepath.Join(directory, "auto_master")
		if err := os.WriteFile(autoMasterPath, []byte("# Automounter master map\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		command := buildDarwinShareCheckCommand([]*models.SharedStorage{{Name: "datasets"}})
		command = strings.ReplaceAll(command, "/etc/sshmgr/auto_share", mapPath)
		command = strings.ReplaceAll(command, "/etc/auto_master", autoMasterPath)
		command = mountFunction + command
		output, err := exec.Command(shell, "-c", command).CombinedOutput()
		if err != nil {
			t.Fatalf("固定检测脚本执行失败：%v\n%s\n脚本：\n%s", err, output, command)
		}
		if !strings.Contains(string(output), "managed=yes share=datasets version=7 mounted=yes") {
			t.Fatalf("固定检测脚本应识别 firmlink 挂载点：%q\n脚本：\n%s", string(output), command)
		}
	})
}

func TestRevokedMarkerGuardScriptParsesVersionsUnderPOSIXShell(t *testing.T) {
	shell := posixShellForTest(t)
	directory := t.TempDir()
	markerPath := filepath.Join(directory, "datasets.revoked")
	otherShareMarkerPath := filepath.Join(directory, "archive.revoked")

	tests := []struct {
		name          string
		marker        string
		wantExitCode  int
		wantStderr    string
		wantVersion   string
		keepOtherFile bool
	}{
		{
			name:        "旧版本撤销标记可以越过",
			marker:      "sshmgr-share=datasets;sshmgr-version=1;sshmgr-state=revoked\n",
			wantVersion: "1",
		},
		{
			name:         "相同版本撤销标记拒绝挂载",
			marker:       "sshmgr-share=datasets;sshmgr-version=2;sshmgr-state=revoked\n",
			wantExitCode: 90,
			wantStderr:   "share-revoked",
		},
		{
			name:         "更高版本撤销标记拒绝挂载",
			marker:       "sshmgr-share=datasets;sshmgr-version=3;sshmgr-state=revoked\n",
			wantExitCode: 90,
			wantStderr:   "share-revoked",
		},
		{
			name:          "其他共享名的撤销标记不受影响",
			marker:        "",
			wantVersion:   "0",
			keepOtherFile: true,
		},
	}

	for _, system := range []struct {
		name   string
		marker string
	}{
		{name: "Linux", marker: linuxMountMarker},
		{name: "macOS", marker: darwinMountMarker},
	} {
		for _, testCase := range tests {
			t.Run(system.name+" / "+testCase.name, func(t *testing.T) {
				if err := os.Remove(markerPath); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
				if testCase.marker != "" {
					if err := os.WriteFile(markerPath, []byte(testCase.marker), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				otherMarker := "sshmgr-share=archive;sshmgr-version=9;sshmgr-state=revoked\n"
				if err := os.Remove(otherShareMarkerPath); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
				if err := os.WriteFile(otherShareMarkerPath, []byte(otherMarker), 0o600); err != nil {
					t.Fatal(err)
				}
				guard := revokedMarkerGuardScript(
					markerPath, system.marker, 90, 89,
				)
				script := `set -eu
sshmgr_expected_share=datasets
sshmgr_expected_version=2
` + guard + `
rm -f "$sshmgr_revoked_marker"
printf 'version=%s\n' "$sshmgr_revoked_version"
`
				output, err := exec.Command(shell, "-c", script).CombinedOutput()
				if testCase.wantExitCode == 0 {
					if err != nil {
						t.Fatalf("撤销标记检查片段执行失败：%v\n%s\n脚本：\n%s", err, output, script)
					}
					if !strings.Contains(string(output), "version="+testCase.wantVersion) {
						t.Fatalf("撤销标记版本号解析结果 = %q，期待 %s；脚本：\n%s",
							string(output), testCase.wantVersion, script)
					}
					if _, statErr := os.Stat(markerPath); !os.IsNotExist(statErr) {
						t.Fatalf("可越过的旧撤销标记应被清理：%v", statErr)
					}
				} else {
					exitErr, ok := err.(*exec.ExitError)
					if !ok || exitErr.ExitCode() != testCase.wantExitCode {
						t.Fatalf("撤销标记检查片段退出结果 = %v，期待退出码 %d；输出：\n%s\n脚本：\n%s",
							err, testCase.wantExitCode, output, script)
					}
					if !strings.Contains(string(output), testCase.wantStderr) {
						t.Fatalf("撤销拒绝输出 = %q，应包含 %q", string(output), testCase.wantStderr)
					}
				}
				if testCase.keepOtherFile {
					if _, statErr := os.Stat(otherShareMarkerPath); statErr != nil {
						t.Fatalf("其他共享名的撤销标记不应被清理：%v", statErr)
					}
				} else if _, statErr := os.Stat(otherShareMarkerPath); statErr != nil {
					t.Fatalf("核对其他共享名撤销标记失败：%v", statErr)
				}
			})
		}
	}
}

func TestDarwinAutomountReloadAllowsNonzeroExitWhenMountSucceeds(t *testing.T) {
	shell := posixShellForTest(t)
	command := buildDarwinMountCommand(
		&models.SharedStorage{Name: "datasets", MountUser: "mount-user", Version: 2},
		[]*Candidate{{Address: "192.0.2.10"}},
	)
	reloadStart := strings.Index(command, "\n# macOS 的 automount 刷新可能在做完挂载后仍返回 1；最终结论以挂载表验证为准。\nautomount -cv >/dev/null 2>&1 || true\n")
	if reloadStart < 0 {
		t.Fatalf("macOS 固定挂载命令缺少自动挂载刷新步骤：%s", command)
	}
	resultStart := strings.Index(command[reloadStart:], "\nif [ \"$sshmgr_managed_count\"")
	if resultStart < 0 {
		t.Fatalf("macOS 固定挂载命令缺少刷新后的结果判定：%s", command)
	}
	fragment := command[reloadStart : reloadStart+resultStart]
	script := `set -eu
mount_point='/mnt/sshmgr/datasets'
sshmgr_managed_count=1
sshmgr_existing_configuration=no
automount() {
  return 1
}
mount() {
  printf 'smbfs@//192.0.2.10/datasets on /System/Volumes/Data/mnt/sshmgr/datasets (smbfs, automounted)\n'
}
` + fragment + `
echo 'SSHMGR-DARWIN-MOUNT reload-accepted'
`
	output, err := exec.Command(shell, "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("macOS 自动挂载刷新返回非零但真实挂载成功时不应失败：%v\n%s\n脚本：\n%s",
			err, output, script)
	}
	if !strings.Contains(string(output), "reload-accepted") {
		t.Fatalf("macOS 刷新片段输出 = %q，未到达最终判定", string(output))
	}
}

func posixShellForTest(t *testing.T) string {
	t.Helper()
	if path, err := exec.LookPath("dash"); err == nil {
		return path
	}
	if path, err := exec.LookPath("sh"); err == nil {
		return path
	}
	t.Skip("本机没有 POSIX shell 解释器")
	return ""
}
