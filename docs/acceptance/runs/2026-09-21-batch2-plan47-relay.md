# 批2 · Plan 47 relay 真机验收运行记录(2026-09-21,agent 代跑)

> 册子:`docs/acceptance/plan-47-relay.md`。环境:A=阿里云「AI大语言服务器」、B=3090x2(局域网 Linux)、Windows 目标=1660Super01(Win32-OpenSSH)、中继宿主=笔记本(缓存客户端形态,v0.17.1)。
> **拓扑语义实证(本轮首要发现)**:本会话工具面的中继任务宿主是**笔记本进程**(stdio 缓存客户端),不是 NUC10 的 serve——空 `from_server_id` 的「broker 本机盘」= 运行 MCP 进程的那台机(工具面定义「with a stdio MCP that is your machine」兑现)。判定实验:NUC10 上存在 `C:\Users\allan716\relay-probe.txt` 时中继源 stat 报「找不到文件」,笔记本同路径文件 stat 即成功。数据路径 = 源机→笔记本→目标机(两端凭据出自缓存)。

## 附加:broker 本机盘源路径 — 过

- 源:笔记本 `C:\Users\allan716\relay-laptop-marker.txt`(29 字节)→ 3090x2 `/tmp/relay-laptop-marker.txt`。
- 报文:`resumed 0`、单块、`space_check ok`;`relay done: file_sha256=sha256:c84a40fc…(total=29) renamed`。
- 核对:笔记本 sha256 = 中继报文 = 3090x2 `sha256sum` 三方一致 `c84a40fcc2973b36dcf626d2a6fcb44cc31ee46dac696f38d95d5d88e9a91050`。

## R1 A↔B 千兆字节级实测 — 过(1GB 档,实测 42 分 22 秒)

- 源:阿里云 `/tmp/relay-acc.src`(`head -c 1G /dev/urandom`,sha256 `4ba441f9e098a0dcf19185fd63f32684b541a5637af5c9758950eee923b27907`)→ 3090x2 `/tmp/relay-acc.dst`。
- 进度轨迹(经 MCP 工具面 `exec_output` 轮询):4 块,每块节奏 `423.0/422.8/422.3/412.5 KiB/s`,总 42m22.31s——瓶颈为阿里云↔家的公网带宽(实测另一窗口一度降至 ~110KiB/s,WAN 波动属环境,LAN 侧腿 R3/R2b 实测 8–14 MiB/s)。
- 册子判据兑现:完成报文 `file_sha256=sha256:4ba441f9…`(byte0→EOF 全新流形态)与 B 侧 `sha256sum` 一致;真名落盘零残留。
- **缩档如实登记**:册子建议 3GB;首轮 3GB 起跑后按实测 419KiB/s 折算需 ~2 小时,超出本窗口,按册子「1–5GB 级」下界改 1GB(4 块,多块语义全在),3GB 任务已显式 `exec_stop` 并清理分块。
- 清理:阿里云源文件、3090x2 落盘均已删除。

## R2 kill broker 中断续传演练 — 过(两个变体,形态按实际拓扑重述)

- **R2b 进程击杀(真正的中断,判据核心)**:驱动脚本另起独立 MCP 子进程发起 NUC10→3090x2 2GB 中继(8 块,`fsutil` 零填充源,sha256 `a7c744c1…`);块 1/8 完成(29.3s,8.8 MiB/s)后**按进程句柄击杀子进程**(绝不按镜像名——会误杀会话进程);接收端留 `manifest.json`(块 0 带逐块摘要 `a6d72ac7…`)+ `.sshmgr-partial` 369,950,720 字节;重发同参数中继,应答 **`resumed_chunks:1`**——续传机制咬合实证。
- **R2a serve 重启(册子原文形态,owner 已确认窗口)**:2GB 中继在飞第 62–70 秒(块 2→3 之间)于 NUC10 `net stop/start sshmgr-serve`(停→起→HEALTHY);**块节奏 30 秒一拍全程未变**(块 2 @1m1.6s、块 3 @1m31.5s、…、块 8 @4m2.9s,8.4 MiB/s 匀速),落盘 `file_sha256=a7c744c1…` 三方一致——**serve 重启与在飞中继完全无关**(中继走两端 sshd 通道,不经 serve),册子「kill broker=中断传输」的前提在缓存客户端拓扑下不成立,已按实际拓扑重述为进程击杀变体完成判据。
- **遗留发现(登记 backlog,不放宽判据)**:续传段吞吐单次观测 ~0.3 MiB/s,同链路同负载下全新跑 8.8–10.2 MiB/s,**~30 倍劣化,机理未查**(单次观测未复现;当轮环境有多次进程击杀残留,不排除干扰)。本轮文件完整性以全新全量跑收口:8/8 块、3m25s、摘要三方一致、零残留。
- 清理:3090x2 落盘与分块/清单、NUC10 源文件均已删除。

## R3 Windows 目标特性实测 — 过(附如实注记)

- 笔记本 0.17.1 二进制(18,501,120 字节)→ 1660Super01 `C:/Users/US/relay-win-test.exe`;路径校验提示 Windows 目标需 `C:/` 正斜杠盘根形态(反斜杠形态被拒,报错文案明确)。
- 报文:`space_check:"ok"`、单块 13.9 MiB/s、`renamed -> C:/Users/US/relay-win-test.exe`;1660Super01 `certutil -hashfile` = `f3f706b9…` 与源一致;**无 `.sshmgr-partial` 残留**(posix-rename 收尾成功)。
- 如实注记:该机 Win32-OpenSSH **实装了 `statvfs@openssh.com`**,空间预检未走降级路径(见 R4 真端点用例同结论);「StatVFS unavailable→照建安全继续」分支在真机上无从触发,由 mcpserver 套件 Windows lane 行为学覆盖(套件头注原话)。册子预期「Windows 目标必现 unavailable」与实际不符,已订正认知。

## R4 conformance docker 真线 — 过(三用例含真 Win32 端点)

- `SSHMGR_CONFORMANCE=1 go test ./internal/conformance/ -run Relay`:docker 27.5.1 本机真线,`TestRelayDualServerRealSSH` PASS(13.25s)、`TestRelayResumeDrillRealSSH` PASS(9.67s)。
- `TestRelayWin32OpenSSHRealTarget` 初跑按设计跳过;**一次性靶子补真端点后 PASS**:1660Super01 当端点(192.168.100.146:22),一次性 ed25519 密钥装于 `C:\ProgramData\ssh\administrators_authorized_keys`(该用户属管理员组,sshd_config 的 Match 覆盖使家目录 authorized_keys 不生效——Win32-OpenSSH 经典行为,已实证);用例输出「StatVFS on the real Win32 target: available (this build implements statvfs@openssh.com)」与 R3 观察互证。
- 一次性材料清理:管理员键文件还原为 owner 原键单行、用户级 authorized_keys 清空(该文件本轮所建)、本地密钥对删除——三处均留验证输出。
- 过程事故如实留档:追加公钥时因原文件末行无换行符发生粘连(两键挤成一行,owner 原键一度失效),当即以 PowerShell 按行重写修复;教训=改键文件前先查末行换行。

## 判定总表

| 项 | 判定 | 一句话 |
|---|---|---|
| 附加(broker 本机盘源) | **过** | 笔记本源→3090x2,摘要三方一致 |
| R1 双机 GB 级 | **过(1GB 档)** | 4 块 42m22s,`file_sha256` 与 B 侧一致;WAN 瓶颈 412–423 KiB/s 如实记 |
| R2 中断续传 | **过(按实际拓扑重述)** | R2b 进程击杀→`resumed_chunks:1` 咬合;R2a serve 重启实证与传输无关;续传吞吐劣化 30 倍发现登记 backlog |
| R3 Windows 目标 | **过** | 摘要一致、posix-rename 零残留;statvfs 实装不走降级(如实订正) |
| R4 conformance 真线 | **过** | docker 双用例 PASS + 真 Win32 端点 PASS(一次性密钥进出,三处清理留证) |
