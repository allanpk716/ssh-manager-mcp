# 执行手册:真机验证共享存储挂载机制的候选做法

> 一次性执行文档,放在不合并的分支 `task/smb-mount-verification` 上。
> 对应票:「共享存储:真机验证挂载机制候选做法」(GitHub issue #41,属于路线图「局域网共享存储、网络拓扑与 Web UI 管理界面」#31)。
> 结论将用于下一张票「共享存储:Windows 与 macOS 上的挂载方式」(issue #42)的决策。

## 0. 给执行者(AI agent)的总规则

1. **只做验证,不做决定。** 每一项记录「执行的命令 + 原样输出 + 一句话结论」。遇到意外不要自行换方案,记下现象后继续下一项,或停下问 owner。
2. **SSH 一律走 ssh-manager MCP**(`mcp__ssh-manager__list_servers` 拿服务器 id,再 `exec_command` / `exec_background`)。不要用裸 `ssh` / `scp`。
3. **公开仓库脱敏**:写回本仓库、GitHub issue 的内容里,不写真实 IP、服务器名、主机名、账号名、密码。用下文的代称(Windows 测试机、macOS 测试机、Linux 测试机、NAS)。命令输出里出现的真实值,用 `<NAS 地址>`、`<测试账号>` 之类替换后再写。
4. **密码**:SMB 测试账号的密码由 owner 当面告诉你,只在命令里使用;记录输出前打码为 `***`。不要写进任何文件(测试过程中必须落盘的凭据文件除外,且清理时删除)。
5. **不碰 NAS 上已有的数据**。只使用 owner 新建的测试共享。
6. **每台机器做完立刻清理**(见各节「清理」),并在记录里写明已清理。
7. 不要 commit 到 master;结果写进本文第 5 节,只在本分支提交。

## 1. 测试环境(owner 填写真实对应关系后交给执行者,不要提交真实值)

| 代称 | 说明 | 真实服务器(只在对话里告诉执行者) |
|---|---|---|
| Windows 测试机 | Windows,密钥认证 OpenSSH,默认 shell 为 cmd.exe | 由 owner 指定 |
| macOS 测试机 | macOS 27,开机自动登录,本地免密 sudo | 由 owner 指定 |
| Linux 测试机 | Ubuntu,需要免密 sudo、允许重启 | 由 owner 指定 |
| NAS | 群晖 DS918+,提供测试共享 | 由 owner 指定 |

owner 需事先准备:

- [ ] 在 NAS 上新建空的测试共享(例如共享名 `mounttest`)和一个只对该共享有读写权限的测试账号;用完删除。
- [ ] 告诉执行者:NAS 在三台测试机各自能到达的地址(物理局域网或虚拟局域网)、共享名、测试账号与密码。
- [ ] 确认三台测试机都能用 ssh-manager 连上(预检时 Linux 测试机候选连接超时,需先处理)。

**已知风险(owner 已知悉,执行者务必遵守):**

- Windows 测试机候选同时是 ssh-manager **多机架构 serve broker 的宿主**。注销或重启它会让所有 ssh-manager 实例(包括执行者自己)断线。需要注销/重启的步骤,**先问 owner**,并预先安排好「重启后谁、从哪里继续」。
- macOS 测试机候选上运行着 Orca、Ferryman 和 Claude Code。重启前**不要在它本机跑执行者**,应从另一台机器经 ssh-manager 操作它;关闭自动登录后,恢复自动登录需要登录密码,由 owner 事后手动恢复。

开始前先做连通预检,并记录:

```
# 每台测试机:到 NAS 的 445 端口是否通
Linux:   nc -z -w 3 <NAS 地址> 445 && echo OK
macOS:   nc -z -G 3 <NAS 地址> 445 && echo OK
Windows: powershell -NoProfile -Command "Test-NetConnection -ComputerName <NAS 地址> -Port 445 -InformationLevel Quiet"
```

另确认 NAS 上「禁止同一 IP 多个连接」开关的状态(控制面板 → 文件服务 → SMB → 高级),记录下来。

## 2. Windows 测试机

背景(来自调研分支 `research/smb-persistent-mount` 的 `docs/research/smb-persistent-mount.md`):密钥认证的 OpenSSH 会话属于网络登录,没有凭据集,`cmdkey /add` 应失败;`net use` 盘符与登录会话绑定;两种替代做法只有二手来源。

注意:默认 shell 是 cmd.exe,PowerShell 命令要包在 `powershell -NoProfile -Command "..."` 里。

先记录会话身份:`whoami /all`(看 Logon Type / 是否有 `NETWORK` 组)、`query user`(当前有无桌面会话)。

### (a) 复现 `cmdkey /add` 失败

```
cmdkey /add:<NAS 地址> /user:<测试账号> /pass:***
cmdkey /list
```

预期:报错「Credentials cannot be saved from this logon session」或中文等价。记录原文。

### (b) 会话内用显式凭据访问 UNC 路径

```
net use \\<NAS 地址>\mounttest /user:<测试账号> ***
dir \\<NAS 地址>\mounttest
echo hello> \\<NAS 地址>\mounttest\from-ssh.txt
net use
```

要回答的问题:

1. 能否成功读写?
2. **作用范围**:同一 SSH 会话里的后续命令能否访问?**新开的另一次** `exec_command`(新 SSH 会话)能否不带凭据直接 `dir \\<NAS 地址>\mounttest`?桌面会话(如有人登录,请 owner 在桌面打开资源管理器访问,或跳过)能否访问?
3. SSH 会话结束后连接是否消失(新会话里 `net use` 看不到)?

再试不带 `net use` 的 PowerShell 方式,作对照:

```
powershell -NoProfile -Command "$c = New-Object System.Management.Automation.PSCredential('<测试账号>', (ConvertTo-SecureString '***' -AsPlainText -Force)); New-PSDrive -Name T -PSProvider FileSystem -Root '\\<NAS 地址>\mounttest' -Credential $c; Get-ChildItem T:\"
```

### (c) 用账号密码注册计划任务执行 `cmdkey` / `net use`

需要 Windows 登录账号的密码(owner 提供,执行时打码)。任务名用 ASCII,例如 `sshmgr-mounttest`。

```
schtasks /create /tn sshmgr-mounttest /ru <Windows 账号> /rp *** /sc onlogon /rl highest /tr "cmd /c cmdkey /add:<NAS 地址> /user:<测试账号> /pass:*** > C:\Users\Public\mounttest.log 2>&1"
schtasks /run /tn sshmgr-mounttest
type C:\Users\Public\mounttest.log
schtasks /query /tn sshmgr-mounttest /v /fo list
```

要回答的问题:

1. `schtasks /create` 在密钥认证会话里能否成功(带 `/rp` 密码)?
2. 手动 `/run` 后,任务里的 `cmdkey /add` 是否成功?之后 `cmdkey /list`(在 SSH 会话里和在桌面会话里)能否看到这条凭据?
3. 凭据存进去后,**新开的 SSH 会话**不带密码 `dir \\<NAS 地址>\mounttest` 是否可行?桌面会话是否可行?
4. 把 `/tr` 换成 `net use \\<NAS 地址>\mounttest /user:<测试账号> *** /persistent:yes` 重复一遍,观察同样的问题。
5. 把 `/sc onlogon` 换成 `/sc onstart`(开机时,不登录也运行),任务能否创建、能否 `/run`,以哪个登录会话运行。
6. 「登录时 / 开机时」触发的真实效果需要注销或重启 Windows 测试机:**先问 owner 是否允许**(见第 1 节风险)。不允许就只记录手动 `/run` 的结果,并注明「未验证真实登录/开机触发」。
7. 服务账号能否访问:可选。用 `schtasks /create ... /ru SYSTEM` 跑 `dir \\<NAS 地址>\mounttest`,看 SYSTEM 是否能用到上面存下的凭据(预期不能,因为凭据按用户存)。

### (d) `net use /persistent:yes` 的实际行为

在 SSH 会话里:

```
net use Z: \\<NAS 地址>\mounttest /user:<测试账号> *** /persistent:yes
net use
reg query HKCU\Network
```

要回答的问题:盘符映射是否成功;注册表 `HKCU\Network\Z` 是否被写入(写入即代表下次该用户交互登录时会尝试恢复);新开的 SSH 会话里是否能看到 Z:;是否保存了密码(预期不保存,恢复时需要凭据管理器里有凭据)。

### 清理(Windows)

```
net use * /delete /y
cmdkey /delete:<NAS 地址>
schtasks /delete /tn sshmgr-mounttest /f
reg delete HKCU\Network\Z /f
del C:\Users\Public\mounttest.log
```

清理后再跑一次 `net use`、`cmdkey /list`、`schtasks /query /tn sshmgr-mounttest` 确认干净。

## 3. macOS 测试机

背景:无人登录也生效的候选只有 autofs(`/etc/auto_master` + `auto_smb`)和 LaunchDaemon;钥匙串依赖登录会话,不可用;社区报告系统升级可能覆盖 `/etc/auto_master`。

**本节要从另一台机器经 ssh-manager 执行**(macOS 测试机会被重启)。ssh-manager 登记里这台 `has_sudo=false`,但它本地是免密 sudo,所以在命令里直接写 `sudo -n ...`,不要用 `sudo=true` 参数。

### 步骤

1. 备份:`sudo -n cp /etc/auto_master /etc/auto_master.bak-mounttest`
2. 在 `/etc/auto_master` 末尾追加一行:`/-  auto_smb  -nosuid,noowners`
3. 写 `/etc/auto_smb`(权限 600,属主 root):
   ```
   /Volumes/mounttest  -fstype=smbfs,soft  ://<测试账号>:***@<NAS 地址>/mounttest
   ```
   (密码含特殊字符时要 URL 编码。)
4. `sudo -n automount -cv`,然后 `ls /Volumes/mounttest`、`mount | grep mounttest`、`ls -ld /Volumes/mounttest`。
   记录:是否自动挂载;**挂载属主**(哪个用户、什么权限);普通用户能否读写;`noowners` 去掉后有何不同(可选)。
5. 记录 `/etc/auto_master` 的修改时间和 `sw_vers`。
6. **重启验证无人登录**(owner 已同意重启一次):
   - 关自动登录前,先请 owner 确认已知道事后要手动恢复。关闭:`sudo -n defaults delete /Library/Preferences/com.apple.loginwindow autoLoginUser`(会让下次开机停在登录界面)。
   - `sudo -n shutdown -r now`。
   - 等它回来(ssh-manager 能连上即可;虚拟局域网客户端若依赖登录则可能连不回,此时请 owner 从局域网或屏幕处理)。
   - 在无人登录状态下:`who`(确认没有桌面用户)、`ls /Volumes/mounttest`、`mount | grep mounttest`。记录是否在无人登录时自动挂上。
7. 系统升级是否覆盖 `/etc/auto_master`:不实际升级。查 Apple 官方文档 / 支持文章,并检查 `/etc/auto_master` 是否属于系统受保护文件(`ls -lO /etc/auto_master`、看是否在已签名系统卷上),记录来源链接。

### 清理(macOS)

```
sudo -n mv /etc/auto_master.bak-mounttest /etc/auto_master
sudo -n rm /etc/auto_smb
sudo -n umount /Volumes/mounttest   # 若仍挂着
sudo -n automount -cv
```

然后请 owner 恢复自动登录(系统设置 → 用户与群组 → 自动登录,需要登录密码),确认 Orca、Ferryman 等自启服务恢复。

## 4. Linux 测试机

背景:方案是 `/etc/fstab` + 0600 root 凭据文件,选项 `vers=3.0`、`uid/gid`、`file_mode/dir_mode`、`_netdev,nofail,x-systemd.automount`;调研推断 `nofail` 使 NAS 不在线时开机不卡,但未真机验证。

### 步骤

1. 预检:`sudo -n true`(必须免密)、`systemctl --version`、`dpkg -l cifs-utils`(没装就 `sudo -n apt-get install -y cifs-utils`,记录是否需要安装)。
2. **仅有免密 sudo 时的最小命令集合**(这是要交付的结论之一,请原样记录实际用到的全部命令):
   ```
   sudo -n mkdir -p /mnt/mounttest
   printf 'username=<测试账号>\npassword=***\n' | sudo -n tee /etc/sshmgr-mounttest.cred >/dev/null
   sudo -n chmod 600 /etc/sshmgr-mounttest.cred
   echo '//<NAS 地址>/mounttest /mnt/mounttest cifs credentials=/etc/sshmgr-mounttest.cred,vers=3.0,uid=1000,gid=1000,file_mode=0664,dir_mode=0775,_netdev,nofail,x-systemd.automount,x-systemd.idle-timeout=600 0 0' | sudo -n tee -a /etc/fstab
   sudo -n systemctl daemon-reload
   sudo -n systemctl restart remote-fs.target
   ls /mnt/mounttest && findmnt /mnt/mounttest
   ```
   修改 fstab 前先 `sudo -n cp /etc/fstab /etc/fstab.bak-mounttest`。
3. NAS 在线时重启:`sudo -n reboot`,回来后 `systemd-analyze`、`systemctl status mnt-mounttest.automount`、`ls /mnt/mounttest`。
4. **NAS 不在线时开机是否卡住**:不要真的关 NAS。改为把 fstab 这一行的地址临时换成一个不可达地址(例如同网段一个确认无人使用的地址,或 `192.0.2.1`),`daemon-reload` 后重启。回来后记录:开机耗时(`systemd-analyze`、`systemd-analyze blame | head`)、是否能正常 SSH 登录、`ls /mnt/mounttest` 的表现(卡多久、报什么错)。
   - 对照(可选):去掉 `nofail` 再重启一次,看是否明显变慢或进入紧急模式。**这一步有让机器开不了机的风险,先问 owner**。
5. 断线重连(可选):挂载后在 NAS 侧无法操作就跳过。

### 清理(Linux)

```
sudo -n umount /mnt/mounttest 2>/dev/null
sudo -n cp /etc/fstab.bak-mounttest /etc/fstab
sudo -n rm /etc/sshmgr-mounttest.cred /etc/fstab.bak-mounttest
sudo -n rmdir /mnt/mounttest
sudo -n systemctl daemon-reload
```

## 5. 结果(执行者填写,脱敏)

按下面格式填写,每个小项都要有:

```
### Windows (a) cmdkey 复现
命令:...
输出:...
结论:...
```

最后写一段「总结」,逐条回答 issue #41 的三组问题(Windows a–d、macOS、Linux),并列出未能验证的项和原因。

填写完成后,把本文件交还 owner;owner 会在新的会话里把结论贴到 issue #41 并关票。
