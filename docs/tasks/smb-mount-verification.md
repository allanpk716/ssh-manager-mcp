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

## 5. 结果(已回填,脱敏)

> 执行日期:2026-10-31。SSH 全程经 ssh-manager。记录中的替换:<NAS 地址>=NAS 的虚拟局域网地址,<测试共享>/<测试账号>/***=本次凭据与共享,<既有共享>/<既有映射>=owner 测试前就存在、与本次无关的数据。部分 Windows 中文报错在传输编码下转写为可读原文,语义未改。

### 5.0 环境与预检

- Windows 测试机:Windows 11(内部版本 22621),密钥认证 OpenSSH,默认 shell 实测为 cmd.exe。它同时是 ssh-manager 多机架构 serve broker 的宿主(因此一切注销/重启类步骤未执行)。机器上存在 owner 自己的持久盘符映射 Z: → `\\<NAS 地址>\<既有共享>`,全程未改动。
- macOS 测试机:macOS 27.0.1,常驻「开机自动登录」的全自动服务器(Orca、Ferryman 等随桌面会话自启)。本地免密 sudo(命令内 `sudo -n`)。
- NAS:群晖 DS918+。445 端口连通:两台测试机经虚拟局域网地址均通(NAS 的局域网地址实测也通)。实测协商 SMB 方言为 **2.0.2**(客户端 Get-SmbConnection 的 Dialect 列),与 NAS smb.conf 的 `max protocol=SMB2` 一致。
- 「禁止同一 IP 多个连接」开关:owner 口头确认无禁止(未在 DSM 界面逐项复核)。测试中同一客户端同时存在多条到 NAS 的会话,未出现被拒。
- 共享与账号:owner 指定使用 NAS 上**已有的** <测试共享> 与专用测试账号(未按第 1 节预期新建空共享)。执行约定:测试文件一律 `sshmgr-` 前缀;不在记录里列举共享中已有文件;测后删除。
- Linux 测试机:手册原定候选机多次 TCP 连接超时(不可达);经 owner 决定,改用另一台受管 Linux 机(Ubuntu 24.04 虚拟机、运行生产服务、本地免密 sudo),由 owner 提供虚拟局域网接入密钥临时入网以到达 NAS,完成全节测试;测毕已退网卸载、完全还原(见 5.7)。

### 5.1 Windows 会话身份

命令:`whoami /all`、`query user`
输出(节选):用户 <Windows 测试机>\<Windows 账号>;组含 `NT AUTHORITY\NETWORK`、`BUILTIN\Administrators`;`query user` 显示该账号有一条远程桌面会话(状态 Active、长时间空闲)。
结论:SSH 会话为网络登录(Network logon),无交互桌面;但机器上存在 owner 的远程桌面会话,后续 (c) 免密码变体即借用它。

### 5.2 Windows (a) cmdkey 复现

命令:`cmdkey /add:<NAS 地址> /user:<测试账号> /pass:***`
输出:`CMDKEY: Credentials cannot be saved from this logon session.`;随后 `cmdkey /list` 为 `* NONE *`。
结论:复现成功。网络登录会话无法向凭据管理器写入。

### 5.3 Windows (b) 显式凭据访问 UNC

命令(同一 exec_command 会话内):
```
net use \\<NAS 地址>\<测试共享> /user:<测试账号> ***
dir \\<NAS 地址>\<测试共享>\sshmgr-mounttest*
echo hello> \\<NAS 地址>\<测试共享>\sshmgr-mounttest-from-ssh.txt
type \\<NAS 地址>\<测试共享>\sshmgr-mounttest-from-ssh.txt
net use
```
输出:连接成功;写入 8 字节文件、读回 `hello`;`net use` 显示 `\\<NAS 地址>\<测试共享>` 状态 OK。

新开 exec_command(新 SSH 登录会话,不带凭据):
命令:`dir \\<NAS 地址>\<测试共享>\sshmgr-mounttest*`
输出:失败——「用户名或密码不正确。」(系统错误 1326);`net use` 中无该连接(仅剩原有 Z: 记忆条目)。

PowerShell 对照(独立会话):
命令:`New-PSDrive -Name T -PSProvider FileSystem -Root '\\<NAS 地址>\<测试共享>' -Credential <PSCredential>` 后 `Get-ChildItem`
输出:成功列出并读到测试文件。
结论:
1. 同会话显式凭据读写可行;
2. 凭据与 SMB 连接都绑定登录会话:新会话不可见、不可用,必须重新携带凭据;
3. PSDrive 是等价的会话内做法(随进程消失,无残留);
4. 桌面会话直接访问:未直接验证(owner 未操作桌面),由 5.4 (c)-2 的任务日志侧面证明桌面会话存在且可写凭据。

### 5.4 Windows (c) 计划任务(免密码变体)

手册原方案需 Windows 登录密码(`/rp`)。owner 决定不提供密码,改为免密码变体:创建任务时不指定 `/ru`/`/rp`,任务登录方式为「仅交互」(Interactive only),手动 `/run` 时借用 owner 活跃的远程桌面会话执行。带密码路径未测(见未验证清单)。

(c)-1 创建:
命令:`schtasks /create /tn sshmgr-mounttest-nopw /sc onlogon /rl highest /tr "cmd /c cmdkey /add:<NAS 地址> /user:<测试账号> /pass:*** > C:\Users\Public\mounttest-nopw.log 2>&1"`
输出:`SUCCESS: ... successfully been created`;查询显示 `Logon Mode: Interactive only`、`Run As User: <Windows 账号>`。
结论:密钥认证会话内创建无密码任务可行。

(c)-2 手动运行与凭据落库:
命令:`schtasks /run /tn sshmgr-mounttest-nopw`,随后读日志、`cmdkey /list`
输出:`Last Result: 0`;日志为 `CMDKEY: 成功保存凭据。`;但 SSH 会话 `cmdkey /list` 仍为 `* NONE *`。
结论:任务在交互会话里执行成功、凭据确实存入;**网络会话枚举不到这条凭据**。

(c)-3 新 SSH 会话静默认证(关键测试):
命令(新会话,无凭据):`dir \\<NAS 地址>\<测试共享>\sshmgr-nosuchpattern*`
输出:「用户名或密码不正确。」(1326)。
结论:**桌面会话存入的凭据,SSH 网络会话自动认证用不上**——对「让 SSH 会话共享桌面侧凭据」是否定性结论。

(c)-4 net use 变体:
命令:任务 `/tr` 换成 `net use \\<NAS 地址>\<测试共享> /user:<测试账号> *** /persistent:yes`
输出:运行成功(日志「命令成功完成。」);但 SSH 会话 `net use` 看不到该 UNC 连接(对比:盘符型记忆条目如原有 Z: 跨会话可见,状态 Unavailable)。
结论:桌面会话内的 net use 同样会话绑定;`/persistent:yes` 的 UNC 记忆对 SSH 会话不可见。

(c)-5 onstart 变体(只创建未运行):
命令:`schtasks /create ... /sc onstart`(免密码)
输出:创建成功;`Logon Mode: Interactive only`。
结论:免密码的 onstart 任务「能创建、但开机无登录时不会运行」;要开机无人登录运行必须带密码(`/rp`),该路径未测。

(c)-7 SYSTEM 变体(手册可选项):
命令:`schtasks /create ... /ru SYSTEM`(免密码,无需桌面)后 `/run`
输出:`Logon Mode: Interactive/Background`,`Last Result: 1`,任务内 `dir` 失败(1326)。
结论:SYSTEM 账户用不到用户凭据,与手册预期一致。

### 5.5 Windows (d) persistent:yes 盘符映射

偏差说明:盘符 Z: 已被 owner 既有映射占用,本项改用 **Y:**;手册清理段的 `net use * /delete /y` 与 `reg delete HKCU\Network\Z /f` 会误删该既有映射,**未按手册执行**,清理改为只删本次建立的条目。
命令(同会话):`net use Y: \\<NAS 地址>\<测试共享> /user:<测试账号> *** /persistent:yes`
输出:成功;`net use` 显示 `Y: ... OK`;`reg query HKCU\Network` 出现 `Y` 键(RemotePath/UserName 等);`cmdkey /list` 仍为 `* NONE *`。
新会话验证:`net use` 显示 `Y: ... Unavailable`(仅注册表记忆,无活动连接);`if exist Y:\` 不成立,无法访问。
结论:盘符映射成功且注册表持久化(下次该用户交互登录时会尝试恢复);**密码不落凭据管理器**,恢复需要凭据;活动连接不跨会话。
附带观察:
- owner 既有 Z:→<既有共享> 持久映射本身就是该机制的现存实例;
- `/persistent:yes` 会把用户级「新连接是否记住」默认翻为「记住」,测后已还原为「不记住」;
- 系统级连接表(Get-SmbConnection)显示桌面会话的既有连接实际存活——SSH 会话里 `net use` 显示的 Unavailable 只代表「盘符未恢复到本会话」,不代表系统无连接;本次测试在桌面会话建立过的一条连接,SSH 侧删不掉,已由桌面会话内任务执行删除。

### 5.6 macOS(两轮:手册直接图 + 补充间接图)

公共条件:macOS 27.0.1;`sudo -n` 可用;凭据写在映射文件里(URL 编码:密码中的反斜杠→`%5C`,实测有效);映射文件权限 600、属主 root。

第一轮——直接图(手册配方,含一次重启):
1. 备份并编辑 `/etc/auto_master`(追加 `/-  auto_smb  -nosuid,noowners`),写 `/etc/auto_smb`(`/Volumes/mounttest  -fstype=smbfs,soft  ://<测试账号>:***@<NAS 地址>/<测试共享>`)。
2. `automount -cv` 后**触发点未生成**:`/Volumes/mounttest` 不存在;automountd 日志:`automount_realpath:getattrlist:fail:2`、`mountpoint unavailable`。
3. 补 `sudo -n mkdir -p /Volumes/mounttest` 后再 `automount -cv`:触发点生成(`map auto_smb on /Volumes/mounttest (autofs, ...)`);一 touch 即挂 SMB(`//<测试账号>@<NAS 地址>/<测试共享> on /Volumes/mounttest (smbfs, ..., mounted by <macOS 账号>)`),读写正常。
   → **手册缺陷:macOS 直接图必须先建挂载点目录**(Linux 的 autofs 会自建,macOS 不会)。
4. 关自动登录(`defaults delete /Library/Preferences/com.apple.loginwindow autoLoginUser`)+ 重启(owner 批准;机上 3 个 agent 会话按约定被终止)。
5. 重启后确认无人登录(`who` 为空、`/dev/console` 属主 root),但 `/Volumes/mounttest` **目录已消失**,开机时 automountd 日志同样报 `getattrlist:fail:2`,触发点没有恢复。
   → **直接图过不了重启:挂载点目录在关机/开机周期中被清除,automountd 不会自建**。
6. 无人登录状态下重演「mkdir + automount -cv」:触发→挂载→写入全部成功。
   → 机制本身无人值守可用(凭据走文件,不依赖登录与钥匙串)。

第二轮——间接图(owner 批准补测,再重启一次):
配置:`/etc/auto_master` 追加 `/mnt  auto_smb  -nosuid,noowners`;`/etc/auto_smb` 为 `mounttest  -fstype=smbfs,soft  ://<测试账号>:***@<NAS 地址>/<测试共享>`(访问路径 `/mnt/mounttest`)。
- 部署:**无需 mkdir**,automount 自建触发点;`/mnt` 由系统目录映射(firmlink)指向数据卷。
- 重启后无人登录(`who` 为空):触发点**由开机过程自动重建**(目录时间戳为开机后),一访问即挂载、写入成功。
结论:**间接图免 mkdir 且抗重启,无人登录即挂即用——macOS 的推荐候选**;直接图若要用,每轮开机都需要有东西补 `mkdir`+`automount -cv`(即仍需 LaunchDaemon 一类开机钩子)。

第 7 步 系统升级是否覆盖 `/etc/auto_master`:
- 实测:`ls -lO /etc/auto_master` 无系统完整性保护旗标;`df` 显示该文件位于 `/System/Volumes/Data`(数据卷,不在密封系统卷快照内);`/etc` 是指向 `private/etc` 的符号链接;root 可改(本轮实际修改/还原过)。
- 公开资料:社区报告互相矛盾——既有「macOS 更新后 `/etc/auto_master` 被重置为默认」的报告,也有「升级保留了自定义」的报告;未见 Apple 官方文档承诺保留。
- 结论:不能假设升级后保留;若 #42 采纳 autofs,应配「升级后检查/重应用」兜底(或并入自愈 LaunchDaemon)。
- 来源:[Apple StackExchange:automount is broken after upgrading macOS Monterey](https://apple.stackexchange.com/questions/440554/automount-is-broken-after-upgrading-macos-monterey)、[autofs on macOS 要点(gist)](https://gist.github.com/rudelm/7bcc905ab748ab9879ea);另经搜索引擎聚合页见到两种矛盾报告,未取得原始直接链接。

### 5.7 Linux(fstab + 凭据文件 + systemd 自动挂载,全节验证)

测试机说明:手册原定候选机多次不可达。备选逐台探测(到 NAS 的 445 可达、免密 sudo、空闲且允许重启三个条件):公司/实验内网的各备选机到 NAS 均不通(无虚拟局域网成员身份,也无到 NAS 所在网段的路由);生产用途机器(推理主力、源码服务器)排除;一台云上机器虽可达,但承载虚拟局域网管理面与多项服务,重启会波及整个虚拟局域网,排除。最终由 owner 决定:一台 Ubuntu 24.04 受管虚拟机(免密 sudo、空闲)临时入虚拟局域网(入网密钥由 owner 提供)后作为测试机;其上的生产服务因重启短暂中断,owner 已知情。NAS SMB 方言上限实测 2.0.2,手册配方的 `vers=3.0` 必失败,本轮按 `vers=2.0` 验证(唯一参数偏差)。

1. 预检:`sudo -n true` 通过;systemd 255;**cifs-utils 未预装**,`sudo -n apt-get install -y cifs-utils` 完成(记录:配方需安装此包)。
2. **仅有免密 sudo 时的最小命令集合**(原样记录,凭据打码;fstab 备份在改动前做):
```
sudo -n cp /etc/fstab /etc/fstab.bak-mounttest
sudo -n mkdir -p /mnt/mounttest
printf '%s\n' 'username=<测试账号>' 'password=***' | sudo -n tee /etc/sshmgr-mounttest.cred >/dev/null
sudo -n chmod 600 /etc/sshmgr-mounttest.cred
echo '//<NAS 地址>/<测试共享> /mnt/mounttest cifs credentials=/etc/sshmgr-mounttest.cred,vers=2.0,uid=1000,gid=1000,file_mode=0664,dir_mode=0775,_netdev,nofail,x-systemd.automount,x-systemd.idle-timeout=600 0 0' | sudo -n tee -a /etc/fstab
sudo -n systemctl daemon-reload
sudo -n systemctl restart remote-fs.target
ls /mnt/mounttest && findmnt /mnt/mounttest
```
输出:`ls` 触发即挂;findmnt 显示 cifs 以 `vers=2.0` 挂载,uid/gid=1000(forceuid/forcegid)与 file_mode=0664/dir_mode=0775 均生效;普通用户(uid 1000)写入测试文件成功。
结论:最小命令集合成立;**`vers` 必须按提供方设备的实际方言生成**(本 NAS 上限 2.0.2),不能写死。
3. NAS 在线重启:开机 6.3 秒(无卡顿);虚拟局域网客户端开机自连后,访问挂载点即自动挂载,写入成功;`mnt-mounttest.automount` 为 active(waiting)。注:挂载发生在首次访问时(automount 惰性),实际使用要先等虚拟局域网连通,时间顺序上由访问时点天然保证。
4. NAS 不在线(fstab 地址临时换为不可达地址 `192.0.2.1`,`daemon-reload` 后重启):开机 6.16 秒,无失败单元、无紧急模式、SSH 正常;访问挂载点约 4 秒返回失败(退出码 2),mount 单元干净置 failed,automount 继续待命重试,不悬挂调用进程。
5. 对照实验(去掉 `nofail` 与 `x-systemd.automount`,纯 fstab,不可达地址;owner 批准后执行):开机 9.47 秒(慢约 3.3 秒,即一次失败的挂载尝试),无紧急模式、SSH 正常,系统状态 degraded(挂载单元 failed)。
   诚实说明:本网络下该测试地址为「快速失败型」(连接尝试约 4 秒被拒);**慢失败/超时型不可达(真正能卡住开机的场景)未能复现**——未验证。据此 #42 配方建议:保留 `nofail`+`x-systemd.automount`,并追加 `x-systemd.mount-timeout=20s`(量级示例)兜底超时场景。
6. 断线重连(NAS 侧断开):未验证(无法在 NAS 侧操作,跳过——手册允许)。

### 5.8 清理记录(逐项复核)

- Windows 测试机:Y: 映射与注册表 `HKCU\Network\Y` 已删;`cmdkey` 为空;全部 6 个测试任务(`sshmgr-mounttest-nopw/nopw2/nopw3/cleanup/cleanup2/sys`)与全部日志已删;「新连接是否记住」默认已还原;原有 Z:/<既有共享> 映射未动(注册表详情前后一致)。桌面会话曾建立的一条到 <测试共享> 的连接已由桌面会话内任务删除,系统级连接表随后仅剩 owner 自己的既有连接;凭据的删除同样在桌面会话内完成,并以 `cmdkey /list`(`* NONE *`)自证。
- macOS 测试机:两轮各自完整清理——`/etc/auto_master` 均从备份还原(字节数一致、内容为原版)、`/etc/auto_smb` 删除、挂载卸载、触发点目录消失、无任何 smbfs 残留;自动登录经 owner 授权恢复(defaults 键写回,原有的开机自动登录口令文件未改动),并经一次重启验证:开机自动登录成功,Orca、Ferryman 随桌面会话恢复。
- Linux 测试机:fstab 从备份还原(0 条 cifs 行)、凭据文件与备份删除、挂载点删除、无 cifs 挂载、reset-failed 后无失败单元;临时入网的虚拟局域网客户端已退网并卸载(二进制/服务/网络接口/软件源配置全清),退网后到 NAS 的 445 恢复不通,网络状态还原到测试前。
- NAS:仅出现过 `sshmgr-` 前缀的测试文件,均已删除;共享、账号、服务配置零改动。

### 5.9 总结(回答 issue #41 的三组问题)

**Windows(a–d):**
1. 网络登录会话 `cmdkey` 必败,凭据管理器写不进((a) 复现)。
2. 会话内「显式凭据 net use / PSDrive」完全可用,连接与凭据随登录会话消失((b));跨会话不可继承。
3. 持久化(`HKCU\Network` 盘符记忆)只对交互登录的下次恢复有意义((d));SSH 会话既拿不到活动连接,也没有可用凭据;密码不会随 `/persistent:yes` 落库。
4. 计划任务路线:免密码(Interactive only,借桌面会话)能把凭据存进交互侧,但 SSH 会话枚举不到、自动认证也用不上((c)-2/3);SYSTEM 用不了用户凭据((c)-7);开机无人登录运行必须带密码((c)-5 的推论,未实测)。
→ 对 sshmgr 的含义:Windows 上不存在「配置一次、SSH 会话自动可用」的开机级做法;可行形态是**每次 exec_command 会话内注入显式凭据**(net use 或 PSDrive,均可程序化),凭据由 sshmgr 的机密项供给。

**macOS:**
- 直接图:需补 mkdir 且过不了重启,不能单独作为方案;
- **间接图:一行 auto_master + 一个 600 权限映射文件,免 mkdir、抗重启、无人登录即挂即用——推荐 #42 采纳的候选**;
- 系统升级可能重置 `/etc/auto_master`(文件在数据卷、无系统完整性保护,但社区报告矛盾),需配升级后自愈检查。

**Linux:** fstab + 仅 root 可读凭据文件 + `_netdev,nofail,x-systemd.automount` 配方真机验证通过(NAS 在线重启 6.3 秒、不可达地址重启 6.16 秒均不卡开机,访问失败快速干净,普通用户可读写);`vers` 必须按提供方设备实际方言生成(本 NAS 上限 2.0.2,取 2.0);去掉 `nofail`+`automount` 的对照显示「快速失败型」不可达仅慢约 3 秒且不卡开机,但慢失败/超时型场景未复现,配方建议追加 `x-systemd.mount-timeout` 兜底。

### 5.10 未验证项及原因

1. Windows (c) 带 `/rp` 密码的全部小项,以及 onlogon/onstart 的真实注销/开机触发:需要重启 serve broker 宿主(已排除)与登录密码(owner 决定不提供)。
2. Windows (b) 桌面会话(资源管理器)直接访问:owner 未操作桌面;已由 (c)-2 任务日志侧面证明桌面会话可存凭据。
3. Linux 慢失败/超时型不可达场景(目标在线但无响应、或防火墙静默丢包)的 fstab 行为:本网络只能构造「快速失败型」不可达(约 4 秒被拒),慢失败场景未复现,`nofail` 在该场景下的防卡价值系依据公开资料的推断;断线重连(NAS 侧断开)因无法在 NAS 侧操作未验证。
4. macOS 系统升级覆盖行为:未实际升级,仅文件位置实测 + 公开资料(互相矛盾),按「可能重置」处置。
5. NAS「禁止同一 IP 多个连接」开关状态:owner 口头确认,未在 DSM 界面逐项复核。

---

填写完成后,把本文件交还 owner;owner 会在新的会话里把结论贴到 issue #41 并关票。
