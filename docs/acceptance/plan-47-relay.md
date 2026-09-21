# Plan 47 验收册:relay_file 大文件中继(v0.14.0 起)

> 来源:backlog「大文件分块续传 API」销项条 + spec(`docs/superpowers/specs/2026-09-06-plan-47-relay*`);术语见 CONTEXT.md「Relay/Chunk/Manifest/Partial File/Transfer Task」。
> 环境:A=阿里云目标机、B=局域网目标机(均经 SSH 工具面可达);broker=NUC10;发起=本机 MCP 工具面。

## 前置

- [ ] 双端 ≥ v0.14.0 且 compat-matrix 已登行
- [ ] A、B 两台目标机上各有充足磁盘;源文件用可再生的随机/合成数据(不碰真实数据)

## R1 A↔B 1–5GB 级实测【agent 可跑】

- **步骤**:
  1. A 上 `head -c 3G /dev/urandom > /tmp/relay-acc.src` 并记 `sha256sum`(Linux 目标);Windows 目标用等价 PowerShell 生成;
  2. 本机经 MCP 工具 `relay_file`(from=A, to=B, 目标路径 `/tmp/relay-acc.dst`);
  3. `exec_output` 轮询 task_id 至完成,留进度轨迹;
  4. B 上 `sha256sum /tmp/relay-acc.dst` 对照。
- **判据**:完成报 `file_sha256` 与 B 侧 sha256sum 一致(byte0→EOF 全新流)。
  - **2026-09-21 批2 实测**:过(1GB 档,4 块 42m22s,WAN 实测 412–423 KiB/s 为瓶颈;3GB 按此速率需 ~2h 超窗,按本节「1–5GB 级」下界缩档并如实登记)。**拓扑订正**:中继任务宿主 = 运行 MCP 进程的机器(本会话为笔记本缓存客户端,空 `from_server_id` 指**本机盘**而非 serve 机)——判定实验与数据路径详见[运行记录](./runs/2026-09-21-batch2-plan47-relay.md)。
- **清理**:两侧 `/tmp` 文件删除。

## R2 kill broker 中断续传演练【agent 可跑;需 owner 放行窗口——重启 serve 会短暂断供】

- **步骤**:
  1. 发起 R1 同款大文件 relay;
  2. 传输中途(chunk 进行中)在 NUC10 停止 serve 服务(等价 kill broker);
  3. NUC10 重启 serve(HEALTHY 确认);
  4. 重新发起同参数 relay_file——应从接收端 Manifest 续传只补缺失块;
  5. B 侧 sha256sum 对照。
- **判据**:续传完成的报文为**块 merkle 根**形态(resumed);补块后文件 sha256sum 与源一致;接收端无 `.sshmgr-partial` 残留(真名 rename 完成)。
  - **2026-09-21 批2 实测订正**:过,但按实际拓扑拆两变体——**R2b 进程击杀**(独立子进程发起中继、块 1/8 完成后按进程句柄击杀)= 真中断,重发应答 `resumed_chunks:1` 续传咬合、清单带逐块摘要;**R2a serve 重启**(本节原文形态,owner 窗口已确认)实证与在飞中继**完全无关**(中继走两端 sshd 通道,serve 停起期间块节奏 30 秒一拍不变)——「kill broker=中断传输」的前提在缓存客户端拓扑下不成立。续传段吞吐单次观测 ~0.3 MiB/s(同链路全新跑 8.8–10.2 MiB/s,~30 倍劣化,机理未查)已登记 backlog;文件完整性以全新全量跑收口。
- **注意**:步骤 2-3 会中断 serve 服务(所有 client 的在线工具面与拉取),跑前与 owner 约窗口。

## R3 Windows 目标特性实测【agent 可跑】

- **步骤**:对 Windows 目标(若库中有;含 Win32-OpenSSH)各跑一次小文件 relay,观察:
  - `StatVFS unavailable` 降级路径(空间预检不可用时的行为——按工具说明 space_check='unavailable' 时继续安全);
  - `posix-rename` 支持(Win32-OpenSSH 应原生支持,启动即 failed 才是异常)。
- **判据**:Windows 目标 relay 成功且摘要对上;无 panic/挂死。
  - **2026-09-21 批2 实测订正**:过;另两处实测——① 路径校验要求 Windows 目标 `to_path` 用 `C:/` 正斜杠盘根形态(反斜杠形态被拒且报错文案明确);② 本机 1660Super01 的 Win32-OpenSSH **实装 `statvfs@openssh.com`**,`space_check` 报 ok **不走降级路径**(R4 真 Win32 端点用例同结论)——本节「StatVFS unavailable 降级」分支在实装该扩展的真机上无从触发,由 mcpserver 套件 Windows lane 行为学覆盖(套件头注)。

## R4 conformance docker 真线【agent 可跑:本机/CI】

- **步骤**:跑 conformance 套件中 relay 相关用例(docker 真 OpenSSH 目标,门内实跑)。
- **判据**:门绿;SKIP 只允许出现在明确登记的正常出口。

## 附加:broker 本机盘源路径

- **步骤**:`relay_file` from 留空(broker 本机盘 → 服务器)小文件一次。
- **判据**:成功且摘要对上(Plan 44「部署 exe 分发通道债」解除的通用路径)。
