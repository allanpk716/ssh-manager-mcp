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
		{"根下通配带结尾斜杠", "rm -rf /*/"},
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
		{"WSL 挂载的盘根本身", "rm -rf /mnt/c"},
		{"WSL 挂载的盘根结尾斜杠", "rm -rf /mnt/d/"},
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
		{"删当前目录通配", "rm -rf *"},
		{"删当前目录通配带点斜杠", "rm -rf ./*"},
		{"删家目录内容", "rm -rf ~/cache"},
		{"循环删除日志", `for f in *.log; do rm "$f"; done`},
		{"rmdir", "rmdir /tmp/empty"},
		{"删 home 下的用户目录", "rm -rf /home/deployer/build"},
		{"删 WSL 挂载盘里的子目录", "rm -rf /mnt/c/Users/me/tmp"},
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

// TestEvaluate_RequiresConfirmationForLockoutOps:会把自己锁在门外的
// 操作(清空防火墙规则、停止 sshd、修改 sshd_config、关闭网卡、macOS
// 关闭远程登录)一律需确认,命中 POSIX-LOCKOUT。
func TestEvaluate_RequiresConfirmationForLockoutOps(t *testing.T) {
	tests := []struct {
		name    string
		command string
	}{
		{"清空 iptables 规则", "iptables -F"},
		{"清空指定链", "iptables -t nat -F PREROUTING"},
		{"iptables --flush", "iptables --flush"},
		{"sudo 清空", "sudo iptables -F"},
		{"nft 冲掉整个规则集", "nft flush ruleset"},
		{"ufw 关停", "ufw disable"},
		{"ufw 重置", "ufw reset"},
		{"systemctl 停 sshd", "systemctl stop sshd"},
		{"systemctl 停 ssh(service 名)", "systemctl stop ssh.service"},
		{"service 停 sshd", "service sshd stop"},
		{"编辑器开 sshd_config", "vim /etc/ssh/sshd_config"},
		{"nano 开 sshd_config", "sudo nano /etc/ssh/sshd_config"},
		{"追加写 sshd_config", "echo 'PermitRootLogin no' >> /etc/ssh/sshd_config"},
		{"覆盖写 sshd_config", "cat new.conf > /etc/ssh/sshd_config"},
		{"sed 就地改 sshd_config", "sed -i s/Port 22/Port 2222/ /etc/ssh/sshd_config"},
		{"tee 写 sshd_config", "tee /etc/ssh/sshd_config"},
		{"拷贝覆盖 sshd_config", "cp new.conf /etc/ssh/sshd_config"},
		{"写增量配置目录里的文件", "echo 'Port 2222' > /etc/ssh/sshd_config.d/99-agent.conf"},
		{"路径变体双斜杠", "sed -i s/a/b/ //etc/ssh//sshd_config"},
		{"ip 关网卡", "ip link set eth0 down"},
		{"ifconfig 关网卡", "ifconfig eth0 down"},
		{"ifdown 关网卡", "ifdown eth0"},
		{"macOS 关远程登录", "systemsetup -setremotelogin off"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evalPOSIX(t, tt.command)
			if got.Disposition != DispositionConfirmRequired || got.RuleID != "POSIX-LOCKOUT" {
				t.Fatalf("Evaluate(%q) = %+v, want 需确认/POSIX-LOCKOUT", tt.command, got)
			}
			if got.Explanation == "" || got.Alternative == "" {
				t.Fatalf("Evaluate(%q) 必须带风险说明与替代做法, got %+v", tt.command, got)
			}
		})
	}
}

// TestEvaluate_AllowsLockoutLookalikes:只是查看防火墙/服务状态、读
// sshd_config、停别的服务、开网卡的命令必须照常放行,不打扰日常运维。
func TestEvaluate_AllowsLockoutLookalikes(t *testing.T) {
	tests := []struct {
		name    string
		command string
	}{
		{"iptables 列规则", "iptables -L -n"},
		{"nft 列规则集", "nft list ruleset"},
		{"ufw 看状态", "ufw status"},
		{"systemctl 看 sshd 状态", "systemctl status sshd"},
		{"systemctl 重载 sshd", "systemctl reload sshd"},
		{"systemctl 重启 nginx", "systemctl restart nginx"},
		{"service 状态", "service ssh status"},
		{"停别的服务", "systemctl stop nginx"},
		{"读 sshd_config", "grep -i port /etc/ssh/sshd_config"},
		{"cat sshd_config", "cat /etc/ssh/sshd_config"},
		{"sed 只打印不改", "sed -n p /etc/ssh/sshd_config"},
		{"备份 sshd_config", "cp /etc/ssh/sshd_config /tmp/sshd_config.bak"},
		{"编辑别的文件", "vim /etc/hosts"},
		{"ip 开网卡", "ip link set eth0 up"},
		{"ifconfig 看网卡", "ifconfig eth0"},
		{"macOS 开远程登录", "systemsetup -setremotelogin on"},
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

// TestEvaluate_RequiresConfirmationForVolumeDestruction:整卷或整池销毁
// (zfs/zpool destroy、lvremove、docker volume rm、docker system prune -a)
// 一律需确认,命中 POSIX-VOLUME-DESTROY——销毁的是整块存储,错了没有
// 回头路。
func TestEvaluate_RequiresConfirmationForVolumeDestruction(t *testing.T) {
	tests := []struct {
		name    string
		command string
	}{
		{"zfs destroy", "zfs destroy tank/vol"},
		{"zfs destroy -r", "zfs destroy -r tank/vol@snap"},
		{"sudo zfs destroy", "sudo zfs destroy tank/data"},
		{"zpool destroy 整池", "zpool destroy tank"},
		{"lvremove 删逻辑卷", "lvremove /dev/vg0/data"},
		{"lvremove -y", "lvremove -y /dev/vg0/data"},
		{"docker volume rm", "docker volume rm data"},
		{"docker volume rm 多个", "docker volume rm a b c"},
		{"docker system prune -a", "docker system prune -a"},
		{"docker system prune --all", "docker system prune --all"},
		{"组合里的销毁", "echo cleaning && docker volume rm data"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evalPOSIX(t, tt.command)
			if got.Disposition != DispositionConfirmRequired || got.RuleID != "POSIX-VOLUME-DESTROY" {
				t.Fatalf("Evaluate(%q) = %+v, want 需确认/POSIX-VOLUME-DESTROY", tt.command, got)
			}
		})
	}
}

// TestEvaluate_AllowsVolumeLookalikes:只是查看或快照存储、prune 不带
// -a 的日常清理,照常放行。
func TestEvaluate_AllowsVolumeLookalikes(t *testing.T) {
	tests := []struct {
		name    string
		command string
	}{
		{"zfs list", "zfs list -t all"},
		{"zfs snapshot", "zfs snapshot tank/vol@today"},
		{"zpool status", "zpool status"},
		{"lvdisplay 查看", "lvdisplay /dev/vg0/data"},
		{"docker ps", "docker ps -a"},
		{"docker volume ls", "docker volume ls"},
		{"docker system prune 不带 -a", "docker system prune"},
		{"docker rm 容器", "docker rm web"},
		{"docker rmi 镜像", "docker rmi nginx:latest"},
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

// TestEvaluate_RequiresConfirmationForDynamicRecursiveDelete:删除目标在
// 执行前无法确定的递归删除(目标含变量或命令替换)一律需确认,命中
// POSIX-DELETE-TARGET-UNKNOWN——变量为空时它就是删错位置的那一下。
func TestEvaluate_RequiresConfirmationForDynamicRecursiveDelete(t *testing.T) {
	tests := []struct {
		name    string
		command string
	}{
		{"变量目标带尾斜杠", "rm -rf $DIR/"},
		{"引号变量目标", `rm -rf "$DIR"/`},
		{"命令替换目标", `rm -rf "$(pwd)"/*`},
		{"反引号命令替换", "rm -rf `cat /tmp/path`"},
		{"仅 -r 变量目标", "rm -r $TARGET"},
		{"sudo 变量目标", "sudo rm -rf $BUILD_DIR"},
		{"静态前缀加变量", "rm -rf /data/$REL"},
		{"变量作多个目标之一", "rm -rf /tmp/cache $DIR"},
		{"组合里的动态删除", "cd /opt && rm -rf $APP_DIR"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evalPOSIX(t, tt.command)
			if got.Disposition != DispositionConfirmRequired || got.RuleID != "POSIX-DELETE-TARGET-UNKNOWN" {
				t.Fatalf("Evaluate(%q) = %+v, want 需确认/POSIX-DELETE-TARGET-UNKNOWN", tt.command, got)
			}
		})
	}
}

// TestEvaluate_AllowsDeterminableDeletes:目标写死或不含动态成分的删除
// 照常放行——只有「危险删除命令 + 动态目标」才触发,变量出现在别的
// 命令里一律不影响处置。
func TestEvaluate_AllowsDeterminableDeletes(t *testing.T) {
	tests := []struct {
		name    string
		command string
	}{
		{"cd 变量路径", "cd $HOME"},
		{"相对路径递归删", "rm -r ./build"},
		{"写死路径递归删", "rm -rf /tmp/build"},
		{"写死通配删", "rm -rf /var/log/app/*.log"},
		{"非递归变量删除", `for f in *.log; do rm "$f"; done`},
		{"非递归单变量", `rm "$file"`},
		{"ls 变量参数", "ls $DIR"},
		{"mkdir 变量路径", "mkdir -p $ROOT/sub"},
		{"cat 变量文件", "cat $CONF"},
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

// TestEvaluate_WrapperPrefixesStrippedBeforeJudging:command、exec、
// doas、busybox 这类前缀包装命令与 sudo/env/nohup 同等待遇——自身的
// 选项与取值不属于真正的命令,剥掉后按内层命令判定;command 的
// -v / -V 是按名查找(打印路径),不执行任何命令,按放行对待。
func TestEvaluate_WrapperPrefixesStrippedBeforeJudging(t *testing.T) {
	blocks := []struct{ name, command string }{
		{"command 前缀", "command rm -rf /"},
		{"command 带默认路径选项", "command -p rm -rf /etc"},
		{"exec 前缀", "exec rm -rf /"},
		{"exec -a 带名字", "exec -a cleanup rm -rf /etc"},
		{"exec -c 清环境", "exec -c rm -rf /usr"},
		{"exec -l 登录形态", "exec -l rm -rf /boot"},
		{"doas 前缀", "doas rm -rf /"},
		{"doas -u 带用户", "doas -u root rm -rf /etc"},
		{"doas -n 不交互", "doas -n rm -rf /usr"},
		{"busybox 前缀", "busybox rm -rf /"},
		{"包装叠加", "doas command rm -rf /etc"},
	}
	for _, tt := range blocks {
		t.Run(tt.name, func(t *testing.T) {
			got := evalPOSIX(t, tt.command)
			if got.Disposition != DispositionBlock || got.RuleID != "POSIX-CRITICAL-DELETE" {
				t.Fatalf("Evaluate(%q) = %+v, want 拦截/POSIX-CRITICAL-DELETE", tt.command, got)
			}
		})
	}
	allows := []struct{ name, command string }{
		{"command -v 是查找不执行", "command -v mkfs.ext4"},
		{"command -V 大写查找", "command -V rm"},
		{"command -v 查多个名字", "command -v mkfs.ext4 wipefs"},
		{"command 查日常命令", "command -v curl"},
		{"busybox 列小程序清单", "busybox --list"},
		{"busybox 日常小程序", "busybox ls -la"},
		{"exec 日常命令", "exec ls -la"},
		{"doas 日常命令", "doas ls -la"},
	}
	for _, tt := range allows {
		t.Run(tt.name, func(t *testing.T) {
			got := evalPOSIX(t, tt.command)
			if got.Disposition != DispositionAllow {
				t.Fatalf("Evaluate(%q) = %+v, want 放行", tt.command, got)
			}
		})
	}
}

// TestEvaluate_CombinedCommandsTakeConfirmTier:组合命令混入需确认档时
// 取最严重处置(拦截 > 需确认 > 放行),规则编号对最严重那条;同严重度
// 保留先出现的那条。
func TestEvaluate_CombinedCommandsTakeConfirmTier(t *testing.T) {
	tests := []struct {
		name       string
		command    string
		want       Disposition
		wantRuleID string
	}{
		{"需确认压过放行", "echo ok && iptables -F", DispositionConfirmRequired, "POSIX-LOCKOUT"},
		{"拦截压过需确认", "rm -rf /etc && iptables -F", DispositionBlock, "POSIX-CRITICAL-DELETE"},
		{"同严重度保留先出现", "ufw disable; zfs destroy tank/vol", DispositionConfirmRequired, "POSIX-LOCKOUT"},
		{"动态删除压过放行", "cd /opt && rm -rf $APP_DIR", DispositionConfirmRequired, "POSIX-DELETE-TARGET-UNKNOWN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evalPOSIX(t, tt.command)
			if got.Disposition != tt.want || got.RuleID != tt.wantRuleID {
				t.Fatalf("Evaluate(%q) = %+v, want %v/%s", tt.command, got, tt.want, tt.wantRuleID)
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
	t.Run("拆不动且含需确认家族命令名需确认", func(t *testing.T) {
		got := evalPOSIX(t, `iptables -F "$(broken`)
		if got.Disposition != DispositionConfirmRequired {
			t.Fatalf("got %+v, want 需确认", got)
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
	// 需确认档的默认规则同样能按编号停用。
	crules := &RuleSet{DisabledDefaultRuleIDs: []string{"POSIX-DELETE-TARGET-UNKNOWN"}}
	if got := Evaluate(ShellPOSIX, "rm -rf $DIR/", crules); got.Disposition != DispositionAllow {
		t.Fatalf("disabled POSIX-DELETE-TARGET-UNKNOWN still fired: %+v", got)
	}
	if got := Evaluate(ShellPOSIX, "iptables -F", crules); got.Disposition != DispositionConfirmRequired {
		t.Fatalf("other confirm rules must stay active: %+v", got)
	}
}

// TestEvaluate_UnknownShellNeedsConfirmation:无法识别的 shell 类型保守
// 返回需确认,不悄悄放行(PowerShell 与 cmd 的判定已由 Windows 规则实现)。
func TestEvaluate_UnknownShellNeedsConfirmation(t *testing.T) {
	got := Evaluate(ShellType("fish"), "Get-ChildItem", nil)
	if got.Disposition != DispositionConfirmRequired {
		t.Fatalf("Evaluate(fish) = %+v, want 需确认", got)
	}
	if got.Explanation == "" {
		t.Fatalf("需确认必须带说明,got %+v", got)
	}
}

// TestEvaluate_ShellAnnotationMismatchNeedsConfirmation:服务器条目上标注
// 错的 shell 类型会让按标注拆解的命令被误放行。按标注判为放行的命令,
// 再用别的 shell 家族语法试判同一文本:另一侧判出拦截或需确认时,
// 整体改判需确认并命中 SHELL-MISMATCH——偏向需确认,不悄悄放行;
// 也绝不因试判升成拦截。
func TestEvaluate_ShellAnnotationMismatchNeedsConfirmation(t *testing.T) {
	tests := []struct {
		name    string
		shell   ShellType
		command string
	}{
		{"POSIX 标注藏 PowerShell 格式化", ShellPOSIX, "Format-Volume -DriveLetter D"},
		{"POSIX 标注藏 cmd 删关键路径", ShellPOSIX, `rd /s /q C:\`},
		{"POSIX 标注藏 Windows 分区工具", ShellPOSIX, "diskpart"},
		{"PowerShell 标注藏 POSIX 格式化", ShellPowerShell, "mkfs.ext4 /dev/sdb"},
		{"PowerShell 标注藏 POSIX 删根", ShellPowerShell, "rm -rf /"},
		{"cmd 标注藏 POSIX 写盘", ShellWindowsCmd, "dd if=/dev/zero of=/dev/sda"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Evaluate(tt.shell, tt.command, nil)
			if got.Disposition != DispositionConfirmRequired || got.RuleID != "SHELL-MISMATCH" {
				t.Fatalf("Evaluate(%s, %q) = %+v, want 需确认/SHELL-MISMATCH", tt.shell, tt.command, got)
			}
			if got.Explanation == "" || got.Alternative == "" {
				t.Fatalf("Evaluate(%s, %q) must carry explanation and alternative, got %+v", tt.shell, tt.command, got)
			}
		})
	}
}

// TestEvaluate_ShellAnnotationMismatchSparesEverydayCommands:三类 shell
// 的日常命令在试判下仍是放行,不因跨 shell 试判被打扰。
func TestEvaluate_ShellAnnotationMismatchSparesEverydayCommands(t *testing.T) {
	tests := []struct {
		name    string
		shell   ShellType
		command string
	}{
		{"POSIX 日常列表", ShellPOSIX, "ls -la"},
		{"POSIX 删写死的构建产物", ShellPOSIX, "rm -r ./build"},
		{"POSIX 打印危险命令文字", ShellPOSIX, `echo "rm -rf /"`},
		{"POSIX 变量目录", ShellPOSIX, "cd $HOME"},
		{"POSIX git 状态", ShellPOSIX, "git status"},
		{"POSIX docker 列容器", ShellPOSIX, "docker ps"},
		{"PowerShell 日常列表", ShellPowerShell, "Get-ChildItem"},
		{"PowerShell 删写死的构建产物", ShellPowerShell, `Remove-Item .\build -Recurse`},
		{"cmd 日常列表", ShellWindowsCmd, "dir"},
		{"cmd 删写死的文件", ShellWindowsCmd, "del foo.txt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Evaluate(tt.shell, tt.command, nil)
			if got.Disposition != DispositionAllow || got.RuleID != "" {
				t.Fatalf("Evaluate(%s, %q) = %+v, want 放行", tt.shell, tt.command, got)
			}
		})
	}
}

// TestEvaluate_ShellMismatchDisableableLikeDefaultRule:SHELL-MISMATCH
// 注册在内置默认规则清单里,可像别的默认规则一样按编号停用;停用后
// 试判不再改判,按标注侧的原结论返回。
func TestEvaluate_ShellMismatchDisableableLikeDefaultRule(t *testing.T) {
	found := false
	for _, id := range DefaultRuleIDs() {
		if id == "SHELL-MISMATCH" {
			found = true
		}
	}
	if !found {
		t.Fatal("SHELL-MISMATCH must be listed among built-in default rule ids")
	}
	rules := &RuleSet{DisabledDefaultRuleIDs: []string{"SHELL-MISMATCH"}}
	if got := Evaluate(ShellPOSIX, "diskpart", rules); got.Disposition != DispositionAllow {
		t.Fatalf("disabled SHELL-MISMATCH still fired: %+v", got)
	}
	// 只停用 SHELL-MISMATCH 不影响标注侧本就危险的判定。
	if got := Evaluate(ShellPOSIX, "rm -rf /etc", rules); got.Disposition != DispositionBlock {
		t.Fatalf("annotated-shell judging must stay active: %+v", got)
	}
}

// TestEvaluate_ShellMismatchUsesEffectiveRuleSet:跨 shell 试判用同一份
// 生效规则集——自定义规则与停用记录对试判同样生效。
func TestEvaluate_ShellMismatchUsesEffectiveRuleSet(t *testing.T) {
	rules := &RuleSet{CustomRules: []CustomRule{{
		ID:          "CUSTOM-WINFMT",
		Shells:      []ShellType{ShellPowerShell},
		Commands:    []string{"myformat"},
		Disposition: DispositionBlock,
		Explanation: "custom rule for the test",
		Alternative: "none",
	}}}
	// POSIX 标注下无规则命中;PowerShell 家族试判命中自定义规则 → 需确认。
	if got := Evaluate(ShellPOSIX, "myformat D:", rules); got.Disposition != DispositionConfirmRequired || got.RuleID != "SHELL-MISMATCH" {
		t.Fatalf("custom rule must fire in the cross-judgement: %+v", got)
	}
	// 停用 SHELL-MISMATCH 后试判不再改判。
	rules.DisabledDefaultRuleIDs = []string{"SHELL-MISMATCH"}
	if got := Evaluate(ShellPOSIX, "myformat D:", rules); got.Disposition != DispositionAllow {
		t.Fatalf("disabled SHELL-MISMATCH still fired: %+v", got)
	}
}
