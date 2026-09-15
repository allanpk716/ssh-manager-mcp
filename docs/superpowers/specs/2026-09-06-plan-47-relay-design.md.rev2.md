# Plan 47 设计:relay_file 大文件中继(服务器↔服务器 / broker 本机→服务器)

> backlog #46 · P0。2026-09-06 grilling 三轮已拍板的决策不在本文重议:**零暂存分块流式**(broker 盘零用户数据残留、内存≈流式缓冲,ADR 0001)、**B 竖 Manifest 为续传唯一事实源**(broker 任务表保持内存态不持久化——重启即失、重跑自愈,ADR 0001)、**并入 Plan 32 任务表**(exec_output 轮询进度 / exec_stop 取消)、**72h 常量时长上限**、**v1 单文件**(目录=官方 tar 惯用法)、**MCP 工具先行**(CLI+笔记本→B 通道=批 2 独立 plan,冻约束:复用同一 Chunk/Manifest 协议)、**from_server_id 空=broker 本机盘**(顺带销 upload_file 1 MiB 单文件债)、**双端 profile 闸 + 审计 + 内容零过境**(工具与 exec_output 只回元数据,文件字节绝不进 agent 上下文)、**B→A 外发方向 v1 放开仅审计**、**StatVFS 失败 fail-open**(`space_check:"unavailable"` 知情继续)、**续传前置三查**(size+mtime 对 Manifest + 抽读一个已完成块复核——抽读在引擎 stage 0)、**partial+rename β 语义**(`<target>.sshmgr-partial` 完成才 rename 真名)、**无 TTL 自动清理**(弃疗残留 owner 手动 exec 清,`--fresh` 清零)、**A 端下载归 exec_background 不管**。本文为实现设计。
> 术语以根目录 `CONTEXT.md` 为准(Relay / Chunk / Manifest / Partial File / Transfer Task / Upload / Download)。
> 本版为第三版(两轮盲评收敛:r1 吸收 kimi 9+codex 15 去重 14 簇;r2 吸收 kimi 10+codex 9 去重 13 簇)。**r2 要点(rev1 修法自身引入的问题占大头)**:①**stage 0 空 Manifest 改条件落盘**——仅 ⑥ 判定无 manifest(fresh/首跑)时执行;续传保留原 manifest 且抽读复核先于一切 manifest 写(rev1 无条件覆写会毁进度+架空三查——kimi#1/codex#2);②**ReserveRelay 持锁原语**——同 TM 内"扫描→Insert"闭区间化,按**工件集**`{to_path, partial, manifest, manifest.tmp}` 相交查重占位(rev1 scan-then-Insert 竞态——kimi#2/codex#3;工件名碰撞=partial 名可以是另一任务的合法目标——codex#4);③**preflight 全错误分支连接所有权钉死**——连接建立到移交引擎之间一切拒绝双 close(rev1 只钉了 Insert 失败——kimi#3/codex#1);④碎片态判别统一——manifest 在+partial 无+真名在=上次提交成功残留,拒绝且不指引重传(rev1 §2⑥ 与 §3 打架——kimi#4);⑤**posix-rename@openssh.com 升格为硬依赖**——stage 0 探测不支持即 fail-closed 拒任务,整个 Remove+Rename 回退矩阵从 spec 砍除(回退窗×200 块=崩溃即进度全毁——kimi#5;OpenSSH 全系含 Win32 端口支持该扩展,兼容面收窄登记);⑥manifest 在+chunks=[]+partial 无=合法自愈态(kimi#6/codex#2);⑦partial 结构校验+**尺寸收敛**——Stat 非常规文件拒/低于最高完成块末尾拒/stage 0 truncate 到 source_size(codex#7);⑧**manifest 解析上限改推导式**——`⌈source_size/chunk_bytes⌉×128B+8KiB`,固定 1 MiB 会拒绝 TB 级合法清单(codex#8);⑨远程路径规范化钉 `path.Clean`(纯 POSIX 命名空间,禁 filepath/ToSlash——反斜杠是合法 POSIX 文件名字符,kimi#7/codex#9);⑩start 行经 AuditStart 闭包在 Insert 锁内先于 goroutine(rev1 字面序会倒挂——kimi#8/codex#6);⑪env seam 口径如实化——每次 server 构造读一次(serve 懒构造现实),runner 启动期仅做验证,跨 project 漂移由 chunk_bytes 闸挡(kimi#9/codex#5);⑫MkdirAll 表述如实——沿用既有原语及其已登记 ToSlash 债,不声称豁免(kimi#10)。评审留底:.xcheck/plan47-r{1,2}-{kimi,codex}.out。

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

- **同步部分只有廉价 preflight**(§2 ①–⑧):全部完成后 Insert、立即返回 `task_id`(exec_background 同款形态)。**抽读复核等一切网络字节移动都在引擎侧**(同步段读 256 MiB 会阻塞 MCP 请求数分钟)。
- **空间不足 = refusal 错误**(`IsError` 返回,不建任务):错误文本带 avail/need 证据。`unavailable` 不是错误——fail-open(grilling 拍板)。
- **路径判据**:
  - **远程源/目标**:`isAbsRemotePath`(core.go:613-620,以 `/` 或盘符 `C:/` 开头——testsshd/Windows 目标一等公民);schema 文案同步提及盘符形态。
  - **本机源**:`filepath.IsAbs` 按 **broker 宿主 OS** 语义——Windows broker 上 `C:\…`/UNC 真、`/foo` **假**(rev0"均真"表述错误);错就错得诚实。
- **同源同径拒绝**:`from_server_id==to_server_id && from_path==to_path` → 参数层拒绝(no-op 自覆盖)。
- **运行中重复拒绝(rev2 重设计)**:经 **ReserveRelay 持锁原语**(§2⑧)按**工件集**查重占位——`{to_path, partial, manifest, manifest.tmp}` 四键 = `to_server_id + "\x00" + path.Clean(工件路径)`,任一与 running/starting relay 的工件集**相交**即拒(工件名碰撞:`…sshmgr-partial` 完全可以是另一任务的合法目标路径——codex#4)。**远程路径只按远程命名空间 `path.Clean` 规范化,禁 `filepath.Clean`/`ToSlash`**(反斜杠是合法 POSIX 文件名字符,`/tmp/a\b` ≠ `/tmp/a/b`——Windows broker 上 filepath 会误合并,kimi#7/codex#9)。**如实声明**:跨 project(serve.go scopedServer per-project)不在保护面内,兜底=B 端事实源 + 首块前空 Manifest;大小写/symlink 别名登记残余(§9)。

### 1.2 Agent 描述文本(模板钉死,%d 动态嵌入)

> Relay a LARGE file server-to-server through the broker, or from the broker's own disk to a server — the zero-context big-file path (file bytes stream through the broker's memory only; neither the tool result nor exec_output ever contains file content, only per-chunk metadata). Use it when a file is too big for upload_file's 1 MiB per-file cap (e.g. model weights, GB-scale artifacts), or when the file lives on one server and must land on another (e.g. downloaded on an internet-facing server, delivered to an air-gapped one). Pass to_server_id + to_path (absolute), and either from_server_id + from_path (absolute path on that server) or just from_path (absolute path on the broker host). Chunk size %d bytes. Returns task_id immediately — poll with exec_output(task_id) for per-chunk progress and the final digests, stop with exec_stop(task_id). TRANSFER IS RESUMABLE: interrupted/stopped/failed transfers leave <to_path>.sshmgr-partial + a manifest on the destination; re-running relay_file with the same paths completes only the missing chunks. fresh=true discards them and restarts. VERIFICATION: a fresh (non-resumed) transfer reports file_sha256 — compare with exec sha256sum <to_path> on the destination; a resumed transfer reports the chunk-merkle root instead (per-chunk integrity was verified against the manifest as each chunk was written; independently re-verifying a resumed file requires splitting it into %d-byte chunks, hashing each, and hashing the concatenated digests). Directories are NOT supported — tar on the source first (exec tar czf), relay the tarball, untar on the destination. No sudo: root-owned destination paths are not writable. Space pre-flight: 'space_check'='unavailable' means the destination couldn't report free space (some Windows targets) — proceeding is safe because a full disk just pauses at a chunk boundary and resumes later. Complete story for an offline server: exec_background on the internet-facing server to download, relay_file to the air-gapped one, then exec sha256sum there and compare file_sha256 from exec_output.

## 2. 执行序(钉死)

```
RelayForProfile(ctx, st, tm, projectID, profileID, in RelayInput, chunkBytes int64)
    (out RelayOutput, err error)        —— 放 core.go(UploadForProfile 同文件旁)

【连接所有权纪律(rev2,kimi#3/codex#1)】③④ 建立的 ConnectKeepAlive 自建立起挂
    defer 双 close;Insert 成功(引擎接管)才解除 defer 并显式移交——此前一切
    return(①-⑧ 的全部拒绝分支)零泄漏。ForwardForProfile 的 err!=nil && cli!=nil
    先例(core.go:664-669)形态。

preflight(同步段,零网络字节移动):
  ① 参数层校验:from_path/to_path 非空且绝对(远程 isAbsRemotePath / 本机 filepath.IsAbs
     按 broker OS)+ 同源同径拒绝
  ② profile gate(denied):from_server_id 非空时须在 profile;to_server_id 必须在 profile。
     双端独立判(任一越权即 denied,优先于一切内容级错误——denied 优先原则)
  ③ 源端 stat:
     - 远程源:ConnectKeepAlive(source) → sftp Stat(from_path) → size/mtime;须常规文件
       (目录/不存在/权限 → error,含 no_credential / hostkey_mismatch / connect_error 全词汇表)
     - 本机源:os.Stat;须常规文件(symlink 解析为 Stat 目标——upload_file 根 symlink 同款)
  ④ 目标端连接:ConnectKeepAlive(dest)
  ⑤ MkdirAll(path.Dir(to_path))(沿用既有 Client.MkdirAll 原语及其已登记 ToSlash 债
     [upload.go:260,rev2 措辞如实]——UploadForProfile core.go:418-428 同款先例;纯 POSIX
     path.Dir 取父)+ Stat(to_path):是目录 → refusal
  ⑥ Manifest 判定 + fresh(先于空间检查;目标同目录 <to_path>.sshmgr-manifest.json):
     - 解析防御:Stat 定大小,> 推导上限(`⌈source_size/chunk_bytes⌉×128B+8KiB`,rev2
       codex#8——固定 1 MiB 会拒绝 16MiB 块×TB 级文件的合法清单)→ 拒绝;解析后结构校验
       ——version==1、chunk_bytes 匹配、index 唯一且 < ⌈source_size/chunk_bytes⌉、sha256
       为 64-hex、完成字节 ≤ source_size
     - fresh=true:删 partial + manifest(当作不存在),ResumedChunks=0
     - 存在且校验通过:source_size / source_mtime_unix 与 ③ 不符 → 拒绝(错误文本指引
       fresh=true 或还原 SSHMGR_TRANSFER_CHUNK——chunk_bytes 不符同款指引)
     - 状态组合判别(判别条件以 §3 为准,rev2 统一——kimi#4):
       · manifest 在 + partial 无 + **真名在** → 上次提交成功、manifest 清理失败的碎片:
         拒绝并明示"destination already holds the completed file; the stale manifest is
         debris — remove it or use fresh",**不逼重传**
       · manifest 在 + chunks==[] + partial 无 → 合法自愈态(rev2,kimi#6/codex#2):首跑
         落清单后建 partial 前崩溃——直接建 partial 续走,ResumedChunks=0
       · partial 在 + manifest 无 → 手动删除等非常态 → 拒绝,指引 fresh=true(引擎侧
         "先建清单后建 partial"的顺序保证该态非本协议产物)
     - 命中:ResumedChunks=已完成块数
     - **partial 结构校验(rev2,codex#7)**:manifest 命中时 Stat partial——非常规文件 → 拒;
       size < 最高已完成块末尾 → 拒(manifest 谎报完成度)
  ⑦ 空间预检(StatVFS 目标同目录;在 MkdirAll 之后):
     口径 = **Available 单值**(Bavail)≥ (source_size − 已完成字节) + 一个 chunk 余量;
     uint64→int64 安全转换。不足 → refusal(不建任务)。不支持/报错 →
     space_check="unavailable" 继续(fail-open,grilling 拍板)
  ⑧ **ReserveRelay(工件集)——rev2 持锁原语(kimi#2/codex#3/#4)**:tm.mu 锁内对四工件键
     {to_path, partial, manifest, manifest.tmp} 查 running/starting relay 工件集,任一相交
     → 拒(错误指引 exec_stop 或等待);否则占位(starting 条目),Insert 同锁转正,
     失败/终态释放——扫描→Insert 闭区间,零竞态窗
  ⑨ Insert(BgTaskSpec.Timeout 已钳定=relayRunCap 72h 常量;AuditAction="relay-bg-end";
     **AuditStart=relay-bg-start 行的落笔闭包——在 Insert 持锁段、引擎 goroutine 启动前
     调用(rev2,kimi#8/codex#6:rev1 字面序"Insert→审计→返回"会与 zero-byte/秒级失败的
     end 行倒挂,违 tasks.go:286-288 顺序钉死)**)→ 返回 task_id + 计划元数据

引擎 goroutine(runRelay——复用 runTask 骨架,exec 闭包泛化为引擎闭包,终态纪律零复制:
     stopReq/timeout/failed 锁内映射、m.closed 抑制、终态 notify、t.cancel() 释放
     WithTimeout、锁外关 client、auditEnd 落笔全继承;进度行一律经 notifyWriter 写 stdout——
     落笔即广播,exec_output 长轮询零额外延迟):
  stage 0(首块前;顺序钉死):
    - **条件落盘(rev2,kimi#1/codex#2)**:仅当 ⑥ 判定"无 manifest"(fresh/首跑)时,原子
      落空 Manifest(chunks=[] 的合法清单 tmp+PosixRename 覆盖);**续传场景绝不写
      manifest——原样保留 k 个已完成块记录**
    - **抽读复核(先于一切 manifest 写,rev2 序钉死)**:已完成块取 index 最大者,源端
      Seek+读+sha256 对 Manifest;不符 → 终态 failed("source file changed since the
      interrupted transfer"),零块移动、原 manifest 完好
    - PosixRename 能力探测:对空 manifest 的 tmp→rename 试一次(**rev2 升格硬依赖,
      kimi#5**:不支持 posix-rename@openssh.com → 终态 failed 明示"destination SFTP
      server lacks posix-rename@openssh.com — relay requires it for atomic manifest
      updates";**Remove+Rename 回退矩阵整体砍除**——回退窗×200 块=崩溃即进度全毁,
      不做概率性原子;OpenSSH 全系含 Win32 端口支持该扩展,兼容面收窄登记 §7/§9)
    - 入场即以 O_RDWR|O_CREATE 建 partial(零块亦是;fresh 路径=重建);**尺寸收敛(rev2,
      codex#7)**:partial Stat size > source_size → truncate(source_size)(清 stale 尾巴)
  逐块(index 升序,跳过已完成):
    源 sftp File.Seek(offset) 读 chunk → io.Copy → 目标 sftp File(partial)Seek(offset) 写;
    源读经**双哈希 TeeReader**:chunkHash(每块重置)+ fileHash(全程不重置——仅当本任务
    从 byte 0 连续流到 EOF 才有效,resumed 任务不启用法线,rely merkle)
    **精确长度纪律**:读出字节数 ≠ min(chunk, size−offset) → error(源 truncate 即时报错);
    末块后 EOF 确认(再读 1 字节须 EOF)
    块完成 → 块哈希入 Manifest + Manifest 原子更新(tmp+PosixRename)
    每块一行进度经 notifyWriter 入 stdout
  完成:
    - **源 re-stat**:size/mtime 对 preflight ③,不符 → 终态 failed 不提交(72h 窗口内源
      变更的最终防线;残余=同 mtime in-place 重写,rsync 同病,登记接受)
    - rootHash = sha256(按 index 升序串接的各块 32B 摘要);file_sha256 = fileHash 终值
      (仅 fresh 全程时存在)
    - partial Close(显式检查)→ **PosixRename(partial → to_path) = 提交点**(此 rename
      成功即传输完成;size 收敛由 stage 0 truncate 保证)
    - 删 Manifest = best-effort:失败 → 任务仍 done,进度行带警告"stale manifest left
      behind — harmless, remove at leisure"
    - 终态 done → 末行进度含摘要(root 恒有;file_sha256 fresh 时有)+ 总字节 + 速率
  失败/取消/超时:
    当前块即断(ctx watchdog 关 sftp 同 Upload 模式);Manifest 保留已完成块;终态
    failed/stopped/timeout → agent 重跑 relay_file 同参数即续传

连接生命周期:
  preflight ③④ 建立的两条 ConnectKeepAlive 即引擎连接,Insert 成功时所有权原子移交
  (defer 解除);⑧ 占位后一切失败(含 Insert 失败/manager 已关)→ 释放工件占位 +
  双 close + (若已 Reserve)ReleaseReservation(Start tasks.go:691-705 同款双保险);
  终态即关、CloseAll 可达即关——bgTask 连接槽扩至两条(与 relay 元数据/工件集同批扩展)
```

- **进度行格式**(stdout 通道,前缀宽松钉死):`relay plan: %d bytes, %d chunks (resumed %d), chunk=%d` / `chunk %d/%d ok bytes=%d/%d rate=%s elapsed=%s` / `relay done: root=sha256:%x file_sha256=sha256:%x(total=%d) renamed -> %s`(file_sha256 段仅 fresh 有)。
- **超时语义**:deadline=Insert 时 now+72h;到点引擎收 ctx cancel → 同 stop 语义(chunk 边界停、可续传、终态 timeout)。50GB@5Mbps≈23h、@2Mbps≈58h——72h 常量覆盖到 ADSL 级带宽仍留余量。

## 3. Manifest 协议(B 端盘上格式 = 跨版本协议,ADR 0001)

目标同目录两个文件:

- Partial:`<to_path>.sshmgr-partial`——按 offset 直写的目标半成品,**引擎入场即创建**(零块亦是;fresh=重建,续传=校验+尺寸收敛)。真名只在**全部完成、源 re-stat 通过、根校验通过后**经 PosixRename 出现;真名即"传完"的可见保证(β 语义)。
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

- `chunks` 仅列**已完成**块(洞=未完成);首跑在**任何数据块之前**落空清单(chunks=[]);摘要(根/文件)不落 Manifest——完成时推导即弃(ADR/CONTEXT 措辞已同步)。
- **原子写 = PosixRename 硬依赖**:先写 `<manifest>.tmp` 再 PosixRename 覆盖;**目标不支持该扩展 → stage 0 探测即拒**(§2;无回退窗)。`.tmp` 残留无害(下次启动忽略)。
- **解析防御**:stat 大小 > `⌈source_size/chunk_bytes⌉×128B+8KiB`(推导上限,随块数伸缩)→ 拒;结构校验(version/chunk_bytes/index 唯一且有界/64-hex/完成字节 ≤ size)。
- **状态组合判别(唯一权威表,§2⑥ 引用此处,rev2 统一)**:

| manifest | partial | 真名 | 判定 |
|---|---|---|---|
| 无 | 无 | 无 | 首跑,fresh 语义 |
| 无 | 有 | 无 | 非常态(手动删?)→ 拒,fresh 指引 |
| 在(chunks=[]) | 无 | 无 | 合法自愈态:建 partial 续走 |
| 在 | 有 | 无 | 中断 → 续传(partial 过结构校验+尺寸收敛) |
| 在 | 无 | 有 | **提交成功碎片**:拒,明示无害+清理指引,不逼重传 |
| 在 | 有 | 有 | 非常态(手动放?)→ 拒,fresh 指引 |
| 无 | 有 | 有 | 非常态 → 拒,fresh 指引 |
| 无 | 无 | 有 | 全新覆盖场景(旧成功传输无碎片)→ 正常 fresh 传输,完成时替换真名 |

- **兼容性承诺(ADR 0001 consequence)**:version 1 字段只增不删不改义;未来版本必须能读 v1 清单续传。改 chunk 网格 = 不兼容,§2⑥ 的 chunk_bytes 拒绝就是防线。
- **mtime 精度边界**:远程源记 SFTP attrs 秒级 mtime;"1 秒内同尺寸内容已变"由抽读块复核兜底(登记残余)。本机源记 `os.Stat` 的整秒(判定口径统一)。

## 4. env seam(新生产路径必须有 seam——SSHMGR_CACHE_DEK 教训)

| seam | 默认 | 钳制 | 语义 |
|---|---|---|---|
| `SSHMGR_TRANSFER_CHUNK` | `256 << 20` | `[16 << 20, 1 << 30]` | 块大小;不可解析/非正/越界 → 构造**失败**(fail-closed) |
| `SSHMGR_TRANSFER_PARALLEL` | `1` | v1 只接受 `1` 或缺省 | 并行块传输是预留能力:**非 1 → 构造失败**,错误文本注明"reserved for a future version"。名称与终态钳域 `[1,8]` 已冻结,v2 放开零迁移 |

- 解析函数 `resolveRelayChunk() / resolveRelayParallel()`(mcpserver 包内,Plan 33 `resolveUploadContentCap` 同款)。**接线口径(rev2 如实,kimi#9/codex#5)**:`NewServerFromSource` **每次构造**时解析(fail-closed 拒启动,值嵌入 §1.2 描述 `%d`);serve 因 `ServerForProject` per-project 懒构造,`NewServeRunner` 在 bind 前调同一解析函数做**启动期验证**(非法值 fail-fast)——进程内多 project 各自在构造时点取值(与 resolveUploadContentCap 完全同形态,Plan 33 §3.1 rev1 口径);运行期改 env 理论上影响之后新建的 project server,**跨 project 续传由 chunk_bytes 闸挡**(不符即拒,指引还原/重启)。不声称"启动时快照"。
- **72h = `relayRunCap` 常量,不设 env、不与 `SSHMGR_BG_RUN_CAP` 联动**。勘误登记:grilling Round 3 曾表述"Plan 32 的 24h 是常量"——实际是 env(tasks.go:129)。不联动理由:relay 时长受物理约束(带宽×字节),exec 受语义约束;一个 env 静默改两个面是坑。要调将来开自己的 seam,进 backlog。

## 5. 资源口径

- **常驻新增**:任务表一个条目(bgTask 同款 + relay 元数据/工件集小结构)+ 传输期两条 `ConnectKeepAlive` 长连接(终态即关)。
- **broker 磁盘**:**零读写**(本机源=读源文件本身,不是暂存;fresh 删除发生在远端)。ADR 0001 核心。
- **瞬时内存**:流式 `io.Copy`(32 KiB 级内部缓冲)+ 双端 SSH 窗口 + 双哈希流式状态——**MiB 级,与文件大小无关**。manifest 解析受推导上限约束。
- **远端写放大**:200 块(50GB/256MiB)= 200 次小 JSON manifest 覆盖写 + 1 次 rename + 1 次 manifest 删除——相对 50GB 数据可忽略。
- **连接失败面**:两条长连接持小时级——keepalive 由 `ConnectKeepAlive` 承担;任一连接死亡 → 任务 failed(可续传),无重连自动恢复(重跑=恢复)。

## 6. 审计与 no-leak

- **action**:`relay-bg-start` / `relay-bg-end` 双行。接线:`BgTaskSpec` 增 `AuditAction string`(Insert 的 auditEnd 闭包用它替代硬编码 `exec-bg-end`[tasks.go:277-278];exec 路径不传 = 默认,**零行为变化**)+ **`AuditStart` 闭包锁内先落 start 行**(§2⑨,顺序钉死)。
- **Command 字段**:start 行 = `relay %s:%s -> %s:%s (%d bytes, %d chunks, resumed %d)`(本机源 `%s`=`local`);end 行 = taskID(Insert 现状形态)。路径是路径不是内容(既有口径)。
- **statuses**:`ok / denied / no_credential(仅远程源)/ hostkey_mismatch / connect_error / cancelled / error`。空间不足、manifest 结构非法/不符、状态组合非常态、目标路径形态错误、posix-rename 缺失等 preflight/引擎拒绝均归 `error`;建任务后的失败路径走 end 行。
- **内容零过境(零入审计、零入工具返回、零入 exec_output)**:三面都只有元数据。测试反向断言:relay 一个内容形如 secret 的 fixture 后,扫审计表+工具返回+任务输出**无内容片段**。
- **no-leak 继承**:connect 错误经 `sshbroker.Connect`(Plan 31 源头 redactAddr 清洗);SFTP 错误为路径/原因文本。断言网扩 relay 全部错误分支。
- **本机源无 1 MiB cap 的安全论证(threat-model 登记全文)**:upload_file 的 1 MiB 单文件上限从来不是安全边界——秘密都是 KB 级,1 MiB 内畅通;50GB 级大文件不是秘密。relay_file 本机源移除该 cap 不引入新暴露类。

## 7. 文档变更

- **agent-tools.md**:relay_file 完整口径——零上下文大文件通道、task_id/exec_output/exec_stop 三件套用法、续传语义、fresh、双摘要验证配方(fresh=file_sha256 对 sha256sum;resumed=merkle 根+切分配方)、tar 目录惯用法、离线机完整故事、root-no-sudo、space_check unavailable、posix-rename 硬依赖(目标须为 OpenSSH 系 sftp)、partial/manifest 弃疗残留清理责任(真名已在+manifest 残留=无害碎片)。
- **threat-model.md**:§6 加注——relay 无内容过境、本机源无 cap 论证、StatVFS fail-open 边界、两个 env seam 登记(含 PARALLEL v1 只接受 1)、B→A exfil 通道登记、跨 project 并发秒级残余窗口登记、**posix-rename 硬依赖的兼容面收窄登记**。
- **concepts.md**:Relay/Chunk/Manifest/Partial File/Transfer Task 术语同步(CONTEXT.md 为源,concepts.md 加指向)。
- **compat-matrix.md**:纯增量(新工具)+ 目标端 posix-rename@openssh.com 硬依赖行。发版行留 owner 拍板(Plan 33 同款占位注释)。
- **README / agent-access / scenarios / differences-ledger**:relay 无 ssh 二进制直接对应物(`scp serverA:… serverB:…` 近似但凭据语义不同),登记 Broker-specific。
- **backlog**:#46「大文件分块续传 API」销项登记;「部署 exe 分发通道债」根因标记已解除。

## 8. 测试矩阵

- **单测(mcpserver / RelayForProfile,core_test.go 同层)**:
  - 参数层拒绝:相对 from_path(远程/本机)/ 相对 to_path / 同源同径 / 空 to_server_id / 本机源 `/foo` 形态在 Windows 上拒绝。
  - denied 双端独立各一——审计行 status=denied。
  - 源 stat 失败词汇表:不存在 / 是目录 / no_credential / connect_error + 本机源同款。
  - to_path 是目录 → refusal;父目录深路径 MkdirAll 创建成功。
  - 空间分支:不足 → refusal(avail/need 文本;**续传按 missing bytes 口径——预置大 partial 不再误拒**);StatVFS 报错 → unavailable 且任务照建;父目录刚建后 StatVFS 正常。
  - Manifest §3 状态表**逐行**用例(8 组合各一,rev2 锚);结构非法拒(越界/重复 index/非 64-hex)/ **推导上限拒(小 chunk×大 size 模拟超限清单数,rev2)**;chunk_bytes/size/mtime 不符拒;fresh=true 清零断言。
  - partial 结构校验:非常规文件拒 / **低于最高完成块末尾拒 / 高于 source_size → stage 0 truncate 断言(rev2)**。
  - ReserveRelay:同工件集并发启动(**真并发调用,非预插**——rev2 锚)只成一败一;**B 目标=A 的 partial/manifest 名的工件碰撞拒(rev2)**;`/tmp/a\b` 与 `/tmp/a/b` 不同键(rev2)。
  - 返回值断言:task_id 非空、ChunksTotal、ChunkBytes、§1.2 描述含实际 chunk 值。
- **sshbroker(新 relay.go + testsshd)**:块管道字节精确;精确长度纪律+末块 EOF;双哈希 TeeReader 两态;Manifest 原子写;**posix-rename 探测拒绝分支(mock 不支持)**;源 re-stat 不符拒绝提交。
- **引擎白盒(tasks 层)**:runRelay 走 runTask 终态纪律——exec_output 长轮询被 notifyWriter 即时唤醒;stop 后 manifest 保留;CloseAll 关两条;**stage 0 抽读不符 → failed 且原 manifest 完好(rev2 锚:kimi#1)**;**续传场景 stage 0 不写 manifest(rev2 锚)**;**AuditStart 锁内先于 goroutine——zero-byte/fast-fail 的 start-before-end 断言(rev2)**;auditEnd action=relay-bg-end;**⑧ 占位后 Insert 失败 → 工件占位释放 + 双连接 close 计数(rev2:全 preflight 拒绝分支 close 计数)**。
- **e2e(e2e_test.go)**:工具集合等式 11→12;全流程 + sha256sum 对 file_sha256;续传全流程(Stop→重跑→只补缺块,源读字节计数断言→字节精确+rename+manifest 删);**首块前中断:双件齐可续传**;**清单后 partial 前中断:自愈态直走(rev2)**;zero-byte 端到端;**manifest 删除失败注入 → done+警告+真名在**;broker restart 语义模拟(任务表清空重跑续传)。
- **conformance(真 OpenSSH,双重门控同款)**:双服务器形态 50MB 级往返 + 续传演练 + Windows 目标 StatVFS unavailable + posix-rename 探测(真 Win32-OpenSSH 目标)。
- **eval**:BrokerTools 单源联动自动含第 12 工具;relay 用例 + scorer(跨机大文件→续传→远端校验闭环)。
- **env seam 解析单测**:CHUNK 三态拒绝+合法接受;PARALLEL 非 1 拒绝。
- **no-leak/零内容反向断言**:三面无内容片段。

## 9. 明确不做(scope 纪律)与登记残余

- **目录递归**(tar 惯用法;真痛了进 backlog)。
- **并行块传输**(v1 单流;PARALLEL seam 已冻,v2 放开)。
- **broker 侧持久化任务状态 / broker 磁盘暂存**(ADR 0001 双否决,不重议)。
- **TTL 自动清理**(弃疗残留手动 rm)。
- **完成后全文件读回校验**(SSH 层逐包 MAC + 每块 sha256 + 完成时源 re-stat 已覆盖;目标盘静默损坏不在威胁模型)。
- **Remove+Rename 回退(rev2 砍除)**:posix-rename 不支持即拒——兼容面收窄(OpenSSH 全系含 Win32 端口支持;非 OpenSSH sftp 目标出局,登记)。
- **跨 project 目标闸 / 路径别名 canonical 解析**:残余=跨 project 并发同目标(秒级,B 端 manifest 兜底)、大小写/symlink 别名——登记接受,不上新机制。
- **72h 内源 in-place 同 mtime 重写**:re-stat+精确长度+EOF 已挡其余;同 mtime 同尺寸原地改 = rsync 同病,登记接受。
- **fsync**(SFTP 无标准 fsync;checked-Close 兜底)。
- **download 1 MiB cap / upload_file / upload_content 语义变动**(纯新增,互不动)。
- **断线自动重连**(重跑=恢复)。
- **relay 并发任务数专用 cap**(32 任务表上限自然约束)。
- **CLI `sshmgr relay` / 笔记本→B 大文件通道**(批 2 独立 plan;复用 Chunk/Manifest 协议,届时独立安全评审)。
- **72h 的 env seam**(进 backlog,§4 勘误登记)。

## 10. 验收与发版注记

- **自动化**:§8 全绿(含状态表逐行、真并发工件闸、续传 stage 0 不写 manifest、start-before-end、提交语义、file_sha256 比对、全分支连接 close 计数)。
- **owner 真机**(GW 门,销 backlog #46 的实证):A=阿里云 ↔ B=LAN 真实服务器 1–5GB 级实测(50GB 全量视带宽择机)+ 中断续传演练(真机 kill broker → 重跑 → 补块完成 + file_sha256 对上)+ Windows 目标(1660Super 系)StatVFS unavailable + posix-rename 实测。
- **发版**:纯增量(新工具 + 任务表扩槽 + 文档)。批次 owner 拍板(compat-matrix 占位注释)。发版后回写删占位。
