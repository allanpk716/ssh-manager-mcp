// Package shellprobe 探测服务器实际的 shell 类型(命令策略的诊断面,
// ticket #17):向服务器发两条固定的无害探测命令,按输出判定
// POSIX / PowerShell / cmd。判定是纯函数(Detect)——不做任何网络或
// 磁盘读写;连接与执行的接线在 ProbeServer,owner 的 CLI 诊断命令与
// TUI 服务器页共用它(ADR 0003 的对等要求)。
package shellprobe

import (
	"context"
	"fmt"
	"strings"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vault"
)

// 两条探测命令。选形原则:三类 shell 对同一条命令的输出互不相同,且
// 命令本身只打印文字、无副作用——探测是只读诊断,不该在服务器上留下
// 任何痕迹。
const (
	// CmdOSExpand 区分 Windows cmd:`%OS%` 是 Windows 内建环境变量,只有
	// cmd 会把它展开成 Windows_NT;POSIX shell 与 PowerShell 都原样打印
	// 百分号字面量。
	CmdOSExpand = "echo %OS%"
	// CmdPSVar 区分 PowerShell:`$PSVersionTable` 是 PowerShell 的内建
	// 变量,echo 它会打印出版本表(含 PSVersion 与 PSEdition 字样);
	// POSIX shell 里这个变量未定义,展开为空(echo 打印空行);cmd 里
	// 原样打印字面量。
	CmdPSVar = "echo $PSVersionTable"
)

// Probes 是两条探测命令的标准输出原文,Detect 的全部输入。
type Probes struct {
	OSExpand string // `echo %OS%` 的标准输出
	PSVar    string // `echo $PSVersionTable` 的标准输出
}

// Detect 按探测输出判定实际 shell 类型,并给出一段给 owner 看的判定
// 依据。返回的 shell 类型为空串表示无法判定——如实报告,不猜(判定不
// 出来时提示 owner 人工确认,比猜错更强)。
func Detect(p Probes) (models.ShellType, string) {
	os := strings.TrimSpace(p.OSExpand)
	if strings.EqualFold(os, "Windows_NT") {
		return models.ShellWindowsCmd, "`echo %OS%` expanded to Windows_NT — only the Windows cmd shell expands that built-in variable (POSIX shells and PowerShell print it literally)"
	}
	if strings.Contains(p.PSVar, "PSVersion") && strings.Contains(p.PSVar, "PSEdition") {
		// 版本表同时含 PSVersion 与 PSEdition 两个键才算:cmd 会把
		// `$PSVersionTable` 原样回显,字面量里也有 "PSVersion" 一词,
		// 只看一个键会把 cmd 误判成 PowerShell。
		return models.ShellPowerShell, "`echo $PSVersionTable` printed a version table — that variable exists only in PowerShell"
	}
	if strings.TrimSpace(p.PSVar) == "" {
		return models.ShellPOSIX, "`echo $PSVersionTable` expanded to nothing — the variable is undefined in a POSIX shell"
	}
	return "", fmt.Sprintf("probe output matches no known shell: `echo %%OS%%` printed %q, `echo $PSVersionTable` printed %q", clipEvidence(os), clipEvidence(strings.TrimSpace(p.PSVar)))
}

// clipEvidence 截断过长的探测输出片段,让无法判定的依据保持一行可读。
func clipEvidence(s string) string {
	const max = 80
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

// probeShellDeadline 约束一台服务器的整次探测——连接与两条探测命令共
// 用它(owner ssh 命令的 ownerSSHDeadline 同款考虑:ssh.Dial 本身不可中
// 断,sshbroker.Connect 在期限到达时放弃拨号,不可达主机不会挂到操作
// 系统级的 TCP 超时)。
var probeShellDeadline = 60 * time.Second

// 每条探测命令的执行超时与输出上限。探测只是 echo,正常毫秒级返回;
// 上限防御异常服务器把诊断命令拖死或刷屏。
const (
	probeExecTimeout = 30 * time.Second
	probeMaxBytes    = int64(16 * 1024)
)

// ProbeServer 连接服务器并探测实际 shell 类型:取凭据、校验主机密钥与
// owner ssh 命令同一条路径(首次连接按既有 TOFU 策略记录主机密钥,与
// 一切 owner 执行路径一致),在一条连接上依次跑两条探测命令,交给纯函
// 数 Detect 判定。除此之外只读——不改服务器条目上的任何字段。detect
// 为空串表示无法判定(见 Detect)。owner 的 CLI 诊断命令与 TUI 服务器
// 页都调它,不各自接绕(ADR 0003)。
func ProbeServer(parent context.Context, st *store.Store, srv *models.Server) (models.ShellType, string, error) {
	auth, err := vault.AuthForServer(st, srv)
	if err != nil {
		return "", "", fmt.Errorf("resolve credential: %w", err)
	}
	hkCb, err := sshbroker.HostKeyTOFU(st, srv.Host, srv.Port)
	if err != nil {
		return "", "", err
	}
	ctx, cancel := context.WithTimeout(parent, probeShellDeadline)
	defer cancel()
	cli, err := sshbroker.Connect(ctx, srv.Host, srv.Port, srv.User, auth, hkCb)
	if err != nil {
		return "", "", fmt.Errorf("connect: %w", err)
	}
	defer cli.Close()
	osRes, err := cli.Exec(ctx, CmdOSExpand, probeExecTimeout, probeMaxBytes)
	if err != nil {
		return "", "", fmt.Errorf("run %q: %w", CmdOSExpand, err)
	}
	if osRes.TimedOut {
		return "", "", fmt.Errorf("probe command %q timed out", CmdOSExpand)
	}
	psRes, err := cli.Exec(ctx, CmdPSVar, probeExecTimeout, probeMaxBytes)
	if err != nil {
		return "", "", fmt.Errorf("run %q: %w", CmdPSVar, err)
	}
	if psRes.TimedOut {
		return "", "", fmt.Errorf("probe command %q timed out", CmdPSVar)
	}
	detected, evidence := Detect(Probes{OSExpand: osRes.Stdout, PSVar: psRes.Stdout})
	return detected, evidence, nil
}
