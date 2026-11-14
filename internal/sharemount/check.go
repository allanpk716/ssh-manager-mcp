package sharemount

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sshbroker"
)

const (
	linuxCheckMarker  = "SSHMGR-LINUX-CHECK"
	darwinCheckMarker = "SSHMGR-DARWIN-CHECK"
	// DefaultCheckTimeout 是单台服务器现场检测的固定超时。检测只读远端
	// 配置与挂载点，不需要挂载流程那样长的窗口。
	DefaultCheckTimeout = 30 * time.Second
)

// 检测结论里的操作系统标识。
const (
	CheckSystemLinux   = "linux"
	CheckSystemDarwin  = "darwin"
	CheckSystemWindows = "windows"
)

// ErrCheckOutputInvalid 表示远端固定检测命令的输出不符合协议。调用方应
// 把它视作这台服务器本次无法给出可靠结论。
var ErrCheckOutputInvalid = errors.New("现场检测固定命令输出不合法")

// CheckShareState 是一台服务器上一个共享的只读检测结论。Managed 表示远
// 端持久配置里存在这个共享的 sshmgr 标记条目；Version 是标记里的共享存
// 储版本号；Mounted 表示挂载点在远端可以列出内容。
type CheckShareState struct {
	Managed bool
	Version int64
	Mounted bool
}

// CheckResult 是一台服务器的现场检测结论。Restricted 表示服务器条目没有
// sudo 凭据，只执行了不读取 root 文件的受限检测；AutoMasterPresent 只在
// macOS 上有意义，表示 /etc/auto_master 仍引用 sshmgr 的间接映射文件。
type CheckResult struct {
	System            string
	Restricted        bool
	AutoMasterPresent bool
	States            []CheckShareState
}

// CheckRequest 是一次现场检测请求：一台服务器和它上面需要检测的共享。
// 检测不读取挂载密码，也不向远端写入任何内容。
type CheckRequest struct {
	Server *models.Server
	Shares []*models.SharedStorage
}

// Check 在目标服务器上执行只读固定检测命令：先识别系统，再按系统与
// sudo 配置选择全量或受限命令。命令不触碰文件锁，不改写远端文件，也
// 不把地址或密码放进输出。
func (s *Service) Check(parent context.Context, request CheckRequest) (*CheckResult, error) {
	if s == nil || (s.Store == nil && s.RemoteExec == nil) {
		return nil, fmt.Errorf("共享存储挂载服务没有绑定存储")
	}
	if request.Server == nil || len(request.Shares) == 0 {
		return nil, fmt.Errorf("共享存储现场检测参数不完整")
	}
	timeout := s.CheckTimeout
	if timeout <= 0 {
		timeout = DefaultCheckTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	systemResult, err := s.exec(ctx, request.Server, RemoteExecRequest{Command: "uname -s"})
	if err != nil {
		return nil, &CheckFailure{Reason: CheckFailureSSHConnection, Err: fmt.Errorf(
			"识别目标服务器操作系统失败：%w", sanitizedExecError(err))}
	}
	full := request.Server.SudoCredentialID != ""
	switch strings.TrimSpace(systemResult.Stdout) {
	case "Linux":
		return s.runCheck(ctx, request, linuxCheckMarker, CheckSystemLinux,
			func(shares []*models.SharedStorage) string { return buildLinuxShareCheckCommand(shares) }, full)
	case "Darwin":
		return s.runCheck(ctx, request, darwinCheckMarker, CheckSystemDarwin,
			func(shares []*models.SharedStorage) string { return buildDarwinShareCheckCommand(shares) }, full)
	default:
		return nil, &CheckFailure{Reason: CheckFailureUnsupportedSystem, Err: ErrUnsupportedSystem}
	}
}

func (s *Service) runCheck(
	ctx context.Context, request CheckRequest, marker, system string,
	fullCommand func([]*models.SharedStorage) string, full bool,
) (*CheckResult, error) {
	command := fullCommand(request.Shares)
	if !full {
		command = buildLimitedShareCheckCommand(system, request.Shares)
	}
	result, err := s.exec(ctx, request.Server, RemoteExecRequest{
		Command: command, Privileged: full,
	})
	if err != nil {
		var sudoFailure *RemoteSudoFailure
		if errors.As(err, &sudoFailure) {
			return nil, &CheckFailure{Reason: CheckFailureSudo, Err: fmt.Errorf(
				"现场检测固定命令执行失败：%w", err)}
		}
		return nil, &CheckFailure{Reason: CheckFailureSSHConnection, Err: fmt.Errorf(
			"现场检测固定命令执行失败：%w", sanitizedExecError(err))}
	}
	if result.ExitCode != 0 {
		return nil, &CheckFailure{Reason: CheckFailureSSHConnection, Err: fmt.Errorf(
			"现场检测固定命令退出码 %d", result.ExitCode)}
	}
	states, autoMaster, err := parseShareCheckOutput(result.Stdout, marker, request.Shares, full)
	if err != nil {
		return nil, err
	}
	return &CheckResult{
		System: system, Restricted: !full, AutoMasterPresent: autoMaster, States: states,
	}, nil
}

// buildLinuxShareCheckCommand 生成 Linux 全量检测命令：逐个共享在
// /etc/fstab 里找 sshmgr 标记条目，并按挂载表核验挂载点。命令只读，
// 不拿锁，也不输出地址或挂载账号。
func buildLinuxShareCheckCommand(shares []*models.SharedStorage) string {
	var command strings.Builder
	command.WriteString("set -eu\necho '" + linuxCheckMarker + " mode=full'\n")
	command.WriteString(linuxFstabOptionParserScript())
	for index, share := range shares {
		fmt.Fprintf(&command, linuxShareFstabScanBlock,
			sshbroker.POSIXShellQuote(url.PathEscape(share.Name)))
		fmt.Fprintf(&command, "mount_point=%s\n", sshbroker.POSIXShellQuote(linuxMountPoint(share.Name)))
		command.WriteString(mountTableCheckScript(CheckSystemLinux))
		fmt.Fprintf(&command, "echo \"%s index=%d managed=$sshmgr_managed share=$sshmgr_share version=$sshmgr_version mounted=$sshmgr_mounted\"\n",
			linuxCheckMarker, index+1)
	}
	return command.String()
}

// linuxShareFstabScanBlock 是单个共享的 Linux fstab 扫描块。共享名匹配值
// 使用标记的百分号编码，必须作为格式化参数传入，不能拼进格式串。
const linuxShareFstabScanBlock = `sshmgr_managed=no
sshmgr_share=''
sshmgr_version=''
if [ -f /etc/fstab ]; then
  while IFS= read -r sshmgr_line || [ -n "$sshmgr_line" ]; do
    case "$sshmgr_line" in '#'*|'') continue ;; esac
    set -f
    set -- $sshmgr_line
    set +f
    [ "$#" -eq 6 ] || continue
    case "$1" in //*) ;; *) continue ;; esac
    [ "$3" = cifs ] || continue
    sshmgr_parse_options "$4"
    [ "$sshmgr_option_share" = %s ] || continue
    sshmgr_managed=yes
    sshmgr_share=$sshmgr_option_share
    sshmgr_version=$sshmgr_option_version
  done < /etc/fstab
fi
`

// buildDarwinShareCheckCommand 生成 macOS 全量检测命令：逐个共享在
// /etc/sshmgr/auto_share 里找 sshmgr 标记行，验证挂载点，并检查
// /etc/auto_master 是否仍引用 sshmgr 的间接映射文件。命令只读。
func buildDarwinShareCheckCommand(shares []*models.SharedStorage) string {
	var command strings.Builder
	command.WriteString("set -eu\necho '" + darwinCheckMarker + " mode=full'\n")
	for index, share := range shares {
		fmt.Fprintf(&command, darwinShareMapScanBlock,
			sshbroker.POSIXShellQuote(url.PathEscape(share.Name)))
		fmt.Fprintf(&command, "mount_point=%s\n", sshbroker.POSIXShellQuote(darwinMountPoint(share.Name)))
		command.WriteString(mountTableCheckScript(CheckSystemDarwin))
		fmt.Fprintf(&command, "echo \"%s index=%d managed=$sshmgr_managed share=$sshmgr_share version=$sshmgr_version mounted=$sshmgr_mounted\"\n",
			darwinCheckMarker, index+1)
	}
	command.WriteString(darwinAutoMasterCheckBlock)
	return command.String()
}

// darwinShareMapScanBlock 是单个共享的 macOS 间接映射扫描块；共享名匹配
// 值同样按标记编码后作为格式化参数传入。
const darwinShareMapScanBlock = `sshmgr_managed=no
sshmgr_share=''
sshmgr_version=''
if [ -f '` + darwinMapFile + `' ]; then
  while IFS= read -r sshmgr_line || [ -n "$sshmgr_line" ]; do
    case "$sshmgr_line" in '# sshmgr-share='*) ;; *) continue ;; esac
    sshmgr_marker=${sshmgr_line#"# "}
    sshmgr_marker_share=${sshmgr_marker%%;*}
    sshmgr_marker_version=${sshmgr_marker##*;}
    sshmgr_marker_share=${sshmgr_marker_share#sshmgr-share=}
    sshmgr_marker_version=${sshmgr_marker_version#sshmgr-version=}
    [ "$sshmgr_marker_share" = %s ] || continue
    sshmgr_managed=yes
    sshmgr_share=$sshmgr_marker_share
    sshmgr_version=$sshmgr_marker_version
  done < '` + darwinMapFile + `'
fi
`

// darwinAutoMasterCheckBlock 检查 /etc/auto_master 是否仍引用 sshmgr 的
// 间接映射文件；系统升级重置主配置后这里会给出 no。
const darwinAutoMasterCheckBlock = `sshmgr_auto_master=no
if [ -f '` + darwinAutoMaster + `' ]; then
  while IFS= read -r sshmgr_line || [ -n "$sshmgr_line" ]; do
    set -f
    set -- $sshmgr_line
    set +f
    if [ "$#" -ge 2 ] && [ "$1" = '` + darwinMountRoot + `' ] &&
       [ "$2" = '` + darwinMapFile + `' ]; then
      sshmgr_auto_master=yes
    fi
  done < '` + darwinAutoMaster + `'
fi
echo "` + darwinCheckMarker + ` auto-master=$sshmgr_auto_master"
`

// buildLimitedShareCheckCommand 生成没有 sudo 凭据时的受限检测命令：只能
// 按挂载表核验每个挂载点，不能读取 root 才能读的持久配置。
func buildLimitedShareCheckCommand(system string, shares []*models.SharedStorage) string {
	marker := linuxCheckMarker
	if system == CheckSystemDarwin {
		marker = darwinCheckMarker
	}
	var command strings.Builder
	command.WriteString("set -eu\necho '" + marker + " mode=limited'\n")
	for index, share := range shares {
		mountPoint := linuxMountPoint(share.Name)
		if system == CheckSystemDarwin {
			mountPoint = darwinMountPoint(share.Name)
		}
		fmt.Fprintf(&command, "mount_point=%s\n", sshbroker.POSIXShellQuote(mountPoint))
		command.WriteString(mountTableCheckScript(system))
		fmt.Fprintf(&command, `echo "%s index=%d mounted=$sshmgr_mounted"
`, marker, index+1)
	}
	return command.String()
}

// parseShareCheckOutput 解析固定检测命令的安全输出行。full 为 true 时要求
// 每个共享都有配置结论（并按跨操作系统标记格式校验共享名与版本号），为
// false 时只解析受限检测的挂载点结论。第二个返回值是 macOS 主配置引用
// 是否存在；Linux 恒为 false。
func parseShareCheckOutput(
	stdout, marker string, shares []*models.SharedStorage, full bool,
) (states []CheckShareState, autoMaster bool, err error) {
	type parsedShare struct {
		seen    bool
		managed string
		share   string
		version string
		mounted string
	}
	mode := ""
	parsed := make([]parsedShare, len(shares))
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Fields(strings.TrimRight(line, "\r"))
		if len(fields) == 0 || fields[0] != marker {
			continue
		}
		current := parsedShare{}
		index := 0
		for _, field := range fields[1:] {
			value := ""
			if eq := strings.IndexByte(field, '='); eq >= 0 {
				value = field[eq+1:]
				field = field[:eq]
			}
			switch field {
			case "mode":
				mode = value
			case "auto-master":
				switch value {
				case "yes":
					autoMaster = true
				case "no":
					autoMaster = false
				default:
					return nil, false, fmt.Errorf("%w：非法主配置引用结论 %q", ErrCheckOutputInvalid, value)
				}
			case "index":
				parsedIndex, parseErr := strconv.Atoi(value)
				if parseErr != nil || parsedIndex < 1 || parsedIndex > len(shares) {
					return nil, false, fmt.Errorf("%w：非法共享序号 %q", ErrCheckOutputInvalid, value)
				}
				index = parsedIndex - 1
				current.seen = true
			case "managed":
				current.managed = value
			case "share":
				current.share = value
			case "version":
				current.version = value
			case "mounted":
				current.mounted = value
			default:
				return nil, false, fmt.Errorf("%w：未知字段 %q", ErrCheckOutputInvalid, field)
			}
		}
		if index == 0 && !current.seen {
			continue
		}
		if !current.seen {
			return nil, false, fmt.Errorf("%w：缺少共享序号", ErrCheckOutputInvalid)
		}
		if parsed[index].seen {
			return nil, false, fmt.Errorf("%w：共享序号 %d 重复", ErrCheckOutputInvalid, index+1)
		}
		parsed[index] = current
	}
	wantMode := "limited"
	if full {
		wantMode = "full"
	}
	if mode != wantMode {
		return nil, false, fmt.Errorf("%w：缺少或错误的模式行 %q", ErrCheckOutputInvalid, mode)
	}
	states = make([]CheckShareState, len(shares))
	for index, item := range parsed {
		if !item.seen {
			return nil, false, fmt.Errorf("%w：缺少共享序号 %d 的结论", ErrCheckOutputInvalid, index+1)
		}
		switch item.mounted {
		case "yes":
			states[index].Mounted = true
		case "no":
			states[index].Mounted = false
		default:
			return nil, false, fmt.Errorf("%w：非法挂载结论 %q", ErrCheckOutputInvalid, item.mounted)
		}
		if !full {
			continue
		}
		switch item.managed {
		case "yes":
			marker, markerErr := parseSharedStorageMarkerOptions(
				"sshmgr-share=" + item.share + ";sshmgr-version=" + item.version)
			if markerErr != nil {
				return nil, false, fmt.Errorf("%w：共享序号 %d 的标记不合法", ErrCheckOutputInvalid, index+1)
			}
			if marker.Share != shares[index].Name {
				return nil, false, fmt.Errorf("%w：共享序号 %d 的共享名不匹配", ErrCheckOutputInvalid, index+1)
			}
			states[index].Managed = true
			states[index].Version = marker.Version
		case "no":
			if item.share != "" || item.version != "" {
				return nil, false, fmt.Errorf("%w：无配置时不应携带标记字段", ErrCheckOutputInvalid)
			}
		default:
			return nil, false, fmt.Errorf("%w：非法配置结论 %q", ErrCheckOutputInvalid, item.managed)
		}
	}
	return states, autoMaster, nil
}
