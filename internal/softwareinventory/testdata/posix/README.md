# POSIX 通用采集夹具（固定命令集 posix-v2）

`../posix-v2.sh` 是所有 POSIX 服务器条目共用的固定命令集：系统类型由第一节 `os` 的内容决定（`kernel-name=Darwin` → macOS；有 `file=/etc.defaults/VERSION` → 群晖 DSM；`file=/etc/openwrt_release` 或 os-release 的 `ID=istoreos`/`ID_LIKE` 含 openwrt → iStoreOS / OpenWrt；os-release 的 `ID` 或 `ID_LIKE` 含 debian/ubuntu → Ubuntu/Debian；其余 Linux → 只采系统版本与监听端口，例如 ARM 板）。它是旧版 Linux 固定命令集的超集：Linux 各节的名字与格式不变，只新增节；不适用的节输出 `ABSENT`。

由主会话在 2026-11-09 录得。处理：去掉行尾空白；把站点内网地址换成文档地址（192.0.2.x、198.51.100.x、203.0.113.x、10.0.65.x、100.64.x）；把用户名换成 `user`。其余为原样输出。

| 文件 | 来源 | 用途 |
|---|---|---|
| `macos-full.txt` | macOS 27（Apple 芯片），root（经 sudo） | 完整：lsof 看得到全部监听进程；apps、brew、容器（colima）均有 |
| `macos-no-process-names.txt` | 同一台，普通用户 | 权限不足：lsof 只列出本用户自己的进程 |
| `macos-truncated.txt` | 由完整夹具在 apps 一节中途截断 | 某节被截断 |
| `dsm7-full.txt` | 群晖 DS918+，DSM 7.1，root（经 sudo） | 完整：netstat 带进程编号/程序名、套件、容器 |
| `dsm6-no-process-names.txt` | 群晖 DS918+，DSM 6.2，普通用户 | 权限不足：netstat 的进程列全是 `-`；docker 被拒（STATUS 1） |
| `dsm7-truncated.txt` | 由完整夹具在 container-images 一节中途截断 | 某节被截断 |
| `istoreos-full.txt` | iStoreOS 24.10.5（x86），root | busybox netstat；只用 os 与 listen-netstat，其余节（容器、固件）解析时忽略，结果不标为不完整 |
| `istoreos-truncated.txt` | 由完整夹具在 listen-netstat 一节中途截断 | 某节被截断 |
| `ubuntu2404-full.txt` | Ubuntu 24.04（4090x2），root | 完整：ss 带进程名、dpkg 包、exe-owner、容器、固件、显卡驱动；替换旧版 Linux 命令集的同名 Ubuntu 夹具 |
| `ubuntu2204-no-process-names.txt` | Ubuntu 22.04（3090x2），普通用户 | 权限不足：ss 绝大多数行没有进程名 |
| `ubuntu2404-truncated.txt` | 由完整夹具在 packages 一节中途截断（前 600 行，无 STATUS、无 END） | 某节被截断 |
| `arm-board-handwritten.txt` | 手写最小输出 | 临时覆盖其他 Linux（ARM 板）；待替换为真实输出 |

ARM 板（血球 ARM 板）在录制时 SSH 握手失败，没有真实夹具；上表的手写文件按 iStoreOS 的格式构造，待替换为真实输出。

各节格式：
- `os`：`kernel-name=` / `kernel=` / `machine=` 三行，之后每个找到的系统版本来源以 `file=<来源>` 行开头，后跟原文（os-release、openwrt_release、DSM 的 VERSION 都是 `键=值`，macOS 的 sw_vers 是 `键:<TAB>值`）。
- `listen`：`ss -H -tulnp` 原样（与旧版 Linux 命令集相同）。
- `listen-netstat`：`netstat -tulnp` 原样，前两行是表头；非 root 时第一行可能是 `(No info could be read for "-p" ...)`，进程列为 `-`。
- `listen-lsof`：`lsof -F pcnPt` 字段输出：`p<进程编号>`、`c<命令名>`、`f<文件描述符>`、`t<IPv4|IPv6>`、`P<TCP|UDP>`、`n<地址:端口>`；地址 `*` 表示所有地址。
- `exe`：`进程编号<TAB>可执行文件`（Linux 读 /proc，macOS 为 `ps -o comm=`）。
- `apps`：`应用路径<TAB>版本<TAB>包标识`。
- `brew`：`formula|cask<TAB><名字> <版本...>`（`brew list --versions`，可能有多个版本以空格分隔）。root 运行时命令集用控制台用户身份执行 brew 与 docker。
- `synology-packages`：`<套件名>-<版本>: <描述>`；版本是最后两段以 `-` 分隔的部分（例：`Node.js_v12-12.22.12-2031` → 名字 `Node.js_v12`，版本 `12.22.12-2031`）。
- 其余节与旧版 Linux 命令集相同。
