package policy

import "testing"

// 嵌套执行递归判定(ticket #13 / 规格 #8 第 15、16 条)的测试:识别把
// 一段命令文本交给另一个 shell 执行的写法——bash -c、sh -c、zsh -c
// (dash、ksh 同形)、pwsh -c / -Command、powershell -Command / -c、
// cmd /c 与 /k、wsl(含 wsl -e、wsl --exec、wsl -- 后接命令)。
// 内层是写死的字符串时按内层 shell 的判定规则递归(跨 shell 也成立),
// 命中时返回内层规则的编号;内层是拼出来的且带危险命令名、或嵌套超过
// 三层时判需确认(稳定编号 NESTED-DYNAMIC / NESTED-TOO-DEEP)。

// TestEvaluate_NestedPOSIXLiteralInnerJudgedRecursively:POSIX 命令里的
// 嵌套执行,内层写死时按 POSIX 规则递归判定,sudo 等前缀先剥掉再识别;
// 内层是普通命令时照常放行。
func TestEvaluate_NestedPOSIXLiteralInnerJudgedRecursively(t *testing.T) {
	tests := []struct {
		name       string
		command    string
		wantRuleID string // 空 = 期望放行
	}{
		{"bash -c 格式化", `bash -c "mkfs.ext4 /dev/sdb"`, "POSIX-FORMAT"},
		{"sh -c 抹签名", `sh -c 'wipefs -a /dev/sda'`, "POSIX-FORMAT"},
		{"zsh -c 删关键路径", `zsh -c "rm -rf /etc"`, "POSIX-CRITICAL-DELETE"},
		{"dash -c 写盘", `dash -c "dd if=a of=/dev/sda"`, "POSIX-RAW-DISK-WRITE"},
		{"ksh -c 改根权限", `ksh -c 'chmod -R 777 /'`, "POSIX-RECURSIVE-ROOT-PERM"},
		{"短旗标簇含 c", `bash -lc "mkfs.ext4 /dev/sdb"`, "POSIX-FORMAT"},
		{"sudo 前缀先剥掉", `sudo bash -c "mkfs.ext4 /dev/sdb"`, "POSIX-FORMAT"},
		{"nohup 前缀", `nohup zsh -c "rm -rf /"`, "POSIX-CRITICAL-DELETE"},
		{"组合命令里藏在后面", `ls; bash -c "mkfs.ext4 /dev/sdb"`, "POSIX-FORMAT"},
		{"命令替换里的嵌套执行", `echo $(bash -c 'chmod -R 777 /')`, "POSIX-RECURSIVE-ROOT-PERM"},
		{"bash -c 后的位置参数不算内层", `bash -c 'mkfs.ext4 /dev/sdb' cleanup /dev/sdb`, "POSIX-FORMAT"},
		{"内层普通命令放行", `bash -c "ls -la /tmp"`, ""},
		{"跑脚本文件不是嵌套执行", `bash deploy.sh`, ""},
		{"无 -c 的裸 shell 放行", `sh`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Evaluate(ShellPOSIX, tt.command, nil)
			want := DispositionAllow
			if tt.wantRuleID != "" {
				want = DispositionBlock
			}
			if got.Disposition != want || got.RuleID != tt.wantRuleID {
				t.Fatalf("Evaluate(%q) = %+v, want %v/%s", tt.command, got, want, tt.wantRuleID)
			}
		})
	}
}

// TestEvaluate_NestedWindowsLaunchersJudgedRecursively:PowerShell 与 cmd
// 命令里的嵌套执行(pwsh / powershell 的 -c 与 -Command、cmd 的 /c 与
// /k、wsl)同样递归判定:内层按对应 shell 的规则走,wsl 的内层按
// POSIX 判定;多层跨 shell 串联(cmd /c wsl bash -c …)时引号里的内层
// 命令文本不散架,命中返回最内层规则的编号。
func TestEvaluate_NestedWindowsLaunchersJudgedRecursively(t *testing.T) {
	tests := []struct {
		name       string
		shell      ShellType
		command    string
		wantRuleID string
	}{
		{"powershell -Command 格式化(验收用例)", ShellPowerShell, `powershell -Command "Format-Volume -DriveLetter D"`, "WIN-FORMAT"},
		{"pwsh -c 删关键路径", ShellPowerShell, `pwsh -c "Remove-Item -Recurse C:\Windows"`, "WIN-CRITICAL-DELETE"},
		{"powershell 大小写不敏感", ShellPowerShell, `POWERSHELL -COMMAND "FORMAT-VOLUME -DRIVELETTER D"`, "WIN-FORMAT"},
		{"pwsh -Command 内层分号分段", ShellPowerShell, `pwsh -Command "Get-Date; format D:"`, "WIN-FORMAT"},
		{"cmd /c 格式化", ShellWindowsCmd, `cmd /c format D:`, "WIN-FORMAT"},
		{"cmd /k 删关键路径", ShellWindowsCmd, `cmd /k del /s C:\Users`, "WIN-CRITICAL-DELETE"},
		{"cmd /c 带引号内层", ShellWindowsCmd, `cmd /c "rd /s /q C:\Windows"`, "WIN-CRITICAL-DELETE"},
		{"wsl 内层按 POSIX 判", ShellWindowsCmd, `cmd /c wsl rm -rf /`, "POSIX-CRITICAL-DELETE"},
		{"PowerShell 里的 wsl", ShellPowerShell, `powershell -Command "wsl mkfs.ext4 /dev/sdb"`, "POSIX-FORMAT"},
		{"wsl 带自身选项", ShellPowerShell, `wsl --distribution Ubuntu rm -rf /etc`, "POSIX-CRITICAL-DELETE"},
		{"三层跨 shell 串联", ShellWindowsCmd, `cmd /c wsl bash -c "rm -rf /etc"`, "POSIX-CRITICAL-DELETE"},
		{"内层是普通命令放行", ShellPowerShell, `powershell -Command "Get-Date"`, ""},
		{"cmd /c 普通命令放行", ShellWindowsCmd, `cmd /c dir C:\Windows`, ""},
		{"wsl 内层普通命令放行", ShellPowerShell, `wsl ls -la /mnt/c`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Evaluate(tt.shell, tt.command, nil)
			want := DispositionAllow
			if tt.wantRuleID != "" {
				want = DispositionBlock
			}
			if got.Disposition != want || got.RuleID != tt.wantRuleID {
				t.Fatalf("Evaluate(%s, %q) = %+v, want %v/%s", tt.shell, tt.command, got, want, tt.wantRuleID)
			}
		})
	}
}

// TestEvaluate_NestedDynamicInnerNeedsConfirmationWhenDangerous:内层是
// 拼出来的(变量、命令替换、拼接)且文本带危险命令名时判需确认,
// 编号 NESTED-DYNAMIC;没有危险命令名时照常放行(bash -c "$CMD")。
func TestEvaluate_NestedDynamicInnerNeedsConfirmationWhenDangerous(t *testing.T) {
	tests := []struct {
		name       string
		shell      ShellType
		command    string
		wantRuleID string // 空 = 期望放行
	}{
		{"变量内层无危险命令名(验收用例)", ShellPOSIX, `bash -c "$CMD"`, ""},
		{"变量拼接的删除目标", ShellPOSIX, `bash -c "rm -rf $DIR/"`, "NESTED-DYNAMIC"},
		{"命令替换带格式化命令名", ShellPOSIX, `bash -c "$(cat cmd.txt) mkfs"`, "NESTED-DYNAMIC"},
		{"拼接出格式化命令名", ShellPOSIX, `sh -c "mkfs.ext4 $DEV"`, "NESTED-DYNAMIC"},
		{"内层普通变量命令放行", ShellPOSIX, `bash -c "ls -la $DIR"`, ""},
		{"wsl 动态命令带危险名", ShellPOSIX, `wsl rm -rf $TARGET`, "NESTED-DYNAMIC"},
		{"wsl 动态命令无危险名", ShellPOSIX, `wsl ls $DIR`, ""},
		{"PowerShell 变量拼接删除", ShellPowerShell, `powershell -Command "Remove-Item -Recurse $x"`, "NESTED-DYNAMIC"},
		{"PowerShell 变量命令无危险名", ShellPowerShell, `pwsh -c "$cmd"`, ""},
		{"cmd 变量命令无危险名", ShellWindowsCmd, `cmd /c echo %TEMP%`, ""},
		{"cmd 变量删除带危险名", ShellWindowsCmd, `cmd /c rd /s %TEMP%`, "NESTED-DYNAMIC"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Evaluate(tt.shell, tt.command, nil)
			want := DispositionAllow
			if tt.wantRuleID != "" {
				want = DispositionConfirmRequired
			}
			if got.Disposition != want || got.RuleID != tt.wantRuleID {
				t.Fatalf("Evaluate(%s, %q) = %+v, want %v/%s", tt.shell, tt.command, got, want, tt.wantRuleID)
			}
			if want == DispositionConfirmRequired && (got.Explanation == "" || got.Alternative == "") {
				t.Fatalf("Evaluate(%s, %q) must carry explanation and alternative, got %+v", tt.shell, tt.command, got)
			}
		})
	}
}

// TestEvaluate_NestedDepthLimit:三层以内的嵌套执行逐层拆开正常判定;
// 要拆第四层时不再递归,内层文本带危险命令名判需确认(编号
// NESTED-TOO-DEEP),不带危险命令名照常放行。
func TestEvaluate_NestedDepthLimit(t *testing.T) {
	tests := []struct {
		name       string
		shell      ShellType
		command    string
		want       Disposition
		wantRuleID string
	}{
		{"两层照常递归", ShellPOSIX, `bash -c "bash -c 'mkfs.ext4 /dev/sdb'"`, DispositionBlock, "POSIX-FORMAT"},
		{"三层照常递归", ShellPOSIX, `bash -c "bash -c 'bash -c \"mkfs.ext4 /dev/sdb\"'"`, DispositionBlock, "POSIX-FORMAT"},
		{"第四层带危险命令名需确认", ShellPOSIX, `bash -c "bash -c 'bash -c \"bash -c mkfs.ext4 /dev/sdb\"'"`, DispositionConfirmRequired, "NESTED-TOO-DEEP"},
		{"第四层不带危险命令名放行", ShellPOSIX, `bash -c "bash -c 'bash -c \"bash -c ls\"'"`, DispositionAllow, ""},
		{"三层跨 shell 照常递归", ShellWindowsCmd, `cmd /c wsl bash -c "mkfs.ext4 /dev/sdb"`, DispositionBlock, "POSIX-FORMAT"},
		{"PowerShell 三层照常递归", ShellPowerShell, `powershell -Command "powershell -Command 'powershell -Command Format-Volume -DriveLetter D'"`, DispositionBlock, "WIN-FORMAT"},
		{"PowerShell 第四层带危险命令名需确认", ShellPowerShell, `powershell -Command "powershell -Command 'powershell -Command powershell -Command Format-Volume'"`, DispositionConfirmRequired, "NESTED-TOO-DEEP"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Evaluate(tt.shell, tt.command, nil)
			if got.Disposition != tt.want || got.RuleID != tt.wantRuleID {
				t.Fatalf("Evaluate(%s, %q) = %+v, want %v/%s", tt.shell, tt.command, got, tt.want, tt.wantRuleID)
			}
			if tt.want == DispositionConfirmRequired && (got.Explanation == "" || got.Alternative == "") {
				t.Fatalf("Evaluate(%s, %q) must carry explanation and alternative, got %+v", tt.shell, tt.command, got)
			}
		})
	}
}

// TestEvaluate_NestedCrossShellFromPOSIX:POSIX 命令里嵌套跨 shell 执行:
// pwsh -c / powershell -Command 的内层按 PowerShell 规则判定,cmd /c
// 的内层按 cmd 规则判定,wsl 的内层按 POSIX 规则判定(含 -e、--exec、
// -- 后接命令与自身带值选项的写法);跨 shell 命中返回内层规则的编号。
func TestEvaluate_NestedCrossShellFromPOSIX(t *testing.T) {
	tests := []struct {
		name       string
		command    string
		wantRuleID string
	}{
		{"pwsh -c 格式化", `pwsh -c "Format-Volume -DriveLetter D"`, "WIN-FORMAT"},
		{"pwsh -Command 删关键路径", `pwsh -Command "Remove-Item -Recurse C:\Windows"`, "WIN-CRITICAL-DELETE"},
		{"powershell -Command 大小写不敏感", `powershell -Command "FORMAT-VOLUME -DriveLetter D"`, "WIN-FORMAT"},
		{"powershell -c 短旗标", `powershell -c "vssadmin delete shadows /all"`, "WIN-DATA-WIPE"},
		{"不带引号的多参数拼接", `pwsh -Command Remove-Item -Recurse "C:\Program Files"`, "WIN-CRITICAL-DELETE"},
		{"cmd /c 格式化", `cmd /c format D:`, "WIN-FORMAT"},
		{"cmd /k 藏在后面", `cmd /k "del /s C:\Users"`, "WIN-CRITICAL-DELETE"},
		{"wsl 直接接命令", `wsl mkfs.ext4 /dev/sdb`, "POSIX-FORMAT"},
		{"wsl -e 直接执行", `wsl -e mkfs.ext4 /dev/sdb`, "POSIX-FORMAT"},
		{"wsl --exec 直接执行", `wsl --exec rm -rf /etc`, "POSIX-CRITICAL-DELETE"},
		{"wsl -- 分隔", `wsl -- rm -rf /etc`, "POSIX-CRITICAL-DELETE"},
		{"wsl 带发行版选项", `wsl -d Ubuntu-22.04 rm -rf /`, "POSIX-CRITICAL-DELETE"},
		{"wsl 长选项带取值", `wsl --user root chmod -R 777 /`, "POSIX-RECURSIVE-ROOT-PERM"},
		{"wsl 内层又套 shell", `wsl bash -c "rm -rf /etc"`, "POSIX-CRITICAL-DELETE"},
		{"sudo 前缀的 wsl", `sudo wsl mkfs.ext4 /dev/sdb`, "POSIX-FORMAT"},
		{"跨 shell 内层是普通命令", `pwsh -c "Get-Date"`, ""},
		{"wsl 内层是普通命令", `wsl ls -la /mnt/c`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Evaluate(ShellPOSIX, tt.command, nil)
			want := DispositionAllow
			if tt.wantRuleID != "" {
				want = DispositionBlock
			}
			if got.Disposition != want || got.RuleID != tt.wantRuleID {
				t.Fatalf("Evaluate(%q) = %+v, want %v/%s", tt.command, got, want, tt.wantRuleID)
			}
		})
	}
}

// TestEvaluate_CustomRulesApplyInsideNested:自定义规则与默认规则一样
// 在嵌套执行的内层生效——结构化字段与正则都对内层命令匹配,正则的
// 匹配对象是内层命令文本;适用 shell 集合的过滤在内层同样成立
// (POSIX 专用规则不适用于 PowerShell 内层)。
func TestEvaluate_CustomRulesApplyInsideNested(t *testing.T) {
	rules := &RuleSet{CustomRules: []CustomRule{{
		ID:          "NO-REBOOT",
		Shells:      []ShellType{ShellPOSIX},
		Commands:    []string{"reboot"},
		Disposition: DispositionBlock,
		Explanation: "reboots are scheduled by the owner",
		Alternative: "ask the owner to schedule a reboot window",
	}, {
		ID:          "ASK-BACKUP-DB",
		Regex:       `backup-db\s+--full`,
		Disposition: DispositionConfirmRequired,
		Explanation: "full backups replace the current archive",
		Alternative: "run an incremental backup instead",
	}, {
		ID:          "NO-RESTART-COMPUTER",
		Shells:      []ShellType{ShellPowerShell},
		Commands:    []string{"Restart-Computer"},
		Args:        []string{"-Force"},
		Disposition: DispositionBlock,
		Explanation: "reboots are scheduled by the owner",
		Alternative: "ask the owner to schedule a reboot window",
	}}}
	tests := []struct {
		name       string
		shell      ShellType
		command    string
		want       Disposition
		wantRuleID string
	}{
		{"结构化字段在内层命中", ShellPOSIX, `bash -c "reboot"`, DispositionBlock, "NO-REBOOT"},
		{"正则对内层命令原文命中", ShellPOSIX, `bash -c "backup-db --full"`, DispositionConfirmRequired, "ASK-BACKUP-DB"},
		{"两层嵌套的内层命中", ShellPOSIX, `bash -c "bash -c 'reboot'"`, DispositionBlock, "NO-REBOOT"},
		{"wsl 内层命中", ShellWindowsCmd, `cmd /c wsl reboot`, DispositionBlock, "NO-REBOOT"},
		{"PowerShell 专用规则在内层命中", ShellPowerShell, `powershell -Command "Restart-Computer -Force"`, DispositionBlock, "NO-RESTART-COMPUTER"},
		{"跨 shell 内层命中", ShellPOSIX, `pwsh -c "Restart-Computer -Force"`, DispositionBlock, "NO-RESTART-COMPUTER"},
		{"POSIX 专用规则不适用于 PowerShell 内层", ShellPOSIX, `pwsh -c "reboot"`, DispositionAllow, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Evaluate(tt.shell, tt.command, rules)
			if got.Disposition != tt.want || got.RuleID != tt.wantRuleID {
				t.Fatalf("Evaluate(%s, %q) = %+v, want %v/%s", tt.shell, tt.command, got, tt.want, tt.wantRuleID)
			}
		})
	}
}

// TestEvaluate_NestedFlagOnlyInLeadingOptionRun:「后跟内层命令」的旗标
// 只在参数开头的选项序列里找:第一个非选项参数之后出现的 -c 只是传给
// 脚本的位置参数(如 bash script.sh -c x、powershell -File 工具.ps1),
// 不当作嵌套执行的旗标,避免误拦;选项序列中段的旗标照常识别。
func TestEvaluate_NestedFlagOnlyInLeadingOptionRun(t *testing.T) {
	tests := []struct {
		name       string
		shell      ShellType
		command    string
		wantRuleID string
	}{
		{"脚本参数里的 -c 不是旗标", ShellPOSIX, `bash script.sh -c rm -rf /`, ""},
		{"PowerShell 脚本文件的参数不误拦", ShellPowerShell, `powershell -File tool.ps1 -c rm -rf /`, ""},
		{"cmd 后面普通参数里的 /c 不算", ShellWindowsCmd, `cmd /e:off tool.bat /c`, ""},
		{"选项序列中段的旗标照常识别", ShellPOSIX, `bash -l -c "mkfs.ext4 /dev/sdb"`, "POSIX-FORMAT"},
		{"PowerShell 选项后接 Command 照常识别", ShellPowerShell, `pwsh -NoProfile -Command "Format-Volume -DriveLetter D"`, "WIN-FORMAT"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Evaluate(tt.shell, tt.command, nil)
			want := DispositionAllow
			if tt.wantRuleID != "" {
				want = DispositionBlock
			}
			if got.Disposition != want || got.RuleID != tt.wantRuleID {
				t.Fatalf("Evaluate(%s, %q) = %+v, want %v/%s", tt.shell, tt.command, got, want, tt.wantRuleID)
			}
		})
	}
}

// TestEvaluate_NestedInnerHitsConfirmRules:嵌套执行的内层按完整生效
// 规则集判定——需确认默认规则(ticket #11)在内层同样命中,写死的
// 内层文本递归判定时给出需确认档与内层规则的编号;组合命令里拦截与
// 需确认并存时整体取更严重的一档(拦截)。
func TestEvaluate_NestedInnerHitsConfirmRules(t *testing.T) {
	tests := []struct {
		name       string
		command    string
		want       Disposition
		wantRuleID string
	}{
		{"bash -c 清空防火墙规则需确认", `bash -c "iptables -F"`, DispositionConfirmRequired, "POSIX-LOCKOUT"},
		{"sh -c 关防火墙需确认", `sh -c 'ufw disable'`, DispositionConfirmRequired, "POSIX-LOCKOUT"},
		{"su -c 停 sshd 需确认", `su -c "systemctl stop sshd"`, DispositionConfirmRequired, "POSIX-LOCKOUT"},
		{"两层嵌套的内层需确认", `bash -c "bash -c 'nft flush ruleset'"`, DispositionConfirmRequired, "POSIX-LOCKOUT"},
		{"单引号内层写死、删除目标在内层拼出来", `sh -c 'rm -rf $DIR/'`, DispositionConfirmRequired, "POSIX-DELETE-TARGET-UNKNOWN"},
		{"bash -c 整卷销毁需确认", `bash -c "zpool destroy tank"`, DispositionConfirmRequired, "POSIX-VOLUME-DESTROY"},
		{"docker prune 带 -a 内层需确认", `bash -c "docker system prune -a"`, DispositionConfirmRequired, "POSIX-VOLUME-DESTROY"},
		{"docker prune 不带 -a 内层放行", `bash -c "docker system prune"`, DispositionAllow, ""},
		{"内层需确认与外层拦截并存,拦截更严重", `bash -c "iptables -F"; mkfs.ext4 /dev/sdb`, DispositionBlock, "POSIX-FORMAT"},
		{"外层需确认与内层拦截并存,拦截更严重", `ufw disable && bash -c "mkfs.ext4 /dev/sdb"`, DispositionBlock, "POSIX-FORMAT"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Evaluate(ShellPOSIX, tt.command, nil)
			if got.Disposition != tt.want || got.RuleID != tt.wantRuleID {
				t.Fatalf("Evaluate(%q) = %+v, want %v/%s", tt.command, got, tt.want, tt.wantRuleID)
			}
			if tt.want == DispositionConfirmRequired && (got.Explanation == "" || got.Alternative == "") {
				t.Fatalf("Evaluate(%q) 需确认必须带风险说明与替代做法, got %+v", tt.command, got)
			}
		})
	}
}

// TestEvaluate_NestedCustomRuleTightensInnerConfirm:自定义规则在内层
// 命中且处置更严重时,与默认需确认的合流同样在内层成立——自定义拦截
// 把内层默认的需确认(POSIX-VOLUME-DESTROY)收紧成拦截,规则编号取
// 自定义规则的。
func TestEvaluate_NestedCustomRuleTightensInnerConfirm(t *testing.T) {
	rules := &RuleSet{CustomRules: []CustomRule{{
		ID:          "NO-DOCKER-PRUNE",
		Commands:    []string{"docker"},
		Args:        []string{"prune"},
		Disposition: DispositionBlock,
		Explanation: "prune decisions belong to the owner",
		Alternative: "ask the owner which images to remove",
	}}}
	got := Evaluate(ShellPOSIX, `bash -c "docker system prune -a"`, rules)
	if got.Disposition != DispositionBlock || got.RuleID != "NO-DOCKER-PRUNE" {
		t.Fatalf(`Evaluate(bash -c "docker system prune -a", 自定义拦截) = %+v, want 拦截/NO-DOCKER-PRUNE`, got)
	}
}

// TestEvaluate_NestedSUCommand:su -c 的取值同样交给目标用户的 shell
// 执行,与 bash -c 同形识别;su 只是切换用户,不接 -c 时不是嵌套执行。
func TestEvaluate_NestedSUCommand(t *testing.T) {
	if got := Evaluate(ShellPOSIX, `su -c "mkfs.ext4 /dev/sdb"`, nil); got.Disposition != DispositionBlock || got.RuleID != "POSIX-FORMAT" {
		t.Fatalf("su -c mkfs = %+v, want 拦截/POSIX-FORMAT", got)
	}
	if got := Evaluate(ShellPOSIX, `sudo su -c 'rm -rf /etc'`, nil); got.Disposition != DispositionBlock || got.RuleID != "POSIX-CRITICAL-DELETE" {
		t.Fatalf("sudo su -c = %+v, want 拦截/POSIX-CRITICAL-DELETE", got)
	}
	if got := Evaluate(ShellPOSIX, `su deployer`, nil); got.Disposition != DispositionAllow {
		t.Fatalf("su deployer = %+v, want 放行", got)
	}
	if got := Evaluate(ShellPOSIX, `su -c "systemctl restart nginx"`, nil); got.Disposition != DispositionAllow {
		t.Fatalf("su -c 普通命令 = %+v, want 放行", got)
	}
}
