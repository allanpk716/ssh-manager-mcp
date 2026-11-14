package sharemount

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"ssh-manager-mcp/internal/models"
)

const (
	linuxReselectMarker  = "SSHMGR-LINUX-RESELECT"
	darwinReselectMarker = "SSHMGR-DARWIN-RESELECT"
)

var (
	ErrReselectSSH           = errors.New("连接目标服务器或执行固定重选地址命令失败")
	ErrReselectNotManaged    = ErrUnmountNotManaged
	ErrReselectLockTimeout   = ErrUnmountLockTimeout
	ErrReselectUnmountFailed = errors.New("卸载共享存储挂载点失败，已保留旧配置")
	ErrReselectOutputInvalid = errors.New("共享存储固定重选地址命令返回了不合法结论")
)

// ReselectRequest 是 owner 重选共享地址的远端目标。Password 只在进程内传
// 递，并仅写入提权会话标准输入。
type ReselectRequest struct {
	Server   *models.Server
	Share    *models.SharedStorage
	Password []byte
	Force    bool
}

// ReselectResult 是固定重选地址命令的安全结论，不包含地址、账号或密码。
type ReselectResult struct {
	System          string
	Result          string
	BusyProcesses   []BusyProcess
	Network         string
	NetworkSequence int
	Physical        bool
}

func buildLinuxReselectCommand(
	loginUser string, share *models.SharedStorage, candidates []*Candidate, force bool,
) string {
	command := buildLinuxMountCommandWithOptions(share, candidates, mountCommandOptions{
		PreferFirstCandidate: true, Reselect: true, Force: force, LoginUser: loginUser,
	})
	return strings.ReplaceAll(command, linuxMountMarker, linuxReselectMarker)
}

func buildDarwinReselectCommand(
	loginUser string, share *models.SharedStorage, candidates []*Candidate, force bool,
) string {
	command := buildDarwinMountCommandWithOptions(share, candidates, mountCommandOptions{
		PreferFirstCandidate: true, Reselect: true, Force: force,
		LoginUser: loginUser,
	})
	return strings.ReplaceAll(command, darwinMountMarker, darwinReselectMarker)
}

// Reselect 在同一把共享存储锁内改写配置、卸载旧挂载并按候选列表的第一个
// 可达地址重新挂载。候选探测与 mount_share 完全共用同一实现。
func (s *Service) Reselect(parent context.Context, request ReselectRequest) (*ReselectResult, error) {
	if s == nil || s.Store == nil {
		return nil, errors.New("共享存储挂载服务没有绑定存储")
	}
	if request.Server == nil || request.Share == nil || len(request.Password) == 0 {
		return nil, errors.New("共享存储重选地址参数不完整")
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	systemResult, err := s.Exec(ctx, request.Server, RemoteExecRequest{Command: "uname -s"})
	if err != nil {
		return nil, fmt.Errorf("%w：识别目标服务器操作系统失败：%w",
			ErrReselectSSH, sanitizedExecError(err))
	}
	if request.Server.SudoCredentialID == "" {
		return nil, ErrNoSudo
	}
	var marker, systemName string
	var command string
	switch strings.TrimSpace(systemResult.Stdout) {
	case "Linux":
		systemName = CheckSystemLinux
	case "Darwin":
		systemName = CheckSystemDarwin
	default:
		return nil, ErrUnsupportedSystem
	}
	selection, selectionErr := s.selectCandidate(ctx, request.Server, request.Share)
	if selectionErr != nil {
		return nil, selectionErr
	}
	switch systemName {
	case CheckSystemLinux:
		marker = linuxReselectMarker
		command = buildLinuxReselectCommand(
			request.Server.User, request.Share, selection.Reachable, request.Force)
	case CheckSystemDarwin:
		marker = darwinReselectMarker
		command = buildDarwinReselectCommand(
			request.Server.User, request.Share, selection.Reachable, request.Force)
	}
	standardInput := append([]byte(nil), request.Password...)
	standardInput = append(standardInput, '\n')
	result, execErr := s.Exec(ctx, request.Server, RemoteExecRequest{
		Command: command, Privileged: true, StandardInput: standardInput,
	})
	if execErr != nil {
		return nil, fmt.Errorf("%w：%s 固定重选地址命令执行失败：%w",
			ErrReselectSSH, systemName, sanitizedExecError(execErr))
	}
	if result.ExitCode != 0 {
		return nil, reselectExitCodeError(systemName, result.ExitCode)
	}
	parsed, parseErr := parseReselectOutput(result.Stdout, marker, systemName)
	if parseErr != nil {
		return nil, parseErr
	}
	selected := selection.Reachable[0]
	parsed.Network = selected.Network
	parsed.NetworkSequence = selected.Sequence
	parsed.Physical = selected.Physical
	return parsed, nil
}

func reselectExitCodeError(systemName string, exitCode int) error {
	if exitCode == 90 {
		if systemName == "Linux" {
			return linuxMountExitCodeError(exitCode)
		}
		return darwinMountExitCodeError(exitCode)
	}
	switch exitCode {
	case 85:
		return ErrReselectUnmountFailed
	case 87:
		return ErrReselectLockTimeout
	case 89:
		return ErrReselectNotManaged
	default:
		return fmt.Errorf("%s 固定重选地址命令失败（退出码 %d）", systemName, exitCode)
	}
}

func parseReselectOutput(stdout, marker, systemName string) (*ReselectResult, error) {
	result := &ReselectResult{System: systemName}
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 {
			continue
		}
		if fields[0] == marker {
			for _, field := range fields[1:] {
				key, value, _ := strings.Cut(field, "=")
				if key == "result" {
					result.Result = value
				}
			}
			continue
		}
		if fields[0] != "SSHMGR-PROCESS" {
			continue
		}
		result.BusyProcesses = append(result.BusyProcesses, parseBusyProcess(fields))
	}
	if result.Result == "" {
		return nil, fmt.Errorf("%w：%s 固定重选地址命令缺少结果行",
			ErrReselectOutputInvalid, systemName)
	}
	switch result.Result {
	case "no-change", "first-mount-success", "update-success", "remount-success", "busy":
	default:
		return nil, fmt.Errorf("%w：%s 固定重选地址命令返回了未知结果 %q",
			ErrReselectOutputInvalid, systemName, result.Result)
	}
	return result, nil
}

func linuxReselectBusyScript(options mountCommandOptions) string {
	if !options.Reselect {
		return ""
	}
	force := "no"
	if options.Force {
		force = "yes"
	}
	return `
` + linuxProcessListScript() + mountTableCheckScript(CheckSystemLinux) + `if [ "$sshmgr_mounted" = yes ]; then
  sshmgr_processes=$(sshmgr_list_processes || true)
  if [ -n "$sshmgr_processes" ] && [ "` + force + `" = no ]; then
    echo ` + linuxReselectMarker + ` result=busy selected=$sshmgr_selected_index version=$sshmgr_expected_version
    printf '%s\n' "$sshmgr_processes"
    exit 0
  fi
  if [ "` + force + `" = yes ]; then
    if ! umount -f "$mount_point" >/dev/null 2>&1; then
      if ! umount -l "$mount_point" >/dev/null 2>&1; then
        echo ` + linuxReselectMarker + ` force-unmount-failed >&2
        exit 85
      fi
    fi
  fi
fi
`
}

func linuxReselectUnmountScript(options mountCommandOptions) string {
	if !options.Reselect {
		return "    umount \"$mount_point\" >/dev/null 2>&1 || true\n"
	}
	if options.Force {
		return "    :\n"
	}
	return mountTableCheckScript(CheckSystemLinux) + `    if [ "$sshmgr_mounted" = yes ] && ! umount "$mount_point" >/dev/null 2>&1; then
      echo ` + linuxReselectMarker + ` unmount-failed >&2
      exit 85
    fi
`
}

func darwinReselectBusyScript(options mountCommandOptions) string {
	if !options.Reselect {
		return ""
	}
	force := "no"
	if options.Force {
		force = "yes"
	}
	return `
` + darwinProcessListScript() + mountTableCheckScript(CheckSystemDarwin) + `if [ "$sshmgr_mounted" = yes ]; then
  sshmgr_processes=$(sshmgr_list_processes || true)
  if [ -n "$sshmgr_processes" ] && [ "` + force + `" = no ]; then
    echo ` + darwinReselectMarker + ` result=busy selected=$sshmgr_selected_index version=$sshmgr_expected_version
    printf '%s\n' "$sshmgr_processes"
    exit 0
  fi
  if [ "` + force + `" = yes ]; then
    if ! diskutil unmount force "$mount_point" >/dev/null 2>&1; then
      echo ` + darwinReselectMarker + ` force-unmount-failed >&2
      exit 85
    fi
  fi
fi
`
}

func darwinReselectUnmountScript(options mountCommandOptions) string {
	if !options.Reselect {
		return `if [ "$sshmgr_managed_count" -gt 0 ] && [ "$sshmgr_existing_configuration" = no ]; then
  ` + mountTableCheckScript(CheckSystemDarwin) + `  if [ "$sshmgr_mounted" = yes ]; then
    if ! umount "$mount_point" >/dev/null 2>&1; then
      if ! diskutil unmount force "$mount_point" >/dev/null 2>&1; then
        echo ` + darwinMountMarker + ` unmount-failed >&2
        exit 85
      fi
    fi
  fi
fi
`
	}
	if options.Force {
		return `if [ "$sshmgr_managed_count" -gt 0 ] && [ "$sshmgr_existing_configuration" = no ]; then
  ` + mountTableCheckScript(CheckSystemDarwin) + `  if [ "$sshmgr_mounted" = yes ] &&
     ! diskutil unmount force "$mount_point" >/dev/null 2>&1; then
    echo ` + darwinReselectMarker + ` force-unmount-failed >&2
    exit 85
  fi
fi
`
	}
	return `if [ "$sshmgr_managed_count" -gt 0 ] && [ "$sshmgr_existing_configuration" = no ]; then
  ` + mountTableCheckScript(CheckSystemDarwin) + `  if [ "$sshmgr_mounted" = yes ]; then
    if ! umount "$mount_point" >/dev/null 2>&1; then
      echo ` + darwinReselectMarker + ` unmount-failed >&2
      exit 85
    fi
  fi
fi
`
}
