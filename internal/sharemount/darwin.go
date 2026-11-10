package sharemount

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sshbroker"
)

const (
	darwinMountMarker        = "SSHMGR-DARWIN-MOUNT"
	darwinExitMissingCommand = 83
	darwinExitVerifyFailed   = 86
	darwinExitLockTimeout    = 87
	darwinExitRemoteNewer    = 88
	darwinExitInvalidConfig  = 89
	darwinAutoMaster         = "/etc/auto_master"
	darwinMapFile            = "/etc/sshmgr/auto_share"
	darwinMountRoot          = "/mnt/sshmgr"
	darwinLockPath           = "/etc/sshmgr/mount.lock"
)

func darwinMountPoint(shareName string) string {
	return darwinMountRoot + "/" + shareName
}

// escapeDarwinPassword 按网址用户信息里的百分号编码规则编码密码。这里不
// 使用查询串编码，因为查询串会把空格写成加号，而 autofs 会把加号当作密码
// 本身；非保留字节保持原样，其余字节逐个写成两个十六进制位。
func escapeDarwinPassword(password []byte) string {
	var encoded strings.Builder
	const hexadecimal = "0123456789ABCDEF"
	for _, value := range password {
		switch {
		case value >= 'a' && value <= 'z', value >= 'A' && value <= 'Z',
			value >= '0' && value <= '9', value == '-', value == '_', value == '.', value == '~':
			encoded.WriteByte(value)
		default:
			encoded.WriteByte('%')
			encoded.WriteByte(hexadecimal[value>>4])
			encoded.WriteByte(hexadecimal[value&0x0f])
		}
	}
	return encoded.String()
}

// buildDarwinAutomountEntry 生成映射条目与它的独立标记行。标记行是注释，
// autofs 不把它当挂载选项；真正触发挂载的每个共享仍是一行。
func buildDarwinAutomountEntry(
	share *models.SharedStorage, address, encodedPassword string,
) (markerLine string, entryLine string) {
	markerLine = "# " + sharedStorageMarkerOptions(share.Name, share.Version)
	entryLine = fmt.Sprintf(
		"%s -fstype=smbfs ://%s:%s@%s/%s",
		share.Name, share.MountUser, encodedPassword, address, share.Name,
	)
	return markerLine, entryLine
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

	candidateVariables := strings.Builder{}
	candidateCases := strings.Builder{}
	candidateSelections := strings.Builder{}
	for index, candidate := range candidates {
		number := index + 1
		fmt.Fprintf(&candidateVariables, "candidate_%d_address=%s\n",
			number, sshbroker.POSIXShellQuote(candidate.Address))
		fmt.Fprintf(&candidateCases, "%d) sshmgr_selected_address=$candidate_%d_address ;;\n", number, number)
		fmt.Fprintf(&candidateSelections,
			"if [ \"$candidate_%d_address\" = \"$sshmgr_existing_address\" ]; then sshmgr_selected_index=%d; fi\n",
			number, number)
	}

	return `set -eu
if ! command -v automount >/dev/null 2>&1; then
  echo ` + darwinMountMarker + ` missing-automount >&2
  exit 83
fi
install -d -m 700 '/etc/sshmgr'
` + sharedStorageDirectoryLockScript(darwinLockPath, 30, darwinExitLockTimeout, darwinMountMarker) + `
sshmgr_share_marker=` + marker + `
sshmgr_expected_share=` + shareValue + `
sshmgr_expected_version=` + versionValue + `
share_name=` + shareName + `
mount_user=` + mountUser + `
mount_point=` + mountPoint + `
` + candidateVariables.String() + `
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

if [ -n "$sshmgr_remote_version" ] && [ "$sshmgr_remote_version" -gt "$sshmgr_expected_version" ]; then
  echo ` + darwinMountMarker + ` remote-version-newer >&2
  exit 88
fi

sshmgr_selected_index=0
` + candidateSelections.String() + `if [ "$sshmgr_selected_index" -eq 0 ]; then
  sshmgr_selected_index=1
fi
case "$sshmgr_selected_index" in
` + candidateCases.String() + `esac

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

// parseDarwinMountOutput 只解析安全结果字段，不接受地址、账号或密码。
func parseDarwinMountOutput(stdout string) (action string, selectedIndex int, reset bool, err error) {
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Fields(strings.TrimRight(line, "\r"))
		if len(fields) == 0 || fields[0] != darwinMountMarker {
			continue
		}
		for _, field := range fields[1:] {
			switch {
			case strings.HasPrefix(field, "result="):
				action = strings.TrimPrefix(field, "result=")
			case strings.HasPrefix(field, "selected="):
				value := strings.TrimPrefix(field, "selected=")
				parsed, parseErr := strconv.Atoi(value)
				if parseErr != nil || parsed < 1 {
					return "", 0, false, errors.New("macOS 固定挂载命令返回了非法候选序号")
				}
				selectedIndex = parsed
			case field == "reset=yes":
				reset = true
			}
		}
	}
	if action == "" || selectedIndex == 0 {
		return "", 0, false, errors.New("macOS 固定挂载命令没有返回成功标记")
	}
	return action, selectedIndex, reset, nil
}
