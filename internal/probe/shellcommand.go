package probe

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"ssh-manager-mcp/internal/models"
)

// OutputMarker 是服务器上固定探测命令输出协议的行首标记。
const OutputMarker = "SSHMGR-PROBE"

// ShellCommandItem 是固定探测命令里的一个地址任务。
type ShellCommandItem struct {
	Address string
	Port    int
	Timeout time.Duration
}

// ShellProbeResult 是服务器为一条地址任务返回的结论。
type ShellProbeResult struct {
	OK       bool
	Category models.ProbeErrorCategory
	// Duration 是建连耗时；负值表示服务器取不到毫秒时钟、耗时未知。
	Duration time.Duration
}

// BuildShellProbeCommand 按服务器条目标注的 shell 类型生成一次跑完全部
// 地址任务的固定命令。命令里只出现校验过的地址、端口与超时，不拼接任
// 何调用方自由文本。
func BuildShellProbeCommand(shell models.ShellType, items []ShellCommandItem) (string, error) {
	if len(items) == 0 {
		return "", fmt.Errorf("探测地址列表为空")
	}
	invocations := make([]string, 0, len(items))
	for index, item := range items {
		if err := validateProbeAddress(item.Address); err != nil {
			return "", err
		}
		if item.Port < 1 || item.Port > 65535 {
			return "", fmt.Errorf("端口 %d 超出范围（1-65535）", item.Port)
		}
		if item.Timeout <= 0 {
			return "", fmt.Errorf("地址 %s 的探测超时必须为正数", item.Address)
		}
		invocations = append(invocations, probeInvocationForShell(shell, index, item))
	}
	switch models.NormalizeShellType(string(shell)) {
	case models.ShellPOSIX:
		return strings.Join([]string{posixProbeFunctions(), strings.Join(invocations, "\n")}, "\n"), nil
	case models.ShellPowerShell:
		return strings.Join([]string{powerShellProbeFunctions(), strings.Join(invocations, "\n")}, "\n"), nil
	case models.ShellWindowsCmd:
		script := strings.Join([]string{powerShellProbeFunctions(), strings.Join(invocations, "\n")}, "\n")
		// cmd 的命令行在换行处会截断，-Command 包装多行脚本不可靠；改用
		// -EncodedCommand 传送按 UTF-16LE 编码的 Base64，彻底避开换行与
		// 引号转义。
		encoded := base64.StdEncoding.EncodeToString(utf16LE(script))
		return "powershell -NoProfile -NonInteractive -EncodedCommand " + encoded, nil
	default:
		return "", fmt.Errorf("未知 shell 类型 %q", shell)
	}
}

// utf16LE 把脚本按 PowerShell -EncodedCommand 要求的 UTF-16LE 小端字节
// 编码。
func utf16LE(script string) []byte {
	units := utf16.Encode([]rune(script))
	blob := make([]byte, len(units)*2)
	for index, unit := range units {
		binary.LittleEndian.PutUint16(blob[index*2:], unit)
	}
	return blob
}

func probeInvocationForShell(shell models.ShellType, index int, item ShellCommandItem) string {
	switch models.NormalizeShellType(string(shell)) {
	case models.ShellPOSIX:
		// 不用 %g：超时超过 1000 秒时它会输出科学计数法，shell 工具无法
		// 解析。
		seconds := strconv.FormatFloat(item.Timeout.Seconds(), 'f', -1, 64)
		return fmt.Sprintf("sshmgr_probe_one %d %s %d %s", index, item.Address, item.Port, seconds)
	case models.ShellPowerShell, models.ShellWindowsCmd:
		return fmt.Sprintf("probe-one %d %s %d %d", index, item.Address, item.Port, item.Timeout.Milliseconds())
	default:
		return ""
	}
}

// validateProbeAddress 只接受合法 IP 或主机名，保证任何 shell 都不会看到
// 地址位置里的命令语法。
func validateProbeAddress(address string) error {
	if strings.TrimSpace(address) == "" {
		return fmt.Errorf("探测地址为空")
	}
	if address != strings.TrimSpace(address) {
		return fmt.Errorf("探测地址 %q 前后含空白", address)
	}
	if net.ParseIP(address) != nil {
		return nil
	}
	for _, label := range strings.Split(address, ".") {
		if label == "" || len(label) > 63 {
			return fmt.Errorf("探测地址 %q 不是合法的 IP 或主机名", address)
		}
		// 拒绝以连字符开头或结尾的标签：传给 nc 等命令行工具时，开头的连
		// 字符会被当成选项前缀。
		if strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return fmt.Errorf("探测地址 %q 不是合法的 IP 或主机名", address)
		}
		for _, symbol := range label {
			if symbol == '-' {
				continue
			}
			if symbol >= 'a' && symbol <= 'z' || symbol >= 'A' && symbol <= 'Z' || symbol >= '0' && symbol <= '9' || symbol == '_' {
				continue
			}
			return fmt.Errorf("探测地址 %q 不是合法的 IP 或主机名", address)
		}
	}
	return nil
}

// ParseShellProbeOutput 解析固定探测命令的标准输出：每个序号一行
// `SSHMGR-PROBE <序号> ok|fail <耗时毫秒> <错误分类>`；不认识的行一律忽
// 略；没有对应行的序号记为「服务器未返回结果」。
func ParseShellProbeOutput(output string, count int) []ShellProbeResult {
	results := make([]ShellProbeResult, count)
	for index := range results {
		results[index] = ShellProbeResult{OK: false, Category: models.ProbeErrorNoResult}
	}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(strings.TrimRight(line, "\r"))
		if len(fields) < 4 || fields[0] != OutputMarker {
			continue
		}
		// 成功行的错误分类为空时行尾会少一个字段。
		categoryCode := ""
		if len(fields) >= 5 {
			categoryCode = fields[4]
		}
		index, err := strconv.Atoi(fields[1])
		if err != nil || index < 0 || index >= count {
			continue
		}
		duration := time.Duration(-1)
		if fields[3] != "?" {
			milliseconds, parseErr := strconv.ParseInt(fields[3], 10, 64)
			if parseErr == nil && milliseconds >= 0 && milliseconds <= math.MaxInt64/int64(time.Millisecond) {
				duration = time.Duration(milliseconds) * time.Millisecond
			}
		}
		category := models.NormalizeProbeErrorCategory(categoryCode)
		if fields[2] == "ok" {
			results[index] = ShellProbeResult{OK: true, Category: models.ProbeErrorNone, Duration: duration}
			continue
		}
		if fields[2] != "fail" {
			continue
		}
		results[index] = ShellProbeResult{OK: false, Category: category, Duration: duration}
	}
	return results
}

func posixProbeFunctions() string {
	return `sshmgr_probe_now_ms() { date +%s%3N 2>/dev/null; }
sshmgr_probe_classify() {
  message="$1"; code="$2"
  case "$code" in 124|137) echo timeout; return;; esac
  case "$message" in
    *"refused"*) echo connection-refused;;
    *"timed out"*|*"timeout"*) echo timeout;;
    *"No route"*|*"unreachable"*) echo no-route;;
    *"Name or service"*|*"not known"*|*"Temporary failure"*) echo dns-failure;;
    *) echo other;;
  esac
}
sshmgr_probe_one() {
  index="$1"; host="$2"; port="$3"; timeout_seconds="$4"
  timeout_whole=$(printf '%d' "$(printf '%s\n' "$timeout_seconds" | awk '{printf "%d", ($1+0.5)}')")
  start="$(sshmgr_probe_now_ms)"
  outcome=fail; category=other; detail=""
  if command -v bash >/dev/null 2>&1 && command -v timeout >/dev/null 2>&1; then
    detail="$(timeout "$timeout_seconds" bash -c 'exec 3<>/dev/tcp/$1/$2' probe "$host" "$port" 2>&1)"
    code="$?"
    if [ "$code" -eq 0 ]; then outcome=ok; category=""; else category="$(sshmgr_probe_classify "$detail" "$code")"; fi
  elif command -v nc >/dev/null 2>&1; then
    detail="$(nc -z -v -w "$timeout_whole" "$host" "$port" 2>&1)"
    code="$?"
    if [ "$code" -eq 0 ]; then outcome=ok; category=""; else category="$(sshmgr_probe_classify "$detail" "$code")"; fi
  else
    category=missing-tool
  fi
  end="$(sshmgr_probe_now_ms)"
  milliseconds="?"
  case "$start$end" in ''|*[!0-9]*) milliseconds='?';; *) milliseconds=$((end-start));; esac
  echo "SSHMGR-PROBE $index $outcome $milliseconds $category"
}`
}

func powerShellProbeFunctions() string {
	return `function probe-one([int]$index, [string]$targetHost, [int]$port, [int]$timeoutMilliseconds) {
  $watch = [System.Diagnostics.Stopwatch]::StartNew()
  $ok = $false
  $category = 'other'
  $client = New-Object System.Net.Sockets.TcpClient
  try {
    $task = $client.ConnectAsync($targetHost, $port)
    if ($task.Wait($timeoutMilliseconds)) { $ok = $true; $category = '' }
    else { $category = 'timeout' }
  } catch {
    $exception = $_.Exception
    while ($null -ne $exception) {
      if ($exception -is [System.Net.Sockets.SocketException]) {
        switch ($exception.SocketErrorCode.ToString()) {
          'ConnectionRefused' { $category = 'connection-refused' }
          'TimedOut' { $category = 'timeout' }
          'HostUnreachable' { $category = 'no-route' }
          'NetworkUnreachable' { $category = 'no-route' }
          'HostNotFound' { $category = 'dns-failure' }
          'TryAgain' { $category = 'dns-failure' }
          default { $category = 'other' }
        }
        break
      }
      $exception = $exception.InnerException
    }
  } finally {
    $client.Close()
  }
  $watch.Stop()
  Write-Output ('SSHMGR-PROBE {0} {1} {2} {3}' -f $index, $(if ($ok) { 'ok' } else { 'fail' }), $watch.ElapsedMilliseconds, $category)
}`
}
