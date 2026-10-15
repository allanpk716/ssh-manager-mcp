package policy

import "testing"

// evalPOSIX 以 POSIX shell 判定一条命令,返回处置与命中规则编号,
// 是本包对外行为断言的主要入口(试算命令与执行工具都只调它)。
func evalPOSIX(t *testing.T, command string) Decision {
	t.Helper()
	return Evaluate(ShellPOSIX, command, nil)
}

// TestEvaluate_BlocksFormatCommands:格式化类命令(格式化、抹签名、
// macOS diskutil 抹卷)一律拦截,命中 POSIX-FORMAT。
func TestEvaluate_BlocksFormatCommands(t *testing.T) {
	tests := []struct {
		name    string
		command string
	}{
		{"mkfs 带 fs 类型后缀", "mkfs.ext4 /dev/sdb"},
		{"mkfs 裸名 + -t", "mkfs -t ext4 /dev/sdb"},
		{"mkfs.vfat", "mkfs.vfat /dev/sdc1"},
		{"mkfs.xfs", "mkfs.xfs -f /dev/nvme0n1"},
		{"wipefs 抹签名", "wipefs -a /dev/sda"},
		{"diskutil eraseDisk", "diskutil eraseDisk apfs MyDisk disk2"},
		{"diskutil eraseVolume", "diskutil eraseVolume jhfs+ Data /dev/disk2s1"},
		{"带全路径的命令名", "/usr/sbin/mkfs.ext4 /dev/sdb"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evalPOSIX(t, tt.command)
			if got.Disposition != DispositionBlock {
				t.Fatalf("Evaluate(%q) disposition = %v, want 拦截", tt.command, got.Disposition)
			}
			if got.RuleID != "POSIX-FORMAT" {
				t.Fatalf("Evaluate(%q) rule id = %q, want POSIX-FORMAT", tt.command, got.RuleID)
			}
			if got.Explanation == "" || got.Alternative == "" {
				t.Fatalf("Evaluate(%q) must carry explanation and alternative, got %+v", tt.command, got)
			}
		})
	}
}

// TestEvaluate_AllowsNonFormatLookalikes:只是名字或文字里出现格式化命令、
// 并不会真的执行它的命令必须放行。
func TestEvaluate_AllowsNonFormatLookalikes(t *testing.T) {
	tests := []struct {
		name    string
		command string
	}{
		{"打印文字里的 mkfs", `echo "mkfs.ext4 /dev/sdb"`},
		{"grep 格式化命令名", "grep mkfs /var/log/install.log"},
		{"列设备信息", "lsblk /dev/sdb"},
		{"查看文件系统类型", "fsck -N /dev/sdb1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evalPOSIX(t, tt.command)
			if got.Disposition != DispositionAllow {
				t.Fatalf("Evaluate(%q) disposition = %v, want 放行", tt.command, got.Disposition)
			}
			if got.RuleID != "" {
				t.Fatalf("Evaluate(%q) rule id = %q, want empty on 放行", tt.command, got.RuleID)
			}
		})
	}
}

// TestEvaluate_BlocksRawDiskDeviceWrites:绕过文件系统直接写 /dev/ 下
// 磁盘设备的命令(dd 的 of=、输出重定向、shred)一律拦截。
func TestEvaluate_BlocksRawDiskDeviceWrites(t *testing.T) {
	tests := []struct {
		name    string
		command string
	}{
		{"dd 写盘", "dd if=/dev/zero of=/dev/sda bs=1M count=10"},
		{"dd 写 nvme", "dd if=backup.img of=/dev/nvme0n1"},
		{"重定向写盘", "cat backup.img > /dev/sda"},
		{"追加重定向写盘", "cat part2.img >> /dev/sdb"},
		{"纯重定向", "> /dev/sda"},
		{"echo 重定向写盘", "echo x > /dev/sdc"},
		{"shred 盘", "shred -n 1 /dev/sdc"},
		{"sudo 前缀 dd", "sudo dd if=/dev/zero of=/dev/sda"},
		{"nohup 前缀重定向", "nohup cat a > /dev/sda"},
		{"子 shell 里重定向", "(cat a) > /dev/sda"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evalPOSIX(t, tt.command)
			if got.Disposition != DispositionBlock || got.RuleID != "POSIX-RAW-DISK-WRITE" {
				t.Fatalf("Evaluate(%q) = %+v, want 拦截/POSIX-RAW-DISK-WRITE", tt.command, got)
			}
		})
	}
}

// TestEvaluate_AllowsSafeDeviceWrites:/dev/null、/dev/stderr 这类
// 非磁盘设备上的写,以及写普通文件的 dd,必须照常放行。
func TestEvaluate_AllowsSafeDeviceWrites(t *testing.T) {
	tests := []struct {
		name    string
		command string
	}{
		{"重定向到 /dev/null", "echo hi > /dev/null"},
		{"dd 写 /dev/null", "dd if=/dev/zero of=/dev/null bs=1M"},
		{"stderr 到 /dev/null", "make 2> /dev/null"},
		{"写 /dev/stderr", "echo warn > /dev/stderr"},
		{"写 /dev/tty", "echo hi > /dev/tty"},
		{"dd 写普通文件", "dd if=/dev/zero of=/tmp/img.raw bs=1M count=100"},
		{"shred 普通文件", "shred /tmp/secret.txt"},
		{"读盘", "dd if=/dev/sda of=/tmp/disk.img bs=512 count=1"},
		{"动态目标", "dd if=a of=$TARGET"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evalPOSIX(t, tt.command)
			if got.Disposition != DispositionAllow {
				t.Fatalf("Evaluate(%q) = %+v, want 放行", tt.command, got)
			}
		})
	}
}

// TestEvaluate_BlocksPartitionTableWrites:改写分区表的命令(fdisk 家族
// 写入、parted 破坏性子命令、sgdisk 非只读选项、diskutil partitionDisk)
// 一律拦截;只读的列表/打印形态放行。
func TestEvaluate_BlocksPartitionTableWrites(t *testing.T) {
	tests := []struct {
		name    string
		command string
	}{
		{"fdisk 打开设备", "fdisk /dev/sda"},
		{"sfdisk 打开设备", "sfdisk /dev/sda"},
		{"cfdisk 打开设备", "cfdisk /dev/sda"},
		{"gdisk 交互编辑", "gdisk /dev/sda"},
		{"parted mklabel", "parted /dev/sda mklabel gpt"},
		{"parted mkpart", "parted /dev/sda mkpart primary ext4 1MiB 100MiB"},
		{"parted rm", "parted /dev/sda rm 3"},
		{"parted set", "parted /dev/sda set 1 boot on"},
		{"sgdisk zap", "sgdisk --zap-all /dev/sda"},
		{"sgdisk -o 清空", "sgdisk -o /dev/sda"},
		{"diskutil partitionDisk", "diskutil partitionDisk 2 gpt APFS MacHD disk2"},
		{"sudo fdisk", "sudo fdisk /dev/sda"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evalPOSIX(t, tt.command)
			if got.Disposition != DispositionBlock || got.RuleID != "POSIX-PARTITION" {
				t.Fatalf("Evaluate(%q) = %+v, want 拦截/POSIX-PARTITION", tt.command, got)
			}
		})
	}
}

// TestEvaluate_AllowsPartitionListing:分区工具的只读形态(-l、print、
// -p、--info)照常放行。
func TestEvaluate_AllowsPartitionListing(t *testing.T) {
	tests := []struct {
		name    string
		command string
	}{
		{"fdisk -l", "fdisk -l /dev/sda"},
		{"fdisk --list", "fdisk --list"},
		{"sfdisk -l", "sfdisk -l"},
		{"parted -l", "parted -l"},
		{"parted print", "parted /dev/sda print"},
		{"sgdisk -p", "sgdisk -p /dev/sda"},
		{"sgdisk --info", "sgdisk --info=1 /dev/sda"},
		{"sgdisk 无操作参数", "sgdisk /dev/sda"},
		{"lsblk 查看分区", "lsblk -f /dev/sda"},
		{"blkid", "blkid /dev/sda1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evalPOSIX(t, tt.command)
			if got.Disposition != DispositionAllow {
				t.Fatalf("Evaluate(%q) = %+v, want 放行", tt.command, got)
			}
		})
	}
}

// TestEvaluate_BlocksCriticalPathDeletion:递归删除根目录或系统关键路径
// (Linux 与 macOS 两套)一律拦截;路径的等价变体(多余斜杠、/./、结尾
// 斜杠、结尾通配)判定必须一致。
func TestEvaluate_BlocksCriticalPathDeletion(t *testing.T) {
	tests := []struct {
		name    string
		command string
	}{
		{"删根目录", "rm -rf /"},
		{"删 /etc", "rm -rf /etc"},
		{"结尾斜杠", "rm -rf /etc/"},
		{"双斜杠", "rm -rf //etc"},
		{"点段", "rm -rf /./etc"},
		{"根下通配", "rm -rf /*"},
		{"关键路径下通配", "rm -rf /etc/*"},
		{"点段升层", "rm -rf /usr/share/../../etc"},
		{"仅 -r", "rm -r /etc"},
		{"flag 顺序颠倒", "rm -fr /etc"},
		{"长选项递归", "rm --recursive /etc"},
		{"sudo 前缀", "sudo rm -rf /etc"},
		{"引号路径", `rm -rf "/etc"`},
		{"多个目标", "rm -rf /etc /var/lib"},
		{"-- 分隔符", "rm -rf -- /boot"},
		{"Linux 关键路径", "rm -rf /var/lib"},
		{"macOS /System", "rm -rf /System"},
		{"macOS /Library", "rm -rf /Library"},
		{"macOS /Applications", "rm -rf /Applications"},
		{"macOS /Users", "rm -rf /Users"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evalPOSIX(t, tt.command)
			if got.Disposition != DispositionBlock || got.RuleID != "POSIX-CRITICAL-DELETE" {
				t.Fatalf("Evaluate(%q) = %+v, want 拦截/POSIX-CRITICAL-DELETE", tt.command, got)
			}
		})
	}
}

// TestEvaluate_AllowsOrdinaryDeletion:目标路径写死且不在系统关键路径
// 本身上的普通删除照常放行,日常工作不被打断。
func TestEvaluate_AllowsOrdinaryDeletion(t *testing.T) {
	tests := []struct {
		name    string
		command string
	}{
		{"删关键路径里面的子目录", "rm -rf /etc/nginx"},
		{"删构建产物", "rm -r ./build"},
		{"删临时目录", "rm -rf /tmp/x"},
		{"删单个文件", "rm -f /etc/hosts"},
		{"删相对路径", "rm -rf build"},
		{"删家目录内容", "rm -rf ~/cache"},
		{"动态目标", "rm -rf $DIR/"},
		{"循环删除日志", `for f in *.log; do rm "$f"; done`},
		{"rmdir", "rmdir /tmp/empty"},
		{"删 home 下的用户目录", "rm -rf /home/deployer/build"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evalPOSIX(t, tt.command)
			if got.Disposition != DispositionAllow {
				t.Fatalf("Evaluate(%q) = %+v, want 放行", tt.command, got)
			}
		})
	}
}

// TestEvaluate_BlocksRecursiveRootPermissionChanges:对根目录递归改
// 权限或属主的命令一律拦截。
func TestEvaluate_BlocksRecursiveRootPermissionChanges(t *testing.T) {
	tests := []struct {
		name    string
		command string
	}{
		{"chmod -R 根目录", "chmod -R 777 /"},
		{"chown -R 根目录", "chown -R root:root /"},
		{"根下通配", "chmod -R 755 /*"},
		{"长选项递归", "chmod --recursive 000 /"},
		{"sudo 前缀", "sudo chown -R user /"},
		{"结尾斜杠变体", "chmod -R 777 //"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evalPOSIX(t, tt.command)
			if got.Disposition != DispositionBlock || got.RuleID != "POSIX-RECURSIVE-ROOT-PERM" {
				t.Fatalf("Evaluate(%q) = %+v, want 拦截/POSIX-RECURSIVE-ROOT-PERM", tt.command, got)
			}
		})
	}
}

// TestEvaluate_AllowsOrdinaryPermissionChanges:不递归、或递归但目标
// 不是根目录的权限/属主修改照常放行。
func TestEvaluate_AllowsOrdinaryPermissionChanges(t *testing.T) {
	tests := []struct {
		name    string
		command string
	}{
		{"不递归", "chmod 755 /etc"},
		{"递归子目录", "chmod -R 755 /var/www"},
		{"chown 单文件", "chown root /etc/hosts"},
		{"递归家目录子树", "chmod -R u+w /home/user/docs"},
		{"chmod 小写 r 是去读位", "chmod -r file.txt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evalPOSIX(t, tt.command)
			if got.Disposition != DispositionAllow {
				t.Fatalf("Evaluate(%q) = %+v, want 放行", tt.command, got)
			}
		})
	}
}

// TestEvaluate_BlocksForkBombs:不断把自身接进管道自我复制的函数定义
// (fork 炸弹)一律拦截,改名变体同样命中。
func TestEvaluate_BlocksForkBombs(t *testing.T) {
	tests := []struct {
		name    string
		command string
	}{
		{"经典形态", ":(){ :|:& };:"},
		{"改名变体", "bomb(){ bomb|bomb&};bomb"},
		{"无后台符", "f(){ f|f; }; f"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evalPOSIX(t, tt.command)
			if got.Disposition != DispositionBlock || got.RuleID != "POSIX-FORK-BOMB" {
				t.Fatalf("Evaluate(%q) = %+v, want 拦截/POSIX-FORK-BOMB", tt.command, got)
			}
		})
	}
}

// TestEvaluate_AllowsBoundedPipelines:普通管道与有界的递归函数不是
// fork 炸弹,照常放行。
func TestEvaluate_AllowsBoundedPipelines(t *testing.T) {
	tests := []struct {
		name    string
		command string
	}{
		{"普通管道", "grep foo access.log | wc -l"},
		{"有界递归函数", "walk(){ ls \"$1\" | head -1; }; walk ."},
		{"自身在消费侧", "f(){ cat | f; }; f"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evalPOSIX(t, tt.command)
			if got.Disposition != DispositionAllow {
				t.Fatalf("Evaluate(%q) = %+v, want 放行", tt.command, got)
			}
		})
	}
}

// TestEvaluate_CombinedCommandsTakeMostSevere:组合命令拆出的每条简单
// 命令分别判定,整体取最严重的处置,规则编号对应最严重的那条;
// sudo 等前缀不影响判断。
func TestEvaluate_CombinedCommandsTakeMostSevere(t *testing.T) {
	tests := []struct {
		name       string
		command    string
		wantRuleID string
	}{
		{"验收组合用例", "cd /x && sudo rm -rf /etc; ls", "POSIX-CRITICAL-DELETE"},
		{"管道前段危险", "wipefs -a /dev/sda | cat", "POSIX-FORMAT"},
		{"或链后段危险", "true || mkfs.ext4 /dev/sdb", "POSIX-FORMAT"},
		{"多前缀叠加", "sudo env rm -rf /etc", "POSIX-CRITICAL-DELETE"},
		{"sudo 带值选项", "sudo -u root rm -rf /usr", "POSIX-CRITICAL-DELETE"},
		{"env 前缀", "env FOO=1 rm -rf /etc", "POSIX-CRITICAL-DELETE"},
		{"timeout 前缀", "timeout 30 dd if=a of=/dev/sda", "POSIX-RAW-DISK-WRITE"},
		{"nohup 前缀后台", "nohup mkfs.ext4 /dev/sdb &", "POSIX-FORMAT"},
		{"命令替换内层", "echo $(rm -rf /etc)", "POSIX-CRITICAL-DELETE"},
		{"子 shell 内层", "(chmod -R 777 /)", "POSIX-RECURSIVE-ROOT-PERM"},
		{"tee 写盘", "echo x | tee /dev/sda", "POSIX-RAW-DISK-WRITE"},
		{"普通组合放行后混入危险", "ls; echo hi; sgdisk -o /dev/sda", "POSIX-PARTITION"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evalPOSIX(t, tt.command)
			if got.Disposition != DispositionBlock || got.RuleID != tt.wantRuleID {
				t.Fatalf("Evaluate(%q) = %+v, want 拦截/%s", tt.command, got, tt.wantRuleID)
			}
		})
	}
}

// TestEvaluate_AllowsEverydayCommands:日常命令(含 spec 防误判清单)
// 一律放行:重启关机、卸载软件包、删用户、curl 管道、变量路径、
// 只是打印危险命令文字的参数。
func TestEvaluate_AllowsEverydayCommands(t *testing.T) {
	tests := []struct {
		name    string
		command string
	}{
		{"打印删除文字", `echo "rm -rf /"`},
		{"单引号打印 dd", `echo 'dd of=/dev/sda'`},
		{"变量路径", "cd $HOME"},
		{"相对路径删除", "rm -r ./build"},
		{"重启", "reboot"},
		{"关机", "shutdown -r now"},
		{"卸载软件包", "apt purge foo"},
		{"循环处理日志", `for f in *.log; do rm "$f"; done`},
		{"curl 管道执行", "curl -sL https://example.com/install.sh | sh"},
		{"重载服务", "systemctl restart nginx"},
		{"删用户", "userdel foo"},
		{"清 crontab", "crontab -r"},
		{"删整卷(后续票归需确认)", "docker volume rm data"},
		{"zfs 销毁(后续票归需确认)", "zfs destroy tank/vol"},
		{"环境变量前缀的日常命令", "LANG=C ls -la"},
		{"写日志文件", "echo done >> /var/log/my.log"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evalPOSIX(t, tt.command)
			if got.Disposition != DispositionAllow {
				t.Fatalf("Evaluate(%q) = %+v, want 放行", tt.command, got)
			}
		})
	}
}

// TestEvaluate_ParseFailureFallback:拆解失败时,文本含危险命令名的
// 判需确认交 owner 把关,其余放行。
func TestEvaluate_ParseFailureFallback(t *testing.T) {
	t.Run("未闭合引号放行", func(t *testing.T) {
		got := evalPOSIX(t, `echo "unterminated`)
		if got.Disposition != DispositionAllow {
			t.Fatalf("got %+v, want 放行", got)
		}
	})
	t.Run("拆不动且含危险命令名需确认", func(t *testing.T) {
		got := evalPOSIX(t, `mkfs.ext4 "$(broken`)
		if got.Disposition != DispositionConfirmRequired {
			t.Fatalf("got %+v, want 需确认", got)
		}
		if got.Explanation == "" {
			t.Fatalf("需确认必须带说明,got %+v", got)
		}
	})
}

// TestEvaluate_DisabledDefaultRuleByID:按编号停用默认规则后,该规则
// 不再参与判定,其余规则照常。
func TestEvaluate_DisabledDefaultRuleByID(t *testing.T) {
	rules := &RuleSet{DisabledDefaultRuleIDs: []string{"POSIX-FORMAT"}}
	if got := Evaluate(ShellPOSIX, "mkfs.ext4 /dev/sdb", rules); got.Disposition != DispositionAllow {
		t.Fatalf("disabled POSIX-FORMAT still fired: %+v", got)
	}
	if got := Evaluate(ShellPOSIX, "rm -rf /etc", rules); got.Disposition != DispositionBlock {
		t.Fatalf("other rules must stay active: %+v", got)
	}
}

// TestEvaluate_UnimplementedShellsNeedConfirmation:尚未实现判定的
// shell 类型(PowerShell、cmd)保守返回需确认,不悄悄放行。
func TestEvaluate_UnimplementedShellsNeedConfirmation(t *testing.T) {
	for _, shell := range []ShellType{ShellPowerShell, ShellWindowsCmd} {
		got := Evaluate(shell, "Get-ChildItem", nil)
		if got.Disposition != DispositionConfirmRequired {
			t.Fatalf("Evaluate(%s) = %+v, want 需确认", shell, got)
		}
	}
}
