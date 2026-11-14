package sshbroker

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"ssh-manager-mcp/internal/models"
)

// ExecTargetLayer 表示注入发生在 SSH 会话的哪一层。普通层是登录 shell；
// 提权层是 sudo 之后的 shell。
type ExecTargetLayer string

const (
	// ExecLayerNormal 表示在登录 shell 层注入。
	ExecLayerNormal ExecTargetLayer = "normal"
	// ExecLayerPrivileged 表示在 sudo 之后的 shell 层注入。
	ExecLayerPrivileged ExecTargetLayer = "privileged"
)

// ExecInjectionKind 是注入项类型。当前有环境变量与 Windows 网络盘符。
type ExecInjectionKind string

const (
	// ExecInjectionEnvironment 把值导出为同名环境变量。
	ExecInjectionEnvironment ExecInjectionKind = "environment"
	// ExecInjectionNetworkDrive 在用户命令前把 SMB 共享映射成 Windows
	// 网络盘符。
	ExecInjectionNetworkDrive ExecInjectionKind = "network-drive"
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

// NetworkDriveInjection 是一条 Windows 网络盘符注入项。Drive 是单个大写
// 盘符字母；Address、ShareName 与 UserName 允许进入远端命令文本；Password
// 只按长度帧写入 SSH 会话标准输入，绝不进入命令文本，也绝不进入目标机任
// 何进程的命令行（映射在 PowerShell 进程内经异步调用执行）。StatusMarker
// 是本次执行随机生成的十六进制标记：远端封装用它回传每个共享的注入结
// 果状态行，调用方解析后必须把状态行从输出里剔除。
type NetworkDriveInjection struct {
	ShareName    string
	Drive        string
	Address      string
	UserName     string
	Password     []byte
	StatusMarker string
}

func (NetworkDriveInjection) injectionKind() ExecInjectionKind {
	return ExecInjectionNetworkDrive
}

// ExecInjectionOptions 携带 shell 类型、目标层与注入项列表，是前台命令
// 与后台任务共用的执行选项。
type ExecInjectionOptions struct {
	ShellType   models.ShellType
	TargetLayer ExecTargetLayer
	Items       []ExecInjection
}

// execOptions 是当前执行路径解析后的选项。
type execOptions struct {
	injection     ExecInjectionOptions
	standardInput []byte
}

func applyExecOptions(options ...ExecOption) execOptions {
	var settings execOptions
	for _, option := range options {
		if option != nil {
			option(&settings)
		}
	}
	return settings
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

// WithSudoStandardInput 把调用方的标准输入载荷送进提权后的固定命令。目
// 标需要 sudo 密码时，载荷写在密码行之后；目标免密 sudo 时，标准输入只
// 有载荷。值绝不进入命令文本，也不进入日志或审计。
func WithSudoStandardInput(input []byte) ExecOption {
	return func(dst *execOptions) {
		dst.standardInput = append([]byte(nil), input...)
	}
}

// preparedExec is the transport-level view of an injection plan: the command
// contains names and lengths only, while standard input carries the values.
type preparedExec struct {
	command string
	frames  []byte
}

// windowsCmdInjectionFailureExitCode 是 cmd 固定引导脚本或生成的注入脚本自
// 身失败时使用的退出码。用户命令也可以主动返回同一个退出码；这里的常量
// 只用于在代码与报告中说明 sshmgr 内部失败路径的取值。
const windowsCmdInjectionFailureExitCode = 120

func prepareExec(command string, options ExecInjectionOptions) (preparedExec, error) {
	return prepareExecForTargetLayer(command, options, ExecLayerNormal)
}

func preparePrivilegedExec(command string, options ExecInjectionOptions) (preparedExec, error) {
	return prepareExecForTargetLayer(command, options, ExecLayerPrivileged)
}

func prepareExecForTargetLayer(command string, options ExecInjectionOptions, targetLayer ExecTargetLayer) (preparedExec, error) {
	if len(options.Items) == 0 {
		return preparedExec{command: command}, nil
	}
	if options.TargetLayer != targetLayer {
		return preparedExec{}, fmt.Errorf("pre-command injection target layer %q does not match %q", options.TargetLayer, targetLayer)
	}
	if targetLayer == ExecLayerPrivileged && options.ShellType != "" && options.ShellType != models.ShellPOSIX {
		return preparedExec{}, fmt.Errorf("privileged pre-command injection shell type %q is not supported", options.ShellType)
	}
	if options.ShellType != "" &&
		options.ShellType != models.ShellPOSIX &&
		options.ShellType != models.ShellPowerShell &&
		options.ShellType != models.ShellWindowsCmd {
		return preparedExec{}, fmt.Errorf("pre-command injection shell type %q is not supported", options.ShellType)
	}

	items := append([]ExecInjection(nil), options.Items...)
	environments := make([]EnvironmentInjection, 0, len(items))
	drives := make([]NetworkDriveInjection, 0)
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		switch injection := item.(type) {
		case EnvironmentInjection:
			environment := injection
			if !models.EnvironmentVariableNameFormatIsValid(environment.EnvironmentName) {
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
			if options.ShellType == models.ShellWindowsCmd && WindowsCmdValueHasLineBreak(environment.Value) {
				return preparedExec{}, fmt.Errorf("environment variable %s has a line break in cmd", environment.EnvironmentName)
			}
			environments = append(environments, environment)
		case NetworkDriveInjection:
			drive := injection
			if err := validateNetworkDriveInjection(drive, options.ShellType, targetLayer); err != nil {
				return preparedExec{}, err
			}
			drives = append(drives, drive)
		default:
			return preparedExec{}, fmt.Errorf("pre-command injection type %q is not supported", item.injectionKind())
		}
	}
	sort.Slice(environments, func(i, j int) bool {
		return environments[i].EnvironmentName < environments[j].EnvironmentName
	})
	sort.Slice(drives, func(i, j int) bool {
		return drives[i].ShareName < drives[j].ShareName
	})

	if options.ShellType == models.ShellWindowsCmd {
		return prepareWindowsCmdExec(command, environments, drives)
	}

	var wrapper strings.Builder
	stdinDelimiter := ""
	if options.ShellType == models.ShellPowerShell {
		writePowerShellWrapper(&wrapper, environments, drives)
	} else {
		if len(drives) > 0 {
			return preparedExec{}, fmt.Errorf("network drive injection requires a Windows shell type")
		}
		if targetLayer == ExecLayerPrivileged {
			stdinDelimiter = "__sshmgr_environment_frames_" + newSudoNonce()
		}
		writePOSIXWrapper(&wrapper, environments, stdinDelimiter)
	}
	wrapper.WriteString("\n")
	wrapper.WriteString(command)

	frames := buildInjectionFrames(options.ShellType, environments, drives)
	if stdinDelimiter != "" {
		frames = append([]byte(stdinDelimiter+"\n"), frames...)
	}
	return preparedExec{command: wrapper.String(), frames: frames}, nil
}

// prepareWindowsCmdExec 保持发往 SSH 的命令短且只含 ASCII 字符。完整注入
// 脚本和用户命令改走第一条标准输入帧，因为 Windows OpenSSH 只把 cmd 命令
// 的一行交给执行，cmd 命令行还有较低长度上限。
func prepareWindowsCmdExec(command string, environments []EnvironmentInjection, drives []NetworkDriveInjection) (preparedExec, error) {
	if strings.IndexByte(command, 0) >= 0 {
		return preparedExec{}, fmt.Errorf("cmd command has a zero byte")
	}
	if !utf8.ValidString(command) {
		return preparedExec{}, fmt.Errorf("cmd command is not valid UTF-8")
	}

	injectionScript := strings.Builder{}
	injectionScript.WriteString("$__sshmgr_input_stream = $__sshmgr_stream\n")
	injectionScript.WriteString("$__sshmgr_utf8 = New-Object System.Text.UTF8Encoding($false, $true)\n")
	injectionScript.WriteString("$__sshmgr_read = 0\n")
	injectionScript.WriteString("$__sshmgr_count = 0\n")
	writePowerShellEnvironmentFrames(&injectionScript, environments)
	writePowerShellNetworkDrives(&injectionScript, drives)
	injectionScript.WriteString("Remove-Variable __sshmgr_utf8\n")
	injectionScript.WriteString("Remove-Variable __sshmgr_read\n")
	injectionScript.WriteString("Remove-Variable __sshmgr_count -ErrorAction SilentlyContinue\n")
	injectionScript.WriteString("$__sshmgr_process = New-Object System.Diagnostics.Process\n")
	injectionScript.WriteString("$__sshmgr_process.StartInfo.FileName = $env:ComSpec\n")
	injectionScript.WriteString("$__sshmgr_process.StartInfo.Arguments = '/d /s /c \"' + " + PowerShellSingleQuote(command) + " + '\"'\n")
	injectionScript.WriteString("$__sshmgr_process.StartInfo.UseShellExecute = $false\n")
	injectionScript.WriteString("[void]$__sshmgr_process.Start()\n")
	injectionScript.WriteString("$__sshmgr_process.WaitForExit()\n")
	injectionScript.WriteString("exit $__sshmgr_process.ExitCode\n")

	script := injectionScript.String()
	if len(script) > 99999999 {
		return preparedExec{}, fmt.Errorf("cmd injection script is too large")
	}
	scriptFrame := make([]byte, 8+len(script))
	copy(scriptFrame, fmt.Sprintf("%08d", len(script)))
	copy(scriptFrame[8:], script)

	values := buildInjectionFrames(models.ShellWindowsCmd, environments, drives)
	frames := make([]byte, 0, len(scriptFrame)+len(values))
	frames = append(frames, scriptFrame...)
	frames = append(frames, values...)
	return preparedExec{
		command: PowerShellEncodedCommand(windowsCmdBootstrapScript()),
		frames:  frames,
	}, nil
}

// windowsCmdBootstrapScript 读取第一条标准输入帧，并在 PowerShell 进程内执
// 行。这个引导脚本固定不变：不随机密项数量、网络共享数量或用户命令增长。
func windowsCmdBootstrapScript() string {
	return `$ProgressPreference='SilentlyContinue'
trap { exit ` + strconv.Itoa(windowsCmdInjectionFailureExitCode) + ` }
$__sshmgr_stream = [Console]::OpenStandardInput()
function Read-SshmgrFrame([int]$size) {
  $frame = New-Object byte[] $size
  $offset = 0
  while ($offset -lt $size) {
    $count = $__sshmgr_stream.Read($frame, $offset, $size - $offset)
    if ($count -le 0) { throw 'frame ended' }
    $offset += $count
  }
  $frame
}
$size = [int][Text.Encoding]::ASCII.GetString((Read-SshmgrFrame 8))
& ([scriptblock]::Create([Text.Encoding]::UTF8.GetString((Read-SshmgrFrame $size))))`
}

// validateNetworkDriveInjection 是网络盘符注入项的最后防线。注入计划层
// 已经做过可恢复的校验（失败只警告、不挡命令）；这里只拦截会破坏封装脚
// 本的值，属于不应出现的进程内错误。
func validateNetworkDriveInjection(drive NetworkDriveInjection, shellType models.ShellType, targetLayer ExecTargetLayer) error {
	if targetLayer != ExecLayerNormal || !isWindowsShellType(shellType) {
		return fmt.Errorf("network drive injection requires the normal layer of a Windows shell")
	}
	if models.NormalizeShellType(string(shellType)) != models.ShellPowerShell &&
		models.NormalizeShellType(string(shellType)) != models.ShellWindowsCmd {
		return fmt.Errorf("network drive injection shell type %q is not supported yet", shellType)
	}
	if err := ValidateNetworkDriveInjectionValues(drive); err != nil {
		return err
	}
	if len(drive.Password) == 0 || bytesContainZero(drive.Password) || !utf8.Valid(drive.Password) {
		return fmt.Errorf("network drive password for share %s is empty or not valid UTF-8", drive.ShareName)
	}
	if err := validateNetworkDriveMarker(drive.StatusMarker); err != nil {
		return err
	}
	return nil
}

// ValidateNetworkDriveInjectionValues 校验会进入远端封装脚本的登记值：盘
// 符、共享名、地址与挂载账号。注入计划层复用这份规则，把单个共享降级为
// 警告；这里仍作为 sshbroker 内部拼装前的防线。
func ValidateNetworkDriveInjectionValues(drive NetworkDriveInjection) error {
	if len(drive.Drive) != 1 || drive.Drive < "D" || drive.Drive > "Z" {
		return fmt.Errorf("network drive letter %q is invalid", drive.Drive)
	}
	if err := validateNetworkDriveScriptValue("share name", drive.ShareName); err != nil {
		return err
	}
	if err := validateNetworkDriveScriptValue("address", drive.Address); err != nil {
		return err
	}
	return validateNetworkDriveScriptValue("user name", drive.UserName)
}

// validateNetworkDriveScriptValue 拒绝会让封装脚本变形的值。值进入
// PowerShell 单引号字符串（内部单引号会双写），但双引号会被 Windows
// OpenSSH 从命令文本里剥掉，换行与控制字符会破坏脚本行结构。
func validateNetworkDriveScriptValue(field, value string) error {
	if value == "" {
		return fmt.Errorf("network drive %s is empty", field)
	}
	if strings.ContainsAny(value, "\"\r\n") {
		return fmt.Errorf("network drive %s contains a double quote or line break", field)
	}
	for _, symbol := range value {
		if symbol < 0x20 || symbol == 0x7f {
			return fmt.Errorf("network drive %s contains a control character", field)
		}
	}
	return nil
}

func validateNetworkDriveMarker(marker string) error {
	if len(marker) < 16 {
		return fmt.Errorf("network drive status marker is too short")
	}
	for _, symbol := range marker {
		if (symbol < '0' || symbol > '9') && (symbol < 'a' || symbol > 'f') {
			return fmt.Errorf("network drive status marker %q is not lowercase hexadecimal", marker)
		}
	}
	return nil
}

func isWindowsShellType(shellType models.ShellType) bool {
	return shellType == models.ShellPowerShell || shellType == models.ShellWindowsCmd
}

// buildInjectionFrames 把注入值按封装脚本的读取顺序写进标准输入。
// Windows PowerShell 与 cmd 的注入脚本都先读取环境变量值，再读取各共
// 享挂载密码；cmd 的注入脚本本体另有一个独立的标准输入帧，由调用方拼
// 在这些值之前。
func buildInjectionFrames(shellType models.ShellType, environments []EnvironmentInjection, drives []NetworkDriveInjection) []byte {
	var frames []byte
	for _, environment := range environments {
		frames = append(frames, environment.Value...)
	}
	for _, drive := range drives {
		frames = append(frames, drive.Password...)
	}
	return frames
}

// powerShellQuoteDelimiters 是 PowerShell 会当作单引号字符串定界符的全部
// 字符：ASCII 单引号与四个弯单引号。任一字符在字符串里出现两次即写字两
// 次，表示一个字面量字符。
const powerShellQuoteDelimiters = "'‘’‚‛"

// PowerShellSingleQuote 把值安全嵌入 PowerShell 单引号字符串；全部单引号
// 类定界符（ASCII 单引号与四个弯单引号）双写转义，其余字符原样保留。
func PowerShellSingleQuote(value string) string {
	var builder strings.Builder
	builder.Grow(len(value) + 2)
	builder.WriteByte('\'')
	for _, character := range value {
		if strings.ContainsRune(powerShellQuoteDelimiters, character) {
			builder.WriteRune(character)
		}
		builder.WriteRune(character)
	}
	builder.WriteByte('\'')
	return builder.String()
}

// NetworkDriveMappingTimeout 是远端 WNetAddConnection2 异步调用的等待上限。
// 地址不可达时该调用可能要约 10 秒才失败，3 秒上限保证注入步骤不会拖垮
// 用户命令（#206 真机结论）。
const NetworkDriveMappingTimeout = 3 * time.Second

// NetworkDriveInjectionTotalTimeout 是一次执行里全部网络盘符注入步骤的固
// 定总时长上限。这个时间属于用户命令超时的一部分，不再额外延长 SSH 会话。
const NetworkDriveInjectionTotalTimeout = 10 * time.Second

// writePowerShellNetworkDrives 先声明一次 Windows 网络连接函数，再为每个
// 共享写一段映射脚本：按长度从标准输入读出密码字节，经 PowerShell 异步
// 调用执行 WNetAddConnection2（密码只存在进程内变量里，不出现在任何进
// 程命令行），用带随机标记的状态行回传结果，再继续执行后续注入与用户
// 命令。超时时只发起异步停止，不等待它完成，也不同步释放管道。任何映
// 射失败都只体现在状态行里，不会中断用户命令。
func writePowerShellNetworkDrives(wrapper *strings.Builder, drives []NetworkDriveInjection) {
	if len(drives) == 0 {
		return
	}
	wrapper.WriteString("$__sshmgr_injection_started = [DateTime]::UtcNow.Ticks\n")
	writePowerShellWNetType(wrapper)
	for index, drive := range drives {
		writePowerShellNetworkDrive(wrapper, drive, index)
	}
	wrapper.WriteString("Remove-Variable __sshmgr_wnet_ready -ErrorAction SilentlyContinue\n")
	wrapper.WriteString("Remove-Variable __sshmgr_injection_started -ErrorAction SilentlyContinue\n")
}

// writePowerShellWNetType 声明调用 mpr.dll 的 Windows 网络连接函数。同一
// 条注入脚本里所有共享共用这一份类型定义；类型已存在时跳过声明。声明失
// 败只记录在就绪变量里，后续每个共享都会回传 error-wrapper。
func writePowerShellWNetType(wrapper *strings.Builder) {
	wrapper.WriteString("$__sshmgr_wnet_ready = $false\n")
	wrapper.WriteString("if (-not ('SshmgrNative.Mpr' -as [type])) {\n")
	wrapper.WriteString("  try {\n")
	wrapper.WriteString("    $__sshmgr_wnet_definition = @'\n")
	wrapper.WriteString("[StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode)]\n")
	wrapper.WriteString("public class NetResource { public int Scope; public int Type; public int DisplayType; public int Usage; public string LocalName; public string RemoteName; public string Comment; public string Provider; }\n")
	wrapper.WriteString("[DllImport('mpr.dll', CharSet = CharSet.Unicode)]\n")
	wrapper.WriteString("public static extern int WNetAddConnection2(NetResource resource, string password, string username, int flags);\n")
	wrapper.WriteString("'@\n")
	wrapper.WriteString("    $__sshmgr_wnet_definition = $__sshmgr_wnet_definition.Replace('''mpr.dll''', [string][char]34 + 'mpr.dll' + [string][char]34)\n")
	wrapper.WriteString("    $null = Add-Type -Namespace SshmgrNative -Name Mpr -MemberDefinition $__sshmgr_wnet_definition -ErrorAction Stop\n")
	wrapper.WriteString("    $__sshmgr_wnet_ready = $true\n")
	wrapper.WriteString("  } catch { }\n")
	wrapper.WriteString("} else {\n")
	wrapper.WriteString("  $__sshmgr_wnet_ready = $true\n")
	wrapper.WriteString("}\n")
	wrapper.WriteString("Remove-Variable __sshmgr_wnet_definition -ErrorAction SilentlyContinue\n")
}

func writePowerShellNetworkDrive(wrapper *strings.Builder, drive NetworkDriveInjection, index int) {
	length := len(drive.Password)
	wrapper.WriteString("$__sshmgr_started_ticks = [Int64]$env:__sshmgr_drive_started\n")
	wrapper.WriteString("if (-not $__sshmgr_started_ticks) { $__sshmgr_started_ticks = [Int64]$__sshmgr_injection_started }\n")
	wrapper.WriteString("$__sshmgr_elapsed = ([DateTime]::UtcNow.Ticks - $__sshmgr_started_ticks) / [TimeSpan]::TicksPerMillisecond\n")
	fmt.Fprintf(wrapper, "$__sshmgr_can_map = ($__sshmgr_elapsed + %d) -le %d\n",
		NetworkDriveMappingTimeout.Milliseconds(), NetworkDriveInjectionTotalTimeout.Milliseconds())
	wrapper.WriteString("try {\n")
	fmt.Fprintf(wrapper, "$__sshmgr_bytes = New-Object byte[] %d\n", length)
	fmt.Fprintf(wrapper, "while ($__sshmgr_read -lt %d) {\n", length)
	fmt.Fprintf(wrapper, "  $__sshmgr_count = $__sshmgr_input_stream.Read($__sshmgr_bytes, $__sshmgr_read, %d - $__sshmgr_read)\n", length)
	wrapper.WriteString("  if ($__sshmgr_count -le 0) { throw 'sshmgr network drive injection frame ended early' }\n")
	wrapper.WriteString("  $__sshmgr_read += $__sshmgr_count\n")
	wrapper.WriteString("}\n")
	fmt.Fprintf(wrapper, "$__sshmgr_password = $__sshmgr_utf8.GetString($__sshmgr_bytes, 0, %d)\n", length)
	wrapper.WriteString("Remove-Variable __sshmgr_bytes\n")
	wrapper.WriteString("$__sshmgr_read = 0\n")
	wrapper.WriteString("$__sshmgr_timed_out = $false\n")
	wrapper.WriteString("if ($__sshmgr_can_map -and $__sshmgr_wnet_ready) {\n")
	wrapper.WriteString("  try {\n")
	wrapper.WriteString("    $__sshmgr_pipeline = [PowerShell]::Create()\n")
	wrapper.WriteString("    $null = $__sshmgr_pipeline.AddScript('param($a,$b,$c,$d) $resource = New-Object SshmgrNative.Mpr+NetResource; $resource.Type = 1; $resource.LocalName = $a; $resource.RemoteName = $b; [SshmgrNative.Mpr]::WNetAddConnection2($resource, $d, $c, 0)')\n")
	fmt.Fprintf(wrapper, "    $null = $__sshmgr_pipeline.AddArgument(%s)\n", PowerShellSingleQuote(drive.Drive+":"))
	fmt.Fprintf(wrapper, "    $null = $__sshmgr_pipeline.AddArgument(%s)\n", PowerShellSingleQuote(`\\`+drive.Address+`\`+drive.ShareName))
	fmt.Fprintf(wrapper, "    $null = $__sshmgr_pipeline.AddArgument(%s)\n", PowerShellSingleQuote(drive.UserName))
	wrapper.WriteString("    $null = $__sshmgr_pipeline.AddArgument($__sshmgr_password)\n")
	wrapper.WriteString("    $__sshmgr_task = $__sshmgr_pipeline.BeginInvoke()\n")
	// BeginInvoke 返回的 PowerShellAsyncResult 没有 Wait 方法，只能等它的
	// 等待句柄（Windows PowerShell 5.1 实测）。
	fmt.Fprintf(wrapper, "    if (-not $__sshmgr_task.AsyncWaitHandle.WaitOne(%d)) {\n", NetworkDriveMappingTimeout.Milliseconds())
	wrapper.WriteString("      $__sshmgr_status = 'timeout'\n")
	wrapper.WriteString("      $__sshmgr_timed_out = $true\n")
	wrapper.WriteString("      try { $null = $__sshmgr_pipeline.BeginStop($null, $null) } catch { }\n")
	wrapper.WriteString("    } else {\n")
	wrapper.WriteString("      try {\n")
	wrapper.WriteString("        $__sshmgr_result = $__sshmgr_pipeline.EndInvoke($__sshmgr_task)\n")
	wrapper.WriteString("        if ($__sshmgr_result.Count -ne 1) { throw 'sshmgr network drive injection returned no integer result' }\n")
	wrapper.WriteString("        $__sshmgr_return = $__sshmgr_result[0]\n")
	wrapper.WriteString("        if ($__sshmgr_return -isnot [int]) { throw 'sshmgr network drive injection returned no integer result' }\n")
	wrapper.WriteString("        if ($__sshmgr_return -eq 0) { $__sshmgr_status = 'ok' } else { $__sshmgr_status = 'error-' + $__sshmgr_return }\n")
	wrapper.WriteString("      } catch {\n")
	wrapper.WriteString("        $__sshmgr_status = 'error-wrapper'\n")
	wrapper.WriteString("      }\n")
	wrapper.WriteString("    }\n")
	wrapper.WriteString("  } catch {\n")
	wrapper.WriteString("    $__sshmgr_status = 'error-wrapper'\n")
	wrapper.WriteString("  }\n")
	wrapper.WriteString("} elseif (-not $__sshmgr_wnet_ready) {\n")
	wrapper.WriteString("  $__sshmgr_status = 'error-wrapper'\n")
	wrapper.WriteString("} else {\n")
	wrapper.WriteString("  $__sshmgr_status = 'skipped'\n")
	wrapper.WriteString("}\n")
	wrapper.WriteString("} catch {\n")
	wrapper.WriteString("  $__sshmgr_status = 'error-wrapper'\n")
	wrapper.WriteString("}\n")
	wrapper.WriteString("if ($null -ne $__sshmgr_password) { Remove-Variable __sshmgr_password }\n")
	wrapper.WriteString("if ($null -ne $__sshmgr_pipeline -and -not $__sshmgr_timed_out) { $__sshmgr_pipeline.Dispose() }\n")
	fmt.Fprintf(wrapper, "Write-Output ('SSHMGR-SMBMAP:%s:%d:' + $__sshmgr_status)\n", drive.StatusMarker, index)
	wrapper.WriteString("Remove-Variable __sshmgr_pipeline,__sshmgr_task,__sshmgr_status,__sshmgr_result,__sshmgr_return,__sshmgr_count,__sshmgr_timed_out,__sshmgr_started_ticks,__sshmgr_elapsed,__sshmgr_can_map -ErrorAction SilentlyContinue\n")
	wrapper.WriteString("$__sshmgr_read = 0\n")
}

// writePOSIXWrapper 只把变量名与字节长度写进远端命令；值由标准输入进入，
// 以八进制转义还原，避免换行、引号等字节影响 shell 语法。
func writePOSIXWrapper(wrapper *strings.Builder, environments []EnvironmentInjection, stdinDelimiter string) {
	if stdinDelimiter != "" {
		wrapper.WriteString("__sshmgr_frame_start_found=false\n")
		wrapper.WriteString("__sshmgr_frame_delimiter=''\n")
		wrapper.WriteString("while IFS= read -r __sshmgr_frame_delimiter; do\n")
		fmt.Fprintf(wrapper, "  if [ \"$__sshmgr_frame_delimiter\" = '%s' ]; then __sshmgr_frame_start_found=true; break; fi\n", stdinDelimiter)
		wrapper.WriteString("done\n")
		wrapper.WriteString("if [ \"$__sshmgr_frame_start_found\" != true ]; then echo 'sshmgr environment injection frame delimiter was not found' >&2; exit 78; fi\n")
		wrapper.WriteString("unset __sshmgr_frame_start_found __sshmgr_frame_delimiter\n")
	}
	wrapper.WriteString("unset __sshmgr_octal_bytes\n")
	for _, environment := range environments {
		fmt.Fprintf(wrapper, "__sshmgr_octal_bytes=$(dd bs=1 count=%d 2>/dev/null | od -An -v -to1 | awk '{for(i=1;i<=NF;i++) printf \"\\\\%%03d\",$i}')\n", len(environment.Value))
		fmt.Fprintf(wrapper, "eval \"%s=\\\"\\$(printf \\\"\\${__sshmgr_octal_bytes}A\\\")\\\"\"\n", environment.EnvironmentName)
		fmt.Fprintf(wrapper, "%s=${%s%%A}\n", environment.EnvironmentName, environment.EnvironmentName)
		fmt.Fprintf(wrapper, "export %s\n", environment.EnvironmentName)
		wrapper.WriteString("unset __sshmgr_octal_bytes\n")
	}
	wrapper.WriteString("unset __sshmgr_octal_bytes\n")
}

// writePowerShellWrapper 用严格 UTF-8 解码按长度读出的字节。环境变量值与
// 网络盘符挂载密码都不进入远端命令文本，封装变量在用户命令开始前清理；
// 读取标准输入字节流不要求改变用户命令的输出编码。
func writePowerShellWrapper(wrapper *strings.Builder, environments []EnvironmentInjection, drives []NetworkDriveInjection) {
	wrapper.WriteString("$__sshmgr_input_stream = [Console]::OpenStandardInput()\n")
	wrapper.WriteString("$__sshmgr_utf8 = New-Object System.Text.UTF8Encoding($false, $true)\n")
	wrapper.WriteString("$__sshmgr_read = 0\n")
	wrapper.WriteString("$__sshmgr_count = 0\n")
	writePowerShellEnvironmentFrames(wrapper, environments)
	writePowerShellNetworkDrives(wrapper, drives)
	wrapper.WriteString("Remove-Variable __sshmgr_input_stream\n")
	wrapper.WriteString("Remove-Variable __sshmgr_utf8\n")
	wrapper.WriteString("Remove-Variable __sshmgr_read\n")
	// 网络盘符段落已经删过它，这里只在没有网络盘符时才真正删除。
	wrapper.WriteString("Remove-Variable __sshmgr_count -ErrorAction SilentlyContinue\n")
}

func writePowerShellEnvironmentFrames(wrapper *strings.Builder, environments []EnvironmentInjection) {
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
}

// PowerShellEncodedCommand 把 PowerShell 脚本封装成跨 PowerShell 与 cmd 都
// 能直接执行的 EncodedCommand 形态：脚本先按 UTF-16LE 编码，再整体 base64。
func PowerShellEncodedCommand(script string) string {
	return "powershell -NoProfile -NonInteractive -EncodedCommand " +
		base64.StdEncoding.EncodeToString(powerShellUTF16LE(script))
}

func powerShellUTF16LE(script string) []byte {
	units := utf16.Encode([]rune(script))
	blob := make([]byte, len(units)*2)
	for index, unit := range units {
		binary.LittleEndian.PutUint16(blob[index*2:], unit)
	}
	return blob
}

func bytesContainZero(value []byte) bool {
	for _, b := range value {
		if b == 0 {
			return true
		}
	}
	return false
}

// WindowsCmdValueHasLineBreak 判断一个环境变量值是否含有回车或换行字节。
// cmd 注入继续沿用既有输入契约并拒绝这类值，避免改变调用方已依赖的行
// 为与警告文案。
func WindowsCmdValueHasLineBreak(value []byte) bool {
	return bytes.IndexByte(value, '\r') >= 0 || bytes.IndexByte(value, '\n') >= 0
}
