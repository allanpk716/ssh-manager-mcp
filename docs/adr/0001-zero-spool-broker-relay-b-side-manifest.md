# 零暂存 broker 中继,B 端 Manifest 为续传唯一锚

50GB+ 模型权重要经 NUC10 broker 从在线服务器搬到真空离线服务器,我们决定:Relay 全程分块流式(每 Chunk 一条 SFTP 读→SFTP 写管道,broker 盘零用户数据残留、内存占用≈流式缓冲),断点续传的唯一事实源是**落在接收端 B 的 Manifest**(源指纹 size+mtime + 各 Chunk 哈希与完成位;根哈希与全文件 sha256 在完成时推导,不落 Manifest),而不是 broker 侧任何状态——因此 broker 后台任务表保持 Plan 32 的"重启即失"内存态**不做持久化**:任务条目丢了,重跑同目标即凭 Manifest 自动续传。

## Considered Options

- **broker 磁盘暂存中转**(先 download 到 NUC10 再 upload):被否——50GB 明文落盘既占 broker SSD 又违背"vault 加密、broker 盘不留用户数据残留"的哲学,且引入暂存文件清理责任。
- **upload_content 式内联(base64 过 agent 上下文)**:被否——内容零过境是底线,大文件进 agent 上下文是既有反模式哲学(download 1 MiB cap)的延续。
- **broker 侧持久化任务状态**:被否——B 端 Manifest 已覆盖全部自愈需求,持久化任务表是重复的事实源,两处状态必有不一致窗口。

## Consequences

- B 端 `.sshmgr-partial` + Manifest 的盘上格式是**跨版本协议**:升级不得破坏旧 Manifest 的续传可读性。
- 续传前置检查 = 源 size+mtime 对 Manifest + 抽读一个已完成 Chunk 复核哈希(防同尺寸同 mtime 内容已变的静默拼接损坏)。
- "没传完"(stop/盘满/崩溃/弃疗)统一为可续传残留,无 TTL 自动清理,弃疗残留由 owner 手动清。
