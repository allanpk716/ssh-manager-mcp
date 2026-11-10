package sharemount

import (
	"errors"
	"net/url"
	"strconv"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sshbroker"
)

const (
	darwinMountMarker        = "SSHMGR-DARWIN-MOUNT"
	darwinExitMissingCommand = 83
	darwinExitUnmountFailed  = 85
	darwinExitVerifyFailed   = 86
	darwinExitLockTimeout    = 87
	darwinExitRemoteNewer    = 88
	darwinExitInvalidConfig  = 89
	darwinAutoMaster         = "/etc/auto_master"
	darwinMapFile            = "/etc/sshmgr/auto_share"
	darwinMountRoot          = "/mnt/sshmgr"
)

// darwinMountExitCodeError 把 macOS 固定挂载命令的已知退出码翻译为中文
// 说明；不在表内的退出码返回 nil，由共用流程给出带退出码的兜底文本。
func darwinMountExitCodeError(exitCode int) error {
	switch exitCode {
	case darwinExitMissingCommand:
		return errors.New("目标服务器缺少 automount，请先由 owner 修复 macOS 系统组件；挂载工具不会自动安装")
	case darwinExitUnmountFailed:
		return errors.New("macOS 卸载旧挂载失败，请 owner 手动检查后重试")
	case darwinExitVerifyFailed:
		return errors.New("macOS 挂载点验证失败，不能确认共享已可用")
	case darwinExitLockTimeout:
		return errors.New("另一个挂载正在进行，请稍后再试")
	case darwinExitRemoteNewer:
		return errors.New("目标服务器上的共享存储版本更新，请先同步快照后再调用挂载工具")
	case darwinExitInvalidConfig:
		return errors.New("目标服务器上的 sshmgr autofs 配置不合法，请 owner 检查后再试")
	default:
		return nil
	}
}

func darwinMountPoint(shareName string) string {
	return darwinMountRoot + "/" + shareName
}

// buildDarwinMountCommand 生成 macOS autofs 固定命令。密码只经提权会话的
// 标准输入进入远端脚本，再由脚本按字节百分号编码；它绝不进入命令文本。
func buildDarwinMountCommand(share *models.SharedStorage, candidates []*Candidate) string {
	shareName := sshbroker.POSIXShellQuote(share.Name)
	mountUser := sshbroker.POSIXShellQuote(share.MountUser)
	mountPoint := sshbroker.POSIXShellQuote(darwinMountPoint(share.Name))
	marker := sshbroker.POSIXShellQuote(sharedStorageMarkerOptions(share.Name, share.Version))
	shareValue := sshbroker.POSIXShellQuote(url.PathEscape(share.Name))
	versionValue := sshbroker.POSIXShellQuote(strconv.FormatInt(share.Version, 10))

	return `set -eu
if ! command -v automount >/dev/null 2>&1; then
  echo ` + darwinMountMarker + ` missing-automount >&2
  exit 83
fi
install -d -m 700 '/etc/sshmgr'
` + sharedStorageDirectoryLockScript(sharedStorageLockPath, 30, darwinExitLockTimeout, darwinMountMarker) + `
sshmgr_share_marker=` + marker + `
sshmgr_expected_share=` + shareValue + `
sshmgr_expected_version=` + versionValue + `
share_name=` + shareName + `
mount_user=` + mountUser + `
mount_point=` + mountPoint + `
` + candidateAddressVariablesScript(candidates) + `
sshmgr_map_candidate=''
sshmgr_cleanup() {
  if [ -n "$sshmgr_map_candidate" ]; then rm -f "$sshmgr_map_candidate"; fi
  sshmgr_release_shared_storage_lock
}
trap sshmgr_cleanup EXIT HUP INT TERM

sshmgr_encode_password() {
  od -An -v -tu1 | awk '{
    for (sshmgr_index = 1; sshmgr_index <= NF; sshmgr_index++) {
      sshmgr_byte = $sshmgr_index
      if ((sshmgr_byte >= 97 && sshmgr_byte <= 122) ||
          (sshmgr_byte >= 65 && sshmgr_byte <= 90) ||
          (sshmgr_byte >= 48 && sshmgr_byte <= 57) ||
          sshmgr_byte == 45 || sshmgr_byte == 95 ||
          sshmgr_byte == 46 || sshmgr_byte == 126) {
        printf "%c", sshmgr_byte
      } else {
        printf "%%%02X", sshmgr_byte
      }
    }
  }'
}

sshmgr_parse_marker() {
  sshmgr_marker=${sshmgr_line#"# "}
  sshmgr_marker_share=${sshmgr_marker%%;*}
  sshmgr_marker_version=${sshmgr_marker##*;}
  sshmgr_marker_share=${sshmgr_marker_share#sshmgr-share=}
  sshmgr_marker_version=${sshmgr_marker_version#sshmgr-version=}
  case "$sshmgr_marker_version" in ''|*[!0-9]*)
    echo ` + darwinMountMarker + ` invalid-map-marker >&2
    exit 89
  esac
  case "$sshmgr_marker_share" in
    ''|*","*|*";"*|*"="*|*"/"*|*" "*)
      echo ` + darwinMountMarker + ` invalid-map-marker >&2
      exit 89
    ;;
  esac
  sshmgr_normalized_version=$((sshmgr_marker_version))
  if [ "$sshmgr_marker_version" != "$sshmgr_normalized_version" ]; then
    echo ` + darwinMountMarker + ` invalid-map-marker >&2
    exit 89
  fi
  if [ -z "$sshmgr_marker_share" ] ||
     [ "$sshmgr_marker" != "sshmgr-share=$sshmgr_marker_share;sshmgr-version=$sshmgr_marker_version" ]; then
    echo ` + darwinMountMarker + ` invalid-map-marker >&2
    exit 89
  fi
}

sshmgr_parse_current_entry() {
  set -f
  set -- $sshmgr_line
  set +f
  if [ "$#" -ne 3 ] || [ "$1" != "$share_name" ] || [ "$2" != -fstype=smbfs ]; then
    echo ` + darwinMountMarker + ` invalid-map-entry >&2
    exit 89
  fi
  sshmgr_location=$3
  sshmgr_location=${sshmgr_location#"://$mount_user:"}
  case "$sshmgr_location" in *@*/*) ;; *)
    echo ` + darwinMountMarker + ` invalid-map-entry >&2
    exit 89
  esac
  sshmgr_encoded_password=${sshmgr_location%%@*}
  sshmgr_location_tail=${sshmgr_location#*@}
  sshmgr_existing_address=${sshmgr_location_tail%%/*}
  sshmgr_location_share=${sshmgr_location_tail#*/}
  if [ -z "$sshmgr_existing_address" ] || [ -z "$sshmgr_encoded_password" ] ||
     [ "$sshmgr_location_share" != "$share_name" ]; then
    echo ` + darwinMountMarker + ` invalid-map-entry >&2
    exit 89
  fi
  case "$sshmgr_encoded_password" in *[!A-Za-z0-9_.~%-]*)
    echo ` + darwinMountMarker + ` invalid-map-entry >&2
    exit 89
  esac
}

sshmgr_managed_count=0
sshmgr_existing_line=''
sshmgr_existing_address=''
sshmgr_remote_version=''
sshmgr_after_marker=0
if [ ! -f ` + sshbroker.POSIXShellQuote(darwinMapFile) + ` ]; then
  touch ` + sshbroker.POSIXShellQuote(darwinMapFile) + `
fi
while IFS= read -r sshmgr_line || [ -n "$sshmgr_line" ]; do
  case "$sshmgr_line" in '# sshmgr-share='*)
    sshmgr_parse_marker
    if [ "$sshmgr_marker_share" = "$sshmgr_expected_share" ]; then
      sshmgr_managed_count=$((sshmgr_managed_count+1))
      if [ "$sshmgr_managed_count" -gt 1 ]; then
        echo ` + darwinMountMarker + ` duplicate-map-entry >&2
        exit 89
      fi
      sshmgr_remote_version=$sshmgr_marker_version
      sshmgr_after_marker=2
    else
      sshmgr_after_marker=1
    fi
    continue
  esac
  if [ "$sshmgr_after_marker" -eq 2 ]; then
    sshmgr_parse_current_entry
    sshmgr_existing_line=$sshmgr_line
  fi
  sshmgr_after_marker=0
done < ` + sshbroker.POSIXShellQuote(darwinMapFile) + `
if [ "$sshmgr_after_marker" -ne 0 ]; then
  echo ` + darwinMountMarker + ` invalid-map-entry >&2
  exit 89
fi

` + remoteVersionGuardScript(darwinMountMarker, darwinExitRemoteNewer) + `
` + candidateAddressSelectionScript(candidates) + `

sshmgr_existing_configuration=no
if [ "$sshmgr_managed_count" -eq 1 ] &&
   [ "$sshmgr_remote_version" = "$sshmgr_expected_version" ] &&
   [ "$sshmgr_existing_address" = "$sshmgr_selected_address" ] &&
   [ -n "$sshmgr_existing_line" ]; then
  sshmgr_existing_configuration=yes
fi

sshmgr_reference_present=0
if [ ! -f ` + sshbroker.POSIXShellQuote(darwinAutoMaster) + ` ]; then
  touch ` + sshbroker.POSIXShellQuote(darwinAutoMaster) + `
fi
while IFS= read -r sshmgr_line || [ -n "$sshmgr_line" ]; do
  set -f
  set -- $sshmgr_line
  set +f
  if [ "$#" -ge 2 ] && [ "$1" = ` + sshbroker.POSIXShellQuote(darwinMountRoot) + ` ]; then
    if [ "$2" = ` + sshbroker.POSIXShellQuote(darwinMapFile) + ` ]; then
      sshmgr_reference_present=1
    else
      echo ` + darwinMountMarker + ` invalid-auto-master >&2
      exit 89
    fi
  fi
done < ` + sshbroker.POSIXShellQuote(darwinAutoMaster) + `
sshmgr_reset=no
if [ "$sshmgr_reference_present" -eq 0 ]; then
  sshmgr_reset=yes
fi

sshmgr_mounted=no
if ls -A "$mount_point" >/dev/null 2>&1; then
  sshmgr_mounted=yes
fi
if [ "$sshmgr_existing_configuration" = yes ] && [ "$sshmgr_reset" = no ] &&
   [ "$sshmgr_mounted" = yes ]; then
  echo ` + darwinMountMarker + ` result=no-change selected=$sshmgr_selected_index reset=no
  exit 0
fi

sshmgr_build_entry_line() {
  sshmgr_entry_line=$(printf '%s -fstype=smbfs ://%s:%s@%s/%s\n' \
    "$share_name" "$mount_user" "$sshmgr_encoded_password" \
    "$sshmgr_selected_address" "$share_name")
}
sshmgr_write_map() {
  IFS= read -r sshmgr_password
  sshmgr_encoded_password=$(printf '%s' "$sshmgr_password" | sshmgr_encode_password)
  sshmgr_build_entry_line
  sshmgr_map_candidate=/etc/sshmgr/auto_share.sshmgr.$$
  : > "$sshmgr_map_candidate"
  sshmgr_replaced=0
  sshmgr_skip_entry=0
  while IFS= read -r sshmgr_line || [ -n "$sshmgr_line" ]; do
    case "$sshmgr_line" in '# sshmgr-share='*)
      if [ "$sshmgr_skip_entry" -eq 1 ]; then
        echo ` + darwinMountMarker + ` invalid-map-entry >&2
        exit 89
      fi
      sshmgr_parse_marker
      if [ "$sshmgr_marker_share" = "$sshmgr_expected_share" ]; then
        if [ "$sshmgr_replaced" -eq 1 ]; then
          echo ` + darwinMountMarker + ` duplicate-map-entry >&2
          exit 89
        fi
        printf '# %s\n' "$sshmgr_share_marker" >> "$sshmgr_map_candidate"
        printf '%s\n' "$sshmgr_entry_line" >> "$sshmgr_map_candidate"
        sshmgr_replaced=1
        sshmgr_skip_entry=1
      else
        printf '%s\n' "$sshmgr_line" >> "$sshmgr_map_candidate"
      fi
      continue
    esac
    if [ "$sshmgr_skip_entry" -eq 1 ]; then
      sshmgr_skip_entry=0
      continue
    fi
    printf '%s\n' "$sshmgr_line" >> "$sshmgr_map_candidate"
  done < ` + sshbroker.POSIXShellQuote(darwinMapFile) + `
  if [ "$sshmgr_skip_entry" -eq 1 ]; then
    echo ` + darwinMountMarker + ` invalid-map-entry >&2
    exit 89
  fi
  if [ "$sshmgr_replaced" -eq 0 ]; then
    printf '# %s\n' "$sshmgr_share_marker" >> "$sshmgr_map_candidate"
    printf '%s\n' "$sshmgr_entry_line" >> "$sshmgr_map_candidate"
  fi
  chown root:wheel "$sshmgr_map_candidate"
  chmod 600 "$sshmgr_map_candidate"
  mv "$sshmgr_map_candidate" ` + sshbroker.POSIXShellQuote(darwinMapFile) + `
  sshmgr_map_candidate=''
}

if [ "$sshmgr_existing_configuration" = no ]; then
  sshmgr_write_map
fi
if [ "$sshmgr_reset" = yes ]; then
  printf '%s %s\n' ` + sshbroker.POSIXShellQuote(darwinMountRoot) + ` ` + sshbroker.POSIXShellQuote(darwinMapFile) + ` >> ` + sshbroker.POSIXShellQuote(darwinAutoMaster) + `
fi

if [ "$sshmgr_managed_count" -gt 0 ] && [ "$sshmgr_existing_configuration" = no ] &&
   mount | grep -qF " on $mount_point (smbfs"; then
  if ! umount "$mount_point" >/dev/null 2>&1; then
    if ! diskutil unmount force "$mount_point" >/dev/null 2>&1; then
      echo ` + darwinMountMarker + ` unmount-failed >&2
      exit ` + strconv.Itoa(darwinExitUnmountFailed) + `
    fi
  fi
fi

automount -cv
if ! ls -A "$mount_point" >/dev/null 2>&1; then
  echo ` + darwinMountMarker + ` verify-failed >&2
  exit 86
fi
if [ "$sshmgr_managed_count" -eq 0 ]; then
  sshmgr_result=first-mount-success
elif [ "$sshmgr_existing_configuration" = yes ]; then
  sshmgr_result=remount-success
else
  sshmgr_result=update-success
fi
echo ` + darwinMountMarker + ` result=$sshmgr_result selected=$sshmgr_selected_index reset=$sshmgr_reset
`
}
