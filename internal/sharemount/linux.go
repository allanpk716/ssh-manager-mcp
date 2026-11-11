package sharemount

import (
	"errors"
	"net/url"
	"strconv"
	"strings"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sshbroker"
)

const (
	linuxMountMarker        = "SSHMGR-LINUX-MOUNT"
	linuxExitMissingCIFS    = 83
	linuxExitProtocolFailed = 84
	// linuxExitTrialUnmountFailed 表示临时挂载点试挂成功后的卸载失败。
	linuxExitTrialUnmountFailed = 85
	linuxExitVerifyFailed       = 86
	linuxExitLockTimeout        = 87
	linuxExitRemoteNewer        = 88
	linuxExitInvalidFstab       = 89
	// linuxExitShareRevoked 表示目标服务器上的撤销标记仍对本次快照生效。
	linuxExitShareRevoked = 90
	linuxCredentialRoot   = "/etc/sshmgr/shares"
	linuxMountRoot        = "/mnt/sshmgr"
)

// linuxMountExitCodeError 把 Linux 固定挂载命令的已知退出码翻译为中文
// 说明；不在表内的退出码返回 nil，由共用流程给出带退出码的兜底文本。
func linuxMountExitCodeError(exitCode int) error {
	switch exitCode {
	case linuxExitMissingCIFS:
		return mountFailure(MountClassMissingSoftware, errors.New(
			"目标服务器缺少 mount.cifs，请先由 owner 安装 cifs-utils；挂载工具不会自动安装"))
	case linuxExitProtocolFailed:
		return mountFailure(MountClassProtocolFailed, errors.New(
			"SMB 协议版本 3.1.1、3.0、2.1、2.0 全部试挂失败，未写入 fstab 配置"))
	case linuxExitTrialUnmountFailed:
		return mountFailure(MountClassUnmountFailed, errors.New(
			"临时挂载点卸载失败，未改写任何正式挂载配置"))
	case linuxExitVerifyFailed:
		return mountFailure(MountClassVerifyFailed, errors.New(
			"Linux 挂载点验证失败，不能确认共享已可用"))
	case linuxExitLockTimeout:
		return mountFailure(MountClassLockTimeout, errors.New(
			"另一个挂载正在进行，请稍后再试"))
	case linuxExitRemoteNewer:
		return mountFailure(MountClassRemoteVersionNewer, errors.New(
			"目标服务器上的共享存储版本更新，请先同步快照后再调用挂载工具"))
	case linuxExitInvalidFstab:
		return mountFailure(MountClassError, errors.New(
			"目标服务器上的 sshmgr fstab 条目无法解析，请 owner 检查后再试"))
	case linuxExitShareRevoked:
		return mountFailure(MountClassShareRevoked, errors.New(
			"共享存储已从这台服务器撤销，不能重新挂载；如 owner 已重新授权，请先同步快照后再试"))
	default:
		return nil
	}
}

func linuxMountPoint(shareName string) string {
	return linuxMountRoot + "/" + shareName
}

func linuxCredentialPath(shareID string) string {
	return linuxCredentialRoot + "/" + shareID + ".credentials"
}

// linuxTrialMountPoint 是变更路径的临时试挂挂载点，位于 /var/run 下的临
// 时文件系统；共享名已禁止空白与斜杠，可直接拼接。
func linuxTrialMountPoint(shareName string) string {
	return "/var/run/sshmgr-trial-" + shareName
}

// buildLinuxMountCommand 生成 Linux 挂载固定命令。命令会在同一把服务器
// 级文件锁内读取 fstab、比较版本与配置、写入凭据与 fstab，并让挂载生
// 效。地址、共享名与挂载账号全部经单引号包装；挂载密码只由调用方经标准
// 输入传入，绝不出现在返回的命令文本里。
func buildLinuxMountCommand(share *models.SharedStorage, candidates []*Candidate) string {
	shareName := sshbroker.POSIXShellQuote(share.Name)
	mountPoint := sshbroker.POSIXShellQuote(linuxMountPoint(share.Name))
	credential := sshbroker.POSIXShellQuote(linuxCredentialPath(share.ID))
	marker := sshbroker.POSIXShellQuote(sharedStorageMarkerOptions(share.Name, share.Version))
	fstabMarker := strings.ReplaceAll(sharedStorageMarkerOptions(share.Name, share.Version), ";", ",")
	shareValue := sshbroker.POSIXShellQuote(url.PathEscape(share.Name))
	versionValue := sshbroker.POSIXShellQuote(strconv.FormatInt(share.Version, 10))

	command := `set -eu
sshmgr_exit_missing_cifs=` + strconv.Itoa(linuxExitMissingCIFS) + `
sshmgr_exit_invalid_fstab=` + strconv.Itoa(linuxExitInvalidFstab) + `
if ! command -v mount.cifs >/dev/null 2>&1; then
  echo ` + linuxMountMarker + ` missing-mount-cifs >&2
  exit "$sshmgr_exit_missing_cifs"
fi
install -d -m 700 ` + sshbroker.POSIXShellQuote(linuxCredentialRoot) + `
install -d -m 755 ` + sshbroker.POSIXShellQuote(linuxMountRoot) + `
` + sharedStorageDirectoryLockScript(sharedStorageLockPath, 30, linuxExitLockTimeout, linuxMountMarker) + `
sshmgr_share_marker=` + marker + `
sshmgr_fstab_marker=` + sshbroker.POSIXShellQuote(fstabMarker) + `
sshmgr_expected_share=` + shareValue + `
sshmgr_expected_version=` + versionValue + `
share_name=` + shareName + `
credential_file=` + credential + `
mount_point=` + mountPoint + `
candidate_count=` + strconv.Itoa(len(candidates)) + `
` + candidateAddressVariablesScript(candidates) + `
install -d -m 755 "$mount_point"

sshmgr_credential_candidate=''
sshmgr_fstab_candidate=''
sshmgr_trial_point=''
sshmgr_cleanup() {
  if [ -n "$sshmgr_credential_candidate" ]; then rm -f "$sshmgr_credential_candidate"; fi
  if [ -n "$sshmgr_fstab_candidate" ]; then rm -f "$sshmgr_fstab_candidate"; fi
  if [ -n "$sshmgr_trial_point" ]; then rmdir "$sshmgr_trial_point" 2>/dev/null || true; fi
  sshmgr_release_shared_storage_lock
}
trap sshmgr_cleanup EXIT HUP INT TERM

` + linuxFstabOptionParserScript() + `

sshmgr_managed_count=0
sshmgr_existing_line=''
sshmgr_existing_address=''
sshmgr_existing_version=''
sshmgr_remote_version=''
if [ ! -f /etc/fstab ]; then
  touch /etc/fstab
fi
while IFS= read -r sshmgr_line || [ -n "$sshmgr_line" ]; do
  case "$sshmgr_line" in '#'*|'') continue ;; esac
  set -f
  set -- $sshmgr_line
  set +f
  [ "$#" -eq 6 ] || continue
  [ "$1" = "//"* ] && [ "$2" = "$mount_point" ] && [ "$3" = cifs ] || continue
  sshmgr_parse_options "$4"
  [ "$sshmgr_option_share" = "$sshmgr_expected_share" ] || continue
  case "$sshmgr_option_version" in ''|*[!0-9]*)
    echo ` + linuxMountMarker + ` invalid-fstab-marker >&2
    exit "$sshmgr_exit_invalid_fstab"
  esac
  sshmgr_managed_count=$((sshmgr_managed_count+1))
  if [ "$sshmgr_managed_count" -gt 1 ]; then
    echo ` + linuxMountMarker + ` duplicate-fstab-entry >&2
    exit "$sshmgr_exit_invalid_fstab"
  fi
  sshmgr_existing_line=$sshmgr_line
  sshmgr_remote_version=$sshmgr_option_version
  case "$sshmgr_option_vers" in 3.1.1|3.0|2.1|2.0) sshmgr_existing_version=$sshmgr_option_vers ;; *)
    echo ` + linuxMountMarker + ` invalid-fstab-entry >&2
    exit "$sshmgr_exit_invalid_fstab"
  esac
  sshmgr_existing_address=${1#//}
  sshmgr_existing_address=${sshmgr_existing_address%/*}
done < /etc/fstab

` + remoteVersionGuardScript(linuxMountMarker, linuxExitRemoteNewer) + `
` + revokedMarkerGuardScript(
		linuxCredentialRoot+"/"+url.PathEscape(share.Name)+".revoked",
		linuxMountMarker, linuxExitShareRevoked, linuxExitInvalidFstab,
	) + `
` + candidateAddressSelectionScript(candidates) + `

sshmgr_selected_version=''
if [ "$sshmgr_selected_address" = "$sshmgr_existing_address" ]; then
  sshmgr_selected_version=$sshmgr_existing_version
fi

sshmgr_is_mounted() {
` + mountTableCheckScript(CheckSystemLinux) + `  [ "$sshmgr_mounted" = yes ]
}

sshmgr_build_fstab_line() {
  sshmgr_fstab_line=$(printf '//%s/%s %s cifs credentials=%s,vers=%s,%s,_netdev,nofail,x-systemd.automount,x-systemd.mount-timeout=10 0 0\n' "$sshmgr_selected_address" "$share_name" "$mount_point" "$credential_file" "${sshmgr_selected_version:-none}" "$sshmgr_fstab_marker")
}
sshmgr_build_fstab_line

if [ "$sshmgr_existing_line" = "$sshmgr_fstab_line" ] && [ -n "$sshmgr_remote_version" ] &&
   [ "$sshmgr_remote_version" = "$sshmgr_expected_version" ] && sshmgr_is_mounted; then
  rm -f "$sshmgr_revoked_marker"
  echo ` + linuxMountMarker + ` result=no-change selected=$sshmgr_selected_index
  exit 0
fi

sshmgr_prepare_remount() {
  if [ "$sshmgr_managed_count" -gt 0 ]; then
    sshmgr_automount_unit=$(systemd-escape -p --suffix=automount "$mount_point")
    systemctl stop "$sshmgr_automount_unit" >/dev/null 2>&1 || true
    umount "$mount_point" >/dev/null 2>&1 || true
  fi
}
sshmgr_write_credentials() {
  sshmgr_credential_candidate=$credential_file.sshmgr.$$
  {
    printf 'username=%s\n' ` + sshbroker.POSIXShellQuote(share.MountUser) + `
    printf 'password='
    cat
    printf '\n'
  } > "$sshmgr_credential_candidate"
  chown root:root "$sshmgr_credential_candidate"
  chmod 600 "$sshmgr_credential_candidate"
}

sshmgr_trial_point=` + sshbroker.POSIXShellQuote(linuxTrialMountPoint(share.Name)) + `
install -d -m 755 "$sshmgr_trial_point"
sshmgr_write_credentials
sshmgr_trial_mounted=no
if [ -n "$sshmgr_selected_version" ] &&
   mount -t cifs "//${sshmgr_selected_address}/${share_name}" "$sshmgr_trial_point" -o "vers=$sshmgr_selected_version,credentials=$sshmgr_credential_candidate"; then
  sshmgr_trial_mounted=yes
else
  sshmgr_selected_version=''
  for sshmgr_version in 3.1.1 3.0 2.1 2.0; do
    if mount -t cifs "//${sshmgr_selected_address}/${share_name}" "$sshmgr_trial_point" -o "vers=$sshmgr_version,credentials=$sshmgr_credential_candidate"; then
      sshmgr_selected_version=$sshmgr_version
      sshmgr_trial_mounted=yes
      break
    fi
  done
fi
if [ "$sshmgr_trial_mounted" = no ]; then
  echo ` + linuxMountMarker + ` protocol-failed >&2
  exit ` + strconv.Itoa(linuxExitProtocolFailed) + `
fi
if ! umount "$sshmgr_trial_point"; then
  echo ` + linuxMountMarker + ` trial-unmount-failed >&2
  exit ` + strconv.Itoa(linuxExitTrialUnmountFailed) + `
fi
rmdir "$sshmgr_trial_point" 2>/dev/null || true
sshmgr_trial_point=''

# 试挂成功后才停 automount、卸载旧挂载并原子替换凭据文件。
sshmgr_prepare_remount
mv "$sshmgr_credential_candidate" "$credential_file"
sshmgr_credential_candidate=''
sshmgr_build_fstab_line

if [ "$sshmgr_existing_line" != "$sshmgr_fstab_line" ]; then
  sshmgr_fstab_candidate=/etc/fstab.sshmgr.$$
  : > "$sshmgr_fstab_candidate"
  sshmgr_replaced=0
  while IFS= read -r sshmgr_line || [ -n "$sshmgr_line" ]; do
    sshmgr_drop_line=0
    case "$sshmgr_line" in '#'*|'') ;; *)
      set -f
      set -- $sshmgr_line
      set +f
      if [ "$#" -eq 6 ]; then
        sshmgr_parse_options "$4"
        if [ "$sshmgr_option_share" = "$sshmgr_expected_share" ]; then
          sshmgr_drop_line=1
        fi
      fi
    esac
    if [ "$sshmgr_drop_line" -eq 0 ]; then
      printf '%s\n' "$sshmgr_line" >> "$sshmgr_fstab_candidate"
    elif [ "$sshmgr_replaced" -eq 0 ]; then
      printf '%s\n' "$sshmgr_fstab_line" >> "$sshmgr_fstab_candidate"
      sshmgr_replaced=1
    fi
  done < /etc/fstab
  if [ "$sshmgr_replaced" -eq 0 ]; then
    printf '%s\n' "$sshmgr_fstab_line" >> "$sshmgr_fstab_candidate"
  fi
  cat "$sshmgr_fstab_candidate" > /etc/fstab
  rm -f "$sshmgr_fstab_candidate"
  sshmgr_fstab_candidate=''
fi

systemctl daemon-reload
sshmgr_automount_unit=$(systemd-escape -p --suffix=automount "$mount_point")
systemctl restart "$sshmgr_automount_unit"
` + mountTableCheckScript(CheckSystemLinux) + `if [ "$sshmgr_mounted" != yes ]; then
  echo ` + linuxMountMarker + ` verify-failed >&2
  exit ` + strconv.Itoa(linuxExitVerifyFailed) + `
fi
if [ "$sshmgr_managed_count" -eq 0 ]; then
  sshmgr_result=first-mount-success
elif [ "$sshmgr_existing_line" = "$sshmgr_fstab_line" ]; then
  sshmgr_result=remount-success
else
  sshmgr_result=update-success
fi
rm -f "$sshmgr_revoked_marker"
echo ` + linuxMountMarker + ` result=$sshmgr_result selected=$sshmgr_selected_index`
	return command
}

// safeMountShareName 保证共享名可以成为 Linux fstab 字段与 macOS autofs
// 键名，不会被空白、斜杠或空字节拆开。
func safeMountShareName(name string) bool {
	return name != "" && name != "." && name != ".." &&
		len(strings.Fields(name)) == 1 && !strings.ContainsAny(name, "/\\\x00")
}
