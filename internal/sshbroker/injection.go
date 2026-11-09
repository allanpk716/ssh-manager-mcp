package sshbroker

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// ExecTargetLayer 表示注入发生在 SSH 会话的哪一层。普通层是登录 shell；
// 提权层是 sudo 之后的 shell。后续提权注入实现复用同一组选项。
type ExecTargetLayer string

const (
	// ExecLayerNormal 是当前已实现的普通层注入。
	ExecLayerNormal ExecTargetLayer = "normal"
	// ExecLayerPrivileged 供提权层注入扩展使用。
	ExecLayerPrivileged ExecTargetLayer = "privileged"
)

// ExecInjectionKind 是注入项类型。当前只有环境变量；后续 Windows 网络
// 盘符注入可以在同一列表中增加新类型。
type ExecInjectionKind string

const (
	// ExecInjectionEnvironment 把值导出为同名环境变量。
	ExecInjectionEnvironment ExecInjectionKind = "environment"
)

// ExecInjection 是一条会在用户命令前生效的注入项。新注入形态在
// sshbroker 包内实现这个非导出标记接口即可进入同一条执行路径。
type ExecInjection interface {
	injectionKind() ExecInjectionKind
}

// EnvironmentInjection 把值导出为同名环境变量。Value 只按长度帧写入
// SSH 会话标准输入，绝不拼进远端命令文本。
type EnvironmentInjection struct {
	EnvironmentName string
	Value           []byte
}

func (EnvironmentInjection) injectionKind() ExecInjectionKind {
	return ExecInjectionEnvironment
}

// ExecInjectionOptions 携带 shell 类型、目标层与注入项列表，是前台命令
// 与后台任务共用的执行选项。
type ExecInjectionOptions struct {
	ShellType   string
	TargetLayer ExecTargetLayer
	Items       []ExecInjection
}

// execOptions 是当前执行路径解析后的选项。
type execOptions struct {
	injection ExecInjectionOptions
}

// ExecOption 让现有 Exec / ExecWriters 接口保持兼容，同时允许调用方追加
// 每条命令前的注入步骤。
type ExecOption func(*execOptions)

// WithPreCommandInjection opens the shared pre-command injection step.
func WithPreCommandInjection(options ExecInjectionOptions) ExecOption {
	return func(dst *execOptions) {
		dst.injection = options
	}
}

// preparedExec is the transport-level view of an injection plan: the command
// contains names and lengths only, while standard input carries the values.
type preparedExec struct {
	command string
	frames  []byte
}

func prepareExec(command string, options ExecInjectionOptions) (preparedExec, error) {
	if len(options.Items) == 0 {
		return preparedExec{command: command}, nil
	}
	if options.TargetLayer != ExecLayerNormal {
		return preparedExec{}, fmt.Errorf("pre-command injection target layer %q is not supported", options.TargetLayer)
	}
	if options.ShellType != "" && options.ShellType != "posix" && options.ShellType != "powershell" && options.ShellType != "cmd" {
		return preparedExec{}, fmt.Errorf("pre-command injection shell type %q is not supported", options.ShellType)
	}

	items := append([]ExecInjection(nil), options.Items...)
	environments := make([]EnvironmentInjection, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		environment, ok := item.(EnvironmentInjection)
		if !ok {
			return preparedExec{}, fmt.Errorf("pre-command injection type %q is not supported", item.injectionKind())
		}
		if !validEnvironmentName(environment.EnvironmentName) {
			return preparedExec{}, fmt.Errorf("environment variable name %q is invalid", environment.EnvironmentName)
		}
		if seen[environment.EnvironmentName] {
			return preparedExec{}, fmt.Errorf("environment variable name %q is repeated", environment.EnvironmentName)
		}
		seen[environment.EnvironmentName] = true
		if bytesContainZero(environment.Value) {
			return preparedExec{}, fmt.Errorf("environment variable %s has a zero byte", environment.EnvironmentName)
		}
		if isWindowsShellType(options.ShellType) && !utf8.Valid(environment.Value) {
			return preparedExec{}, fmt.Errorf("environment variable %s has invalid UTF-8 on Windows", environment.EnvironmentName)
		}
		if options.ShellType == "cmd" && windowsCmdValueHasLineBreak(environment.Value) {
			return preparedExec{}, fmt.Errorf("environment variable %s has a line break in cmd", environment.EnvironmentName)
		}
		environments = append(environments, environment)
	}
	sort.Slice(environments, func(i, j int) bool {
		return environments[i].EnvironmentName < environments[j].EnvironmentName
	})

	var wrapper strings.Builder
	if options.ShellType == "powershell" {
		writePowerShellWrapper(&wrapper, environments)
	} else if options.ShellType == "cmd" {
		writeWindowsCmdWrapper(&wrapper, environments)
	} else {
		writePOSIXWrapper(&wrapper, environments)
	}
	wrapper.WriteString("\n")
	wrapper.WriteString(command)

	frames := buildEnvironmentFrames(options.ShellType, environments)
	return preparedExec{command: wrapper.String(), frames: frames}, nil
}

func isWindowsShellType(shellType string) bool {
	return shellType == "powershell" || shellType == "cmd"
}

// buildEnvironmentFrames 把注入值按封装脚本的读取顺序写进标准输入。cmd
// 逐行读取，因此每个值后补一次行分隔。
func buildEnvironmentFrames(shellType string, environments []EnvironmentInjection) []byte {
	var frames []byte
	for _, environment := range environments {
		frames = append(frames, environment.Value...)
		if shellType == "cmd" {
			frames = append(frames, '\r', '\n')
		}
	}
	return frames
}

// writePOSIXWrapper 只把变量名与字节长度写进远端命令；值由标准输入进入，
// 以八进制转义还原，避免换行、引号等字节影响 shell 语法。
func writePOSIXWrapper(wrapper *strings.Builder, environments []EnvironmentInjection) {
	wrapper.WriteString("unset __sshmgr_format_0\n")
	for _, environment := range environments {
		fmt.Fprintf(wrapper, "__sshmgr_format_0=$(dd bs=1 count=%d 2>/dev/null | od -An -v -to1 | awk '{for(i=1;i<=NF;i++) printf \"\\\\%%03d\",$i}')\n", len(environment.Value))
		fmt.Fprintf(wrapper, "eval \"%s=\\\"\\$(printf \\\"\\${__sshmgr_format_0}A\\\")\\\"\"\n", environment.EnvironmentName)
		fmt.Fprintf(wrapper, "%s=${%s%%A}\n", environment.EnvironmentName, environment.EnvironmentName)
		fmt.Fprintf(wrapper, "export %s\n", environment.EnvironmentName)
		wrapper.WriteString("unset __sshmgr_format_0\n")
	}
	wrapper.WriteString("unset __sshmgr_format_0\n")
}

// writePowerShellWrapper 用严格 UTF-8 解码按长度读出的字节。值不进入远端
// 命令文本，封装变量在用户命令开始前清理。
func writePowerShellWrapper(wrapper *strings.Builder, environments []EnvironmentInjection) {
	wrapper.WriteString("$__sshmgr_input_stream = [Console]::OpenStandardInput()\n")
	wrapper.WriteString("$__sshmgr_utf8 = New-Object System.Text.UTF8Encoding($false, $true)\n")
	wrapper.WriteString("[Console]::OutputEncoding = $__sshmgr_utf8\n")
	wrapper.WriteString("$__sshmgr_read = 0\n")
	wrapper.WriteString("$__sshmgr_count = 0\n")
	for _, environment := range environments {
		length := len(environment.Value)
		fmt.Fprintf(wrapper, "$__sshmgr_bytes = New-Object byte[] %d\n", length)
		fmt.Fprintf(wrapper, "while ($__sshmgr_read -lt %d) {\n", length)
		fmt.Fprintf(wrapper, "  $__sshmgr_count = $__sshmgr_input_stream.Read($__sshmgr_bytes, $__sshmgr_read, %d - $__sshmgr_read)\n", length)
		wrapper.WriteString("  if ($__sshmgr_count -le 0) { throw 'sshmgr environment injection frame ended early' }\n")
		wrapper.WriteString("  $__sshmgr_read += $__sshmgr_count\n")
		wrapper.WriteString("}\n")
		fmt.Fprintf(wrapper, "$env:%s = $__sshmgr_utf8.GetString($__sshmgr_bytes, 0, %d)\n", environment.EnvironmentName, length)
		wrapper.WriteString("Remove-Variable __sshmgr_bytes\n")
		wrapper.WriteString("$__sshmgr_read = 0\n")
	}
	wrapper.WriteString("Remove-Variable __sshmgr_input_stream\n")
	wrapper.WriteString("Remove-Variable __sshmgr_utf8\n")
	wrapper.WriteString("Remove-Variable __sshmgr_read\n")
	wrapper.WriteString("Remove-Variable __sshmgr_count\n")
}

// writeWindowsCmdWrapper 先切到 UTF-8 代码页，再用 set /p 逐行读取值。
// 每个值在标准输入末尾补回车换行；这个分隔符不进入环境变量。
func writeWindowsCmdWrapper(wrapper *strings.Builder, environments []EnvironmentInjection) {
	wrapper.WriteString("chcp 65001 >NUL\n")
	for _, environment := range environments {
		fmt.Fprintf(wrapper, "set \"%s=\"\n", environment.EnvironmentName)
		fmt.Fprintf(wrapper, "set /p %s=\n", environment.EnvironmentName)
	}
}

func validEnvironmentName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

func bytesContainZero(value []byte) bool {
	for _, b := range value {
		if b == 0 {
			return true
		}
	}
	return false
}

func windowsCmdValueHasLineBreak(value []byte) bool {
	return bytes.IndexByte(value, '\r') >= 0 || bytes.IndexByte(value, '\n') >= 0
}
