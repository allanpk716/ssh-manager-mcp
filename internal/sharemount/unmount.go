package sharemount

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sshbroker"
)

const (
	linuxUnmountMarker  = "SSHMGR-LINUX-UNMOUNT"
	darwinUnmountMarker = "SSHMGR-DARWIN-UNMOUNT"
)

var (
	ErrUnmountUnsupportedSystem = errors.New("目标服务器操作系统不支持共享存储卸载")
	ErrUnmountSSH               = errors.New("连接目标服务器或执行固定卸载命令失败")
	ErrUnmountNotManaged        = errors.New("目标服务器上没有这个共享名的 sshmgr 挂载配置")
	ErrUnmountLockTimeout       = errors.New("另一个共享存储操作正在进行，请稍后再试")
	ErrUnmountFailed            = errors.New("卸载共享存储挂载点失败")
	ErrUnmountOutputInvalid     = errors.New("共享存储固定卸载命令返回了不合法结论")
)

// UnmountRequest 是一次 owner 卸载调用的远端目标。WriteRevokedMarker 由
// 管理模块按登记与可挂载范围判定；这里只负责在目标服务器上执行固定命令。
type UnmountRequest struct {
	Server             *models.Server
	ShareName          string
	Version            int64
	Force              bool
	WriteRevokedMarker bool
}

// BusyProcess 是占用挂载点的进程。命令名来自远端固定命令输出，不包含
// 完整命令参数。
type BusyProcess struct {
	ProcessID string
	User      string
	Command   string
}

// UnmountResult 是远端固定命令的安全结论，不包含共享地址、账号或密码。
type UnmountResult struct {
	System        string
	Result        string
	Version       int64
	BusyProcesses []BusyProcess
}

// Unmount 删除目标服务器上属于指定共享的持久挂载配置，再卸载挂载点。
func (s *Service) Unmount(parent context.Context, request UnmountRequest) (*UnmountResult, error) {
	if s == nil || s.Store == nil {
		return nil, errors.New("共享存储挂载服务没有绑定存储")
	}
	if request.Server == nil || request.ShareName == "" {
		return nil, errors.New("共享存储卸载参数不完整")
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	systemResult, err := s.Exec(ctx, request.Server, RemoteExecRequest{Command: "uname -s"})
	if err != nil {
		return nil, fmt.Errorf("%w：识别目标服务器操作系统失败：%w", ErrUnmountSSH, err)
	}
	if request.Server.SudoCredentialID == "" {
		return nil, ErrNoSudo
	}
	switch strings.TrimSpace(systemResult.Stdout) {
	case "Linux":
		return s.unmountLinux(ctx, request)
	case "Darwin":
		return s.unmountDarwin(ctx, request)
	default:
		return nil, ErrUnmountUnsupportedSystem
	}
}

func (s *Service) unmountDarwin(ctx context.Context, request UnmountRequest) (*UnmountResult, error) {
	result, err := s.Exec(ctx, request.Server, RemoteExecRequest{
		Command: buildDarwinUnmountCommand(request), Privileged: true,
	})
	if err != nil {
		return nil, fmt.Errorf("%w：macOS 固定卸载命令执行失败：%w",
			ErrUnmountSSH, sanitizedExecError(err))
	}
	if result.ExitCode != 0 {
		return nil, darwinUnmountExitCodeError(result.ExitCode)
	}
	return parseUnmountOutput(result.Stdout, darwinUnmountMarker, "Darwin")
}

func (s *Service) unmountLinux(ctx context.Context, request UnmountRequest) (*UnmountResult, error) {
	result, err := s.Exec(ctx, request.Server, RemoteExecRequest{
		Command: buildLinuxUnmountCommand(request), Privileged: true,
	})
	if err != nil {
		return nil, fmt.Errorf("%w：Linux 固定卸载命令执行失败：%w",
			ErrUnmountSSH, sanitizedExecError(err))
	}
	if result.ExitCode != 0 {
		return nil, linuxUnmountExitCodeError(result.ExitCode)
	}
	return parseUnmountOutput(result.Stdout, linuxUnmountMarker, "Linux")
}

func linuxUnmountExitCodeError(exitCode int) error {
	switch exitCode {
	case 85:
		return ErrUnmountFailed
	case 87:
		return ErrUnmountLockTimeout
	case 89:
		return ErrUnmountNotManaged
	default:
		return fmt.Errorf("Linux 固定卸载命令失败（退出码 %d）", exitCode)
	}
}

func darwinUnmountExitCodeError(exitCode int) error {
	switch exitCode {
	case 85:
		return ErrUnmountFailed
	case 87:
		return ErrUnmountLockTimeout
	case 89:
		return ErrUnmountNotManaged
	default:
		return fmt.Errorf("macOS 固定卸载命令失败（退出码 %d）", exitCode)
	}
}

func buildLinuxUnmountCommand(request UnmountRequest) string {
	shareName := sshbroker.POSIXShellQuote(request.ShareName)
	mountPoint := sshbroker.POSIXShellQuote(linuxMountPoint(request.ShareName))
	version := sshbroker.POSIXShellQuote(strconv.FormatInt(request.Version, 10))
	expectedShare := sshbroker.POSIXShellQuote(url.PathEscape(request.ShareName))
	revokedMarker := sshbroker.POSIXShellQuote(
		linuxCredentialRoot + "/" + url.PathEscape(request.ShareName) + ".revoked")
	force := "no"
	if request.Force {
		force = "yes"
	}
	writeMarker := "no"
	if request.WriteRevokedMarker {
		writeMarker = "yes"
	}
	return `set -eu
` + sharedStorageDirectoryLockScript(sharedStorageLockPath, 30, 87, linuxUnmountMarker) + `
share_name=` + shareName + `
mount_point=` + mountPoint + `
sshmgr_expected_share=` + expectedShare + `
sshmgr_version=` + version + `
sshmgr_force=` + force + `
sshmgr_write_revoked_marker=` + writeMarker + `
sshmgr_fstab_candidate=''
sshmgr_cleanup() {
  if [ -n "$sshmgr_fstab_candidate" ]; then rm -f "$sshmgr_fstab_candidate"; fi
  sshmgr_release_shared_storage_lock
}
trap sshmgr_cleanup EXIT HUP INT TERM

` + linuxFstabOptionParserScript() + `
sshmgr_managed_count=0
sshmgr_remote_version=0
sshmgr_credential_file=''
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
    echo ` + linuxUnmountMarker + ` invalid-fstab-marker >&2
    exit 89
  esac
  sshmgr_managed_count=$((sshmgr_managed_count+1))
  if [ "$sshmgr_managed_count" -gt 1 ]; then
    echo ` + linuxUnmountMarker + ` duplicate-fstab-entry >&2
    exit 89
  fi
  sshmgr_remote_version=$sshmgr_option_version
  sshmgr_saved_ifs=$IFS
  IFS=,
  set -f
  set -- $4
  set +f
  IFS=$sshmgr_saved_ifs
  for sshmgr_option in "$@"; do
    case "$sshmgr_option" in credentials=*) sshmgr_credential_file=${sshmgr_option#credentials=} ;; esac
  done
done < /etc/fstab
if [ "$sshmgr_managed_count" -eq 0 ]; then
  echo ` + linuxUnmountMarker + ` not-managed >&2
  exit 89
fi

sshmgr_write_version=$sshmgr_version
if [ "$sshmgr_write_version" -eq 0 ]; then
  sshmgr_write_version=$((sshmgr_remote_version+1))
fi
sshmgr_fstab_candidate=/etc/fstab.sshmgr.$$
: > "$sshmgr_fstab_candidate"
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
  fi
done < /etc/fstab
cat "$sshmgr_fstab_candidate" > /etc/fstab
rm -f "$sshmgr_fstab_candidate"
sshmgr_fstab_candidate=''
if [ -n "$sshmgr_credential_file" ]; then
  rm -f "$sshmgr_credential_file"
fi
if [ "$sshmgr_write_revoked_marker" = yes ]; then
  install -d -m 700 ` + sshbroker.POSIXShellQuote(linuxCredentialRoot) + `
  printf 'sshmgr-share=%s;sshmgr-version=%s;sshmgr-state=revoked\n' \
    "$sshmgr_expected_share" "$sshmgr_write_version" > ` + revokedMarker + `
  chown root:root ` + revokedMarker + `
  chmod 400 ` + revokedMarker + `
fi

sshmgr_automount_unit=$(systemd-escape -p --suffix=automount "$mount_point")
systemctl stop "$sshmgr_automount_unit" >/dev/null 2>&1 || true
` + linuxProcessListScript() + mountTableCheckScript(CheckSystemLinux) + `if [ "$sshmgr_mounted" = yes ]; then
  sshmgr_processes=$(sshmgr_list_processes || true)
  if [ -n "$sshmgr_processes" ] && [ "$sshmgr_force" = no ]; then
    echo ` + linuxUnmountMarker + ` result=busy version=$sshmgr_write_version
    printf '%s\n' "$sshmgr_processes"
    exit 0
  fi
  if [ "$sshmgr_force" = yes ]; then
    if ! umount -f "$mount_point" >/dev/null 2>&1; then
      if ! umount -l "$mount_point" >/dev/null 2>&1; then
        echo ` + linuxUnmountMarker + ` force-unmount-failed >&2
        exit 85
      fi
    fi
  else
    if ! umount "$mount_point" >/dev/null 2>&1; then
      echo ` + linuxUnmountMarker + ` unmount-failed >&2
      exit 85
    fi
  fi
fi
rmdir "$mount_point" 2>/dev/null || true
echo ` + linuxUnmountMarker + ` result=success version=$sshmgr_write_version
`
}

// linuxProcessListScript 生成 Linux 占用进程清单。只取进程号、用户名与
// 命令名，不携带完整命令参数。
func linuxProcessListScript() string {
	return `sshmgr_list_processes() {
  if command -v lsof >/dev/null 2>&1; then
    sshmgr_pid_list=$(lsof -t "$mount_point" 2>/dev/null || true)
  elif command -v fuser >/dev/null 2>&1; then
    sshmgr_pid_list=$(fuser "$mount_point" 2>/dev/null || true)
  else
    return 0
  fi
  [ -n "$sshmgr_pid_list" ] || return 0
  for sshmgr_pid in $sshmgr_pid_list; do
    sshmgr_user=$(ps -o user= -p "$sshmgr_pid" 2>/dev/null || true)
    sshmgr_command=$(ps -o comm= -p "$sshmgr_pid" 2>/dev/null || true)
    [ -n "$sshmgr_user" ] || sshmgr_user=unknown
    [ -n "$sshmgr_command" ] || sshmgr_command=unknown
    printf 'SSHMGR-PROCESS pid=%s user=%s command=%s\n' "$sshmgr_pid" "$sshmgr_user" "$sshmgr_command"
  done
}
`
}

func buildDarwinUnmountCommand(request UnmountRequest) string {
	shareName := sshbroker.POSIXShellQuote(request.ShareName)
	mountPoint := sshbroker.POSIXShellQuote(darwinMountPoint(request.ShareName))
	expectedShare := sshbroker.POSIXShellQuote(url.PathEscape(request.ShareName))
	revokedMarker := sshbroker.POSIXShellQuote(
		"/etc/sshmgr/" + url.PathEscape(request.ShareName) + ".revoked")
	version := sshbroker.POSIXShellQuote(strconv.FormatInt(request.Version, 10))
	force := "no"
	if request.Force {
		force = "yes"
	}
	writeMarker := "no"
	if request.WriteRevokedMarker {
		writeMarker = "yes"
	}
	return `set -eu
` + sharedStorageDirectoryLockScript(sharedStorageLockPath, 30, 87, darwinUnmountMarker) + `
share_name=` + shareName + `
mount_point=` + mountPoint + `
sshmgr_expected_share=` + expectedShare + `
sshmgr_version=` + version + `
sshmgr_force=` + force + `
sshmgr_write_revoked_marker=` + writeMarker + `
sshmgr_map_candidate=''
sshmgr_auto_candidate=''
sshmgr_cleanup() {
  if [ -n "$sshmgr_map_candidate" ]; then rm -f "$sshmgr_map_candidate"; fi
  if [ -n "$sshmgr_auto_candidate" ]; then rm -f "$sshmgr_auto_candidate"; fi
  sshmgr_release_shared_storage_lock
}
trap sshmgr_cleanup EXIT HUP INT TERM

if [ ! -f /etc/sshmgr/auto_share ]; then
  echo ` + darwinUnmountMarker + ` not-managed >&2
  exit 89
fi
sshmgr_managed_count=0
sshmgr_remote_version=0
while IFS= read -r sshmgr_line || [ -n "$sshmgr_line" ]; do
  case "$sshmgr_line" in '# sshmgr-share='*)
    sshmgr_marker=${sshmgr_line#"# "}
    sshmgr_marker_share=${sshmgr_marker%%;*}
    sshmgr_marker_share=${sshmgr_marker_share#sshmgr-share=}
    [ "$sshmgr_marker_share" = "$sshmgr_expected_share" ] || continue
    sshmgr_marker_version=${sshmgr_marker##*;}
    sshmgr_marker_version=${sshmgr_marker_version#sshmgr-version=}
    case "$sshmgr_marker_version" in ''|*[!0-9]*)
      echo ` + darwinUnmountMarker + ` invalid-map-marker >&2
      exit 89
    esac
    sshmgr_managed_count=$((sshmgr_managed_count+1))
    if [ "$sshmgr_managed_count" -gt 1 ]; then
      echo ` + darwinUnmountMarker + ` duplicate-map-entry >&2
      exit 89
    fi
    sshmgr_remote_version=$sshmgr_marker_version
  esac
done < /etc/sshmgr/auto_share
if [ "$sshmgr_managed_count" -eq 0 ]; then
  echo ` + darwinUnmountMarker + ` not-managed >&2
  exit 89
fi

sshmgr_write_version=$sshmgr_version
if [ "$sshmgr_write_version" -eq 0 ]; then
  sshmgr_write_version=$((sshmgr_remote_version+1))
fi
if [ ! -f /etc/auto_master ]; then
  touch /etc/auto_master
fi
sshmgr_map_candidate=/etc/sshmgr/auto_share.sshmgr.$$
: > "$sshmgr_map_candidate"
sshmgr_skip_entry=0
while IFS= read -r sshmgr_line || [ -n "$sshmgr_line" ]; do
  if [ "$sshmgr_skip_entry" -eq 1 ]; then
    sshmgr_skip_entry=0
    continue
  fi
  case "$sshmgr_line" in '# sshmgr-share='*)
    sshmgr_marker=${sshmgr_line#"# "}
    sshmgr_marker_share=${sshmgr_marker%%;*}
    sshmgr_marker_share=${sshmgr_marker_share#sshmgr-share=}
    if [ "$sshmgr_marker_share" = "$sshmgr_expected_share" ]; then
      sshmgr_skip_entry=1
      continue
    fi
  esac
  printf '%s\n' "$sshmgr_line" >> "$sshmgr_map_candidate"
done < /etc/sshmgr/auto_share
mv "$sshmgr_map_candidate" /etc/sshmgr/auto_share
sshmgr_map_candidate=''
if [ "$sshmgr_write_revoked_marker" = yes ]; then
  install -d -m 700 /etc/sshmgr
  printf 'sshmgr-share=%s;sshmgr-version=%s;sshmgr-state=revoked\n' \
    "$sshmgr_expected_share" "$sshmgr_write_version" > ` + revokedMarker + `
  chown root:wheel ` + revokedMarker + `
  chmod 400 ` + revokedMarker + `
fi
if ! grep -q '^# sshmgr-share=' /etc/sshmgr/auto_share; then
  sshmgr_auto_candidate=/etc/auto_master.sshmgr.$$
  : > "$sshmgr_auto_candidate"
  while IFS= read -r sshmgr_line || [ -n "$sshmgr_line" ]; do
    [ "$sshmgr_line" = '/mnt/sshmgr /etc/sshmgr/auto_share' ] && continue
    printf '%s\n' "$sshmgr_line" >> "$sshmgr_auto_candidate"
  done < /etc/auto_master
  mv "$sshmgr_auto_candidate" /etc/auto_master
  sshmgr_auto_candidate=''
fi

` + darwinProcessListScript() + mountTableCheckScript(CheckSystemDarwin) + `if [ "$sshmgr_mounted" = yes ]; then
  sshmgr_processes=$(sshmgr_list_processes || true)
  if [ -n "$sshmgr_processes" ] && [ "$sshmgr_force" = no ]; then
    echo ` + darwinUnmountMarker + ` result=busy version=$sshmgr_write_version
    printf '%s\n' "$sshmgr_processes"
    exit 0
  fi
  if [ "$sshmgr_force" = yes ]; then
    if ! diskutil unmount force "$mount_point" >/dev/null 2>&1; then
      echo ` + darwinUnmountMarker + ` force-unmount-failed >&2
      exit 85
    fi
  else
    if ! umount "$mount_point" >/dev/null 2>&1; then
      echo ` + darwinUnmountMarker + ` unmount-failed >&2
      exit 85
    fi
  fi
fi
automount -cv >/dev/null 2>&1
echo ` + darwinUnmountMarker + ` result=success version=$sshmgr_write_version
`
}

func darwinProcessListScript() string {
	return `sshmgr_list_processes() {
  command -v lsof >/dev/null 2>&1 || return 0
  for sshmgr_pid in $(lsof -t "$mount_point" 2>/dev/null); do
    sshmgr_user=$(ps -o user= -p "$sshmgr_pid" 2>/dev/null || true)
    sshmgr_command=$(ps -o comm= -p "$sshmgr_pid" 2>/dev/null || true)
    [ -n "$sshmgr_user" ] || sshmgr_user=unknown
    [ -n "$sshmgr_command" ] || sshmgr_command=unknown
    printf 'SSHMGR-PROCESS pid=%s user=%s command=%s\n' "$sshmgr_pid" "$sshmgr_user" "$sshmgr_command"
  done
}
`
}

func parseUnmountOutput(stdout, marker, system string) (*UnmountResult, error) {
	result := &UnmountResult{System: system}
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 {
			continue
		}
		if fields[0] == marker {
			for _, field := range fields[1:] {
				key, value, _ := strings.Cut(field, "=")
				switch key {
				case "result":
					result.Result = value
				case "version":
					version, err := strconv.ParseInt(value, 10, 64)
					if err != nil {
						return nil, fmt.Errorf("%w：固定卸载命令返回了不合法版本号 %q",
							ErrUnmountOutputInvalid, value)
					}
					result.Version = version
				}
			}
			continue
		}
		if fields[0] != "SSHMGR-PROCESS" {
			continue
		}
		process := BusyProcess{}
		for _, field := range fields[1:] {
			key, value, _ := strings.Cut(field, "=")
			switch key {
			case "pid":
				process.ProcessID = value
			case "user":
				process.User = value
			case "command":
				process.Command = value
			}
		}
		result.BusyProcesses = append(result.BusyProcesses, process)
	}
	if result.Result == "" {
		return nil, fmt.Errorf("%w：%s 固定卸载命令缺少结果行",
			ErrUnmountOutputInvalid, system)
	}
	if result.Result != "success" && result.Result != "busy" {
		return nil, fmt.Errorf("%w：%s 固定卸载命令返回了未知结果 %q",
			ErrUnmountOutputInvalid, system, result.Result)
	}
	if result.Version <= 0 {
		return nil, fmt.Errorf("%w：%s 固定卸载命令缺少版本号",
			ErrUnmountOutputInvalid, system)
	}
	return result, nil
}
