# 共享存储:各系统持久挂载机制与群晖 SMB 限制调研

对应议题:#33(上级 #31,阻塞 #37)。调研日期:2026-10-30。

约定:每条结论后标注来源。「未核实」表示本次没有读到一手来源,只是常识或推断,落实现前需要真机验证。文中不出现真实地址、服务器名、账号名;示例一律用 `nas.example.com`、`Share`、`nasuser`。

## 总览(结论摘要)

1. Linux:推荐 `/etc/fstab` + 权限 0600 的凭据文件 + `vers=3.x` + `_netdev,nofail`;想避免开机卡住或网络晚到,用 `x-systemd.automount`。软件包为 cifs-utils。非 root 用户经 sudo 写 root 拥有的文件即可。
2. macOS:无人登录也能生效的机制是 autofs(`/etc/auto_master` + `auto_smb`)或 LaunchDaemon;密码只能放在 map 文件的 URL 里(0600 root 文件),不会用到钥匙串。macOS 升级可能覆盖 `/etc/auto_master`(社区报告)。
3. Windows:公钥认证的 OpenSSH 会话是「网络登录」,没有凭据集,`cmdkey /add` 会失败(已核实);`net use X:` 盘符只在创建它的登录会话内可见。可行路径是会话内带显式凭据直连 UNC,或计划任务以账号密码运行(见 3.5)。
4. 群晖:DS918+ 规格为 SMB/NFS/AFP/FTP 合计最多 1000 个并发连接;规格页和帮助页均未写按账号的同时登录数限制;DSM 有「同一 IP 只允许一个连接」开关;空闲会话断开没有找到文档化的 SMB 专项设置。
5. 探测 445 端口:Linux `nc -z -w 3`、macOS `nc -z -G 3`、Windows `Test-NetConnection -Port 445`,都只做 TCP 握手不发应用数据。

## 1. Linux:mount.cifs 持久挂载

### 1.1 推荐写法

`/etc/fstab` 一行(示例,选项含义见下):

```
//nas.example.com/Share  /mnt/share  cifs  credentials=/etc/sshmgr/smb-share.cred,vers=3.0,uid=1000,gid=1000,file_mode=0660,dir_mode=0770,_netdev,nofail,x-systemd.automount,x-systemd.mount-timeout=30  0  0
```

凭据文件(root 拥有,权限 0600):

```
username=nasuser
password=<密码>
domain=<可选>
```

逐项依据:

| 项目 | 结论 | 来源 |
|---|---|---|
| 凭据文件 | 格式为每行 `key=value`,键为 `username`、`password`、`password2`、`domain`;手册称其优于把密码写进 /etc/fstab 这类共享文件,并要求「妥善保护」凭据文件 | mount.cifs(8) https://man7.org/linux/man-pages/man8/mount.cifs.8.html |
| 密码里有逗号 | 命令行选项写法不能解析含逗号的密码;通过凭据文件、PASSWD 环境变量或交互提示则可以 | 同上 |
| `vers=` | 不写时为 `vers=default`,协商双方都支持的最高 SMB2+ 版本;内核 4.13.5 起协商结果至少为 2.1;默认不请求 SMB1;手册建议 2.1 及以上(含 3.1.1)。显式写 `vers=3.0` 或 `3.1.1` 是工程取舍,不是手册要求 | 同上 |
| `uid` / `gid` | 不写则为 0(root);接受数字或名字(名字需 mount.cifs 1.10+);`forceuid`/`forcegid` 使客户端忽略服务器提供的属主 | 同上 |
| `file_mode` / `dir_mode` | 只在服务器**不支持** CIFS Unix 扩展时生效(群晖是否属于此情形:未核实);手册未给默认值 | 同上 |
| `noperm` | 关闭客户端权限检查,本机其他用户可能看到挂载内容;服务器端的访问控制仍然生效。一般不建议开 | 同上 |
| `soft` / `hard` | 手册写 `soft` 为默认:服务器崩溃时程序得到错误而不是挂死;`hard` 则挂起。近年内核实际语义是否与手册措辞完全一致:未核实 | 同上 |
| `echo_interval` | 默认 60 秒(范围 1 到 600);对无响应服务器的重连发生在该值的三倍时间处 | 同上 |
| 软件包 | mount.cifs 属于 cifs-utils;内核需有 cifs 文件系统支持。各发行版包名与安装命令:未核实 | 同上 |

### 1.2 网络未就绪时的开机行为,以及 automount 是否更合适

- `_netdev`:把挂载标记为「需要网络」,排序位置在 `remote-fs-pre.target` 与 `remote-fs.target` 之间,而不是本地文件系统目标;会拉起 `network-online.target`。(systemd.mount(5) https://www.freedesktop.org/software/systemd/man/latest/systemd.mount.html)
- 网络文件系统类型(含 cifs)本来就会被 systemd 自动识别为网络挂载,获得对 `network-online.target` 的 `After=`/`Wants=`;`_netdev` 是强制标记。若写了 `nofail`、`x-systemd.wanted-by=` 或 `x-systemd.required-by=`,这些默认依赖会被跳过(同上文档)。含义:**同时写 `nofail` 时,systemd 不再自动保证网络已就绪才挂载**,需要时要显式补 `x-systemd.requires=` 或依赖 automount。此推论由上述文档字句推出,真机行为未验证。
- `nofail`:该挂载只被 `local-fs.target`/`remote-fs.target`「想要」而非「必需」,也不排在它们之前,所以挂载失败不会阻塞或失败整个开机。(同上)
- `x-systemd.automount`:为该挂载生成 automount 单元,首次访问时才挂载;此时 `auto`/`noauto` 不再起作用,automount 单元作为依赖加入相应目标。(同上)
- `x-systemd.mount-timeout=`:systemd 等待 mount 命令的时长,只在 /etc/fstab 里有效。(同上)
- 结论:对「NAS 可能晚于服务器启动、或可能长期不在线」的场景,`_netdev,nofail,x-systemd.automount` 组合更稳:开机不依赖 NAS,首次访问时才挂载。代价:挂载点在首次访问前只是自动挂载点,探测「是否已挂载」要先触发访问。此为工程判断,不是文档原话。

### 1.3 非 root 但有 sudo

- 修改 `/etc/fstab`、写 `/etc/...` 下的凭据文件、执行 `mount`、`systemctl daemon-reload` 都需要 root。有免密 sudo 时,经 SSH 执行 `sudo` 命令即可;凭据文件应以 `sudo install -m 0600 -o root -g root` 之类方式创建,避免先以宽松权限落盘再 chmod。需要 sudo 免密的命令范围、`requiretty` 配置:未核实,要在目标发行版上测。
- 修改 fstab 后需 `sudo systemctl daemon-reload` 让 systemd 重新生成 mount/automount 单元(fstab 生成器行为,本次未抓取其手册,未核实)。
- 密码不要经命令行参数传递(会出现在进程列表);应通过标准输入写入凭据文件。此条为推断。

### 1.4 重连表现

内核 cifs 客户端在连接断开后会自动重新协商、重建会话与树连接;已打开文件在后续读写时才逐个重新打开。(内核源码 fs/smb/client/cifssmb.c `cifs_reconnect_tcon`,https://github.com/torvalds/linux/blob/0d839570/fs/smb/client/cifssmb.c)重连期间 I/O 的阻塞或报错取决于 `soft`/`hard`。

## 2. macOS:开机自动挂载 SMB

### 2.1 机制比较

| 机制 | 无人登录是否生效 | 凭据 | 说明 / 来源 |
|---|---|---|---|
| autofs:`/etc/auto_master` 加 `/- auto_smb`,`/etc/auto_smb` 写 `<挂载点> -fstype=smbfs,soft ://user:pass@host/share` | 推断为是(autofs 由系统守护进程 automountd 处理,不依赖登录会话);无人登录时认证是否成功:未核实,需真机验证 | 密码写在 map 的 URL 里,需百分号转义特殊字符;文件应设 `root:wheel 0600` | auto_master(5) https://keith.github.io/xcode-man-pages/auto_master.5.html 说明 map 格式、`-fstype=smb` 与 SMB URL 写法;Apple 开源 autofs 的默认 auto_master 含 `/- -static` https://github.com/apple-oss-distributions/autofs/blob/main/files/auto_master 。含密码的完整示例来自社区文章(二手):https://www.antoinemenard.com/articles/automatically-mount-smb-shares-macos/ |
| LaunchDaemon 开机执行 `mount -t smbfs //user:pass@host/share <挂载点>` | 推断为是(LaunchDaemon 以 root 在系统域运行,不需要登录);plist 写法与权限本次未抓取一手来源,未核实 | URL 里写密码,或 `nsmb.conf` | mount_smbfs(8) https://keith.github.io/xcode-man-pages/mount_smbfs.8.html 。手册写明「应始终使用系统 mount 命令,不要直接调用 mount_smbfs」 |
| `~/Library/Preferences/nsmb.conf` 存密码 | 取决于调用者 | 明文存储,不是钥匙串 | mount_smbfs(8):`-N` 时读 nsmb.conf 取附加参数与密码 |
| LaunchAgent / 登录项挂载 | 否(用户登录后才触发) | 可借助钥匙串(Finder 保存的凭据) | 未找到一手来源说明「命令行 mount 能否读取钥匙串」;手册只提到 URL 与 nsmb.conf,未核实 |

结论:**需要无人登录也生效,只能选 autofs 或 LaunchDaemon,二者都要把密码放进只有 root 可读的文件**;钥匙串路线依赖用户登录会话,不适合无人值守。

### 2.2 限制与坑

- autofs 默认空闲 10 分钟(`AUTOMOUNT_TIMEOUT`,默认 600 秒)后卸载未被访问的自动挂载,可在 `autofs.conf` 调整。(autofs.conf(5) 旧版镜像:https://spec-zone.ru/RU/OSX/documentation/Darwin/Reference/ManPages/man5/autofs.conf.5.html ;近年 macOS 是否仍同一默认值:未核实)
- 自动挂载默认带 `nobrowse`(Finder 不显示),由 `AUTOMOUNTD_NOBROWSE` 控制。(同上;二手:https://apple.stackexchange.com/questions/140748 )
- 社区报告:macOS 更新可能覆盖 `/etc/auto_master`,导致 `/- auto_smb` 行丢失,需升级后检查。(二手:https://www.antoinemenard.com/articles/automatically-mount-smb-shares-macos/ ,Apple 无官方说明)
- 社区报告:重启后 autofs 挂载可能以 root 属主出现,普通用户无权限访问。(二手:https://discussions.apple.com/thread/4927134 ,旧系统帖,新版本是否仍存在:未核实)
- macOS 近几个版本对磁盘访问、系统卷、`/etc` 下 autofs 文件的限制(完全磁盘访问、System Integrity Protection):本次没有读到一手来源,未核实,必须在目标 macOS 版本上实测。
- 群晖特有:macOS 15 与群晖 SMB 在高并发读写下可能死锁、连接断开;群晖给出的缓解是最大 SMB 协议降到 SMB2,或在 DSM 7.4 及以上启用「异步读取」,或客户端 `nsmb.conf` 设 `signing_required=yes`。(Synology 知识中心 https://kb.synology.com/en-us/DSM/tutorial/smb_macos15_finder_hang )

## 3. Windows:凭据与 UNC / 盘符可见性

### 3.1 `cmdkey` 做了什么

`cmdkey /add:<目标> /user:<用户> /pass:<密码>` 在凭据管理器中保存「目标主机名到用户名密码」的映射;`/list`、`/delete:<目标>` 查看与删除。命令文档没有谈会话范围或限制。(https://learn.microsoft.com/en-us/windows-server/administration/windows-commands/cmdkey )

底层是凭据管理器的 `CREDENTIAL`:类型 `CRED_TYPE_DOMAIN_PASSWORD` 会被 NTLM/Kerberos/Negotiate 在连接对应目标时自动使用;`Persist` 取值:

- `CRED_PERSIST_SESSION`:仅存在于该登录会话,同一用户的其他登录会话看不到,登出即消失。
- `CRED_PERSIST_LOCAL_MACHINE`:本机后续所有登录会话可见(同一用户)。
- `CRED_PERSIST_ENTERPRISE`:本机和其他电脑上的该用户登录会话可见。

(https://learn.microsoft.com/en-us/windows/win32/api/wincred/ns-wincred-credentiala )

### 3.2 OpenSSH 会话下 `cmdkey` 是否可用:核实结果

**不可用,是已知并由设计决定的行为。**

- 微软文档:密钥认证打开的远程会话「没有关联的用户凭据,因此不能以该用户身份做出站认证,这是设计如此」。(https://learn.microsoft.com/en-us/windows-server/administration/openssh/openssh_keymanagement ;Win32-OpenSSH wiki https://github.com/PowerShell/Win32-OpenSSH/wiki/SSH-remote-sessions-on-Windows 同旨:密码认证的会话有凭据可二跳,密钥认证的没有。)
- 现象:`cmdkey /add` 报「Credentials cannot be saved from this logon session」。(Win32-OpenSSH issue #1950 https://github.com/PowerShell/Win32-OpenSSH/issues/1950 、#996 https://github.com/PowerShell/Win32-OpenSSH/issues/996 )
- 原因(#996 中贡献者的说明):网络登录或网络明文登录类型下 `CredWrite` 返回 `ERROR_NO_SUCH_LOGON_SESSION`,其文档措辞为「网络登录会话没有关联的凭据集」;交互、远程交互、批处理登录可写。CredWrite 的 Win32 文档页本次未直接抓取,此条为二手转述,较可信但未直接核实。
- 登录类型细节(个人整理,非微软官方):公钥认证为网络登录(S4U),密码认证为网络明文登录,二者都没有凭据集;没有 sshd 配置项可以更改。来源:https://gist.github.com/YoraiLevi/995c7558842d263585a0a6aa3bff5a8a (可信度中,与上面的官方文档不矛盾)。
- 例外:#1950 评论称,若 sshd 服务本身以真实域/本地账号(而非默认的本地系统账号)运行,则 cmdkey 可用。仅为评论,未核实。

### 3.3 可见性矩阵(依据 3.1、3.2)

| 位置 | cmdkey 写入 | 其他会话是否可见 |
|---|---|---|
| 桌面交互会话(同一用户)写入,持久化到本机 | 可写 | 同一用户的其他登录会话可见,但密钥认证 OpenSSH 会话是否能使用该条目:无凭据集、DPAPI 无法解锁(二手综述的说法),按此推断**不能**,需真机验证 |
| 密钥认证 OpenSSH 会话 | 失败 | 不适用 |
| 密码认证 OpenSSH 会话 | 同样失败(网络明文登录仍无凭据集) | 不适用 |
| 以其他账号运行的服务 / 计划任务 | 服务登录与批处理登录有凭据集,能写;条目属于该账号自己的凭据集 | 凭据按账号隔离,不会在不同账号之间共享 |

### 3.4 `net use` 盘符的作用范围

- Win32-OpenSSH issue #139(维护者回复):映射的网络盘与登录会话关联;SSH 远程会话带有独立的网络登录会话,所以看不到桌面会话里映射的盘。密码登录时可在会话内 `net use t: \\host\share` 映射;密钥登录或需要不同账号时可用 `net use t: \\host\share /user:... *` 手输密码。(https://github.com/PowerShell/Win32-OpenSSH/issues/139 )
- 同 issue 的另一位参与者(社区经验):盘符只在创建它的桌面会话有效,服务会话看不到;持久映射由 explorer.exe 恢复,脱离 explorer 不一定重连;提权进程看不到非提权进程的映射;可以用 UNC 或符号链接代替盘符。
- `net use /persistent` 的官方文档本次抓取失败(页面不可达),持久映射的存储位置与重连机制:未核实,仅有二手说明「写入 HKCU\Network,在下次交互登录时恢复,而不是在 SSH 会话」(上述个人综述)。
- 对本项目的含义:经 SSH 通道映射的盘符,别的会话(桌面、服务)看不到;若目标是让其他进程能读写共享存储,盘符这条路几乎不可行。

### 3.5 可行替代(均为二手来源:#996 贡献者与个人综述)

- 在 SSH 会话内用显式凭据直连 UNC:`net use \\host\share /user:... <密码>`,仅对该会话有效,会话结束即失效。网络登录能用显式凭据建立新的 SMB 会话,但无法复用已存凭据。
- 以账号+密码注册计划任务(「无论用户是否登录都运行」),任务以批处理登录运行,可在其上下文里 `cmdkey` 或 `net use`。
- 编程手段:`LogonUser` + `CreateProcessAsUser`。
- 哪种适合「密码不经过 AI agent」的要求,留给后续设计决定;本调研不下结论。

## 4. 群晖 DSM:SMB 限制

### 4.1 并发连接上限

- DS918+ 数据表:最大并发 SMB/NFS/AFP/FTP 连接 1,000。(https://global.download.synology.com/download/Document/Hardware/DataSheet/DiskStation/18-year/DS918+/enu/Synology_DS918_Plus_Data_Sheet_enu.pdf )日文规格表另写「通过 RAM 扩展可到 2,000」(https://global.download.synology.com/download/Document/Hardware/ProductSpec/DiskStation/18-year/DS918+/jpn/Product_Spec_DS918+_jpn.pdf ,表格抽取不完整,以原 PDF 为准)。
- DSM 软件规格页:最多 10,000 个并发 SMB 连接,「容量因型号而异」,该上限与 AFP、FTP 共享。(https://www.synology.com/en-us/dsm/7.4/software_spec/file_services_protocols )
- 其他型号:到各型号规格页逐一查,本次只核实了 DS918+。

### 4.2 按账号的同时登录数

- 规格页与 SMB 帮助页均**没有**列出按账号的同时登录数限制。「没有找到」不等于「不存在」,标为未发现一手来源。
- 相关开关「禁止同一 IP 地址的多个连接」:启用后每个客户端 IP 只允许一个 SMB 连接,同 IP 的新连接会替换旧连接;NAT 环境可能受影响。(https://kb.synology.com/en-global/DSM/help/SMBService/smbservice_smb_settings?version=7 )含义:同一台服务器(同一 IP)上的多个挂载,若该开关启用会互相踢掉。落实现前应在目标 NAS 上确认该开关状态。
- 登录失败自动封锁:SMB 客户端会自动重试登录,所以自动封锁对前几次失败宽容,且只封锁公网 IP。(https://kb.synology.com/en-us/DSM/help/DSM/AdminCenter/connection_security_protection?version=7 )

### 4.3 空闲会话断开

- 规格页写有「支持超时设置以断开空闲用户」,位于 Protocol & Security 一节,上下文是 FTP 等协议,不是 SMB 专项;**没有找到** DSM 界面上的 SMB 空闲超时设置。(规格页同上)
- 2009 年社区旧帖提到在 smb.conf 手写 `deadtime`(非官方,新版 DSM 是否生效:未核实):https://community.synology.com/enu/forum/17/post/14836
- 协议层:SMB2 规范要求客户端定期扫描连接,无打开句柄且无操作超时的连接由客户端断开,超时值由实现决定。(MS-SMB2 https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-smb2/8de88c48-3e38-428f-b7e3-46af91c6ae24 )
- 第三方文章称 DSM 有 SMB 空闲时间设置,未见于官方帮助页,不采信。

### 4.4 客户端自动重连

- 群晖提供「SMB durable handles」:允许 SMB2 连接短暂断开后恢复,期间打开的文件保持可用,代价是关闭跨协议文件锁。(SMB 设置帮助页同上)
- Linux cifs 客户端会自动重连(见 1.4)。macOS 与 Windows 客户端对群晖的重连细节:未找到一手来源,未核实。
- 近期已知问题:macOS 15 与群晖在高负载下可能断连(见 2.2)。

## 5. 无副作用探测目标 445 端口

目标:只做 TCP 三次握手,不发 SMB 报文,不认证。

| 系统 | 命令 | 说明 |
|---|---|---|
| Linux | `nc -z -w 3 nas.example.com 445; echo $?`;无 nc 时 bash 内建:`timeout 3 bash -c '</dev/tcp/nas.example.com/445'; echo $?` | `-z` 只扫描不发数据,`-w` 超时。`/dev/tcp` 是 bash 特性,不是 POSIX。两条命令的手册本次未抓取,未核实;nc 有多个实现,参数略有差异 |
| macOS | `nc -z -G 3 nas.example.com 445; echo $?` | macOS 自带 BSD nc,用 `-G` 设连接超时(与 Linux 的 `-w` 不同);手册本次未抓取,未核实 |
| Windows(PowerShell) | `Test-NetConnection -ComputerName nas.example.com -Port 445 -InformationLevel Quiet` | 返回布尔值;指定 `-Port` 时做 TCP 连接测试(https://learn.microsoft.com/en-us/powershell/module/nettcpip/test-netconnection )。OpenSSH 默认 shell 为 cmd 时需包一层 `powershell -NoProfile -Command ...`;`Quiet` 的返回细节本次未逐字核实 |

补充:在受管服务器上经 SSH 执行,得到的是「该服务器视角」能否连到 NAS,这正是需要的。只探 445 不能说明账号与共享名有效。

## 6. 未解决、需真机验证的问题清单

1. Linux:各目标发行版的 cifs-utils 包名与 sudo 免密命令范围;`nofail` 与 `_netdev` 并用时的真实开机顺序;群晖上 `file_mode`/`dir_mode` 是否生效。
2. macOS:无人登录时 autofs 的 SMB 认证是否成功;近期版本对 `/etc` 下 autofs 文件的保护;LaunchDaemon 方案的具体写法与权限。
3. Windows:`net use /persistent` 官方说明;服务账号 / 计划任务方案下凭据的实际可见性;OpenSSH 默认 shell 对命令引用的影响。
4. 群晖:目标型号的并发上限;「同一 IP 单连接」开关状态;是否存在官方的 SMB 空闲超时设置。
5. 探测命令:Linux / macOS 的 nc 与 `/dev/tcp` 手册核实。
