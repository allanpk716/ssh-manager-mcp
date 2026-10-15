package policy

import (
	"ssh-manager-mcp/internal/models"
	"testing"
)

// evalWindows 以 PowerShell 或 cmd 判定一条命令,返回完整结论,是
// Windows 判定对外行为断言的主要入口(试算命令与执行工具都只调 Evaluate)。
func evalWindows(t *testing.T, shell models.ShellType, command string) Decision {
	t.Helper()
	return Evaluate(shell, command, nil)
}

// TestEvaluate_WindowsBlocksFormatCommands:格式化类命令(cmd 的 format
// 与 PowerShell 的 Format-Volume)一律拦截,命中 WIN-FORMAT;命令名匹配
// 不区分大小写,全路径与带可执行扩展名的写法同样命中,两类 shell 共用
// 同一套规则。
func TestEvaluate_WindowsBlocksFormatCommands(t *testing.T) {
	tests := []struct {
		name    string
		shell   models.ShellType
		command string
	}{
		{"powershell Format-Volume", models.ShellPowerShell, "Format-Volume -DriveLetter D"},
		{"powershell 全小写", models.ShellPowerShell, "format-volume -drivetter d"},
		{"powershell 大小写混杂", models.ShellPowerShell, "FORMAT-VOLUME -DriveLetter D"},
		{"powershell 带 -Force", models.ShellPowerShell, "Format-Volume -DriveLetter D -Force"},
		{"cmd format", models.ShellWindowsCmd, "format D:"},
		{"cmd 全大写", models.ShellWindowsCmd, "FORMAT D: /FS:NTFS"},
		{"cmd 带扩展名", models.ShellWindowsCmd, "format.com D:"},
		{"cmd 全路径", models.ShellWindowsCmd, `C:\Windows\System32\format.com D:`},
		{"powershell 跑 format.com", models.ShellPowerShell, "format.com D:"},
	}
	for _, tt := range tests {
		t.Run(tt.name+"/"+string(tt.shell), func(t *testing.T) {
			got := evalWindows(t, tt.shell, tt.command)
			if got.Disposition != DispositionBlock {
				t.Fatalf("Evaluate(%s, %q) disposition = %v, want 拦截", tt.shell, tt.command, got.Disposition)
			}
			if got.RuleID != "WIN-FORMAT" {
				t.Fatalf("Evaluate(%s, %q) rule id = %q, want WIN-FORMAT", tt.shell, tt.command, got.RuleID)
			}
			if got.Explanation == "" || got.Alternative == "" {
				t.Fatalf("Evaluate(%s, %q) must carry explanation and alternative, got %+v", tt.shell, tt.command, got)
			}
		})
	}
}

// TestEvaluate_WindowsBlocksPartitionTools:清盘与改写磁盘/分区结构的
// 命令(diskpart、Clear-Disk、Initialize-Disk)一律拦截,命中
// WIN-PARTITION;大小写与全路径写法同样命中。
func TestEvaluate_WindowsBlocksPartitionTools(t *testing.T) {
	tests := []struct {
		name    string
		shell   models.ShellType
		command string
	}{
		{"powershell Clear-Disk", models.ShellPowerShell, "Clear-Disk -Number 1"},
		{"powershell Clear-Disk 小写", models.ShellPowerShell, "clear-disk -number 1 -removeoem"},
		{"powershell Initialize-Disk", models.ShellPowerShell, "Initialize-Disk -Number 2"},
		{"powershell Initialize-Disk 大写", models.ShellPowerShell, "INITIALIZE-DISK -NUMBER 2 -PARTITIONSTYLE GPT"},
		{"cmd diskpart", models.ShellWindowsCmd, "diskpart"},
		{"cmd diskpart 大写", models.ShellWindowsCmd, "DISKPART /S clean.txt"},
		{"cmd diskpart 全路径", models.ShellWindowsCmd, `C:\Windows\System32\diskpart.exe`},
		{"powershell 跑 diskpart", models.ShellPowerShell, "diskpart"},
	}
	for _, tt := range tests {
		t.Run(tt.name+"/"+string(tt.shell), func(t *testing.T) {
			got := evalWindows(t, tt.shell, tt.command)
			if got.Disposition != DispositionBlock || got.RuleID != "WIN-PARTITION" {
				t.Fatalf("Evaluate(%s, %q) = %+v, want 拦截/WIN-PARTITION", tt.shell, tt.command, got)
			}
		})
	}
}

// TestEvaluate_WindowsAllowsPartitionLookalikes:只读的磁盘查询命令
// (Get-Disk、Get-Partition)照常放行。
func TestEvaluate_WindowsAllowsPartitionLookalikes(t *testing.T) {
	tests := []struct {
		name    string
		shell   models.ShellType
		command string
	}{
		{"Get-Disk", models.ShellPowerShell, "Get-Disk"},
		{"Get-Partition", models.ShellPowerShell, "Get-Partition -DiskNumber 1"},
		{"Get-Volume 使用统计", models.ShellPowerShell, "Get-Volume | Sort-Object SizeRemaining"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evalWindows(t, tt.shell, tt.command)
			if got.Disposition != DispositionAllow {
				t.Fatalf("Evaluate(%s, %q) = %+v, want 放行", tt.shell, tt.command, got)
			}
		})
	}
}

// TestEvaluate_WindowsBlocksDataWipe:擦除整盘空闲空间(cipher /w)与
// 删除系统还原点(vssadmin delete shadows)一律拦截,命中 WIN-DATA-WIPE;
// 大小写与驱动器写法变体同样命中。
func TestEvaluate_WindowsBlocksDataWipe(t *testing.T) {
	tests := []struct {
		name    string
		shell   models.ShellType
		command string
	}{
		{"cmd cipher /w 当前目录所在盘", models.ShellWindowsCmd, "cipher /w"},
		{"cmd cipher /w:C", models.ShellWindowsCmd, "cipher /w:C"},
		{"cmd cipher /w: 带空格", models.ShellWindowsCmd, "cipher /w: D:"},
		{"cmd CIPHER 大写", models.ShellWindowsCmd, "CIPHER /W:C:\\"},
		{"cmd vssadmin 删还原点", models.ShellWindowsCmd, "vssadmin delete shadows /all"},
		{"cmd vssadmin 大写", models.ShellWindowsCmd, "VSSADMIN DELETE SHADOWS /ALL /QUIET"},
		{"powershell 跑 vssadmin", models.ShellPowerShell, "vssadmin delete shadows /for=C:"},
		{"powershell 跑 cipher", models.ShellPowerShell, "cipher /w:C"},
	}
	for _, tt := range tests {
		t.Run(tt.name+"/"+string(tt.shell), func(t *testing.T) {
			got := evalWindows(t, tt.shell, tt.command)
			if got.Disposition != DispositionBlock || got.RuleID != "WIN-DATA-WIPE" {
				t.Fatalf("Evaluate(%s, %q) = %+v, want 拦截/WIN-DATA-WIPE", tt.shell, tt.command, got)
			}
		})
	}
}

// TestEvaluate_WindowsAllowsBenignCipherAndVssadmin:cipher 的加解密文件
// 与 vssadmin 的只读子命令照常放行。
func TestEvaluate_WindowsAllowsBenignCipherAndVssadmin(t *testing.T) {
	tests := []struct {
		name    string
		shell   models.ShellType
		command string
	}{
		{"cipher 加密单个文件", models.ShellWindowsCmd, "cipher /e C:\\temp\\secret.txt"},
		{"cipher 解密目录", models.ShellWindowsCmd, "cipher /d C:\\temp\\docs"},
		{"vssadmin 列还原点", models.ShellWindowsCmd, "vssadmin list shadows"},
		{"vssadmin 列写入器", models.ShellWindowsCmd, "vssadmin list writers"},
	}
	for _, tt := range tests {
		t.Run(tt.name+"/"+string(tt.shell), func(t *testing.T) {
			got := evalWindows(t, tt.shell, tt.command)
			if got.Disposition != DispositionAllow {
				t.Fatalf("Evaluate(%s, %q) = %+v, want 放行", tt.shell, tt.command, got)
			}
		})
	}
}

// TestEvaluate_WindowsBlocksCriticalPathDeletion:递归删除盘根或系统
// 关键路径(C:\、C:\Windows、C:\Program Files、C:\Users 本身)一律
// 拦截,命中 WIN-CRITICAL-DELETE;删除类命令名带别名表
// (Remove-Item 与 rm、del、erase、rd、rmdir、ri)且不区分大小写,
// 路径不区分大小写、正反斜杠等价,结尾通配、点段、升层等变体判定一致。
func TestEvaluate_WindowsBlocksCriticalPathDeletion(t *testing.T) {
	tests := []struct {
		name    string
		shell   models.ShellType
		command string
	}{
		{"Remove-Item 删盘根", models.ShellPowerShell, "Remove-Item -Recurse -Force C:\\"},
		{"Remove-Item 删 Windows", models.ShellPowerShell, "Remove-Item -Recurse C:\\Windows"},
		{"小写命令名与路径", models.ShellPowerShell, "remove-item -recurse c:\\windows"},
		{"全大写", models.ShellWindowsCmd, "REMOVE-ITEM -RECURSE C:\\WINDOWS"},
		{"rm 别名 + 短递归旗标", models.ShellPowerShell, "rm -r C:\\Users"},
		{"ri 别名", models.ShellPowerShell, "ri -Recurse C:\\Users"},
		{"PowerShell del 别名", models.ShellPowerShell, "del -Recurse C:\\Windows"},
		{"cmd rd /s", models.ShellWindowsCmd, "rd /s C:\\Windows"},
		{"cmd rmdir /s /q", models.ShellWindowsCmd, "rmdir /s /q c:\\windows"},
		{"cmd del /s", models.ShellWindowsCmd, "del /s /q C:\\Users"},
		{"正斜杠路径", models.ShellPowerShell, "Remove-Item -Recurse C:/Windows"},
		{"双反斜杠与结尾斜杠", models.ShellPowerShell, "Remove-Item -Recurse C:\\\\Windows\\\\"},
		{"盘根下通配", models.ShellPowerShell, "Remove-Item -Recurse C:\\*"},
		{"盘根下通配带结尾斜杠", models.ShellWindowsCmd, "rd /s /q C:\\*\\"},
		{"关键路径下通配", models.ShellWindowsCmd, "rd /s /q C:\\Windows\\*"},
		{"通配加点扩展", models.ShellWindowsCmd, "del /s C:\\Users\\*.*"},
		{"点段", models.ShellPowerShell, "Remove-Item -Recurse C:\\.\\Users"},
		{"升层回到盘根", models.ShellPowerShell, "Remove-Item -Recurse C:\\Users\\Public\\..\\.."},
		{"带空格路径加引号", models.ShellPowerShell, `Remove-Item -Recurse "C:\Program Files"`},
		{"cmd 带空格路径加引号", models.ShellWindowsCmd, `rmdir /s "C:\Program Files"`},
		{"多个目标其中一个关键", models.ShellPowerShell, "Remove-Item -Recurse C:\\temp C:\\Users"},
		{"冒号带值写路径", models.ShellPowerShell, "Remove-Item -Path:C:\\Windows -Recurse"},
		{"不带盘符的根定位", models.ShellPowerShell, "Remove-Item -Recurse \\Windows"},
		{"正斜杠根定位路径", models.ShellPowerShell, "Remove-Item -Recurse /Windows"},
	}
	for _, tt := range tests {
		t.Run(tt.name+"/"+string(tt.shell), func(t *testing.T) {
			got := evalWindows(t, tt.shell, tt.command)
			if got.Disposition != DispositionBlock || got.RuleID != "WIN-CRITICAL-DELETE" {
				t.Fatalf("Evaluate(%s, %q) = %+v, want 拦截/WIN-CRITICAL-DELETE", tt.shell, tt.command, got)
			}
		})
	}
}

// TestEvaluate_WindowsAllowsOrdinaryDeletion:目标写死且不在系统关键路径
// 本身上的普通删除照常放行:关键路径里的子目录、别的盘、写死的普通
// 文件、不递归的删除。
func TestEvaluate_WindowsAllowsOrdinaryDeletion(t *testing.T) {
	tests := []struct {
		name    string
		shell   models.ShellType
		command string
	}{
		{"删关键路径里的子目录", models.ShellPowerShell, "Remove-Item -Recurse C:\\Windows\\Temp"},
		{"删普通目录", models.ShellPowerShell, "Remove-Item -Recurse C:\\temp\\build"},
		{"删别的盘", models.ShellPowerShell, "Remove-Item -Recurse D:\\"},
		{"删写死的普通文件", models.ShellPowerShell, "Remove-Item C:\\temp\\debug.log"},
		{"cmd 删写死的普通文件", models.ShellWindowsCmd, "del C:\\temp\\debug.log"},
		{"erase 删普通文件", models.ShellWindowsCmd, "erase old.txt"},
		{"不递归删关键路径里的文件", models.ShellPowerShell, "Remove-Item C:\\Windows\\notes.txt"},
		{"cmd 删子目录树", models.ShellWindowsCmd, "rd /s /q C:\\Temp\\build"},
		{"相对路径递归删", models.ShellPowerShell, "Remove-Item -Recurse .\\build"},
		{"当前目录通配", models.ShellWindowsCmd, "del /s *"},
		{"PowerShell 当前目录通配", models.ShellPowerShell, "Remove-Item -Recurse *"},
		{"遍历删除当前目录日志", models.ShellWindowsCmd, `for %f in (*.log) do del "%f"`},
		{"cmd 带属性过滤的删除", models.ShellWindowsCmd, "del /a:h *.log"},
	}
	for _, tt := range tests {
		t.Run(tt.name+"/"+string(tt.shell), func(t *testing.T) {
			got := evalWindows(t, tt.shell, tt.command)
			if got.Disposition != DispositionAllow {
				t.Fatalf("Evaluate(%s, %q) = %+v, want 放行", tt.shell, tt.command, got)
			}
		})
	}
}

// TestEvaluate_WindowsConfirmsRecursiveDeleteWithUnknownTarget:递归删除
// 的目标在执行前无法确定(含 $ 变量、% 变量%、裸 . 或 ..)时判需确认,
// 命中 WIN-DELETE-TARGET-UNKNOWN——兑住变量为空时删错位置的情况。
func TestEvaluate_WindowsConfirmsRecursiveDeleteWithUnknownTarget(t *testing.T) {
	tests := []struct {
		name    string
		shell   models.ShellType
		command string
	}{
		{"Remove-Item 变量目标", models.ShellPowerShell, "Remove-Item -Recurse $x"},
		{"Remove-Item 环境变量拼接", models.ShellPowerShell, "Remove-Item -Path $env:TEMP\\build -Recurse"},
		{"rm 别名变量目标", models.ShellPowerShell, "rm -r $DIR"},
		{"cmd rd /s 变量目标", models.ShellWindowsCmd, "rd /s %TEMP%"},
		{"cmd del /s 批参数", models.ShellWindowsCmd, "del /s %1"},
		{"裸点当前目录", models.ShellPowerShell, "Remove-Item -Recurse ."},
		{"cmd 裸双点", models.ShellWindowsCmd, "rd /s .."},
		{"带斜杠的裸点", models.ShellPowerShell, "Remove-Item -Recurse .\\"},
		{"写死与动态目标混合", models.ShellPowerShell, "Remove-Item -Recurse C:\\temp $x"},
	}
	for _, tt := range tests {
		t.Run(tt.name+"/"+string(tt.shell), func(t *testing.T) {
			got := evalWindows(t, tt.shell, tt.command)
			if got.Disposition != DispositionConfirmRequired || got.RuleID != "WIN-DELETE-TARGET-UNKNOWN" {
				t.Fatalf("Evaluate(%s, %q) = %+v, want 需确认/WIN-DELETE-TARGET-UNKNOWN", tt.shell, tt.command, got)
			}
			if got.Explanation == "" || got.Alternative == "" {
				t.Fatalf("Evaluate(%s, %q) must carry explanation and alternative, got %+v", tt.shell, tt.command, got)
			}
		})
	}
}

// TestEvaluate_WindowsAllowsDeterminateDeletesAndVariables:不递归的变量
// 目标删除、以及含变量但不涉及删除的日常命令照常放行。
func TestEvaluate_WindowsAllowsDeterminateDeletesAndVariables(t *testing.T) {
	tests := []struct {
		name    string
		shell   models.ShellType
		command string
	}{
		{"不递归的变量目标删除", models.ShellPowerShell, "Remove-Item $log"},
		{"不递归的环境变量文件", models.ShellPowerShell, "Remove-Item $env:TEMP\\debug.log"},
		{"变量路径导航", models.ShellPowerShell, "Set-Location $env:TEMP"},
		{"cmd 变量路径导航", models.ShellWindowsCmd, "cd %TEMP%"},
		{"变量参数的日常命令", models.ShellPowerShell, "Get-ChildItem $HOME\\docs"},
		{"写死的递归普通目录", models.ShellPowerShell, "Remove-Item -Recurse C:\\temp\\build"},
	}
	for _, tt := range tests {
		t.Run(tt.name+"/"+string(tt.shell), func(t *testing.T) {
			got := evalWindows(t, tt.shell, tt.command)
			if got.Disposition != DispositionAllow {
				t.Fatalf("Evaluate(%s, %q) = %+v, want 放行", tt.shell, tt.command, got)
			}
		})
	}
}

// TestEvaluate_WindowsConfirmsLockoutOps:会把自己锁在门外的操作
// (netsh advfirewall 改防火墙、Set-NetFirewallProfile、停 sshd 服务、
// 禁用网卡)判需确认,命中 WIN-LOCKOUT;只读的 show 与 Get- 查询
// 命令、停其他服务照常放行。
func TestEvaluate_WindowsConfirmsLockoutOps(t *testing.T) {
	tests := []struct {
		name    string
		shell   models.ShellType
		command string
	}{
		{"netsh 加防火墙规则", models.ShellWindowsCmd, `netsh advfirewall firewall add rule name="agent" dir=in action=allow protocol=tcp localport=8080`},
		{"netsh 关防火墙", models.ShellWindowsCmd, "netsh advfirewall set allprofiles state off"},
		{"netsh 大写删规则", models.ShellWindowsCmd, `NETSH ADVFIREWALL FIREWALL DELETE RULE NAME=SSH`},
		{"powershell 跑 netsh", models.ShellPowerShell, "netsh advfirewall firewall delete rule name=all"},
		{"Set-NetFirewallProfile", models.ShellPowerShell, "Set-NetFirewallProfile -Profile Public -Enabled False"},
		{"set-netfirewallprofile 小写", models.ShellPowerShell, "set-netfirewallprofile -allprofiles enabled false"},
		{"Stop-Service sshd", models.ShellPowerShell, "Stop-Service sshd"},
		{"Stop-Service -Name", models.ShellPowerShell, "Stop-Service -Name sshd"},
		{"spsv 别名", models.ShellPowerShell, "spsv sshd"},
		{"cmd net stop sshd", models.ShellWindowsCmd, "net stop sshd"},
		{"Disable-NetAdapter", models.ShellPowerShell, "Disable-NetAdapter -Name Ethernet"},
		{"disable-netadapter 小写裸名", models.ShellPowerShell, "disable-netadapter ethernet"},
	}
	for _, tt := range tests {
		t.Run(tt.name+"/"+string(tt.shell), func(t *testing.T) {
			got := evalWindows(t, tt.shell, tt.command)
			if got.Disposition != DispositionConfirmRequired || got.RuleID != "WIN-LOCKOUT" {
				t.Fatalf("Evaluate(%s, %q) = %+v, want 需确认/WIN-LOCKOUT", tt.shell, tt.command, got)
			}
			if got.Explanation == "" || got.Alternative == "" {
				t.Fatalf("Evaluate(%s, %q) must carry explanation and alternative, got %+v", tt.shell, tt.command, got)
			}
		})
	}
}

// TestEvaluate_WindowsAllowsReadonlyAndOtherServices:只读查询与不碰
// 通道的服务操作照常放行。
func TestEvaluate_WindowsAllowsReadonlyAndOtherServices(t *testing.T) {
	tests := []struct {
		name    string
		shell   models.ShellType
		command string
	}{
		{"netsh 只读 show", models.ShellWindowsCmd, "netsh advfirewall show allprofiles"},
		{"netsh 其他子树", models.ShellWindowsCmd, "netsh interface show interface"},
		{"Get-NetFirewallProfile", models.ShellPowerShell, "Get-NetFirewallProfile"},
		{"Get-NetFirewallRule", models.ShellPowerShell, "Get-NetFirewallRule -Enabled True"},
		{"Get-Service sshd", models.ShellPowerShell, "Get-Service sshd"},
		{"停其他服务", models.ShellPowerShell, "Stop-Service WinDefend"},
		{"Restart-Service sshd", models.ShellPowerShell, "Restart-Service sshd"},
		{"Get-NetAdapter", models.ShellPowerShell, "Get-NetAdapter | Format-Table Name,Status"},
	}
	for _, tt := range tests {
		t.Run(tt.name+"/"+string(tt.shell), func(t *testing.T) {
			got := evalWindows(t, tt.shell, tt.command)
			if got.Disposition != DispositionAllow {
				t.Fatalf("Evaluate(%s, %q) = %+v, want 放行", tt.shell, tt.command, got)
			}
		})
	}
}

// TestEvaluate_WindowsConfirmsDynamicExecution:动态执行写法(iex、
// Invoke-Expression、-EncodedCommand)且文本中出现危险命令名时判需确认,
// 命中 WIN-DYNAMIC-EXEC——拼出来的内层没法拆开判定,交 owner 把关。
func TestEvaluate_WindowsConfirmsDynamicExecution(t *testing.T) {
	tests := []struct {
		name    string
		shell   models.ShellType
		command string
	}{
		{"iex 拼删除命令", models.ShellPowerShell, `iex "Remove-Item -Recurse C:\Windows -Force"`},
		{"iex 拼 rm 别名", models.ShellPowerShell, `iex "rm -r C:\"`},
		{"Invoke-Expression 拼 format", models.ShellPowerShell, "Invoke-Expression 'format D:'"},
		{"invoke-expression 小写", models.ShellPowerShell, `invoke-expression "del /s C:\Users"`},
		{"EncodedCommand 旁边有危险名", models.ShellPowerShell, "Remove-Item $log; powershell -EncodedCommand UgBtACAALQByIDpcAA=="},
		{"cmd 里的 powershell -EncodedCommand", models.ShellWindowsCmd, "echo rd /s C:\\ & powershell -EncodedCommand UgBtACAA=="},
	}
	for _, tt := range tests {
		t.Run(tt.name+"/"+string(tt.shell), func(t *testing.T) {
			got := evalWindows(t, tt.shell, tt.command)
			if got.Disposition != DispositionConfirmRequired || got.RuleID != "WIN-DYNAMIC-EXEC" {
				t.Fatalf("Evaluate(%s, %q) = %+v, want 需确认/WIN-DYNAMIC-EXEC", tt.shell, tt.command, got)
			}
			if got.Explanation == "" || got.Alternative == "" {
				t.Fatalf("Evaluate(%s, %q) must carry explanation and alternative, got %+v", tt.shell, tt.command, got)
			}
		})
	}
}

// TestEvaluate_WindowsAllowsDynamicExecutionWithoutDanger:动态执行写法
// 但文本里没有危险命令名时照常放行;只是提到这些词的命令也不误触。
func TestEvaluate_WindowsAllowsDynamicExecutionWithoutDanger(t *testing.T) {
	tests := []struct {
		name    string
		shell   models.ShellType
		command string
	}{
		{"iex 跑脚本文件", models.ShellPowerShell, "iex (Get-Content C:\\scripts\\build.ps1)"},
		{"iex 变量命令无危险名", models.ShellPowerShell, "iex $cmd"},
		{"Invoke-Expression 跑 Get-Date", models.ShellPowerShell, `Invoke-Expression "Get-Date"`},
		{"纯 EncodedCommand 无危险名", models.ShellPowerShell, "powershell -EncodedCommand UgBlAHQAIAB=="},
		{"iex 拼 Format-Table", models.ShellPowerShell, `iex "Get-Process | Format-Table"`},
		{"查命令文档", models.ShellPowerShell, "Get-Command Invoke-Expression"},
		{"打印文字提到 iex", models.ShellPowerShell, `Write-Host "Invoke-Expression is risky"`},
	}
	for _, tt := range tests {
		t.Run(tt.name+"/"+string(tt.shell), func(t *testing.T) {
			got := evalWindows(t, tt.shell, tt.command)
			if got.Disposition != DispositionAllow {
				t.Fatalf("Evaluate(%s, %q) = %+v, want 放行", tt.shell, tt.command, got)
			}
		})
	}
}

// TestEvaluate_WindowsCombinedCommandsTakeMostSevere:组合命令拆出的
// 每段分别判定,整体取最严重的处置,规则编号对应最严重的那段;
// 需确认与拦截同现时拦截优先。
func TestEvaluate_WindowsCombinedCommandsTakeMostSevere(t *testing.T) {
	tests := []struct {
		name       string
		shell      models.ShellType
		command    string
		want       Disposition
		wantRuleID string
	}{
		{"分号后删关键路径", models.ShellPowerShell, "Get-ChildItem; Remove-Item -Recurse C:\\Windows", DispositionBlock, "WIN-CRITICAL-DELETE"},
		{"cmd 双与号接 format", models.ShellWindowsCmd, "dir && format D:", DispositionBlock, "WIN-FORMAT"},
		{"cmd 单与号接 rd /s 盘根", models.ShellWindowsCmd, "cd C:\\temp & rd /s C:\\", DispositionBlock, "WIN-CRITICAL-DELETE"},
		{"需确认藏在管道后", models.ShellPowerShell, "Get-Process; Stop-Service sshd", DispositionConfirmRequired, "WIN-LOCKOUT"},
		{"需确认与拦截同现", models.ShellWindowsCmd, "netsh advfirewall set allprofiles state off; format D:", DispositionBlock, "WIN-FORMAT"},
		{"动态目标递归删单独成段", models.ShellPowerShell, "Remove-Item -Recurse $x; echo done", DispositionConfirmRequired, "WIN-DELETE-TARGET-UNKNOWN"},
		{"换行分段", models.ShellPowerShell, "Get-ChildItem\r\nRemove-Item -Recurse C:\\Users", DispositionBlock, "WIN-CRITICAL-DELETE"},
		{"重定向流合并不误判", models.ShellPowerShell, "Get-Process 2>&1 | Out-Null", DispositionAllow, ""},
		{"引号里的分隔符不成段", models.ShellPowerShell, `Write-Host "a;b|c&d"`, DispositionAllow, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name+"/"+string(tt.shell), func(t *testing.T) {
			got := evalWindows(t, tt.shell, tt.command)
			if got.Disposition != tt.want || got.RuleID != tt.wantRuleID {
				t.Fatalf("Evaluate(%s, %q) = %+v, want %v/%s", tt.shell, tt.command, got, tt.want, tt.wantRuleID)
			}
		})
	}
}

// TestEvaluate_WindowsAllowsEverydayCommands:日常 PowerShell 与 cmd 命令
// (含验收点名的 Get-ChildItem、dir、删除写死的普通文件)一律放行,
// 日常工作不被打断。
func TestEvaluate_WindowsAllowsEverydayCommands(t *testing.T) {
	tests := []struct {
		name    string
		shell   models.ShellType
		command string
	}{
		{"Get-ChildItem", models.ShellPowerShell, "Get-ChildItem C:\\"},
		{"Get-ChildItem 管道统计", models.ShellPowerShell, "Get-ChildItem C:\\Windows\\System32 | Measure-Object -Line"},
		{"dir", models.ShellWindowsCmd, "dir"},
		{"dir 带路径", models.ShellWindowsCmd, "dir C:\\Windows"},
		{"type 看日志", models.ShellWindowsCmd, "type C:\\logs\\app.log"},
		{"Get-Content 看日志尾部", models.ShellPowerShell, "Get-Content C:\\logs\\app.log -Tail 100"},
		{"进程排序", models.ShellPowerShell, "Get-Process | Sort-Object CPU -Descending | Select-Object -First 10"},
		{"切目录再看", models.ShellPowerShell, "Set-Location C:\\work; Get-ChildItem"},
		{"建目录", models.ShellPowerShell, "New-Item -ItemType Directory C:\\work\\build"},
		{"复制文件", models.ShellPowerShell, "Copy-Item C:\\a.txt C:\\b.txt"},
		{"移动文件", models.ShellPowerShell, "Move-Item C:\\a.txt D:\\"},
		{"删写死的普通文件", models.ShellPowerShell, "Remove-Item C:\\temp\\debug.log"},
		{"cmd 删写死的普通文件", models.ShellWindowsCmd, "del C:\\temp\\debug.log"},
		{"ping", models.ShellWindowsCmd, "ping example.com"},
		{"ipconfig", models.ShellWindowsCmd, "ipconfig /all"},
		{"systeminfo", models.ShellWindowsCmd, "systeminfo"},
		{"tasklist 过滤", models.ShellWindowsCmd, "tasklist | findstr python"},
		{"查询停止的服务", models.ShellPowerShell, "Get-Service | Where-Object Status -eq Stopped"},
		{"重定向写文件", models.ShellWindowsCmd, "echo done > C:\\temp\\out.txt"},
		{"建目录与上跳", models.ShellWindowsCmd, "mkdir build & cd .."},
		{"打印删除文字不误触", models.ShellPowerShell, `Write-Output "Remove-Item -Recurse C:\Windows"`},
	}
	for _, tt := range tests {
		t.Run(tt.name+"/"+string(tt.shell), func(t *testing.T) {
			got := evalWindows(t, tt.shell, tt.command)
			if got.Disposition != DispositionAllow {
				t.Fatalf("Evaluate(%s, %q) = %+v, want 放行", tt.shell, tt.command, got)
			}
			if got.RuleID != "" {
				t.Fatalf("Evaluate(%s, %q) rule id = %q, want empty on 放行", tt.shell, tt.command, got.RuleID)
			}
		})
	}
}

// TestEvaluate_WindowsDisabledDefaultRuleByID:按编号停用默认规则后,
// 该规则不再参与判定,其余规则照常。
func TestEvaluate_WindowsDisabledDefaultRuleByID(t *testing.T) {
	rules := &RuleSet{DisabledDefaultRuleIDs: []string{"WIN-FORMAT"}}
	if got := Evaluate(models.ShellWindowsCmd, "format D:", rules); got.Disposition != DispositionAllow {
		t.Fatalf("disabled WIN-FORMAT still fired: %+v", got)
	}
	if got := Evaluate(models.ShellPowerShell, "diskpart", rules); got.Disposition != DispositionBlock {
		t.Fatalf("other rules must stay active: %+v", got)
	}
	lockoutOff := &RuleSet{DisabledDefaultRuleIDs: []string{"WIN-LOCKOUT"}}
	if got := Evaluate(models.ShellPowerShell, "Stop-Service sshd", lockoutOff); got.Disposition != DispositionAllow {
		t.Fatalf("disabled WIN-LOCKOUT still fired: %+v", got)
	}
}

// TestEvaluate_WindowsAllowsFormatLookalikes:只是名字长得像格式化命令的
// 日常命令(Format-Table 这类格式化输出表格的命令、打印文字)必须放行。
func TestEvaluate_WindowsAllowsFormatLookalikes(t *testing.T) {
	tests := []struct {
		name    string
		shell   models.ShellType
		command string
	}{
		{"Format-Table", models.ShellPowerShell, "Get-Process | Format-Table"},
		{"Format-List", models.ShellPowerShell, "Get-Service | Format-List -Property Name"},
		{"Format-Hex", models.ShellPowerShell, "Format-Hex -Path C:\\temp\\dump.bin"},
		{"打印文字里的 format", models.ShellWindowsCmd, "echo format D:"},
		{"打印文字里的 Format-Volume", models.ShellPowerShell, `Write-Host "Format-Volume -DriveLetter D"`},
	}
	for _, tt := range tests {
		t.Run(tt.name+"/"+string(tt.shell), func(t *testing.T) {
			got := evalWindows(t, tt.shell, tt.command)
			if got.Disposition != DispositionAllow {
				t.Fatalf("Evaluate(%s, %q) = %+v, want 放行", tt.shell, tt.command, got)
			}
			if got.RuleID != "" {
				t.Fatalf("Evaluate(%s, %q) rule id = %q, want empty on 放行", tt.shell, tt.command, got.RuleID)
			}
		})
	}
}
