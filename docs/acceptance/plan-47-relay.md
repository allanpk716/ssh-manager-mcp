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
- **清理**:两侧 `/tmp` 文件删除。

## R2 kill broker 中断续传演练【agent 可跑;需 owner 放行窗口——重启 serve 会短暂断供】

- **步骤**:
  1. 发起 R1 同款大文件 relay;
  2. 传输中途(chunk 进行中)在 NUC10 停止 serve 服务(等价 kill broker);
  3. NUC10 重启 serve(HEALTHY 确认);
  4. 重新发起同参数 relay_file——应从接收端 Manifest 续传只补缺失块;
  5. B 侧 sha256sum 对照。
- **判据**:续传完成的报文为**块 merkle 根**形态(resumed);补块后文件 sha256sum 与源一致;接收端无 `.sshmgr-partial` 残留(真名 rename 完成)。
- **注意**:步骤 2-3 会中断 serve 服务(所有 client 的在线工具面与拉取),跑前与 owner 约窗口。

## R3 Windows 目标特性实测【agent 可跑】

- **步骤**:对 Windows 目标(若库中有;含 Win32-OpenSSH)各跑一次小文件 relay,观察:
  - `StatVFS unavailable` 降级路径(空间预检不可用时的行为——按工具说明 space_check='unavailable' 时继续安全);
  - `posix-rename` 支持(Win32-OpenSSH 应原生支持,启动即 failed 才是异常)。
- **判据**:Windows 目标 relay 成功且摘要对上;无 panic/挂死。

## R4 conformance docker 真线【agent 可跑:本机/CI】

- **步骤**:跑 conformance 套件中 relay 相关用例(docker 真 OpenSSH 目标,门内实跑)。
- **判据**:门绿;SKIP 只允许出现在明确登记的正常出口。

## 附加:broker 本机盘源路径

- **步骤**:`relay_file` from 留空(broker 本机盘 → 服务器)小文件一次。
- **判据**:成功且摘要对上(Plan 44「部署 exe 分发通道债」解除的通用路径)。
