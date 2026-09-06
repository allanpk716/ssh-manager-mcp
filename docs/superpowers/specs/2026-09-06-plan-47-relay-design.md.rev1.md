# Plan 47 设计:relay_file 大文件中继(服务器↔服务器 / broker 本机→服务器)

> backlog #46 · P0。2026-09-06 grilling 三轮已拍板的决策不在本文重议:**零暂存分块流式**(broker 盘零用户数据残留、内存≈流式缓冲,ADR 0001)、**B 端 Manifest 为续传唯一事实源**(broker 任务表保持内存态不持久化——重启即失、重跑自愈,ADR 0001)、**并入 Plan 32 任务表**(exec_output 轮询进度 / exec_stop 取消)、**72h 常量时长上限**、**v1 单文件**(目录=官方 tar 惯用法)、**MCP 工具先行**(CLI+笔记本→B 通道=批 2 独立 plan,冻约束:复用同一 Chunk/Manifest 协议)、**from_server_id 空=broker 本机盘**(顺带销 upload_file 1 MiB 单文件债)、**双端 profile 闸 + 审计 + 内容零过境**(工具与 exec_output 只回元数据,文件字节绝不进 agent 上下文)、**B→A 外发方向 v1 放开仅审计**、**StatVFS 失败 fail-open**(`space_check:"unavailable"` 知情继续)、**续传前置三查**(size+mtime 对 Manifest + 抽读一个已完成块复核——rev1 起抽读移入引擎 stage 0,见下)、**partial+rename β 语义**(`<target>.sshmgr-partial` 完成才 rename 真名)、**无 TTL 自动清理**(弃疗残留 owner 手动 exec 清,`--fresh` 清零)、**A 端下载归 exec_background 不管**。本文为实现设计。
> 术语以根目录 `CONTEXT.md` 为准(Relay / Chunk / Manifest / Partial File / Transfer Task / Upload / Download)。
> 本版为第二版(2026-09-06 一轮盲评收敛修订:kimi 9 项 + codex 15 项,去重后 14 簇全吸收,4 项残余登记接受)。要点:①**双摘要体系**——file_sha256(本任务全程流式时 TeeReader 顺带算出,零额外 IO)+ merkle 根(恒可算),四处校验故事改用前者、resumed 报后者(根哈希 ≠ 文件 sha256,rev0 让 sha256sum 对根哈希是必败比对——kimi#1/codex#1);②preflight 补 **MkdirAll 父目录 + to_path 目录形态拒绝**(rev0 全新路径必 ENOENT 且 StatVFS 被吞——kimi#2/codex#10);③**空间预检重排**——manifest 判定(含 fresh 删除)先行、口径改 Available 单值 ≥ 缺失字节 + 块余量(rev0 顺序+口径系统性误拒续传——kimi#3/codex#6);④**首块前原子建空 Manifest**(消 partial-only 单边态,"重跑即续传"承诺闭合——codex#7)+ zero-size 显式闭合(kimi#8/codex#8);⑤**任务身份模型**——bgTask 增 relay 元数据、BgTaskSpec 增 AuditAction(Insert 硬编码 exec-bg-end 是现实——kimi#4/codex#2)、runRelay 复用 runTask 终态纪律 + notifyWriter(kimi#5);⑥去重保护面如实收窄 per-project + B 端事实源兜底,**不上跨 project 全局 lease**(kimi#6/codex#3,新机制超纯增量口径,残余登记);⑦preflight 连接移交引擎 + Insert 失败双 close 释放(kimi#7);⑧**源变更防线**——精确长度块读 + 末块 EOF + 完成时 re-stat 不符拒绝提交(codex#4,残余:rsync 同病的同 mtime in-place 重写);⑨**Manifest 解析防御**——stat 大小上限 + 结构校验(codex#5);⑩PosixRename 能力矩阵统一 + **提交语义=最终 rename 成功**(manifest 删除失败仅警告,rev0 会逼 50GB 重传——codex#9);⑪路径判据修正——Windows broker 上 `filepath.IsAbs("/foo")=false`,本机绝对=盘符/UNC;远程 isAbsRemotePath 含盘符形态与 schema 文案对齐(codex#11/kimi#9);⑫serve env seam 接线如实化(懒构造两读点快照口径)+ §1.2 描述补 chunk 值(codex#13);⑬抽读复核移引擎 stage 0(同步 preflight 会被 256MiB 网络读阻塞数分钟——codex#14);⑭测试矩阵补 10 类边界(codex#15)。评审留底:.xcheck/plan47-r1-{kimi,codex}.out(codex 沙箱全拒本地读,以 112KB 内联材料喂入完成评审)。

## 0. 目标与缺口

50GB+ 模型权重场景:在线服务器 A(阿里云)上下载好的大文件,要送到真空离线服务器 B(与 A/本机零网络可达,唯一交汇点是 NUC10 broker)。现状四条通道全部不通:

| 通道 | 断点 |
|---|---|
| `download_file` | 1 MiB 前缀截断——大文件从 A 拿不回 broker |
| `upload_file` | 单文件 1 MiB 硬顶——大文件推不进 B;且 LocalPath=broker 本机盘 |
| `upload_content` | 8 MiB 内联且内容过 agent 上下文——大文件反模式 |
| 服务器间直传 | 不存在;且凭据不出 vault,不可能在 A 上种 B 的凭据 |

`relay_file`:broker 内部分块流式中继,源=远程服务器或 broker 本机盘,目标=远程服务器。断点续传锚在接收端 Manifest。= backlog #46「大文件分块续传 API」销项,同时以本机源路径还掉「部署 exe 分发通道」债的根因(upload_file 1 MiB 单文件上限)。

## 1. 工具契约

`relay_file` = **BrokerTools[11]**(第 12 个工具;`internal/mcpserver/server.go` 单源切片追加 + `NewServerFromSource` 对应 `mcp.AddTool`——eval scorer 读同一切片,集合断言自动联动为 12)。

### 1.1 入参 / 出参

```go
// types.go
type RelayInput struct {
    FromServerID string `json:"from_server_id,omitempty" jsonschema:"source server id from list_servers; OMIT or empty = the broker's own local disk (e.g. moving a large file already on the broker host)"`
    FromPath     string `json:"from_path" jsonschema:"absolute path of the SOURCE file: a POSIX absolute path (/...) or Windows drive root (C:/...) on the from-server; or a broker-host absolute path (drive-letter or UNC on a Windows broker) when from_server_id is empty"`
    ToServerID   string `json:"to_server_id" jsonschema:"destination server id from list_servers"`
    ToPath       string `json:"to_path" jsonschema:"absolute destination path on the destination server (POSIX /... or Windows drive root C:/...); its parent directory is created if missing; an existing file at the path is replaced on completion"`
    Fresh        bool   `json:"fresh,omitempty" jsonschema:"discard any existing partial/manifest at the destination and restart from byte 0 (default: resume missing chunks when the manifest matches)"`
}

type RelayOutput struct {
    TaskID        string `json:"task_id" jsonschema:"background task id — poll progress with exec_output(task_id), stop with exec_stop(task_id); the transfer is resumable: re-run relay_file with the same paths after any failure/stop"`
    BytesTotal    int64  `json:"bytes_total" jsonschema:"total source bytes"`
    ChunksTotal   int    `json:"chunks_total" jsonschema:"total chunk count"`
    ResumedChunks int    `json:"resumed_chunks" jsonschema:"chunks already complete per the destination manifest (0 on a fresh start)"`
    ChunkBytes    int64  `json:"chunk_bytes" jsonschema:"the chunk size in bytes (from SSHMGR_TRANSFER_CHUNK at server construction)"`
    SpaceCheck    string `json:"space_check" jsonschema:"destination free-space pre-flight: 'ok' or 'unavailable' (the destination SFTP server does not support statvfs — e.g. some Windows targets; proceeding is safe: a full disk mid-transfer just stops at a chunk boundary and resumes after space is freed)"`
}
```

- **同步部分只有廉价 preflight**(§2 ①–⑧):全部完成后 Insert、立即返回 `task_id`(exec_background 同款形态)。**抽读复核等一切网络字节移动都在引擎侧**(codex#14:同步段读 256 MiB 会阻塞 MCP 请求数分钟)。
- **空间不足 = refusal 错误**(`IsError` 返回,不建任务):错误文本带 avail/need 证据。`unavailable` 不是错误——fail-open(grilling 拍板)。
- **路径判据(rev1 修正,codex#11/kimi#9)**:
  - **远程源/目标**:`isAbsRemotePath`(core.go:613-620,以 `/` 或盘符 `C:/` 开头——testsshd/Windows 目标一等公民);schema 文案同步提及盘符形态,消除文案/判据分岔。
  - **本机源**:`filepath.IsAbs` 按 **broker 宿主 OS** 语义——Windows broker 上 `C:\…`/UNC 真、`/foo` **假**(rev0"均真"表述错误,codex#11);不做自定义宽判,错就错得诚实。
- **同源同径拒绝**:`from_server_id==to_server_id && from_path==to_path` → 参数层拒绝(no-op 自覆盖)。
- **运行中重复拒绝(保护面如实收窄,rev1)**:扫描**本 TaskManager** 内 running relay 条目(锚键=规范化 to 键,`bgTask` 新增 relay 元数据,§2;路径经 Clean+ToSlash 规范化)。**如实声明**:TaskManager 是 per-project 的(serve.go scopedServer,跨 project 隔离是 Plan 32 结构性设计)——跨 project 同目标并发**不在本闸保护面内**,兜底=B 端事实源(⑥ 的 manifest 不命中即拒)+ 首块前空 Manifest 原子落盘(把无防线窗口压到秒级);路径别名/大小写变体(Windows 目标不区分大小写、symlink 别名)同样登记为接受残余。**不上跨 project 全局 lease**——新机制超纯增量口径(kimi#6/codex#3 取轻方案)。

### 1.2 Agent 描述文本(模板钉死,%d 动态嵌入)

> Relay a LARGE file server-to-server through the broker, or from the broker's own disk to a server — the zero-context big-file path (file bytes stream through the broker's memory only; neither the tool result nor exec_output ever contains file content, only per-chunk metadata). Use it when a file is too big for upload_file's 1 MiB per-file cap (e.g. model weights, GB-scale artifacts), or when the file lives on one server and must land on another (e.g. downloaded on an internet-facing server, delivered to an air-gapped one). Pass to_server_id + to_path (absolute), and either from_server_id + from_path (absolute path on that server) or just from_path (absolute path on the broker host). Chunk size %d bytes. Returns task_id immediately — poll with exec_output(task_id) for per-chunk progress and the final digests, stop with exec_stop(task_id). TRANSFER IS RESUMABLE: interrupted/stopped/failed transfers leave <to_path>.sshmgr-partial + a manifest on the destination; re-running relay_file with the same paths completes only the missing chunks. fresh=true discards them and restarts. VERIFICATION: a fresh (non-resumed) transfer reports file_sha256 — compare with exec sha256sum <to_path> on the destination; a resumed transfer reports the chunk-merkle root instead (per-chunk integrity was verified against the manifest as each chunk was written; independently re-verifying a resumed file requires splitting it into %d-byte chunks, hashing each, and hashing the concatenated digests). Directories are NOT supported — tar on the source first (exec tar czf), relay the tarball, untar on the destination. No sudo: root-owned destination paths are not writable. Space pre-flight: 'space_check'='unavailable' means the destination couldn't report free space (some Windows targets) — proceeding is safe because a full disk just pauses at a chunk boundary and resumes later. Complete story for an offline server: exec_background on the internet-facing server to download, relay_file to the air-gapped one, then exec sha256sum there and compare file_sha256 from exec_output.

## 2. 执行序(钉死)

```
RelayForProfile(ctx, st, tm, projectID, profileID, in RelayInput, chunkBytes int64)
    (out RelayOutput, err error)        —— 放 core.go(UploadForProfile 同文件旁)

preflight(同步段,只做零网络字节移动的廉价检查;两条连接建立后即移交引擎,见"连接生命周期"):
  ① 参数层校验:from_path/to_path 非空且绝对(远程 isAbsRemotePath / 本机 filepath.IsAbs 按 broker OS)
     + 同源同径拒绝
  ② profile gate(denied):from_server_id 非空时须在 profile;to_server_id 必须在 profile。
     双端独立判(任一越权即 denied,优先于一切内容级错误——denied 优先原则)
  ③ 源端 stat:
     - 远程源:ConnectKeepAlive(source) → sftp Stat(from_path) → size/mtime;须常规文件
       (目录/不存在/权限 → error,含 no_credential / hostkey_mismatch / connect_error 全词汇表)
     - 本机源:os.Stat;须常规文件(symlink 解析为 Stat 目标——upload_file 根 symlink 同款)
  ④ 目标端连接:ConnectKeepAlive(dest)
  ⑤ MkdirAll(path.Dir(to_path))(rev1:kimi#2/codex#10——纯 POSIX path.Dir,不踩 upload.go:289-292
     登记的 ToSlash 們;UploadForProfile core.go:418-428 同款先例)
     + Stat(to_path):是目录 → refusal(路径形态错误,非"已存在文件"——后者是合法覆盖对象)
  ⑥ Manifest 判定 + fresh(rev1:先于空间检查——kimi#3/codex#6;目标同目录 <to_path>.sshmgr-manifest.json):
     - 解析防御(rev1,codex#5):Stat 定大小,> 1 MiB → 拒绝(合法清单 ≤ ~20KB@50GB,余量 50×);
       解析后结构校验——version==1、chunk_bytes 匹配、index 唯一且 < ⌈source_size/chunk_bytes⌉、
       sha256 为 64-hex、完成字节 ≤ source_size
     - fresh=true:删 partial + manifest(当作不存在),ResumedChunks=0
     - 存在且校验通过:source_size / source_mtime_unix 与 ③ 不符 → 拒绝(错误文本指引自建 fresh=true
       或还原 SSHMGR_TRANSFER_CHUNK——chunk_bytes 不符同款指引)
     - partial 与 manifest 仅有其一:
       · manifest 无 + partial 有 + **to_path 真名已在**(rev1,codex#9 提交语义):上次提交成功但
         manifest 清理失败的残留 → 拒绝并明示"destination already holds the completed file; the stale
         manifest is debris — remove it or use fresh";**不逼重传**
       · 其余单边态(首块前崩溃已被引擎侧"先建空 manifest"消解,理论上仅剩手动删除场景)→ 拒绝,
         指引 fresh=true
     - 命中:ResumedChunks=已完成块数
  ⑦ 空间预检(StatVFS 目标同目录;rev1:在 MkdirAll 之后——消"父目录不存在→StatVFS 报错被
     fail-open 吞成 unavailable"的混淆,kimi#2):
     口径(rev1:kimi#3/codex#6)= **Available 单值**(Bavail——Bfree 含 root 保留,相加是重复计数)
     ≥ (source_size − 已完成字节) + 一个 chunk 余量;uint64→int64 安全转换。不足 → refusal(不建任务)。
     StatVFS 不支持/报错 → space_check="unavailable" 继续(fail-open,grilling 拍板)
  ⑧ 运行中重复拒绝:扫描本 TM running relay 条目规范化 to 键(§1.1 保护面声明)
  ⑨ Reserve → Insert(BgTaskSpec.Timeout 已钳定=relayRunCap 72h 常量,不走 exec 的
     clampBgTimeout/SSHMGR_BG_RUN_CAP;AuditAction="relay-bg-end"——rev1,§6)→ 审计 relay-bg-start
     → 返回 task_id + 计划元数据

引擎 goroutine(runRelay——rev1 钉死:kimi#5/codex#2 复用 runTask 骨架,exec 闭包泛化为引擎闭包,
     终态纪律零复制:stopReq/timeout/failed 锁内映射、m.closed 抑制、终态 notify、t.cancel() 释放
     WithTimeout、锁外关 client、auditEnd 落笔全继承;进度行一律经 notifyWriter 写 stdout——
     落笔即广播,exec_output 长轮询零额外延迟):
  stage 0(首块前):
    - **原子落空 Manifest**(rev1,codex#7:chunks=[] 的合法清单 tmp+rename 覆盖落盘)——从此
      partial-only 单边态不存在,中断永远双件齐
    - PosixRename 能力探测:对空 manifest 的 tmp→rename 试一次,不支持 → 本连接 sticky 切
      Remove+Rename 回退(rev1,codex#9:能力矩阵统一,manifest 更新与最终提交同一套)
    - 入场即以 O_RDWR|O_CREATE 建 partial(rev1:kimi#8/codex#8——zero-size 零块自然落成空文件)
    - **抽读复核**(grilling 三查之三,rev1 移入异步,codex#14):已完成块取 index 最大者,源端
      Seek+读+sha256 对 Manifest;不符 → 终态 failed("source file changed since the interrupted
      transfer"),零块移动
  逐块(index 升序,跳过已完成):
    源 sftp File.Seek(offset) 读 chunk → io.Copy → 目标 sftp File(partial)Seek(offset) 写;
    源读经**双哈希 TeeReader**:chunkHash(每块重置)+ fileHash(全程不重置——仅当本任务从
    byte 0 连续流到 EOF 才有效,resumed 任务不启用法线,rely merkle)
    **精确长度纪律(rev1,codex#4)**:读出字节数 ≠ min(chunk, size−offset) → error(源被 truncate
    的即时报错,不留静默短块);末块后 EOF 确认(再读 1 字节须 EOF)
    块完成 → 块哈希入 Manifest + Manifest 原子更新(tmp+PosixRename/回退)
    每块一行进度经 notifyWriter 入 stdout
  完成(rev1 提交语义钉死,codex#9):
    - **源 re-stat**:size/mtime 对 preflight ③,不符 → 终态 failed 不提交(72h 窗口内源变更的
      最终防线;残余=同 mtime in-place 重写,rsync 同病,登记接受)
    - rootHash = sha256(按 index 升序串接的各块 32B 摘要);file_sha256 = fileHash 终值(仅
      fresh 全程时存在)
    - partial Close(显式检查,Plan 33 WriteFile 同款)→ **rename partial → to_path = 提交点**
      (PosixRename/回退;此 rename 成功即传输完成)
    - 删 Manifest = best-effort:失败 → 任务仍 done,进度行带警告"stale manifest left behind —
      harmless, remove at leisure"(rev1:rev0 会形成逼 50GB 重传的死态)
    - 终态 done → 末行进度含摘要(root 恒有;file_sha256 fresh 时有)+ 总字节 + 速率
  失败/取消/超时:
    当前块即断(ctx watchdog 关 sftp 同 Upload 模式);Manifest 保留已完成块;终态
    failed/stopped/timeout → agent 重跑 relay_file 同参数即续传

连接生命周期(rev1 钉死,kimi#7):
  preflight ③④ 建立的两条 ConnectKeepAlive **即引擎连接,直接移交**(零重连);Insert 失败
  (manager 已关)→ 双 close + ReleaseReservation(Start tasks.go:691-705 同款双保险,注释照抄
  论证);终态即关、CloseAll 可达即关——bgTask 连接槽扩至两条(§1.1 relay 元数据同批扩展)
```

- **进度行格式**(stdout 通道,前缀宽松钉死):`relay plan: %d bytes, %d chunks (resumed %d), chunk=%d` / `chunk %d/%d ok bytes=%d/%d rate=%s elapsed=%s` / `relay done: root=sha256:%x file_sha256=sha256:%x(total=%d) renamed -> %s`(file_sha256 段仅 fresh 有)。
- **超时语义**:deadline=Insert 时 now+72h;到点引擎收 ctx cancel → 同 stop 语义(chunk 边界停、可续传、终态 timeout)。50GB@5Mbps≈23h、@2Mbps≈58h——72h 常量覆盖到 ADSL 级带宽仍留余量。

## 3. Manifest 协议(B 端盘上格式 = 跨版本协议,ADR 0001)

目标同目录两个文件:

- Partial:`<to_path>.sshmgr-partial`——按 offset 直写的目标半成品,**引擎入场即创建**(零块亦是)。真名只在**全部完成、源 re-stat 通过、根校验通过后**经 rename 出现;真名即"传完"的可见保证(β 语义)。
- Manifest:`<to_path>.sshmgr-manifest.json`

```json
{
  "version": 1,
  "chunk_bytes": 268435456,
  "source_size": 53687091200,
  "source_mtime_unix": 1757126400,
  "chunks": [ {"i": 0, "sha256": "hex…"}, {"i": 3, "sha256": "hex…"} ]
}
```

- `chunks` 仅列**已完成**块(洞=未完成);**stage 0 落空 Manifest(chunks=[])先于任何数据块**(codex#7)。摘要(根/文件)**不落 Manifest**——完成时推导即弃(ADR/CONTEXT 措辞 rev1 同步)。
- **原子写**:先写 `<manifest>.tmp` 再 rename 覆盖(PosixRename,探测不支持则 Remove+Rename 回退——回退窗口内崩溃 = manifest 缺失 + partial 在,⑥ 的单边态分支接住,fresh 指引)。`.tmp` 残留无害(下次启动忽略)。
- **解析防御**(§2⑥):stat 大小 ≤ 1 MiB 才读;结构校验(version/chunk_bytes/index 唯一且有界/64-hex/完成字节 ≤ size)。
- **兼容性承诺(ADR 0001 consequence)**:version 1 字段只增不删不改义;未来版本必须能读 v1 清单续传。改 chunk 网格(改 `chunk_bytes`)= 不兼容,§2⑥ 的 chunk_bytes 不符拒绝就是防线。
- **提交语义(rev1,codex#9)**:最终 rename 成功 = 传输完成(唯一提交点);manifest 删除是 best-effort 清理,失败仅警告。残留态判别:manifest 在 + partial 无 + **真名在** = 上次提交成功的碎片,拒绝新传输并明示(不逼重传);真名不在 = 中断,正常续传路径。
- **mtime 精度边界**:远程源记 SFTP attrs 秒级 mtime(部分服务器无 ns)——"1 秒内同尺寸内容已变"由抽读块复核兜底(grilling 已接受残余)。本机源记 `os.Stat` 的整秒(ModTime().Unix(),与远程同粒度,判定口径统一)。

## 4. env seam(新生产路径必须有 seam——SSHMGR_CACHE_DEK 教训)

| seam | 默认 | 钳制 | 语义 |
|---|---|---|---|
| `SSHMGR_TRANSFER_CHUNK` | `256 << 20` | `[16 << 20, 1 << 30]` | 块大小;不可解析/非正/越界 → 构造**失败**(fail-closed) |
| `SSHMGR_TRANSFER_PARALLEL` | `1` | v1 只接受 `1` 或缺省 | 并行块传输是预留能力:**非 1 → 构造失败**,错误文本注明"reserved for a future version"。名称与终态钳域 `[1,8]` 已冻结,v2 放开取值零迁移 |

- 解析函数 `resolveRelayChunk() / resolveRelayParallel()`(mcpserver 包内,Plan 33 `resolveUploadContentCap` 同款):**接线如实(rev1,codex#13)**——`NewServerFromSource` 构造时解析(fail-closed 拒启动,值嵌入 §1.2 描述 `%d`);serve 模式因 `ServerForProject` **per-project 懒构造**(serve.go 现实),`NewServeRunner` 在 bind 前调同一解析函数做**启动期验证**(非法值 fail-fast),各 project 的 server 构造再各自快照——两读点口径 = Plan 33 §3.1 rev1 同款:进程内多次读均取启动时快照,运行期 env 变更不热生效,重启后自然一致。
- **72h = `relayRunCap` 常量,不设 env、不与 `SSHMGR_BG_RUN_CAP` 联动**。勘误登记:grilling Round 3 曾表述"Plan 32 的 24h 是常量"——实际 `SSHMGR_BG_RUN_CAP` 是 env(tasks.go:129)。不联动理由:relay 时长受物理约束(带宽×字节),exec 受语义约束;一个 env 静默改两个面是坑。要调将来开自己的 seam,进 backlog。

## 5. 资源口径

- **常驻新增**:任务表一个条目(bgTask 同款 + relay 元数据小结构)+ 传输期两条 `ConnectKeepAlive` 长连接(终态即关)。
- **broker 磁盘**:**零读写**(本机源=读源文件本身,不是暂存;fresh 删除发生在远端)。ADR 0001 核心。
- **瞬时内存**:流式 `io.Copy`(内部 32 KiB 级缓冲)+ 双端 SSH 窗口 + 双哈希流式状态——**MiB 级,与文件大小无关**。manifest 解析受 §3 的 1 MiB 上限约束。
- **远端写放大**:200 块(50GB/256MiB)= 200 次小 JSON manifest 覆盖写 + 1 次 rename + 1 次 manifest 删除——相对 50GB 数据可忽略。
- **连接失败面**:两条长连接持小时级——keepalive 由 `ConnectKeepAlive` 承担(Plan 32 已验证姿态);任一连接死亡 → 任务 failed(可续传),无重连自动恢复(重跑=恢复,不留半自动状态机)。

## 6. 审计与 no-leak

- **action**:`relay-bg-start` / `relay-bg-end` 双行(照 `exec-bg-start`/`exec-bg-end` 形态)。**接线(rev1,kimi#4/codex#2)**:`BgTaskSpec` 增 `AuditAction string` 字段,Insert 的 auditEnd 闭包用它替代硬编码字面量 `exec-bg-end`(tasks.go:277-278);exec 路径不传 = 默认 `"exec-bg-end"` **零行为变化**,relay 传 `"relay-bg-end"`。
- **Command 字段**:start 行 = `relay %s:%s -> %s:%s (%d bytes, %d chunks, resumed %d)`(本机源 `%s`=`local`);end 行 = taskID(Insert 现状形态,与 exec-bg-end 对齐,不另造)。路径是路径不是内容;与 exec/upload 的路径暴露同级(既有口径)。
- **statuses**:`ok / denied / no_credential(仅远程源)/ hostkey_mismatch / connect_error / cancelled / error`。空间不足、manifest 结构非法/不符、单边态、目标路径形态错误等 preflight 拒绝均归 `error`(preflight 阶段单行——不建任务,无 end 行);建任务后的失败路径走 end 行。
- **内容零过境(零入审计、零入工具返回、零入 exec_output)**:三面都只有元数据(块号/字节/速率/摘要)。测试反向断言:relay 一个内容形如 secret 的 fixture 后,扫审计表+工具返回+任务输出**无内容片段**。
- **no-leak 继承**:connect 错误经 `sshbroker.Connect`(Plan 31 源头 redactAddr 清洗);SFTP 错误为路径/原因文本,无地址形态预期。断言网扩 relay 全部错误分支。
- **本机源无 1 MiB cap 的安全论证(threat-model 登记全文)**:upload_file 的 1 MiB 单文件上限从来不是安全边界——秘密(私钥/凭据文件)都是 KB 级,1 MiB 内畅通;50GB 级大文件不是秘密。relay_file 本机源移除该 cap 不引入新暴露类。既有面(upload_file 本机读)不变,语义对齐。

## 7. 文档变更

- **agent-tools.md**:relay_file 完整口径——零上下文大文件通道、task_id/exec_output/exec_stop 三件套用法、续传语义(partial+manifest+重跑补块)、fresh、**双摘要验证配方**(fresh=file_sha256 对 sha256sum;resumed=merkle 根,独立复验的切分配方)、tar 目录惯用法、**离线机完整故事**(exec_background 在线机下载 → relay → exec sha256sum 对 file_sha256)、root-no-sudo、space_check unavailable 说明、partial/manifest 弃疗残留的清理责任(owner 手动 rm;真名已在+manifest 残留=无害碎片)。
- **threat-model.md**:§6(传输封顶)加注——relay 无内容过境(元数据 only)、本机源无 cap 论证(§6 全文)、StatVFS fail-open 边界、两个 env seam 登记(含 PARALLEL v1 只接受 1)、B→A 方向 exfil 通道登记(单 owner 拍板:审计留痕即可)、**跨 project 同目标并发的秒级残余窗口登记**(§1.1 保护面收窄)。
- **concepts.md**:Relay/Chunk/Manifest/Partial File/Transfer Task 术语同步(CONTEXT.md 为词汇表源,concepts.md 加指向)。
- **compat-matrix.md**:纯增量(新工具)。发版行留 owner 拍板(Plan 33 同款占位注释)。
- **README / agent-access / scenarios / differences-ledger**:relay 无 ssh 二进制直接对应物(`scp serverA:… serverB:…` 近似但凭据语义完全不同——broker 代理 vs 两端直连),登记 Broker-specific。
- **backlog**:#46「大文件分块续传 API」销项登记;「部署 exe 分发通道债」根因(upload 1 MiB)标记已由 relay 本机源路径解除。

## 8. 测试矩阵

- **单测(mcpserver / RelayForProfile,core_test.go 同层)**:
  - 参数层拒绝:相对 from_path(远程/本机各一)/ 相对 to_path / 同源同径 / 空 to_server_id / **本机源 `/foo` 形态在 Windows 上拒绝**(rev1 判据锚)。
  - denied 双端独立:from 越权(远程源)/ to 越权各一——审计行 status=denied。
  - 源 stat 失败词汇表:不存在 / 是目录 / no_credential / connect_error(remote 源)+ 本机源不存在/是目录。
  - to_path 是目录 → refusal;父目录深路径 MkdirAll 创建成功。
  - 空间分支:不足 → refusal 不建任务(错误文本含 avail/need;**续传场景 avail 按 missing bytes 口径——预置大 partial 后不再误拒**,rev1 锚);StatVFS 报错 → space_check="unavailable" 且任务照建;**父目录刚创建后 StatVFS 正常**(rev1 顺序锚)。
  - Manifest 分支:命中续传(ResumedChunks>0)/ chunk_bytes 不符拒 / size 不符拒 / mtime 不符拒 / **结构非法拒(越界 index/重复 index/非 64-hex)/ 超 1 MiB 拒(rev1)** / partial-manifest 单边拒 + **真名已在的碎片态:拒绝且文案不指引重传(rev1)** / fresh=true 清零(断言 partial+manifest 被删)。
  - 运行中重复:预插 running relay 任务同规范化 to 键 → 拒绝;不同路径变体(多余斜杠)规范化后同键 → 拒绝。
  - 返回值断言:task_id 非空、ChunksTotal=⌈size/chunk⌉、ChunkBytes=解析值、§1.2 描述含实际 chunk 值。
- **sshbroker(新 relay.go + testsshd)**:块管道字节精确(Seek 读→offset 写→读回比对);**精确长度纪律(短读→error)与末块 EOF(rev1)**;双哈希 TeeReader 正确性(chunk 边界重置/不重置两态);Manifest 原子写;PosixRename 探测与 Remove+Rename 回退;**源 re-stat 不符拒绝提交(rev1)**。
- **引擎白盒(tasks 层)**:runRelay 走 runTask 终态纪律——exec_output 长轮询在进度行落笔后被 notifyWriter **即时唤醒**(非等 timer);stop 后 manifest 保留已完成块;CloseAll 关两条连接;**stage 0 抽读复核不符 → failed 零块移动(rev1)**;**auditEnd action=relay-bg-end(rev1,kimi#4 锚)**。
- **e2e(e2e_test.go)**:工具集合等式 11→12;全流程:造文件 → relay_file → exec_output 轮询至 done → 目标 `exec sha256sum` 与末行 **file_sha256** 比对(rev1:不再是根哈希);**续传全流程**:小 chunk 强制多块 → 人为中途 Stop → 断言 partial+manifest 存在 → 重跑 → 只补缺块(源读字节计数断言)→ 字节精确 + rename 发生 + manifest 删除;**首块前中断:双件齐可续传(rev1,codex#7 锚)**;**zero-byte 文件端到端(rev1)**;**broker restart 语义模拟**(任务表清空后重跑同参数 → 凭 manifest 续传)。
- **conformance(真 OpenSSH,双重门控同款)**:双服务器形态 50MB 级实测往返 + 续传演练 + Windows 目标 StatVFS 不可用分支 + **manifest 删除失败注入 → done + 警告行 + 真名在(rev1 提交语义锚)**。
- **eval**:BrokerTools 单源联动自动含第 12 工具;relay agent 用例 + scorer(覆盖:跨机大文件搬运→续传→远端校验闭环)。
- **env seam 解析单测**:CHUNK 非法/非正/越钳三态拒绝 + 合法接受;PARALLEL 非 1 拒绝(v1 语义锚)。
- **no-leak/零内容反向断言**:§6 三面(审计/返回/输出)无内容片段。

## 9. 明确不做(scope 纪律)与登记残余

- **目录递归**(tar 惯用法已写 agent 描述;真痛了进 backlog)。
- **并行块传输**(v1 单流;PARALLEL seam 名与钳域已冻,取值 v2 放开)。
- **broker 侧持久化任务状态 / broker 磁盘暂存**(ADR 0001 双否决,不重议)。
- **TTL 自动清理**(远端时钟不可信 + 后台 sweeper 复杂度;弃疗残留手动 rm)。
- **完成后全文件读回校验**(SSH 层逐包 MAC + 每块 sha256 + 完成时源 re-stat 已覆盖;目标盘静默损坏不在威胁模型——留痕)。
- **跨 project 目标 lease / 路径别名 canonical 解析(rev1 收窄)**:残余窗口=跨 project 并发同目标(秒级,B 端 manifest 兜底)、大小写/别名变体——登记接受,不上新机制。
- **72h 内源 in-place 同 mtime 重写(rev1 收窄)**:re-stat+精确长度+EOF 已挡 truncate/grow/异 mtime 重写;同 mtime 同尺寸原地改字节 = rsync 同病,登记接受。
- **fsync**(SFTP 无标准 fsync 扩展;checked-Close 兜底,Plan 33 同款口径)。
- **download 1 MiB cap / upload_file / upload_content 语义变动**(纯新增,互不动)。
- **断线自动重连**(重跑=恢复;不留半自动状态机)。
- **relay 并发任务数专用 cap**(32 任务表上限自然约束;单 owner 自用)。
- **CLI `sshmgr relay` / 笔记本→B 大文件通道**(批 2 独立 plan;冻约束=复用 Chunk/Manifest 协议,批 2 的 serve 面扩展届时独立安全评审)。
- **72h 的 env seam**(要调开自己的 seam 进 backlog,§4 勘误登记)。

## 10. 验收与发版注记

- **自动化**:§8 全绿(含续传全流程、首块前中断、结构非法 manifest、提交语义、file_sha256 比对、relay-bg-end action、PARALLEL v1 拒绝、三面零内容断言)。
- **owner 真机**(GW 门,销 backlog #46 的实证):A=阿里云 ↔ B=LAN 真实服务器 1–5GB 级实测(50GB 全量视带宽择机)+ **中断续传演练**(真机 kill broker 进程 → 重跑 → 补块完成 + file_sha256 对上)+ Windows 目标(1660Super 系)StatVFS unavailable 分支实测。
- **发版**:纯增量(新工具 + 任务表扩槽 + 文档)。批次 owner 拍板(compat-matrix 占位注释,Plan 33 同款)。发版后回写删占位。
