package sharemount

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"ssh-manager-mcp/internal/models"
)

const (
	linuxEnumerateMarker  = "SSHMGR-LINUX-ENUM"
	darwinEnumerateMarker = "SSHMGR-DARWIN-ENUM"
)

// ErrEnumerateSSH 表示连接目标服务器或执行只读枚举命令失败。
var ErrEnumerateSSH = errors.New("连接目标服务器或执行共享存储枚举命令失败")

// ErrEnumerateOutputInvalid 表示只读枚举命令的输出不符合协议。
var ErrEnumerateOutputInvalid = errors.New("共享存储枚举命令输出不合法")

// EnumerateShareState 是枚举到的一个共享在服务器上的现场状态。Share 是
// 解码后的共享名；其余字段描述哪几类 sshmgr 痕迹仍然存在。
type EnumerateShareState struct {
	Share            string
	Version          int64
	HasConfigEntry   bool
	HasCredential    bool
	HasRevokedMarker bool
	HasMount         bool
}

// EnumerateResult 是一台服务器上全部 sshmgr 共享痕迹的只读枚举结论。
type EnumerateResult struct {
	System string
	States []EnumerateShareState
}

// Enumerate 在目标服务器上执行只读固定枚举命令。与按共享逐个询问的现场
// 检测不同，这里不依赖登记清单：直接枚举持久配置里的 sshmgr 标记、Linux
// 凭据文件、撤销标记与 /mnt/sshmgr 下的现有挂载。命令不触碰文件锁，也
// 不改写远端文件。
func (s *Service) Enumerate(parent context.Context, server *models.Server) (*EnumerateResult, error) {
	if s == nil || (s.Store == nil && s.RemoteExec == nil) {
		return nil, errors.New("共享存储挂载服务没有绑定存储")
	}
	if server == nil {
		return nil, errors.New("共享存储枚举参数不完整")
	}
	timeout := s.CheckTimeout
	if timeout <= 0 {
		timeout = DefaultCheckTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	systemResult, err := s.exec(ctx, server, RemoteExecRequest{Command: "uname -s"})
	if err != nil {
		return nil, fmt.Errorf("%w：识别目标服务器操作系统失败：%w",
			ErrEnumerateSSH, sanitizedExecError(err))
	}
	if server.SudoCredentialID == "" {
		return nil, ErrNoSudo
	}
	switch strings.TrimSpace(systemResult.Stdout) {
	case "Linux":
		return s.enumerateSystem(ctx, server, buildLinuxEnumerateCommand(),
			linuxEnumerateMarker, CheckSystemLinux)
	case "Darwin":
		return s.enumerateSystem(ctx, server, buildDarwinEnumerateCommand(),
			darwinEnumerateMarker, CheckSystemDarwin)
	default:
		return nil, ErrUnsupportedSystem
	}
}

func (s *Service) enumerateSystem(
	ctx context.Context, server *models.Server, command, marker, system string,
) (*EnumerateResult, error) {
	result, err := s.exec(ctx, server, RemoteExecRequest{Command: command, Privileged: true})
	if err != nil {
		return nil, fmt.Errorf("%w：%s 固定枚举命令执行失败：%w",
			ErrEnumerateSSH, system, sanitizedExecError(err))
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("%w：%s 固定枚举命令退出码 %d",
			ErrEnumerateSSH, system, result.ExitCode)
	}
	states, err := parseEnumerateOutput(result.Stdout, marker)
	if err != nil {
		return nil, err
	}
	return &EnumerateResult{System: system, States: states}, nil
}

// buildLinuxEnumerateCommand 生成 Linux 只读枚举命令：逐条解析 /etc/fstab
// 里的 sshmgr 标记条目并核验其凭据文件，列出 /proc/mounts（读不到时退回
// mount 输出）里 /mnt/sshmgr 下的 cifs 挂载，再读 sshmgr 目录下的撤销标
// 记。命令不输出地址、账号或密码。
func buildLinuxEnumerateCommand() string {
	return `set -eu
` + linuxFstabOptionParserScript() + `if [ -f /etc/fstab ]; then
  while IFS= read -r sshmgr_line || [ -n "$sshmgr_line" ]; do
    case "$sshmgr_line" in '#'*|'') continue ;; esac
    set -f
    set -- $sshmgr_line
    set +f
    [ "$#" -eq 6 ] || continue
    case "$1" in //*) ;; *) continue ;; esac
    [ "$3" = cifs ] || continue
    case "$2" in /mnt/sshmgr/?*) ;; *) continue ;; esac
    sshmgr_parse_options "$4"
    [ -n "$sshmgr_option_share" ] || continue
    case "$sshmgr_option_version" in ''|*[!0-9]*) continue ;; esac
    sshmgr_entry_share=$sshmgr_option_share
    sshmgr_entry_version=$sshmgr_option_version
    sshmgr_credentials=no
    sshmgr_saved_ifs=$IFS
    IFS=,
    set -f
    set -- $4
    set +f
    IFS=$sshmgr_saved_ifs
    for sshmgr_option in "$@"; do
      case "$sshmgr_option" in credentials=/etc/sshmgr/shares/*.credentials)
        if [ -f "${sshmgr_option#credentials=}" ]; then
          sshmgr_credentials=yes
        fi
      ;; esac
    done
    printf '%s entry=%s version=%s credentials=%s\n' '` + linuxEnumerateMarker + `' \
      "$sshmgr_entry_share" "$sshmgr_entry_version" "$sshmgr_credentials"
  done < /etc/fstab
fi
if [ -r /proc/mounts ]; then
  while IFS= read -r sshmgr_mount_entry || [ -n "$sshmgr_mount_entry" ]; do
    set -f
    set -- $sshmgr_mount_entry
    set +f
    [ "$#" -ge 3 ] || continue
    [ "$3" = cifs ] || continue
    case "$2" in /mnt/sshmgr/?*) ;; *) continue ;; esac
    printf '%s mount=%s\n' '` + linuxEnumerateMarker + `' "${2#/mnt/sshmgr/}"
  done < /proc/mounts
else
  mount 2>/dev/null | while IFS= read -r sshmgr_mount_entry || [ -n "$sshmgr_mount_entry" ]; do
    set -f
    set -- $sshmgr_mount_entry
    set +f
    [ "$#" -ge 5 ] || continue
    [ "$2" = on ] || continue
    [ "$5" = cifs ] || continue
    case "$3" in /mnt/sshmgr/?*) ;; *) continue ;; esac
    printf '%s mount=%s\n' '` + linuxEnumerateMarker + `' "${3#/mnt/sshmgr/}"
  done
fi
` + revokedMarkerEnumerateScript("/etc/sshmgr/shares/*.revoked", linuxEnumerateMarker)
}

// buildDarwinEnumerateCommand 生成 macOS 只读枚举命令：读取 autofs 映射
// 文件里的 sshmgr 标记行、mount 输出里 /mnt/sshmgr 下的 smbfs 挂载与
// sshmgr 目录下的撤销标记。命令不输出地址、账号或密码。
func buildDarwinEnumerateCommand() string {
	return `set -eu
if [ -f /etc/sshmgr/auto_share ]; then
  while IFS= read -r sshmgr_line || [ -n "$sshmgr_line" ]; do
    case "$sshmgr_line" in '# sshmgr-share='*';sshmgr-version='*[0-9]) ;; *) continue ;; esac
    sshmgr_marker=${sshmgr_line#"# "}
    sshmgr_marker_share=${sshmgr_marker%%;*}
    sshmgr_marker_share=${sshmgr_marker_share#sshmgr-share=}
    sshmgr_marker_version=${sshmgr_marker##*;}
    sshmgr_marker_version=${sshmgr_marker_version#sshmgr-version=}
    case "$sshmgr_marker_version" in ''|*[!0-9]*) continue ;; esac
    printf '%s entry=%s version=%s\n' '` + darwinEnumerateMarker + `' \
      "$sshmgr_marker_share" "$sshmgr_marker_version"
  done < /etc/sshmgr/auto_share
fi
mount | while IFS= read -r sshmgr_mount_entry || [ -n "$sshmgr_mount_entry" ]; do
  set -f
  set -- $sshmgr_mount_entry
  set +f
  [ "$#" -ge 4 ] || continue
  [ "$2" = on ] || continue
  case "$3" in
    /mnt/sshmgr/?*|/System/Volumes/Data/mnt/sshmgr/?*) ;;
    *) continue ;;
  esac
  case "$4" in '(smbfs'*) ;; *) continue ;; esac
  sshmgr_mount_share=${3#'/System/Volumes/Data/mnt/sshmgr/'}
  sshmgr_mount_share=${sshmgr_mount_share#'/mnt/sshmgr/'}
  printf '%s mount=%s\n' '` + darwinEnumerateMarker + `' "$sshmgr_mount_share"
done
` + revokedMarkerEnumerateScript("/etc/sshmgr/*.revoked", darwinEnumerateMarker)
}

// revokedMarkerEnumerateScript 生成撤销标记枚举片段。标记内容本身使用百
// 分号编码的共享名，输出原样保留这一编码，由 Go 侧统一解码。
func revokedMarkerEnumerateScript(glob, marker string) string {
	return `for sshmgr_file in ` + glob + `; do
  [ -f "$sshmgr_file" ] || continue
  sshmgr_marker=$(head -n 1 "$sshmgr_file" 2>/dev/null || true)
  case "$sshmgr_marker" in
    'sshmgr-share='*';sshmgr-version='*[0-9]';sshmgr-state=revoked')
      sshmgr_marker_share=${sshmgr_marker#sshmgr-share=}
      sshmgr_marker_share=${sshmgr_marker_share%%;*}
      sshmgr_marker_version=${sshmgr_marker#*';sshmgr-version='}
      sshmgr_marker_version=${sshmgr_marker_version%%';*'}
      case "$sshmgr_marker_version" in ''|*[!0-9]*) continue ;; esac
      printf '%s revoked=%s version=%s\n' '` + marker + `' \
        "$sshmgr_marker_share" "$sshmgr_marker_version"
    ;;
  esac
done
`
}

// parseEnumerateOutput 解析只读枚举命令的安全输出行。entry 与 revoked 携
// 带百分号编码的共享名；mount 携带挂载点路径里的原始共享名。同一共享的
// 多条来源在解析时合并。
func parseEnumerateOutput(stdout, marker string) ([]EnumerateShareState, error) {
	states := make(map[string]*EnumerateShareState)
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Fields(strings.TrimRight(line, "\r"))
		if len(fields) == 0 || fields[0] != marker {
			continue
		}
		if len(fields) < 2 {
			return nil, fmt.Errorf("%w：缺少枚举字段：%q", ErrEnumerateOutputInvalid, line)
		}
		key, value, found := strings.Cut(fields[1], "=")
		if !found {
			return nil, fmt.Errorf("%w：未知枚举字段 %q", ErrEnumerateOutputInvalid, fields[1])
		}
		encoded := key == "entry" || key == "revoked"
		shareName := value
		if encoded {
			decoded, err := url.PathUnescape(value)
			if err != nil || decoded == "" {
				return nil, fmt.Errorf("%w：共享名编码不合法 %q", ErrEnumerateOutputInvalid, value)
			}
			shareName = decoded
		}
		state := states[shareName]
		if state == nil {
			state = &EnumerateShareState{Share: shareName}
			states[shareName] = state
		}
		switch key {
		case "entry":
			state.HasConfigEntry = true
		case "revoked":
			state.HasRevokedMarker = true
		case "mount":
			state.HasMount = true
		default:
			return nil, fmt.Errorf("%w：未知枚举字段 %q", ErrEnumerateOutputInvalid, key)
		}
		for _, field := range fields[2:] {
			fieldKey, fieldValue, _ := strings.Cut(field, "=")
			switch fieldKey {
			case "version":
				version, err := strconv.ParseInt(fieldValue, 10, 64)
				if err != nil || version < 0 {
					return nil, fmt.Errorf("%w：不合法版本号 %q",
						ErrEnumerateOutputInvalid, fieldValue)
				}
				if version > state.Version {
					state.Version = version
				}
			case "credentials":
				switch fieldValue {
				case "yes":
					state.HasCredential = true
				case "no":
				default:
					return nil, fmt.Errorf("%w：不合法凭据结论 %q",
						ErrEnumerateOutputInvalid, fieldValue)
				}
			default:
				return nil, fmt.Errorf("%w：未知枚举字段 %q", ErrEnumerateOutputInvalid, field)
			}
		}
	}
	out := make([]EnumerateShareState, 0, len(states))
	for _, state := range states {
		out = append(out, *state)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Share < out[j].Share })
	return out, nil
}
