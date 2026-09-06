# Plan 47 设计：relay_file 大文件中继（服务器↔服务器 / broker 本机→服务器）

> backlog #46 · P0。2026-09-06 grilling 三轮已拍板的决策不在本文重议：**零暂存分块流式**（broker 盘零用户数据残留、内存≈流式缓冲，ADR 0001）、**B 端 Manifest 为续传唯一事实源**（broker 任务表保持内存态不持久化——重启即失、重跑自愈，ADR 0001）、**并入 Plan 32 任务表**（exec_output 轮询进度 / exec_stop 取消）、**72h 常量时长上限**、**v1 单文件**（目录=官方 tar 惯用法）、**MCP 工具先行**（CLI+笔记本→B 通道=批 2 独立 plan，冻约束：复用同一 Chunk/Manifest 协议）、**from_server_id 空=broker 本机盘**（顺带销 upload_file 1 MiB 单文件债）、**双端 profile 闸 + 审计 + 内容零过境**（工具与 exec_output 只回元数据，文件字节绝不进 agent 上下文）、**B→A 外发方向 v1 放开仅审计**、**StatVFS 失败 fail-open**（`space_check:"unavailable"` 知情继续——可续传已把盘满从灾难降级为普通事件）、**续传前置三查**（size+mtime 对 Manifest + 抽读一个已完成块复核）、**partial+rename β 语义**（`<target>.sshmgr-partial` 完成才 rename 真名）、**无 TTL 自动清理**（弃疗残留 owner 手动 exec 清，`--fresh` 清零）、**A 端下载归 exec_background 不管**。本文为实现设计。
> 术语以根目录 `CONTEXT.md` 为准（Relay / Chunk / Manifest / Partial File / Transfer Task / Upload / Download）。

## 0. 目标与缺口

50GB+ 模型权重场景：在线服务器 A（阿里云）上下载好的大文件，要送到真空离线服务器 B（与 A/本机零网络可达，唯一交汇点是 NUC10 broker）。现状四条通道全部不通：

| 通道 | 断点 |
|---|---|
| `download_file` | 1 MiB 前缀截断——大文件从 A 拿不回 broker |
| `upload_file` | 单文件 1 MiB 硬顶——大文件推不进 B；且 LocalPath=broker 本机盘 |
| `upload_content` | 8 MiB 内联且内容过 agent 上下文——大文件反模式 |
| 服务器间直传 | 不存在；且凭据不出 vault，不可能在 A 上种 B 的凭据 |

`relay_file`：broker 内部分块流式中继，源=远程服务器或 broker 本机盘，目标=远程服务器。断点续传锚在接收端 Manifest。= backlog #46「大文件分块续传 API」销项，同时以本机源路径还掉「部署 exe 分发通道」债的根因（upload_file 1 MiB 单文件上限）。

## 1. 工具契约

`relay_file` = **BrokerTools[11]**（第 12 个工具；`internal/mcpserver/server.go` 单源切片追加 + `NewServerFromSource` 对应 `mcp.AddTool`——eval scorer 读同一切片，集合断言自动联动为 12）。

### 1.1 入参 / 出参

```go
// types.go
type RelayInput struct {
    FromServerID string `json:"from_server_id,omitempty" jsonschema:"source server id from list_servers; OMIT or empty = the broker's own local disk (e.g. moving a large file already on the broker host)"`
    FromPath     string `json:"from_path" jsonschema:"absolute path of the SOURCE file: a POSIX absolute path on the from-server (/...), or a broker-host absolute path when from_server_id is empty"`
    ToServerID   string `json:"to_server_id" jsonschema:"destination server id from list_servers"`
    ToPath       string `json:"to_path" jsonschema:"absolute destination path on the destination server (must start with /)"`
    Fresh        bool   `json:"fresh,omitempty" jsonschema:"discard any existing partial/manifest at the destination and restart from byte 0 (default: resume missing chunks when the manifest matches)"`
}

type RelayOutput struct {
    TaskID        string `json:"task_id" jsonschema:"background task id — poll progress with exec_output(task_id), stop with exec_stop(task_id); the transfer is resumable: re-run relay_file with the same paths after any failure/stop"`
    BytesTotal    int64  `json:"bytes_total" jsonschema:"total source bytes"`
    ChunksTotal   int    `json:"chunks_total" jsonschema:"total chunk count"`
    ResumedChunks int    `json:"resumed_chunks" jsonschema:"chunks already complete per the destination manifest (0 on a fresh start)"`
    ChunkBytes    int64  `json:"chunk_bytes" jsonschema:"the chunk size in bytes"`
    SpaceCheck    string `json:"space_check" jsonschema:"destination free-space pre-flight: 'ok' or 'unavailable' (the destination SFTP server does not support statvfs — e.g. some Windows targets; proceeding is safe: a full disk mid-transfer just stops at a chunk boundary and resumes after space is freed)"`
}
```

- **同步部分只有 preflight**：参数校验、双端闸、源 stat、空间预检、Manifest 命中判定、任务插入——然后立即返回 `task_id`（exec_background 同款形态）。传输本体在后台引擎 goroutine。
- **空间不足 = refusal 错误**（`IsError` 返回，不建任务）：错误文本带 free/need 证据。`unavailable` 不是错误——fail-open（grilling 已拍板）。
- **from=broker 本机盘时 from_path 校验**：`filepath.IsAbs`（Windows broker 上 `C:\...` 或 `/...` 均真）。**远程源/目标**：`isAbsRemotePath`（以 `/` 开头，复用 upload_content ① 的判据）。
- **同源同径拒绝**：`from_server_id==to_server_id && from_path==to_path` → 参数层拒绝（no-op 自覆盖）。
- **运行中重复拒绝**（best-effort）：TaskManager 扫描现有 running 任务中 to_server_id+to_path 相同的 relay 条目 → 拒绝（错误指引 exec_stop 或等待）。跨 broker 重启不覆盖（两进程场景不存在——单 broker 进程语义），留痕。

### 1.2 Agent 描述文本（模板钉死）

> Relay a LARGE file server-to-server through the broker, or from the broker's own disk to a server — the zero-context big-file path (file bytes stream through the broker's memory only; neither the tool result nor exec_output ever contains file content, only per-chunk metadata). Use it when a file is too big for upload_file's 1 MiB per-file cap (e.g. model weights, GB-scale artifacts), or when the file lives on one server and must land on another (e.g. downloaded on an internet-facing server, delivered to an air-gapped one). Pass to_server_id + to_path (absolute), and either from_server_id + from_path (absolute POSIX path on that server) or just from_path (absolute path on the broker host). Returns task_id immediately — poll with exec_output(task_id) for per-chunk progress and the final root hash, stop with exec_stop(task_id). TRANSFER IS RESUMABLE: interrupted/stopped/failed transfers leave <to_path>.sshmgr-partial + a manifest on the destination; re-running relay_file with the same paths completes only the missing chunks. fresh=true discards them and restarts. Directories are NOT supported — tar on the source first (exec tar czf), relay the tarball, untar on the destination. No sudo: root-owned destination paths are not writable. Space pre-flight: a 'space_check' of 'unavailable' means the destination couldn't report free space (some Windows targets) — proceeding is safe because a full disk just pauses at a chunk boundary and resumes later. Complete story for an offline server: exec_background on the internet-facing server to download, relay_file to the air-gapped one, then exec sha256sum there and compare the root hash from exec_output.

## 2. 执行序（钉死）

```
RelayForProfile(ctx, st, tm, projectID, profileID, in RelayInput, chunkBytes int64)
    (out RelayOutput, err error)        —— 放 core.go（UploadForProfile 同文件旁）

preflight（同步段，全部完成后才 Insert）:
  ① 参数层校验：from_path/to_path 非空且绝对（本机源 filepath.IsAbs / 远程 isAbsRemotePath）
     + 同源同径拒绝 + encoding 无（无此参数）
  ② profile gate（denied）：from_server_id 非空时须在 profile；to_server_id 必须在 profile。
     双端独立判（任一越权即 denied，优先于一切内容级错误——denied 优先原则）
  ③ 源端 stat：
     - 远程源：ConnectKeepAlive(source) → sftp Stat(from_path) → size/mtime；须为常规文件
       （目录/不存在/权限 → error，含 no_credential / hostkey_mismatch / connect_error 全词汇表）
     - 本机源：os.Stat；须常规文件（symlink 解析为 Stat 目标——upload_file 根 symlink 同款）
  ④ 目标端连接：ConnectKeepAlive(dest)
  ⑤ StatVFS 预检（dest 同目录）：FreeSpace+Available < size + chunkBytes 余量 → refusal（不建任务）；
     StatVFS 不支持/报错 → space_check="unavailable" 继续（fail-open，grilling 拍板）
  ⑥ Manifest 命中判定（目标同目录 <to_path>.sshmgr-manifest.json）：
     - 存在且可解析：version!=1 / chunk_bytes != 当前生效值 / source_size|source_mtime_unix 与 ③ 不符
       → 拒绝（错误文本指引自建 fresh=true 或还原 SSHMGR_TRANSFER_CHUNK）
     - 抽读复核：取已完成块中 index 最大者，源端 Seek+读该块+sha256，与 Manifest 记录不符
       → 拒绝（"source file changed since the interrupted transfer"）
     - partial 与 manifest 仅有其一（状态不一致）→ 拒绝，指引 fresh=true
     - fresh=true：删除 partial+manifest（当作不存在），ResumedChunks=0
     - 命中：ResumedChunks=已完成块数
  ⑦ TaskManager 接入：Reserve → Insert（BgTaskSpec.Timeout 已钳定=relayRunCap 72h 常量，
     不走 exec 的 clampBgTimeout/SSHMGR_BG_RUN_CAP）→ 返回 task_id + 计划元数据
  ⑧ 审计 relay-bg-start（§6）

引擎 goroutine（runRelay，Insert 的 wg 票，bgTask 挂两条连接）:
  逐块（index 升序，跳过已完成）:
    源 sftp File.Seek(offset) 读 chunk → io.Copy → 目标 sftp File（partial，O_RDWR|O_CREATE，
    Seek(offset)）写；TeeReader 同步喂 sha256（源读即算，零额外读盘）
    块完成 → 块哈希入 Manifest + Manifest 原子更新（<manifest>.tmp + PosixRename 覆盖）
    每块一行进度入 stdout RollingBuffer（exec_output 增量可见）
  全块完成:
    rootHash = sha256(按 index 升序串接的各块 32B 摘要)
    partial Close（显式检查，Plan 33 WriteFile 同款）→ PosixRename(partial → to_path)
      （目标端不支持 PosixRename 时回退 Remove+Rename，窗口留痕）
    删 Manifest → 终态 done → 末行进度含 rootHash + 总字节 + 速率
  失败/取消/超时:
    当前块即断（ctx watchdog 关 sftp 同 Upload 模式）；Manifest 保留已完成块；终态
    failed/stopped/timeout → agent 重跑 relay_file 同参数即续传
```

- **连接管理**：`bgTask` 现有单 `client` 槽扩为**至多两条**（实现形态：加 `auxClient` 字段或泛化为切片，T 实现拍板；语义钉死：引擎入场即挂、终态即关、`CloseAll` 可达即关——与现有槽逐字同款）。远端源+远端目标=两条；本机源=一条。全部 `ConnectKeepAlive`（后台任务同款长活姿态）。
- **进度行格式**（stdout 通道，格式宽松钉死前缀）：`relay plan: %d bytes, %d chunks (resumed %d), chunk=%d` / `chunk %d/%d ok bytes=%d/%d rate=%s elapsed=%s` / `relay done: root=sha256:%x total=%d renamed -> %s`。首块完成前 agent 轮询只得 plan 行——exec_output 的 wait 长轮询语义天然适配。
- **超时语义**：deadline=Insert 时 now+72h；到点引擎收 ctx cancel → 同 stop 语义（chunk 边界停、可续传、终态 timeout）。50GB@5Mbps≈23h、@2Mbps≈58h——72h 常量覆盖到 ADSL 级带宽仍留余量。

## 3. Manifest 协议（B 端盘上格式 = 跨版本协议，ADR 0001）

目标同目录两个文件：

- Partial：`<to_path>.sshmgr-partial`——按 offset 直写的目标半成品。真名只在**全部完成且根校验通过后**经 rename 出现；真名即"传完"的可见保证（β 语义）。
- Manifest：`<to_path>.sshmgr-manifest.json`

```json
{
  "version": 1,
  "chunk_bytes": 268435456,
  "source_size": 53687091200,
  "source_mtime_unix": 1757126400,
  "chunks": [ {"i": 0, "sha256": "hex…"}, {"i": 3, "sha256": "hex…"} ]
}
```

- `chunks` 仅列**已完成**块（洞=未完成）。size==0 的源：chunks 空、直接 rename。
- **原子写**：每次块完成先写 `<manifest>.tmp` 再 PosixRename 覆盖——崩溃在 manifest 写中途不产生损坏清单（tmp 残留无害：下次 relay 启动忽略 .tmp）。
- **兼容性承诺（ADR 0001 consequence）**：version 1 字段只增不删不改义；未来版本必须能读 v1 清单续传。改 chunk 网格（改 `chunk_bytes`）= 不兼容，§2⑥ 的 chunk_bytes 不符拒绝就是防线。
- **mtime 精度边界**：远程源记 SFTP attrs 秒级 mtime（部分服务器无 ns 精度）——"1 秒内同尺寸内容已变"由抽读块复核兜底（grilling 已接受残余）。本机源记 `os.Stat` 的整秒（ModTime().Unix()，与远程同粒度，判定口径统一）。

## 4. env seam（新生产路径必须有 seam——SSHMGR_CACHE_DEK 教训）

| seam | 默认 | 钳制 | 语义 |
|---|---|---|---|
| `SSHMGR_TRANSFER_CHUNK` | `256 << 20` | `[16 << 20, 1 << 30]` | 块大小；不可解析/非正/越界 → 构造**失败**（fail-closed） |
| `SSHMGR_TRANSFER_PARALLEL` | `1` | v1 只接受 `1` 或缺省 | 并行块传输是预留能力：**非 1 → 构造失败**，错误文本注明"reserved for a future version"。名称与终态钳域 `[1,8]` 已冻结，v2 放开取值零迁移 |

- 解析函数 `resolveRelayChunk() / resolveRelayParallel()`（mcpserver 包内，Plan 33 `resolveUploadContentCap` 同款）：接线点两处、fail-closed 先于对外服务——`NewServerFromSource` 构造失败拒绝启动；serve 模式 `NewServeRunner` 读一次存字段、`RunServe` 在 bind 前失败退出（不出现"已监听但首个请求 503"半死态）。
- **72h = `relayRunCap` 常量，不设 env、不与 `SSHMGR_BG_RUN_CAP` 联动**。勘误登记：grilling Round 3 曾表述"Plan 32 的 24h 是常量"——实际 `SSHMGR_BG_RUN_CAP` 是 env（tasks.go:129）。不联动的理由反而更硬：relay 时长受物理约束（带宽×字节），exec 受语义约束；一个 env 静默改两个面是坑。要调 Relay 上限将来开自己的 seam，进 backlog。
- chunk 值嵌入 Agent 描述（§1.2 `%d` 同 Plan 33 动态 cap 手法——env 调整后描述如实反映）。

## 5. 资源口径

- **常驻新增**：任务表一个条目（bgTask 同款）+ 传输期两条 `ConnectKeepAlive` 长连接（终态即关）。
- **broker 磁盘**：**零读写**（本机源=读源文件本身，不是暂存）。ADR 0001 核心。
- **瞬时内存**：流式 `io.Copy`（内部 32 KiB 级缓冲）+ 双端 SSH 窗口 + sha256 流式状态——**MiB 级，与文件大小无关**。不存在块级大缓冲（块只是 offset 区间，不是内存对象）。
- **远端写放大**：200 块（50GB/256MiB）= 200 次小 JSON manifest 覆盖写 + 1 次 rename + 1 次 manifest 删除——相对 50GB 数据可忽略。
- **连接失败面**：两条长连接持小时级——keepalive 由 `ConnectKeepAlive` 承担（Plan 32 已验证姿态）；任一连接死亡 → 任务 failed（可续传），无重连自动恢复（重跑=恢复，不留半自动状态机）。

## 6. 审计与 no-leak

- **action**：`relay-bg-start` / `relay-bg-end` 双行（照 `exec-bg-start`/`exec-bg-end` 形态）。
- **Command 字段**：`relay %s:%s -> %s:%s (%d bytes, %d chunks, resumed %d)`——本机源 `%s`=`local`。路径是路径不是内容；与 exec/upload 的路径暴露同级（既有口径）。
- **statuses**：`ok / denied / no_credential（仅远程源）/ hostkey_mismatch / connect_error / cancelled / error`。空间不足拒绝、manifest 不符拒绝、partial/manifest 不一致拒绝均归 `error`（preflight 阶段单行——不建任务，无 end 行；建任务成功的失败路径走 end 行，与 exec-bg 双行制一致）。
- **内容零过境（零入审计、零入工具返回、零入 exec_output）**：三面都只有元数据（块号/字节/速率/根哈希）。测试反向断言：relay 一个内容形如 secret 的 fixture 后，扫审计表+工具返回+任务输出**无内容片段**。这是 download 1 MiB cap 反模式哲学的正面延续——50GB 文件从头到尾不碰 agent 上下文。
- **no-leak 继承**：connect 错误经 `sshbroker.Connect`（Plan 31 源头 redactAddr 清洗）；SFTP 错误为路径/原因文本，无地址形态预期。断言网扩 relay 全部错误分支。
- **本机源无 1 MiB cap 的安全论证（threat-model 登记全文）**：upload_file 的 1 MiB 单文件上限从来不是安全边界——秘密（私钥/凭据文件）都是 KB 级，1 MiB 内畅通；50GB 级大文件不是秘密。relay_file 本机源移除该 cap 不引入新暴露类。既有面（upload_file 本机读）不变，语义对齐。

## 7. 文档变更

- **agent-tools.md**：relay_file 完整口径——零上下文大文件通道、task_id/exec_output/exec_stop 三件套用法、续传语义（partial+manifest+重跑补块）、fresh、tar 目录惯用法、**离线机完整故事**（exec_background 在线机下载 → relay → exec sha256sum 对根哈希）、root-no-sudo、space_check unavailable 说明、partial/manifest 弃疗残留的清理责任（owner 手动 rm）。
- **threat-model.md**：§6（传输封顶）加注——relay 无内容过境（元数据 only）、本机源无 cap 论证（§6 全文）、StatVFS fail-open 边界（可续传降级论证）、两个 env seam 登记（含 PARALLEL v1 只接受 1 的预留语义）、B→A 方向=数据可经在线机出网的 exfil 通道登记（单 owner 自用拍板：审计留痕即可，不加闸）。
- **concepts.md**：Relay/Chunk/Manifest/Partial File/Transfer Task 术语同步（根目录 CONTEXT.md 为词汇表源，concepts.md 加指向）。
- **compat-matrix.md**：纯增量（新工具）。发版行留 owner 拍板（Plan 33 同款占位注释）。
- **README / agent-access / scenarios / differences-ledger**：relay 无 ssh 二进制直接对应物（`scp serverA:… serverB:…` 近似但凭据语义完全不同——broker 代理 vs 两端直连），登记 Broker-specific。
- **backlog**：#46「大文件分块续传 API」销项登记；「部署 exe 分发通道债」根因（upload 1 MiB）标记已由 relay 本机源路径解除。

## 8. 测试矩阵

- **单测（mcpserver / RelayForProfile，core_test.go 同层）**：
  - 参数层拒绝：相对 from_path（远程/本机各一）/ 相对 to_path / 同源同径 / 空 to_server_id。
  - denied 双端独立：from 越权（远程源）/ to 越权各一——审计行 status=denied。
  - 源 stat 失败词汇表：不存在 / 是目录 / no_credential / connect_error（remote 源）+ 本机源不存在/是目录。
  - 空间不足 → refusal 不建任务（错误文本含 free/need）；StatVFS 报错 → space_check="unavailable" 且任务照建。
  - Manifest 分支：命中续传（ResumedChunks>0）/ chunk_bytes 不符拒 / size 不符拒 / mtime 不符拒 / **抽读复核不符拒**（fixture：manifest 哈希故意错）/ partial-manifest 单边存在拒 / fresh=true 清零（断言 partial+manifest 被删）。
  - 运行中重复：预插 running relay 任务同 to → 拒绝。
  - 返回值断言：task_id 非空、ChunksTotal=⌈size/chunk⌉、ChunkBytes=解析值。
- **sshbroker（新 relay.go + testsshd）**：块管道字节精确（Seek 读→offset 写→读回比对）；块哈希流式计算正确性；Manifest 原子写（tmp+rename，中途 kill 不留损坏清单——白盒）；PosixRename 不支持时 Remove+Rename 回退。
- **e2e（e2e_test.go）**：工具集合等式 11→12；全流程：造文件 → relay_file → exec_output 轮询至 done → 目标 `exec sha256sum` 与末行 root 比对；**续传全流程**：小 chunk 强制多块 → 人为中途 Stop → 断言 partial+manifest 存在 → 重跑 → 只补缺块（源读字节计数断言）→ 字节精确 + rename 发生 + manifest 删除。
- **conformance（真 OpenSSH，双重门控同款）**：双服务器形态（两个 testsshd 实例或同实例两路径）50MB 级实测往返 + 续传演练 + Windows 目标 StatVFS 不可用分支（testsshd 配置禁 extended 或 mock）。
- **eval**：BrokerTools 单源联动自动含第 12 工具；relay agent 用例 + scorer（覆盖：跨机大文件搬运→续传→远端校验闭环）。
- **env seam 解析单测**：CHUNK 非法/非正/越钳三态拒绝 + 合法接受；PARALLEL 非 1 拒绝（v1 语义锚）。
- **no-leak/零内容反向断言**：§6 三面（审计/返回/输出）无内容片段。

## 9. 明确不做（scope 纪律）

- **目录递归**（tar 惯用法已写 agent 描述；真痛了进 backlog）。
- **并行块传输**（v1 单流；PARALLEL seam 名与钳域已冻，取值 v2 放开）。
- **broker 侧持久化任务状态 / broker 磁盘暂存**（ADR 0001 双否决，不重议）。
- **TTL 自动清理**（远端时钟不可信 + 后台 sweeper 复杂度；弃疗残留手动 rm）。
- **完成后全文件读回校验**（SSH 层逐包 MAC + 每块 sha256 已覆盖传输完整性；目标盘静默损坏不在威胁模型——留痕）。
- **fsync**（SFTP 无标准 fsync 扩展；checked-Close 兜底，Plan 33 同款口径）。
- **download 1 MiB cap / upload_file / upload_content 语义变动**（纯新增，互不动；1 MiB cap 的反模式哲学由 relay 正面补全而非拆除）。
- **断线自动重连**（重跑=恢复；不留半自动状态机）。
- **relay 并发任务数专用 cap**（32 任务表上限自然约束；单 owner 自用）。
- **CLI `sshmgr relay` / 笔记本→B 大文件通道**（批 2 独立 plan；冻约束=复用 Chunk/Manifest 协议，批 2 的 serve 面扩展届时独立安全评审）。
- **72h 的 env seam**（要调开自己的 seam 进 backlog，§4 勘误登记）。

## 10. 验收与发版注记

- **自动化**：§8 全绿（含续传全流程、抽读复核拒绝、PARALLEL v1 拒绝、三面零内容断言）。
- **owner 真机**（GW 门，销 backlog #46 的实证）：A=阿里云 ↔ B=LAN 真实服务器 1–5GB 级实测（50GB 全量视带宽择机）+ **中断续传演练**（真机 kill broker 进程 → 重跑 → 补块完成 + 根哈希对上）+ Windows 目标（1660Super 系）StatVFS unavailable 分支实测。
- **发版**：纯增量（新工具 + 任务表扩槽 + 文档）。批次 owner 拍板（compat-matrix 占位注释，Plan 33 同款）。发版后回写删占位。
