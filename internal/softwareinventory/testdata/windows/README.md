# Windows 采集夹具（固定命令集 windows-v1 与 windows-v2）

`../../windows-v1.ps1` 与 `../../windows-v2.ps1` 是 Windows 固定命令集原文（PowerShell 5.1 可用，不含 `Win32_Product`）。当前发送的是第 2 版；第 1 版文件保留供对照，旧版结果仍按第 1 版解析。录制方式：脚本按 UTF-16LE 做 Base64，运行 `powershell -NoProfile -NonInteractive -EncodedCommand <编码>`；cmd 条目与 PowerShell 条目都用这一种包装（PowerShell 条目也可直接运行）。脚本开头的 `[Console]::OutputEncoding=UTF8` 与 `$ProgressPreference='SilentlyContinue'` 必须保留：少了后者，输出里会混入 `#< CLIXML` 进度记录。编码后长度约 5 KB，低于 cmd 的 8191 字符命令行上限，修改脚本时注意别超。

由主会话在 2026-11-09 用 sshmgr 录得。处理：把站点地址换成文档地址，把用户名换成 `user`。**输出是 CRLF 换行，解析器必须容忍行尾 `\r`。**

| 文件 | 来源 | 用途 |
|---|---|---|
| `win10-powershell-full.txt` | Windows 10 Pro 22H2，管理员，默认 shell 为 PowerShell 的条目 | 第 1 版完整样例，软件多 |
| `win10-cmd-full.txt` | 注册表报 Windows 10 Pro / 构建号 22621，管理员，默认 shell 为 cmd | 第 1 版完整（注意 ProductName 与构建号不一致是真实情况，原样保存） |
| `win10-cmd-no-process-names-derived.txt` | 由另一台管理员机器的真实输出派生：processes 一节里非本用户目录下的进程把名字和路径清空 | 权限不足。没有找到真实的非管理员 Windows 条目，测试里注释「派生夹具，待替换为真实输出」 |
| `win10-cmd-truncated.txt` | 由 `win10-cmd-full.txt` 在 listen-udp 一节中途截断 | 某节被截断 |
| `win10-powershell-full-v2-derived.txt` | 由 `win10-powershell-full.txt` 派生，三处变换：开头标记版本改 `windows-v2`；os 节 `arch` 行前插入 `CurrentMajorVersionNumber	10` 与 `CurrentMinorVersionNumber	0` 两键；processes 节 45 行各追加文件版本与产品名两列（按各软件公开版本构造的近似值） | 第 2 版解析。派生夹具，待替换为真实输出 |

各节格式（制表符分隔）：
- `os`：`键<TAB>值`，键为 ProductName、DisplayVersion、ReleaseId、CurrentBuild、UBR、EditionID、InstallationType、CurrentMajorVersionNumber、CurrentMinorVersionNumber、arch。两个版本号键是第 2 版新增（Windows 10 起注册表才有；老系统输出空值行，解析器回退第 1 版口径）。
- `installed`：`64|32<TAB>显示名<TAB>版本<TAB>发布者<TAB>注册表键名`。
- `listen-tcp` / `listen-udp`：`本地地址<TAB>端口<TAB>进程编号`；`::` 与 `0.0.0.0` 为全零地址。
- `processes`：第 1 版为 `进程编号<TAB>进程名<TAB>可执行文件路径`（路径或名字可能为空）；第 2 版追加两列成 `进程编号<TAB>进程名<TAB>可执行文件路径<TAB>文件版本<TAB>产品名`（第 4、5 列是可执行文件自带的版本资源，读不到时为空）。
- `services`：`进程编号<TAB>服务名<TAB>服务命令行`。
- `firmware`：与 Linux 同名的键。
- `gpu-driver`：`显卡名<TAB>驱动版本<TAB>驱动日期`。
- 节内出错时输出 `error<TAB>消息` 并以 `STATUS <节名> 1` 结束。
