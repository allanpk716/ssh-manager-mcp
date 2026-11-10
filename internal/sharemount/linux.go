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
	linuxMountMarker        = "SSHMGR-LINUX-MOUNT"
	linuxExitMissingCIFS    = 83
	linuxExitProtocolFailed = 84
	linuxExitLockTimeout    = 87
	linuxExitRemoteNewer    = 88
	linuxExitInvalidFstab   = 89
	linuxCredentialRoot     = "/etc/sshmgr/shares"
	linuxMountRoot          = "/mnt/sshmgr"
	linuxLockPath           = "/etc/sshmgr/mount.lock"
)

// ErrLinuxFstabEntryNotManaged 表示 fstab 配置行没有完整的 sshmgr 标记。
var ErrLinuxFstabEntryNotManaged = errors.New("fstab 配置行不是 sshmgr 管理的条目")

// LinuxFstabEntry 是一条 sshmgr 管理的 Linux fstab 配置的可比较结论。
type LinuxFstabEntry struct {
	Address         string
	MountPoint      string
	ProtocolVersion string
	Marker          SharedStorageMarker
}

func linuxMountPoint(shareName string) string {
	return linuxMountRoot + "/" + shareName
}

func linuxCredentialPath(shareID string) string {
	return linuxCredentialRoot + "/" + shareID + ".credentials"
}

// buildLinuxFstabLine 拼装一条 Linux fstab 配置行。标记文本来自跨操作系
// 统的 sharedStorageMarkerOptions；这里把分号换成 fstab 选项之间的逗号，
// 形成两个相邻选项：sshmgr-share 与 sshmgr-version。
func buildLinuxFstabLine(
	share *models.SharedStorage, address, protocolVersion, marker string,
) string {
	options := strings.Join([]string{
		"credentials=" + linuxCredentialPath(share.ID),
		"vers=" + protocolVersion,
		strings.ReplaceAll(marker, ";", ","),
		"_netdev,nofail,x-systemd.automount,x-systemd.mount-timeout=10",
	}, ",")
	return strings.Join([]string{
		"//" + address + "/" + share.Name,
		linuxMountPoint(share.Name),
		"cifs", options, "0", "0",
	}, " ")
}

// parseLinuxFstabLine 解析 sshmgr 管理的 fstab 配置行，供进程内测试与后
// 续现场检测复用。owner 手写行没有完整标记，返回
// ErrLinuxFstabEntryNotManaged；标记存在但不合法时返回
// ErrInvalidSharedStorageMarker，调用方必须停止改写。
func parseLinuxFstabLine(line string) (LinuxFstabEntry, error) {
	fields := strings.Fields(line)
	if len(fields) != 6 || fields[2] != "cifs" || !strings.HasPrefix(fields[0], "//") {
		return LinuxFstabEntry{}, ErrLinuxFstabEntryNotManaged
	}
	source := strings.TrimPrefix(fields[0], "//")
	shareSeparator := strings.LastIndex(source, "/")
	if shareSeparator <= 0 {
		return LinuxFstabEntry{}, ErrLinuxFstabEntryNotManaged
	}
	address := source[:shareSeparator]
	protocolVersion := ""
	shareOption := ""
	versionOption := ""
	for _, option := range strings.Split(fields[3], ",") {
		switch {
		case strings.HasPrefix(option, "vers="):
			protocolVersion = strings.TrimPrefix(option, "vers=")
		case strings.HasPrefix(option, "sshmgr-share="):
			shareOption = option
		case strings.HasPrefix(option, "sshmgr-version="):
			versionOption = option
		}
	}
	if protocolVersion == "" || shareOption == "" || versionOption == "" {
		return LinuxFstabEntry{}, ErrLinuxFstabEntryNotManaged
	}
	marker, err := parseSharedStorageMarkerOptions(shareOption + ";" + versionOption)
	if err != nil {
		return LinuxFstabEntry{}, err
	}
	return LinuxFstabEntry{
		Address: address, MountPoint: fields[1], ProtocolVersion: protocolVersion, Marker: marker,
	}, nil
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

	candidateVariables := strings.Builder{}
	candidateCases := strings.Builder{}
	candidateSelections := strings.Builder{}
	for index, candidate := range candidates {
		number := index + 1
		fmt.Fprintf(&candidateVariables, "candidate_%d_address=%s\n", number, sshbroker.POSIXShellQuote(candidate.Address))
		fmt.Fprintf(&candidateCases, "%d) sshmgr_selected_address=$candidate_%d_address ;;\n", number, number)
		fmt.Fprintf(&candidateSelections, "if [ \"$candidate_%d_address\" = \"$sshmgr_existing_address\" ]; then sshmgr_selected_index=%d; fi\n", number, number)
	}

	command := `set -eu
if ! command -v mount.cifs >/dev/null 2>&1; then
  echo ` + linuxMountMarker + ` missing-mount-cifs >&2
  exit 83
fi
install -d -m 700 ` + sshbroker.POSIXShellQuote(linuxCredentialRoot) + `
install -d -m 755 ` + sshbroker.POSIXShellQuote(linuxMountRoot) + `
` + sharedStorageDirectoryLockScript(linuxLockPath, 30, linuxExitLockTimeout, linuxMountMarker) + `
sshmgr_share_marker=` + marker + `
sshmgr_fstab_marker=` + sshbroker.POSIXShellQuote(fstabMarker) + `
sshmgr_expected_share=` + shareValue + `
sshmgr_expected_version=` + versionValue + `
share_name=` + shareName + `
credential_file=` + credential + `
mount_point=` + mountPoint + `
candidate_count=` + strconv.Itoa(len(candidates)) + `
` + candidateVariables.String() + `
install -d -m 755 "$mount_point"

sshmgr_credential_candidate=''
sshmgr_fstab_candidate=''
sshmgr_cleanup() {
  if [ -n "$sshmgr_credential_candidate" ]; then rm -f "$sshmgr_credential_candidate"; fi
  if [ -n "$sshmgr_fstab_candidate" ]; then rm -f "$sshmgr_fstab_candidate"; fi
  sshmgr_release_shared_storage_lock
}
trap sshmgr_cleanup EXIT HUP INT TERM

sshmgr_parse_options() {
  sshmgr_option_share=''
  sshmgr_option_version=''
  sshmgr_option_vers=''
  sshmgr_saved_ifs=$IFS
  IFS=,
  set -f
  set -- $1
  set +f
  IFS=$sshmgr_saved_ifs
  for sshmgr_option in "$@"; do
    case "$sshmgr_option" in
      sshmgr-share=*) sshmgr_option_share=${sshmgr_option#sshmgr-share=} ;;
      sshmgr-version=*) sshmgr_option_version=${sshmgr_option#sshmgr-version=} ;;
      vers=*) sshmgr_option_vers=${sshmgr_option#vers=} ;;
    esac
  done
}

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
    exit 89
  esac
  sshmgr_managed_count=$((sshmgr_managed_count+1))
  if [ "$sshmgr_managed_count" -gt 1 ]; then
    echo ` + linuxMountMarker + ` duplicate-fstab-entry >&2
    exit 89
  fi
  sshmgr_existing_line=$sshmgr_line
  sshmgr_remote_version=$sshmgr_option_version
  case "$sshmgr_option_vers" in 3.1.1|3.0|2.1|2.0) sshmgr_existing_version=$sshmgr_option_vers ;; *)
    echo ` + linuxMountMarker + ` invalid-fstab-entry >&2
    exit 89
  esac
  sshmgr_existing_address=${1#//}
  sshmgr_existing_address=${sshmgr_existing_address%/*}
done < /etc/fstab

if [ -n "$sshmgr_remote_version" ] && [ "$sshmgr_remote_version" -gt "$sshmgr_expected_version" ]; then
  echo ` + linuxMountMarker + ` remote-version-newer >&2
  exit 88
fi

sshmgr_selected_index=0
` + candidateSelections.String() + `if [ "$sshmgr_selected_index" -eq 0 ]; then
  sshmgr_selected_index=1
fi
case "$sshmgr_selected_index" in
` + candidateCases.String() + `esac

sshmgr_selected_version=''
if [ "$sshmgr_selected_address" = "$sshmgr_existing_address" ]; then
  sshmgr_selected_version=$sshmgr_existing_version
fi

sshmgr_is_mounted() {
  if command -v mountpoint >/dev/null 2>&1; then
    mountpoint -q "$mount_point"
  elif [ -r /proc/mounts ]; then
    grep -qF " $mount_point cifs " /proc/mounts
  else
    mount | grep -qF " on $mount_point type cifs "
  fi
}

sshmgr_build_fstab_line() {
  sshmgr_fstab_line=$(printf '//%s/%s %s cifs credentials=%s,vers=%s,%s,_netdev,nofail,x-systemd.automount,x-systemd.mount-timeout=10 0 0\n' "$sshmgr_selected_address" "$share_name" "$mount_point" "$credential_file" "${sshmgr_selected_version:-none}" "$sshmgr_fstab_marker")
}
sshmgr_build_fstab_line

if [ "$sshmgr_existing_line" = "$sshmgr_fstab_line" ] && [ -n "$sshmgr_remote_version" ] &&
   [ "$sshmgr_remote_version" = "$sshmgr_expected_version" ] && sshmgr_is_mounted; then
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

sshmgr_prepare_remount
if [ ! -f "$credential_file" ] || [ -z "$sshmgr_remote_version" ] ||
   [ "$sshmgr_remote_version" != "$sshmgr_expected_version" ]; then
  sshmgr_write_credentials
fi
if [ -n "$sshmgr_credential_candidate" ]; then
  sshmgr_trial_credential=$sshmgr_credential_candidate
else
  sshmgr_trial_credential=$credential_file
fi

if [ -z "$sshmgr_selected_version" ]; then
  for sshmgr_version in 3.1.1 3.0 2.1 2.0; do
    if mount -t cifs "//${sshmgr_selected_address}/${share_name}" "$mount_point" -o "vers=$sshmgr_version,credentials=$sshmgr_trial_credential"; then
      sshmgr_selected_version=$sshmgr_version
      break
    fi
  done
else
  mount -t cifs "//${sshmgr_selected_address}/${share_name}" "$mount_point" -o "vers=$sshmgr_selected_version,credentials=$sshmgr_trial_credential"
fi
if [ -z "$sshmgr_selected_version" ]; then
  echo ` + linuxMountMarker + ` protocol-failed >&2
  exit 84
fi
if ! umount "$mount_point"; then
  echo ` + linuxMountMarker + ` trial-unmount-failed >&2
  exit 85
fi
if [ -n "$sshmgr_credential_candidate" ]; then
  mv "$sshmgr_credential_candidate" "$credential_file"
  sshmgr_credential_candidate=''
fi
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
if ! ls -A "$mount_point" >/dev/null 2>&1; then
  echo ` + linuxMountMarker + ` verify-failed >&2
  exit 86
fi
if [ "$sshmgr_managed_count" -eq 0 ]; then
  sshmgr_result=first-mount-success
elif [ "$sshmgr_existing_line" = "$sshmgr_fstab_line" ]; then
  sshmgr_result=remount-success
else
  sshmgr_result=update-success
fi
echo ` + linuxMountMarker + ` result=$sshmgr_result selected=$sshmgr_selected_index`
	return command
}

// safeMountShareName 保证共享名可以成为 Linux fstab 字段与 macOS autofs
// 键名，不会被空白、斜杠或空字节拆开。
func safeMountShareName(name string) bool {
	return name != "" && name != "." && name != ".." &&
		len(strings.Fields(name)) == 1 && !strings.ContainsAny(name, "/\\\x00")
}

// parseLinuxMountOutput 解析远端固定命令的安全输出。输出只包含动作与候
// 选序号，不回传地址、协议版本或任何机密。
func parseLinuxMountOutput(stdout string) (action string, selectedIndex int, err error) {
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Fields(strings.TrimRight(line, "\r"))
		if len(fields) == 0 || fields[0] != linuxMountMarker {
			continue
		}
		for _, field := range fields[1:] {
			if strings.HasPrefix(field, "result=") {
				action = strings.TrimPrefix(field, "result=")
			}
			if strings.HasPrefix(field, "selected=") {
				value := strings.TrimPrefix(field, "selected=")
				parsed, parseErr := strconv.Atoi(value)
				if parseErr != nil || parsed < 1 {
					return "", 0, errors.New("Linux 固定挂载命令返回了非法候选序号")
				}
				selectedIndex = parsed
			}
		}
	}
	if action == "" || selectedIndex == 0 {
		return "", 0, errors.New("Linux 固定挂载命令没有返回成功标记")
	}
	return action, selectedIndex, nil
}
